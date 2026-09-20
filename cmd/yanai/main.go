// yanai command: orchestrates a team of agents (Product Owner, DB Architect,
// Engineer and Designer) over OpenRouter to develop the teaching app.
//
// Nothing gets implemented without a person's explicit approval.
package main

import (
	"context"
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
	"github.com/yanai/yanai-harness/internal/ws"
)

const usage = `yanai — agent team for the teaching app

USAGE
  yanai <command> [options]

COMMANDS
  init      [--repo path]   Creates or upgrades the workspace (config, prompts, context)
  analyze   <file|->        The Product Owner reads the interviews and decides direction
  discuss                   Team working session and the PO's consolidated plan
  status                    Shows where the current cycle stands
  approve   [--note ...]    Human gate: authorizes implementation
  reject    --note "..."    Sends the plan back to the team with a reason
  run       [--task ID]     Runs the approved tasks (only after 'approve')
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
  2. Review yanai.config.json: the repo path and each agent's model.
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

func openWorkspace(path string) (*team.Runner, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	target, err := repository.Open(cfg.Repo, path)
	if err != nil {
		return nil, err
	}
	cfg.Repo.Path = target.Root
	w, err := ws.Open(path)
	if err != nil {
		return nil, err
	}
	cli, err := openrouter.New(cfg)
	if err != nil {
		return nil, err
	}
	if cli.IsMock() {
		fmt.Fprintln(os.Stderr, "⚠  YANAI_MOCK=1: mock responses, OpenRouter is not being called.")
	}
	return &team.Runner{Cfg: cfg, Client: cli, Workspace: w}, nil
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

func cmdAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	path := wsPath(fs)
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
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("the interviews file is empty")
	}

	r, err := openWorkspace(*path)
	if err != nil {
		return err
	}
	ctx, cancel := withContext()
	defer cancel()

	st, err := r.Analyze(ctx, text)
	if err != nil {
		return err
	}

	dir := r.Workspace.CycleDir(st.Cycle)
	fmt.Printf("\nCycle %03d — Product Owner verdict: %s\n", st.Cycle, st.Verdict)
	if team.IsTerminalVerdict(st.Verdict) {
		fmt.Printf("%s\n", verdictMeaning(st.Verdict))
		fmt.Printf("Report: %s\n", filepath.Join(dir, "02-reporte-suficiencia.md"))
		return nil
	}
	fmt.Printf("Proposal: %s\n", filepath.Join(dir, "02-propuesta.md"))
	fmt.Printf("\nNext step:  yanai discuss\n")
	return nil
}

func cmdDiscuss(args []string) error {
	fs := flag.NewFlagSet("discuss", flag.ExitOnError)
	path := wsPath(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, err := openWorkspace(*path)
	if err != nil {
		return err
	}
	ctx, cancel := withContext()
	defer cancel()

	st, err := r.Discuss(ctx)
	if err != nil {
		return err
	}
	dir := r.Workspace.CycleDir(st.Cycle)
	fmt.Printf("\nCycle %03d — plan consolidated with %d tasks.\n", st.Cycle, len(st.Tasks))
	fmt.Printf("Discussion: %s\n", filepath.Join(dir, "03-discusion.md"))
	fmt.Printf("Plan:       %s\n", filepath.Join(dir, "04-plan.md"))
	printTasks(st)
	fmt.Printf("\n⏸  STOPPED: nothing gets implemented without your approval.\n")
	fmt.Printf("   Read the plan and then:\n     yanai approve\n     yanai reject --note \"why\"\n")
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	path := wsPath(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	w, err := ws.Open(*path)
	if err != nil {
		return err
	}
	st, err := w.LoadState()
	if err != nil {
		return err
	}
	fmt.Printf("Cycle:   %03d\n", st.Cycle)
	fmt.Printf("Phase:   %s\n", st.Phase)
	if st.Verdict != "" {
		fmt.Printf("Verdict: %s\n", st.Verdict)
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
	case ws.PhaseSufficient:
		return "Cycle closed without a plan — " + verdictMeaning(st.Verdict) +
			"\nFor a new cycle, run 'yanai analyze' with other interviews."
	case ws.PhaseWaiting:
		return "⏸  Awaiting your decision:  yanai approve   |   yanai reject --note \"...\""
	case ws.PhaseRejected:
		return "Plan rejected. To have the team redo it with your reason:  yanai discuss"
	case ws.PhaseApproved:
		return "Next step:  yanai run"
	case ws.PhaseExecuted:
		return "Cycle complete. Review entregables/ and update context/producto.md."
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	w, err := ws.Open(*path)
	if err != nil {
		return err
	}
	r := &team.Runner{Workspace: w}
	st, err := r.Approve(*note)
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
	w, err := ws.Open(*path)
	if err != nil {
		return err
	}
	r := &team.Runner{Workspace: w}
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, err := openWorkspace(*path)
	if err != nil {
		return err
	}
	ctx, cancel := withContext()
	defer cancel()

	st, err := r.Execute(ctx, *task)
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
