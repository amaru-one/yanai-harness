package workflow

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Command request states. A pending request stops the agent's run until a
// human decides it; the run resumes at the same step.
const (
	CommandPending  = "pending"
	CommandApproved = "approved"
	CommandDenied   = "denied"
)

// CommandRequest is one command an agent asked to run.
type CommandRequest struct {
	ID        string   `json:"id"`
	Run       string   `json:"run"`
	Seq       int      `json:"seq"`
	Role      string   `json:"role"`
	Args      []string `json:"args"`
	Dir       string   `json:"dir"`
	Reason    string   `json:"reason"`
	Timeout   int      `json:"timeout_seconds,omitempty"`
	Status    string   `json:"status"`
	Always    bool     `json:"always,omitempty"`
	Note      string   `json:"note,omitempty"`
	CreatedAt string   `json:"created_at"`
	DecidedAt string   `json:"decided_at,omitempty"`
}

// CommandID identifies the request made by one agent step, so a replayed
// step finds its own request instead of creating another.
func CommandID(run string, seq int) string {
	return "C-" + Digest(fmt.Sprintf("%s|%d", run, seq))[:12]
}

// commandKey compares commands for "approve always": same arguments, same
// directory.
func commandKey(args []string, dir string) string {
	raw, _ := json.Marshal(args)
	return dir + "\x00" + string(raw)
}

func scanCommand(row interface{ Scan(...any) error }) (CommandRequest, error) {
	var c CommandRequest
	var raw string
	var always int
	if err := row.Scan(&c.ID, &c.Run, &c.Seq, &c.Role, &raw, &c.Dir, &c.Reason, &c.Timeout, &c.Status, &always, &c.Note, &c.CreatedAt, &c.DecidedAt); err != nil {
		return c, err
	}
	c.Always = always == 1
	return c, json.Unmarshal([]byte(raw), &c.Args)
}

const commandColumns = `id,run,seq,role,command,dir,reason,timeout_seconds,status,always,note,created_at,decided_at`

// AutoApproveRule pre-approves commands whose arguments match Args
// position by position ("*" any text, "?" one character); a final "..."
// accepts any further arguments. Only the operator writes these rules.
type AutoApproveRule struct {
	Args        []string `json:"args"`
	Description string   `json:"description"`
}

// shells never match a rule: a script's content is not in its arguments'
// shape, so it always needs a human. The one exception is a syntax check
// ("bash -n FILE"), which parses the file without running it.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true, "ksh": true, "csh": true, "tcsh": true}

func shellRun(args []string) bool {
	return len(args) > 0 && shells[path.Base(args[0])] && !(len(args) == 3 && args[1] == "-n")
}

// Validate reports a rule that could not match safely.
func (r AutoApproveRule) Validate() error {
	if len(r.Args) == 0 || r.Args[0] == "" || r.Args[0] == "..." {
		return errors.New("auto_approve rule needs a program as its first argument")
	}
	if strings.TrimSpace(r.Description) == "" {
		return fmt.Errorf("auto_approve rule %v needs a description", r.Args)
	}
	if shellRun(r.Args) || strings.ContainsAny(r.Args[0], "*?[") {
		return fmt.Errorf("auto_approve rule %v: the program must be named exactly and cannot be a shell", r.Args)
	}
	for i, a := range r.Args {
		if a == "..." && i != len(r.Args)-1 {
			return fmt.Errorf("auto_approve rule %v: \"...\" is only allowed last", r.Args)
		}

	}
	return nil
}

// argMatch matches one whole argument against a pattern in which "*"
// stands for any text (slashes included) and "?" for one character.
func argMatch(pattern, arg string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(arg)
}

// Matches reports whether args is pre-approved by the rule.
func (r AutoApproveRule) Matches(args []string) bool {
	if r.Validate() != nil || len(args) == 0 || shellRun(args) {
		return false
	}
	for i, pattern := range r.Args {
		if pattern == "..." {
			return true
		}
		if i >= len(args) {
			return false
		}
		if !argMatch(pattern, args[i]) {
			return false
		}
	}
	return len(args) == len(r.Args)
}

