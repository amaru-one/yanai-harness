// yanai command: orchestrates a team of agents (Product Owner, DB Architect,
// Engineer and Designer) over OpenRouter to develop the teaching app.
//
// Nothing gets implemented without a person's explicit approval.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/openrouter"
	"github.com/yanai/yanai-harness/internal/repoctx"
	"github.com/yanai/yanai-harness/internal/repository"
	"github.com/yanai/yanai-harness/internal/team"
	"github.com/yanai/yanai-harness/internal/templates"
	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

const usage = `yanai — agent team for the teaching app

USAGE
  yanai <command> [options]

COMMANDS
  init      [--repo path]   Creates or upgrades the workspace (config, prompts, context)
  analyze --privacy-reviewed <file|->   Validate reviewed evidence and decide direction
  discuss   [--retry-unresolved]   Team working session and the PO's consolidated plan
  status    [--json] [--attempts]   Shows where the current cycle stands
  review                    Prints the full executable contract and cycle budget
  policy    [--note ...]     Shows budget; with a note adopts configured policy explicitly
  invalidate --note ...     Revokes approval so discuss can prepare a revised plan
  reconcile-attempt --id ID --cost-usd N --tokens N --reference ...
  approve   [--note ...]    Human gate: authorizes implementation
  reject    --note "..."    Sends the plan back to the team with a reason
  run       [--task ID] [--retry-unresolved]   Runs the approved tasks (only after 'approve')
  import    --legacy         Imports file-era cycles into the workflow store, read-only
  context   [--files a,b]   Prints the repo index (or specific files)

GLOBAL OPTIONS
  --ws <path>   Workspace (defaults to ./yanai-workspace or $YANAI_WS)

ENVIRONMENT VARIABLES
  OPENROUTER_API_KEY   Your OpenRouter key
  YANAI_MOCK=1         Mocks the responses: tests the flow without spending the key
  YANAI_NO_REPO=1      Skips reading the repository

FLOW
  analyze → PROPOSE_CHANGE: discuss → awaiting_approval → approve → run
          → NO_CHANGE_NEEDED | NEEDS_EVIDENCE | OUT_OF_SCOPE | BLOCKED_BY_BASELINE:
            closes with a report. Only the first of these says the app is enough.
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return nil
	}
	command := os.Args[1]
	args := os.Args[2:]

	switch command {
	case "-h", "--help", "help", "ayuda":
		fmt.Print(usage)
		return nil
	case "init":
		return cmdInit(args)
	case "analyze":
		return cmdAnalyze(args)
	case "discuss":
		return cmdDiscuss(args)
	case "status":
		return cmdStatus(args)
	case "review":
		return cmdReview(args)
	case "policy":
		return cmdPolicy(args)
	case "invalidate":
		return cmdInvalidate(args)
	case "reconcile-attempt":
		return cmdReconcileAttempt(args)
	case "approve":
		return cmdApprove(args)
	case "reject":
		return cmdReject(args)
	case "run":
		return cmdRun(args)
	case "import":
		return cmdImport(args)
	case "context":
		return cmdContext(args)
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command: %s", command)
	}
}

func wsPath(fs *flag.FlagSet) *string {
	def := os.Getenv("YANAI_WS")
	if def == "" {
		def = "yanai-workspace"
	}
	return fs.String("ws", def, "workspace")
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := wsPath(fs)
	repo := fs.String("repo", "", "path to the teaching app repository")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Resolve and validate before creating/upgrading any workspace files.
	var repoConfig config.Repo
	if _, err := os.Stat(filepath.Join(*path, "yanai.config.json")); err == nil {
		cfg, err := config.Load(*path)
		if err != nil {
			return err
		}
		repoConfig = cfg.Repo
	} else if !os.IsNotExist(err) {
		return err
	}
	if *repo != "" {
		repoConfig.Path = *repo // --repo is relative to the invocation directory
	} else if repoConfig.Path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		executable, _ := os.Executable()
		repoConfig.Path, err = repository.DiscoverSibling(cwd, filepath.Dir(executable))
		if err != nil {
			return err
		}
	}
	target, err := repository.Open(repoConfig, *path)
	if err != nil {
		return err
	}
	if _, err := target.Snapshot(); err != nil {
		return err
	}

	if err := os.MkdirAll(*path, 0o755); err != nil {
		return err
	}
	results, fromVersion, err := templates.Extract(*path)
	if err != nil {
		return err
	}
	for _, d := range []string{"interviews", "cycles"} {
		if err := os.MkdirAll(filepath.Join(*path, d), 0o755); err != nil {
			return err
		}
	}

	if err := config.SetRepoPath(*path, target.Root); err != nil {
		return err
	}

	abs, _ := filepath.Abs(*path)
	fmt.Printf("Workspace ready at %s\n", abs)
	fmt.Printf("Application repository: %s (module: %s)\n", target.Root, repository.ModuleDir)
	printTemplateResults(results, fromVersion)
	fmt.Printf(`
