// yanai command: orchestrates an engineer-led team over OpenRouter.
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

const usage = `yanai — engineer-led development team

USAGE
  yanai <command> [options]

COMMANDS
  init --repo PATH [--module-dir DIR] [--allow paths]  Bind an explicit Git repository
  plan [--retry-unresolved] <ticket.md>               Plan an already-decided ticket
  resolve --observation ID --note "..."               Record a human response
  status [--json] [--attempts]                        Show cycle, observations, budget
  review                                             Show contract and review token
  approve [--contract TOKEN] [--note "..."]            Human execution gate
  reject --note "..."                                 Reject a plan
  invalidate --note "..."                             Revoke approval before replanning
  run [--task ID] [--retry-unresolved]                 Apply approved changes and checks
  policy [--note "..."]                               Inspect or adopt configured limits
  reconcile-attempt --id ID --cost-usd N --tokens N --reference ...
  context [--files a,b]                               Inspect repository context

Every command accepts --ws PATH (default: $YANAI_WS or ./yanai-workspace).
OPENROUTER_API_KEY configures paid calls; YANAI_MOCK=1 uses synthetic responses.
YANAI_TEST_ADMIN_URL supplies disposable loopback PostgreSQL for checks marked
requires_postgres. YANAI_GO_BINARY, YANAI_GO_CACHE, YANAI_GO_MODCACHE override
check toolchain/cache locations. Checks use approved tool allowlists; Go has built-in compatibility support.

FLOW
  plan → resolve every observation → review → approve → run → awaiting_review
  Execution observations also require resolution and fresh review/approval.
  No automatic commit, merge, deployment, or destructive database action.
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
	case "plan":
		return cmdPlan(args)
	case "resolve":
		return cmdResolve(args)
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
	repo := fs.String("repo", "", "path to the target Git repository")
	module := fs.String("module-dir", "", "repository-relative Go module directory")
	allowed := fs.String("allow", "", "comma-separated allowed repository paths; . allows the root")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Resolve and validate before creating/upgrading any workspace files.
	repoConfig := config.Repo{AllowedPaths: []string{"."}}
	var existingConfig *config.Config
	if _, err := os.Stat(filepath.Join(*path, "yanai.config.json")); err == nil {
		cfg, err := config.Load(*path)
		if err != nil {
			return err
		}
		repoConfig = cfg.Repo
		existingConfig = cfg
	} else if !os.IsNotExist(err) {
		return err
	}
	if *repo != "" {
		repoConfig.Path = *repo // --repo is relative to the invocation directory
	} else if repoConfig.Path == "" {
		return fmt.Errorf("new workspaces require init --repo PATH")
	}
	if *module != "" {
		repoConfig.ModuleDir = *module
	}
	if *allowed != "" {
		repoConfig.AllowedPaths = strings.Split(*allowed, ",")
	}

	target, err := repository.Open(repoConfig, *path)
	if err != nil {
		return err
	}
	if _, err := target.Snapshot(); err != nil {
		return err
	}

	// Upgrade checks and the workspace lock precede any template/config writes.
	// A pending old mutation must be reconciled under its original binding.
	if existingConfig != nil {
		if _, e := os.Stat(filepath.Join(*path, "workflow.db")); e == nil {
			workspace, e := ws.Open(*path)
			if e != nil {
				return e
			}
			cleanup, e := attachStore(workspace, existingConfig)
			if e != nil {
				return e
			}
			defer cleanup()
		} else if !os.IsNotExist(e) {
			return e
		}
	}

	if err := os.MkdirAll(*path, 0o755); err != nil {
		return err
	}
	results, fromVersion, err := templates.ExtractProject(*path)
	if err != nil {
		return err
	}
	for _, d := range []string{"tickets", "cycles"} {
		if err := os.MkdirAll(filepath.Join(*path, d), 0o755); err != nil {
			return err
		}
	}

	if err := config.SetRepoBinding(*path, target.Root, repoConfig.ModuleDir, repoConfig.AllowedPaths); err != nil {
		return err
	}

	abs, _ := filepath.Abs(*path)
	fmt.Printf("Workspace ready at %s\n", abs)
	fmt.Printf("Application repository: %s\n", target.Root)
	printTemplateResults(results, fromVersion)
	fmt.Printf(`
Next steps:
  1. Review yanai.config.json: allowed paths, models, limits, prices, and checks.
  2. Keep your local engineer, DB architect, and designer prompts as configured.
  3. export OPENROUTER_API_KEY=sk-or-...
  4. yanai plan tickets/task.md
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
// workspace writer lock for the returned Runner's lifetime and reconciles any
// claim or attempt left behind by a process that died. It also binds an OpenRouter client, so it requires an API key
// (or YANAI_MOCK=1) even for a command that turns out not to call a model —
// plan and run may call the model. Approve and Reject never do, and use
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
// on first use and reconciling abandoned claims/attempts). Used directly by approve/reject, which never call a model
// and so must not require an API key just to record a human decision.
func bindWorkspace(path string) (*team.Runner, func(), error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	w, err := ws.Open(path)
	if err != nil {
		return nil, nil, err
	}
	cleanup, err := attachStore(w, cfg)
	if err != nil {
		return nil, nil, err
	}
	return &team.Runner{Cfg: cfg, Workspace: w}, cleanup, nil
}