// RequestCommand records a request as pending, or returns the existing
// request of the same step. A request whose exact command was approved
// "always" earlier in the cycle, or that matches an operator auto-approval
// rule, is recorded as approved at once.
func (s *Store) RequestCommand(cycle int, run string, seq int, role string, args []string, dir, reason string, timeout int, rules []AutoApproveRule) (CommandRequest, error) {
	id := CommandID(run, seq)
	if existing, err := s.Command(cycle, id); err == nil {
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return CommandRequest{}, err
	}
	status, note, decided := CommandPending, "", ""
	all, err := s.Commands(cycle)
	if err != nil {
		return CommandRequest{}, err
	}
	for _, c := range all {
		if c.Always && c.Status == CommandApproved && commandKey(c.Args, c.Dir) == commandKey(args, dir) {
			status, note, decided = CommandApproved, "approved always with "+c.ID, now()
		}
	}
	if status == CommandPending {
		for _, rule := range rules {
			if rule.Matches(args) {
				status, note, decided = CommandApproved, "auto-approved by rule: "+rule.Description, now()
				break
			}
		}
	}
	raw, _ := json.Marshal(args)
	tx, err := s.db.Begin()
	if err != nil {
		return CommandRequest{}, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO workflow_command_requests(project,cycle,id,run,seq,role,command,dir,reason,timeout_seconds,status,note,created_at,decided_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.project, cycle, id, run, seq, role, string(raw), dir, reason, timeout, status, note, now(), decided); err != nil {
		return CommandRequest{}, err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "command.requested", Payload: id + " " + string(raw)}); err != nil {
		return CommandRequest{}, err
	}
	if status == CommandApproved {
		if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "command.approved", Payload: id + " " + note}); err != nil {
			return CommandRequest{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return CommandRequest{}, err
	}
	return s.Command(cycle, id)
}

// Command returns one request; sql.ErrNoRows when it does not exist.
func (s *Store) Command(cycle int, id string) (CommandRequest, error) {
	return scanCommand(s.db.QueryRow(`SELECT `+commandColumns+` FROM workflow_command_requests WHERE project=? AND cycle=? AND id=?`, s.project, cycle, id))
}

// Commands lists a cycle's requests in the order they were made.
func (s *Store) Commands(cycle int) ([]CommandRequest, error) {
	rows, err := s.db.Query(`SELECT `+commandColumns+` FROM workflow_command_requests WHERE project=? AND cycle=? ORDER BY created_at, id`, s.project, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommandRequest
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PendingCommands lists the requests still waiting for a human decision.
func (s *Store) PendingCommands(cycle int) ([]CommandRequest, error) {
	all, err := s.Commands(cycle)
	if err != nil {
		return nil, err
	}
	var out []CommandRequest
	for _, c := range all {
		if c.Status == CommandPending {
			out = append(out, c)
		}
	}
	return out, nil
}

// DecideCommand records the human's decision on a pending request. always
// approves the same command (arguments and directory) for the rest of the
// cycle. A denial needs a note: it is what the agent reads.
func (s *Store) DecideCommand(cycle int, id string, approve, always bool, note string) error {
	if !approve && strings.TrimSpace(note) == "" {
		return errors.New("a denial requires --note: the agent reads it to choose another approach")
	}
	if !approve && always {
		return errors.New("--always only applies to an approval")
	}
	status := CommandDenied
	if approve {
		status = CommandApproved
	}
	flag := 0
	if always {
		flag = 1
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE workflow_command_requests SET status=?,always=?,note=?,decided_at=? WHERE project=? AND cycle=? AND id=? AND status=?`, status, flag, note, now(), s.project, cycle, id, CommandPending)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("command request %s not found or already decided", id)
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorHuman, Type: "command." + status, Payload: strings.TrimSpace(id + " " + note)}); err != nil {
		return err
	}
	return tx.Commit()
}