Next steps:
  1. Edit context/alcance.md — this is what the Product Owner uses to reject
     what falls outside the project.
  2. Review yanai.config.json: repo, models, and explicit execution limits, prices and checks.
  3. export OPENROUTER_API_KEY=sk-or-...
  4. yanai analyze interviews/my-interview.md
`)
	return nil
}

// printTemplateResults reports what init did to each template file. A conflict
// is the case worth reading: the user's file was kept and ours is beside it.
func printTemplateResults(results []templates.Result, fromVersion int) {
	byHow := map[templates.Disposition][]templates.Result{}
	for _, r := range results {
		byHow[r.How] = append(byHow[r.How], r)
	}

	switch {
	case fromVersion < 0 && len(byHow[templates.Created]) == len(results):
		fmt.Printf("Templates v%d\n", templates.TemplateVersion)
	case fromVersion < 0:
		fmt.Printf("\nThis workspace predates template versioning, so there is no record of\n"+
			"which files you edited. Nothing was overwritten. Templates now at v%d.\n",
			templates.TemplateVersion)
	case fromVersion != templates.TemplateVersion:
		fmt.Printf("\nTemplates v%d -> v%d\n", fromVersion, templates.TemplateVersion)
	default:
		fmt.Printf("\nTemplates v%d (no change)\n", templates.TemplateVersion)
	}

	for _, s := range []struct {
		how   templates.Disposition
		label string
	}{
		{templates.Created, "created"},
		{templates.Updated, "updated (you hadn't edited these)"},
		{templates.Conflict, "changed upstream — yours kept"},
	} {
		rs := byHow[s.how]
		if len(rs) == 0 {
			continue
		}
		fmt.Printf("\n  %s:\n", s.label)
		for _, r := range rs {
			fmt.Printf("    %s\n", r.Path)
			if r.New != "" {
				fmt.Printf("      -> wrote %s\n", r.New)
			}
		}
	}
	if n := len(byHow[templates.Unchanged]); n > 0 {
		fmt.Printf("\n  unchanged: %d file(s)\n", n)
	}
	if n := len(byHow[templates.Conflict]); n > 0 {
		fmt.Printf("\nMerge the .new file(s) into yours, then delete them.\n")
	}
}

// openWorkspace is the entry point for every command that reads or mutates
// cycle state: it binds the application repository, attaches this
// workspace's workflow store (creating it on first use), and returns a
// cleanup func the caller must defer. Attaching the store also takes the
// workspace writer lock for the returned Runner's lifetime, reconciles any
// claim or attempt left behind by a process that died, and refuses outright
// if file-era cycles exist that this workspace has never imported — see
// attachStore. It also binds an OpenRouter client, so it requires an API key
// (or YANAI_MOCK=1) even for a command that turns out not to call a model —
// analyze, discuss and run all do. Approve and Reject never do, and use
// bindWorkspace instead so the human gate doesn't need a key to operate.
func openWorkspace(path string) (*team.Runner, func(), error) {
	r, cleanup, err := bindWorkspace(path)
	if err != nil {
		return nil, nil, err
	}
	cli, err := openrouter.New(r.Cfg)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if cli.IsMock() {
		fmt.Fprintln(os.Stderr, "⚠  YANAI_MOCK=1: mock responses, OpenRouter is not being called.")
	}
	r.Client = cli
	return r, cleanup, nil
}

// bindWorkspace does everything openWorkspace does except bind a model
// provider: config, repository binding, and the workflow store (creating it
// on first use, reconciling abandoned claims/attempts, refusing unimported
// legacy cycles). Used directly by approve/reject, which never call a model
// and so must not require an API key just to record a human decision.
func bindWorkspace(path string) (*team.Runner, func(), error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	target, err := repository.Open(cfg.Repo, path)
	if err != nil {
		return nil, nil, err
	}
	cfg.Repo.Path = target.Root
	w, err := ws.Open(path)
	if err != nil {
		return nil, nil, err
	}
	cleanup, err := attachStore(w, cfg, false)
	if err != nil {
		return nil, nil, err
	}
	return &team.Runner{Cfg: cfg, Workspace: w}, cleanup, nil
}

// attachStore opens (creating if needed) w's workflow.db, takes the
// workspace writer lock for the duration, and reconciles what a dead
// process may have left behind: an expired ticket claim goes back to
// pending, and an attempt still marked in_flight is stamped unknown — cost
// unknown, never assumed zero — for run/discuss to refuse on
// (checkUnresolvedAttempts) until the operator explicitly acknowledges it.
//
// Unless allowLegacy, it also refuses when this workspace has file-era
// cycles no one has run 'yanai import --legacy' on yet: letting most
// commands quietly work around them is what would let an unverified
// pre-Step-5 cycle drift back into the live flow unnoticed.
func attachStore(w *ws.Workspace, cfg *config.Config, allowLegacy bool) (func(), error) {
	unlock, err := w.LockWriter()
	if err != nil {
		return nil, err
	}
	project := strings.TrimSpace(cfg.Project)
	if project == "" {
		project = filepath.Base(w.Root)
	}
	store, err := workflow.OpenStore(filepath.Join(w.Root, "workflow.db"), project)
	if err != nil {
		unlock()
		return nil, err
	}
	w.Store = store
	cleanup := func() { store.Close(); unlock() }

	if !allowLegacy {
		legacy, err := w.LegacyCycles()
		if err != nil {
			cleanup()
			return nil, err
		}
		if len(legacy) > 0 {
			cleanup()
			return nil, fmt.Errorf("%d legacy, file-era cycle(s) predate this workspace's store and are not yet imported (%v); run: yanai import --legacy", len(legacy), legacy)
		}
	}
	if _, err := store.ExpireClaims(); err != nil {
		cleanup()
		return nil, err
	}
	if err := store.RecoverSessions(); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := store.ReconcileAttempts(); err != nil {
		cleanup()
		return nil, err
	}
	if err := reconcileArtifacts(store, w.Root); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

// reconcileArtifacts resolves every pending artifact left by a process that
// died between committing the row and finishing the publish:
//
//	file present, hash matches    -> flip to published (the file made it,
//	                                  only the final store write didn't)
//	file absent                   -> drop the pending row (nothing to adopt)
//	file present, hash mismatches -> report and leave pending; never adopt
//
// Never promoting an unverified file into looking authoritative mirrors the
// same rule legacy import already follows.
func reconcileArtifacts(store *workflow.Store, root string) error {
	pending, err := store.PendingArtifacts()
	if err != nil {
		return err
	}
	for _, a := range pending {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(a.Path)))
		switch {
		case os.IsNotExist(err):
			if err := store.DropPendingArtifact(a.Cycle, a.RefID); err != nil {
				return err
			}
		case err != nil:
			return err
		case contentHash(data) == a.SHA256:
			if _, err := store.PublishArtifact(a.Cycle, a.RefID, a.SHA256); err != nil {
				return err
			}
		default:
			fmt.Fprintf(os.Stderr, "⚠  artifact %s (cycle %03d) on disk does not match its recorded hash; left pending for a human to look at\n", a.RefID, a.Cycle)
		}
	}
	return nil
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func withContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Fprintln(os.Stderr, "\ncancelling…")
		cancel()
	}()
	return ctx, cancel
}

type redactionTerms []string

func (r *redactionTerms) String() string { return "" }
func (r *redactionTerms) Set(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("--redact requires a nonblank identifier")
	}
	*r = append(*r, s)
	return nil
}

func cmdAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	path := wsPath(fs)
	sourceID := fs.String("source-id", "", "opaque source identifier; do not use a person's name")
	date := fs.String("date", "", "interview date YYYY-MM-DD; omit if unknown")
	reviewed := fs.Bool("privacy-reviewed", false, "source reviewed for personal/indirect identifiers")
	technical := fs.Bool("technical-enabler", false, "input is an explicit engineering finding, not teacher demand")
	var redactions redactionTerms
	fs.Var(&redactions, "redact", "known personal identifier to remove; repeat as needed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("missing the file with the interview notes\n  yanai analyze interviews/teacher-01.md\n  (use '-' to read from standard input)")
	}

	var text string
	var err error
	if fs.Arg(0) == "-" {
		b, e := io.ReadAll(os.Stdin)
		text, err = string(b), e
	} else {
		var b []byte
		b, err = os.ReadFile(fs.Arg(0))
		text = string(b)
	}
	if err != nil {
		return err
	}
	origin := workflow.Product
	if *technical {
		origin = workflow.TechnicalEnabler
	}
	sourceName := fs.Arg(0)
	if sourceName != "-" {
		sourceName, err = filepath.Abs(sourceName)
		if err != nil {
			return err
		}
	}
	intake, err := workflow.NewIntake(text, workflow.IntakeOptions{ID: *sourceID, Name: sourceName, Date: *date, Origin: origin, PrivacyReviewed: *reviewed, Redactions: redactions})
	if err != nil {
		return err
	}

	r, cleanup, err := openWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := withContext()
	defer cancel()

	st, err := r.Analyze(ctx, text, intake)
	if err != nil {
		return err
	}

	dir := r.Workspace.CycleDir(st.Cycle)
	fmt.Printf("\nCycle %03d — Product Owner verdict: %s\n", st.Cycle, st.Verdict)
	if team.IsTerminalVerdict(st.Verdict) {
		fmt.Printf("%s\n", verdictMeaning(st.Verdict))
		fmt.Printf("Report: %s\n", filepath.Join(dir, "02-propuesta.md"))
		return nil
	}
	fmt.Printf("Proposal: %s\n", filepath.Join(dir, "02-propuesta.md"))
	fmt.Printf("\nNext step:  yanai discuss\n")
	return nil
}

func cmdDiscuss(args []string) error {
	fs := flag.NewFlagSet("discuss", flag.ExitOnError)
	path := wsPath(fs)
	retryUnresolved := fs.Bool("retry-unresolved", false, "acknowledge an interrupted prior model call (cost unknown) and proceed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, cleanup, err := openWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := withContext()
	defer cancel()

	st, err := r.Discuss(ctx, *retryUnresolved)
	if err != nil {
		return err
	}
	dir := r.Workspace.CycleDir(st.Cycle)
	if team.IsTerminalVerdict(st.Verdict) {
		fmt.Printf("Cycle %03d — %s\n%s\nReport: %s\n", st.Cycle, st.Verdict, verdictMeaning(st.Verdict), filepath.Join(dir, "04-plan.md"))
		return nil
	}
	fmt.Printf("\nCycle %03d — plan consolidated with %d tasks.\n", st.Cycle, len(st.Tasks))
	fmt.Printf("Discussion: %s\n", filepath.Join(dir, "03-discusion.md"))
	fmt.Printf("Plan:       %s\n", filepath.Join(dir, "04-plan.md"))
	printTasks(st)
	fmt.Printf("\n⏸  STOPPED: nothing gets implemented without your approval.\n")
	fmt.Printf("   Read the plan and then:\n     yanai approve\n     yanai reject --note \"why\"\n")
	return nil
}

// cmdStatus is deliberately the one command that keeps working with no
// store, no valid repo binding and no live provider: it's what an operator
// reaches for exactly when something else is broken. It attaches the store
// best-effort (allowLegacy: status is also how a legacy, un-imported cycle
// gets inspected in the first place) and falls back to reading state.json
// directly whenever that isn't possible.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	path := wsPath(fs)
	asJSON := fs.Bool("json", false, "print the raw cycle state as JSON")
	showAttempts := fs.Bool("attempts", false, "list unresolved model-call attempts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	w, err := ws.Open(*path)
	if err != nil {
		return err
	}
	if cfg, err := config.Load(*path); err == nil {
		if cleanup, err := attachStore(w, cfg, true); err == nil {
			defer cleanup()
		} else {
			fmt.Fprintf(os.Stderr, "⚠  workflow store unavailable, showing file state only: %v\n", err)
		}
	}

	if *showAttempts {
		return printUnresolvedAttempts(w)
	}

	st, err := w.LoadState()
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("Cycle:   %03d\n", st.Cycle)
	fmt.Printf("Phase:   %s\n", st.Phase)
	if st.SchemaVersion != "1" {
		fmt.Println("Legacy cycle: read-only. Re-import 00-entrada.md with analyze --privacy-reviewed; old deliverables remain unverified.")
	}
	if st.Verdict != "" {
		fmt.Printf("Verdict: %s\n", st.Verdict)
	}
	if w.Store != nil {
		if budget, err := w.Store.Budget(st.Cycle); err == nil {
			data, _ := json.MarshalIndent(budget, "", "  ")
			fmt.Printf("Budget:\n%s\n", data)
		}
	}
	fmt.Printf("Folder:  %s\n", w.CycleDir(st.Cycle))
	printTasks(st)
	fmt.Printf("\nHistory:\n")
	for _, e := range st.History {
		line := fmt.Sprintf("  %s  %s", e.When.Format("2006-01-02 15:04"), e.What)
		if e.Who != "" {
			line += " (" + e.Who + ")"
		}
		if e.Note != "" {
			line += " — " + e.Note
		}
		fmt.Println(line)
	}
	fmt.Printf("\n%s\n", suggestion(st))
	return nil
}

func printUnresolvedAttempts(w *ws.Workspace) error {
	if w.Store == nil {
		fmt.Println("No workflow store attached; nothing to report.")
		return nil
	}
	unresolved, err := w.Store.UnreconciledAttempts()
	if err != nil {
		return err
	}
	if len(unresolved) == 0 {
		fmt.Println("No unresolved attempts.")
		return nil
	}
	fmt.Printf("%d attempt(s) with unresolved billing, usage, or dispatch:\n\n", len(unresolved))
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tCYCLE\tTICKET\tROLE\tSTATE\tCOST KNOWN\tUSAGE KNOWN")
	for _, a := range unresolved {
		fmt.Fprintf(tw, "  %s\t%03d\t%s\t%s\t%s\t%t\t%t\n", a.ID, a.Cycle, a.TicketID, a.Role, a.State, a.CostKnown, a.UsageKnown)
	}
	tw.Flush()
	fmt.Println("\nReconcile missing billing/usage with reconcile-attempt, then acknowledge uncertain dispatch using --retry-unresolved.")
	return nil
}

// verdictMeaning renders a terminal verdict in the user's words. Each one is a
// different finding: only NO_CHANGE_NEEDED/SUFICIENTE claims the product covers
// the need. Reporting the others as sufficiency is what turned "we don't know"
// into "nothing to do".
func verdictMeaning(v string) string {
	switch strings.ToUpper(v) {
	case team.VerdictNoChangeNeeded, team.VerdictLegacySufficient:
		return "the app covers what the interviewed teachers need."
	case team.VerdictNeedsEvidence:
		return "there isn't enough evidence to decide. This is not sufficiency: gather more interviews and reopen."
	case team.VerdictOutOfScope:
		return "what the teachers asked for falls outside the defined scope. The need stands; this project won't address it."
	case team.VerdictBlockedByBaseline:
		return "the proposal can't be evaluated until the backend baseline is established."
	default:
		return "cycle closed with verdict " + v + "."
	}
}

func suggestion(st *ws.State) string {
	switch st.Phase {
	case ws.PhaseAnalyzed:
		return "Next step:  yanai discuss"
	case ws.PhaseNoChange:
		return "No change needed: retain the positive evidence; reassess when new evidence arrives."
	case ws.PhaseNeedsEvidence:
		return "Collect answers to the recorded questions, then analyze the reviewed evidence in a new cycle."
	case ws.PhaseOutOfScope:
		return "Request is outside scope. Defer it or have the human scope owner amend scope before a new analysis."
	case ws.PhaseBlockedBaseline:
		return "Resolve the recorded baseline blocker, then analyze again."
	case ws.PhaseSufficient:
		return "Cycle closed without a plan — " + verdictMeaning(st.Verdict) +
			"\nFor a new cycle, run 'yanai analyze' with other interviews."
	case ws.PhaseWaiting:
		return "⏸  Awaiting your decision:  yanai approve   |   yanai reject --note \"...\""
	case ws.PhaseRejected:
		return "Plan rejected. To have the team redo it with your reason:  yanai discuss"
	case ws.PhaseApproved:
		return "Next step:  yanai run"
	case ws.PhaseAwaitingExecution:
		return "Every ticket has a candidate staged in entregables/. Nothing has been\napplied to yanai yet — review the candidates, then update context/producto.md.\n(Applying an approved candidate to the yanai checkout is Step 8.)"
	default:
		return "Next step:  yanai analyze <file>"
	}
}

func printTasks(st *ws.State) {
	if len(st.Tasks) == 0 {
		return
	}
	fmt.Println("\nTasks:")
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "  ID\tOWNER\tSTATUS\tTITLE")
	for _, t := range st.Tasks {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", t.ID, t.Owner, t.Status, t.Title)
	}
	w.Flush()
}

func cmdApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	path := wsPath(fs)
	note := fs.String("note", "", "optional comment")
	expected := fs.String("contract", "", "contract hash from yanai review")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, cleanup, err := bindWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	review, err := r.Review()
	if err != nil {
		return err
	}
	fmt.Print(review)
	reviewedHash := strings.TrimPrefix(strings.SplitN(review, "\n", 2)[0], "Contract: ")
	if *expected != "" && *expected != reviewedHash {
		return fmt.Errorf("contract differs from the reviewed hash; review again")
	}
	st, err := r.Approve(*note, reviewedHash)
	if err != nil {
		return err
	}
	fmt.Printf("Cycle %03d APPROVED.\nNext step:  yanai run\n", st.Cycle)
	return nil
}

func cmdReject(args []string) error {
	fs := flag.NewFlagSet("reject", flag.ExitOnError)
	path := wsPath(fs)
	note := fs.String("note", "", "reason for the rejection (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, cleanup, err := bindWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	st, err := r.Reject(*note)
	if err != nil {
		return err
	}
	fmt.Printf("Cycle %03d REJECTED.\nThe team can redo the plan with your reason:  yanai discuss\n", st.Cycle)
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := wsPath(fs)
	task := fs.String("task", "", "run only this task (e.g. T-002)")
	retryUnresolved := fs.Bool("retry-unresolved", false, "acknowledge an interrupted prior model call (cost unknown) and proceed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, cleanup, err := openWorkspace(*path)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := withContext()
	defer cancel()

	st, err := r.Execute(ctx, *task, *retryUnresolved)
	if st != nil {
		printTasks(st)
		fmt.Printf("\nDeliverables in: %s\n", filepath.Join(r.Workspace.CycleDir(st.Cycle), "entregables"))
	}
	if err != nil {
		return err
	}
	fmt.Println("\nReview the deliverables before bringing them into the repository.")
	return nil
}

// cmdImport records file-era cycles into the workflow store as legacy and
// unverified: never promotable (internal/workflow's transition table gives
// TicketLegacyUnverified no outgoing edge, and the v2 legacy marker blocks a
// cycle transition too), but present so 'status' and cycle history see the
// project's whole past rather than only what started after Step 5.
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	path := wsPath(fs)
	legacy := fs.Bool("legacy", false, "import file-era cycles into the workflow store, read-only and unverified")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*legacy {
		return fmt.Errorf("nothing to import without --legacy")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	w, err := ws.Open(*path)
	if err != nil {
		return err
	}
	cleanup, err := attachStore(w, cfg, true)
	if err != nil {
		return err
	}
	defer cleanup()

	cycles, err := w.LegacyCycles()
	if err != nil {
		return err
	}
	if len(cycles) == 0 {
		fmt.Println("No file-era cycles to import.")
		return nil
	}
	for _, n := range cycles {
		if err := importLegacyCycle(w, n); err != nil {
			return fmt.Errorf("cycle %03d: %w", n, err)
		}
	}
	fmt.Println("\nImported cycles are read-only: their staged deliverables were never checked and cannot be promoted.")
	return nil
}

func importLegacyCycle(w *ws.Workspace, n int) error {
	st, err := w.LoadCycleState(n)
	if err != nil {
		return err
	}
	origin := workflow.Product
	if st.Intake != nil && st.Intake.Origin != "" {
		origin = st.Intake.Origin
	}
	payload, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if _, err := w.Store.ImportLegacyCycle(n, st.Phase, st.Verdict, origin, string(payload)); err != nil {
		return err
	}
	for _, t := range st.Tasks {
		tp, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if _, err := w.Store.ImportLegacyTicket(n, t.ID, t.Owner, string(tp)); err != nil {
			return err
		}
	}
	artifacts := 0
	root := w.CycleDir(n)
	_ = filepath.WalkDir(filepath.Join(root, "entregables"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		refID := filepath.ToSlash(rel)
		if err := w.Store.ImportLegacyArtifact(n, refID, refID, hex.EncodeToString(sum[:])); err == nil {
			artifacts++
		}
		return nil
	})
	fmt.Printf("Imported cycle %03d (phase=%s, %d ticket(s), %d deliverable(s)) as legacy/unverified.\n", n, st.Phase, len(st.Tasks), artifacts)
	return nil
}

func cmdContext(args []string) error {
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	path := wsPath(fs)
	files := fs.String("files", "", "comma-separated paths: shows their content instead of the index")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	target, err := repository.Open(cfg.Repo, *path)
	if err != nil {
		return err
	}
	cfg.Repo.Path = target.Root
	if os.Getenv("YANAI_NO_REPO") == "1" {
		fmt.Println("_(skipped via YANAI_NO_REPO=1)_")
		return nil
	}

	if *files != "" {
		text, err := repoctx.Files(cfg.Repo, strings.Split(*files, ","))
		if err != nil {
			return err
		}
		fmt.Println(text)
		return nil
	}

	text, err := repoctx.Index(cfg.Repo)
	if err != nil {
		return err
	}
	fmt.Println(text)
	fmt.Fprintln(os.Stderr, "\n(this is the index every agent sees first; use --files a,b,c to preview a selection)")
	return nil
}