// attachStore opens (creating if needed) w's workflow.db, takes the
// workspace writer lock for the duration, and reconciles what a dead
// process may have left behind: an expired ticket claim goes back to
// pending, and an attempt still marked in_flight is stamped unknown — cost
// unknown, never assumed zero — for planning or execution to refuse on
// (checkUnresolvedAttempts) until the operator explicitly acknowledges it.
//
// A workspace is always opened against the current store schema and live
// ticket workflow.
func attachStore(w *ws.Workspace, cfg *config.Config) (func(), error) {
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
// Never promote an unverified file into looking authoritative.
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

// cmdStatus remains useful when the provider or repository is unavailable.
// It reads the current store when possible and falls back to its projection.
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
		if cleanup, err := attachStore(w, cfg); err == nil {
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
		var observations []workflow.Observation
		if w.Store != nil {
			var e error
			observations, e = w.Store.Observations(st.Cycle)
			if e != nil {
				return e
			}
		}
		b, err := json.MarshalIndent(struct {
			*ws.State
			Observations []workflow.Observation `json:"observations"`
		}{st, observations}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("Cycle:   %03d\n", st.Cycle)
	fmt.Printf("Phase:   %s\n", st.Phase)
	if st.Verdict != "" {
		fmt.Printf("Verdict: %s\n", st.Verdict)
	}
	if w.Store != nil {
		if budget, err := w.Store.Budget(st.Cycle); err == nil {
			data, _ := json.MarshalIndent(budget, "", "  ")
			fmt.Printf("Budget:\n%s\n", data)
		}
	}
	if w.Store != nil {
		observations, e := w.Store.Observations(st.Cycle)
		if e != nil {
			return e
		}
		for _, o := range observations {
			fmt.Printf("Observation %s (%s): %s\n  Requirement: %s\n  Question: %s\n  Human response: %s\n", o.ID, o.Role, o.Detail.Description, o.Detail.Requirement, o.Detail.Question, o.Resolution)
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

// verdictMeaning renders a terminal finding in the user's words.
func verdictMeaning(v string) string {
	switch strings.ToUpper(v) {
	case team.VerdictNoChangeNeeded:
		return "the app covers what the interviewed teachers need."
	case team.VerdictNeedsEvidence:
		return "There is not enough evidence to decide. Submit a revised ticket with the missing evidence."
	case team.VerdictOutOfScope:
		return "what the teachers asked for falls outside the defined scope. The need stands; this project won't address it."
	case team.VerdictBlockedByBaseline:
		return "the proposal can't be evaluated until the backend baseline is established."
	default:
		return "cycle closed with verdict " + v + "."
	}
}

func suggestion(st *ws.State) string {
	if st.Markdown != nil && st.Phase == ws.PhaseAnalyzed {
		return "Resolve observations, then resume: yanai plan <ticket.md>"
	}

	switch st.Phase {
	case ws.PhaseAnalyzed:
		return "Resolve observations, then resume: yanai plan <ticket.md>"
	case ws.PhaseNoChange:
		return "No change needed: retain the positive evidence; reassess when new evidence arrives."
	case ws.PhaseNeedsEvidence:
		return "Collect the requested evidence, then submit a revised ticket."
	case ws.PhaseOutOfScope:
		return "Request is outside scope. Submit a revised ticket if the scope changes."
	case ws.PhaseBlockedBaseline:
		return "Resolve the recorded baseline blocker, then submit the ticket again."
	case ws.PhaseWaiting:
		return "⏸  Awaiting your decision:  yanai approve   |   yanai reject --note \"...\""
	case ws.PhaseRejected:
		return "Plan rejected. Resume with a revised ticket: yanai plan <ticket.md>"
	case ws.PhaseApproved:
		return "Next step:  yanai run"
	case ws.PhaseAwaitingExecution:
		return "Execution is ready to resume: 'yanai review', 'yanai approve', 'yanai run'."
	case ws.PhaseAwaitingReview:
		return "The approved change is applied in the configured checkout and its checks passed.\nReview the uncommitted diff and recorded evidence. Independent technical review is not implemented."
	default:
		return "Next step:  yanai plan <ticket.md>"
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
	fmt.Printf("Cycle %03d REJECTED.\nResume with a revised ticket: yanai plan <ticket.md>\n", st.Cycle)
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
		fmt.Printf("\nApplied to: %s\nEvidence:   %s\n", r.Cfg.Repo.Path, filepath.Join(r.Workspace.CycleDir(st.Cycle), "execution"))
	}
	if err != nil {
		return err
	}
	fmt.Printf("\n%s\n", suggestion(st))
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
