# yanai-harness

A Go CLI for parent-led development cycles over OpenRouter (level 0). Supply an
already-decided Markdown ticket; a **parent agent** reads the project scope and
state, the ticket and the repository, and creates **exactly one worker** with a
generated role prompt, one task that covers every acceptance criterion, and the
complete configuration it will run under. A human reviews and approves that
proposal once. The worker then reads files, edits only the files it owns, runs
the approved checks, and makes Conventional Commits on a ticket branch
(`<type>/<slug>`) **in the configured checkout itself** — no clones, worktrees or
sandboxes. The checkout returns to its original branch, and the parent proposes
an update to `project/estado.md` that the human accepts with `yanai close`.

Every observation pauses for a human response. Nothing merges, pushes or deploys.

## Start a project

Requires Go 1.26.6, Git, an existing committed Git repository, and a separate workspace.

```sh
go build -o yanai ./cmd/yanai
./yanai init --ws /path/to/workspace --repo /path/to/project
# For a nested module:
./yanai init --ws /path/to/workspace --repo /path/to/project --module-dir services/api --allow AGENTS.md,services/api
```

New workspaces require an explicit target. `--repo` is resolved from the invocation
directory; configured relative paths are resolved from the workspace. `module_dir`
is an optional Go-specific validation setting, relative to the Git root. By
default agents see the whole repository (`allowed_paths` `["."]`), including
ordinary dotfiles such as `.github/` or `.gitignore`; narrower `--allow` paths
restrict what they read and write. `.git`, environment files (`.env`,
`.env.*`; `.env.example` and `.env.sample` are allowed), names suggesting
secrets, credentials or private keys, key and database file types, symlinks, and
ignored files are never readable or writable. Fingerprints cover safe repository files independently of narrowed
context filters, plus the whole Git status and index.

Review `yanai.config.json`: the orchestrator model and budget, allowed paths,
finite execution budgets, price bounds, and required checks. Budgets default to
zero deliberately: configure them before paid calls. Existing custom configuration
is preserved; rebinding does not silently widen an existing allowlist.

A check has an ID, working directory relative to the Git root (`.` is valid),
argument array, timeout, and optional project-specific input names. Existing Go checks
retain their built-in adapter:

```json
{"id":"unit","dir":".","args":["go","test","-v","-race","-shuffle=on","./..."],"timeout_seconds":120,"required_env":["PROJECT_TEST_ADMIN_URL"],"postgres_url_var":"PROJECT_TEST_ADMIN_URL"}
```

Other languages use explicitly configured, operator-installed executables in
`execution.tools`. For example, inside `execution` (replace the binary path):

```json
{
  "tools": {"python": {"binary": "/absolute/path/to/python3"}},
  "checks": [{
    "id": "unit", "dir": ".",
    "args": ["python", "-B", "-m", "unittest", "discover", "-v"],
    "timeout_seconds": 120,
    "evidence": "output",
    "success_pattern": "(?s)Ran [1-9][0-9]* tests?.*OK",
    "failure_pattern": "FAILED|skipped"
  }]
}
```

The binary must be executable and installed outside the target and workspace.
Tool paths, exact arguments, and evidence rules are bound to human approval.
Unknown tools, direct Git/shell commands, and commit/merge/deploy/destructive_db
command tokens are refused. Commands run without shell interpolation. Installed
runtimes and project test code are trusted: a runtime can launch subprocesses, so
this allowlist is not an OS sandbox or protection against malicious test scripts.

Non-Go checks require `evidence: "exit_code"` or `"output"`. Both require a zero
exit code; output evidence also requires a matching `success_pattern`. An optional
`failure_pattern` rejects matching output. Patterns use Go's regular-expression
syntax. Exit-code evidence proves command success only; configure output rules to
reject zero-test or skipped suites when that matters. Output limits, timeouts,
repository mutation detection, and durable evidence apply to every tool.

Generic checks receive scratch HOME/TMPDIR, LANG, TZ, a PATH built from configured
tool directories and system binary directories, and only their declared ticket
inputs. They do not inherit provider credentials. Provision dependencies first
and configure tools to keep generated caches outside the target. The optional Go
adapter supports validated `go test`, `go vet`, and `go build` with external caches;
`YANAI_GO_BINARY`, `YANAI_GO_CACHE`, and `YANAI_GO_MODCACHE` override its locations.
The tool name `go` is reserved for this compatibility adapter. A project without
Go checks does not need a Go toolchain at runtime.

Language/framework requirements belong in `project/alcance.md` and the optional
base prompts under `prompts/base/`; the parent carries them into the worker prompt
it generates. New configurations leave `repo.extensions` empty (no language filter);
set explicit extensions to narrow context for a project. Repository protections
and context byte limits still apply.

Declare `required_env` names for each check and supply their values in the
ticket's optional `## Check inputs` section. `postgres_url_var`, when set, must
name one of those inputs and is restricted to a disposable PostgreSQL URL with
an explicit loopback host and port. The exact source ticket is bound to human
approval; agents and displayed check evidence see names, never values. Missing
inputs or tools block planning/review before approval, and are rechecked before
execution. This static preflight does not prove a test suite can run; a runtime
dependency failure remains a failed check requiring diagnosis. Skipped tests do
not establish acceptance.

## Project documents and prompts

Write all prompts and Markdown documents in English, including generated worker
prompts, proposals, reports, and state updates. Keep existing file paths and schema
values unchanged for compatibility.

`yanai init` creates `project/alcance.md` (project scope) and `project/estado.md`
(current state). Planning refuses to start while either holds only headings or
comments. Optional base prompts go in `prompts/base/*.md`; the parent adapts one
or writes the worker prompt from scratch into `prompts/generated/<slug>/<worker>.md`.

Configure `orchestrator` in `yanai.config.json` (model, max tokens, max steps and a
separate finite budget with price bounds). The parent runs with the settings
captured when its cycle starts. Its proposal carries `config_changes`, a JSON
merge patch over the live configuration (`{}` for none), rather than a whole new
file; the harness builds the single `agents` entry from the proposed worker. The
result is shown as a full diff at review, and the proposal cannot raise the
parent's budget or change the catalog. A rejected proposal lists every problem
found, so one retry can fix them all. `agents` starts empty.

The worker's model comes from the operator's `models` catalog. Each category (for
example `engineering`, `database`) has a description and 1–5 options; each
option names a model with an `execution.prices` bound, the task difficulties it
handles (`low`, `medium`, `high`), its `context_tokens` (context window), its
`strengths` and, optionally, its `weaknesses` and `reasoning_max_tokens`. Set the
reasoning cap only for models that support one: when present it is sent as
OpenRouter's `reasoning.max_tokens`; when absent the harness sends no reasoning
setting. Together a category's options must cover all three difficulties. A
worker's `max_tokens` must be at least 32,000, because reasoning models spend
much of it thinking before they answer.

The parent follows a fixed method: understand the project, analyze the ticket
(reading the files it will touch), define the work, rate the difficulty, then choose
the model. The difficulty rubric:

- `low`: 1–3 files in one module, an existing pattern to follow, no schema, public
  API, concurrency or security change.
- `medium`: several files or two modules, moderate new logic, new tests.
- `high`: crosses modules; touches schema, migrations, contracts, concurrency,
  security or sensitive data; ambiguous design or non-trivial algorithms. When in
  doubt, the higher level.

The parent declares `task_complexity` and `complexity_reason`, a `model_category`,
and a model of that category that covers the difficulty. It should pick the cheapest
such option unless a declared weakness matters for this ticket, and must explain in
`model_reason` why it discarded every other option. It reasons only from the
catalog and cannot change it.
Go rejects a model that does not cover the declared difficulty. Review shows the
difficulty, every option with its price and coverage, and the reason, and warns
when a cheaper option also covered the level. All of it is bound by the approval.

Example (prices and context from OpenRouter, 2026-09-28):

```json
"models": {
  "engineering": {
    "description": "Go/Svelte code, tests, refactoring",
    "options": [
      {"model": "z-ai/glm-5.3-flash", "difficulty": ["low", "medium"], "context_tokens": 1310720,
       "strengths": "tool calling and reasoning; lowest price in the catalog"},
      {"model": "deepseek/deepseek-v4.1-flash", "difficulty": ["low", "medium"], "context_tokens": 1048576,
       "strengths": "tool calling and reasoning; low price"},
      {"model": "deepseek/deepseek-v4-pro-0813", "difficulty": ["low", "medium", "high"], "context_tokens": 1048576,
       "strengths": "tool calling and reasoning; flagship model at a low input price",
       "weaknesses": "high output price"},
      {"model": "z-ai/glm-5.3", "difficulty": ["low", "medium", "high"], "context_tokens": 1310720,
       "strengths": "tool calling and reasoning; flagship model",
       "weaknesses": "highest price in the catalog"}
    ]
  }
}
```

Matching `execution.prices` (USD per million tokens, OpenRouter, 2026-09-28):

```json
"z-ai/glm-5.3-flash":            {"input_usd_per_million": 0.15,  "output_usd_per_million": 0.50},
"deepseek/deepseek-v4.1-flash":  {"input_usd_per_million": 0.30,  "output_usd_per_million": 1.20},
"deepseek/deepseek-v4-pro-0813": {"input_usd_per_million": 0.395, "output_usd_per_million": 4.20},
"z-ai/glm-5.3":                  {"input_usd_per_million": 1.40,  "output_usd_per_million": 4.40}
```

## Ticket cycle

Tickets need a `## Type` section with a Conventional Commits type
(`feat`, `fix`, `refactor`, `perf`, `test`, `docs`, `build`, `ci`, `chore`, `style`);
see [the template](docs/ticket-template.md). The title's slug names the branch.

```sh
export OPENROUTER_API_KEY=...
./yanai plan --ws /path/to/workspace task.md      # parent proposes one worker
./yanai resolve --ws ... --observation O-ID --note 'Your decision'   # if paused
./yanai review --ws /path/to/workspace            # worker, prompt, task, config diff
./yanai approve --ws /path/to/workspace --contract REVIEW_TOKEN
./yanai run --ws /path/to/workspace               # worker loop on <type>/<slug>, then closing
./yanai review --ws /path/to/workspace            # final report + proposed estado.md diff
./yanai close --ws /path/to/workspace --state-update HASH
```

Approval activates the proposed configuration (recoverably) and authorizes the
worker to create the ticket branch from the approved base, write repository files,
run approved checks, and commit — only after its required checks pass against the
current files, with messages `<type>(<scope>): <summary>` plus `Yanai-Ticket` and
`Yanai-Agent` trailers. Every model answer, tool intent and result, and every Git
operation is journaled, so `run` resumes after an interruption without repeating
a paid call, a write or a commit. External edits, a moved branch or a changed
approved input stop the run without resetting anything.

The worker ends with `completed` (commits exist, nothing uncommitted, all approved
checks pass against the final committed state), `no_change`, or `blocked` with
observations; after human responses, `review`/`approve` confirm them under the same
contract and `run` continues on the same branch.

Checks may run any command; the human approves each one in the review. A check
names an installed program (`docker`, `npm`, `bash`, ...), which the harness
resolves from `PATH` into `execution.tools` so the review shows the exact binary,
or a repository script by relative path (`./scripts/smoke.sh`). Its `dir` is a
repository directory (`.` is the root), evidence defaults to the exit code, and
non-Go checks run with the operator's environment minus `OPENROUTER_API_KEY` and
Git redirection variables (Go checks keep their isolated toolchain environment).

Agents may also ask to run a command at any time with `run_command`. Every such
command waits for a human: the run stops with the command, its directory and the
agent's reason, and resumes at the same step after a decision.

```sh
./yanai command --ws W                                   # list requests
./yanai command --ws W --id C-… --approve [--always]     # --always: same command, same dir, rest of the cycle
./yanai command --ws W --id C-… --deny --note "use the approved check instead"
./yanai run --ws W                                       # or: ./yanai plan --ws W ticket.md
```

A denial reaches the agent as an error with your note. A worker command that
changes files hands them to the worker, who commits them with its own changes; a
command that moves HEAD, changes the index or switches branch stops the run. A
planning command must leave the repository unchanged. A command interrupted by a
crash is never rerun automatically; the agent is told its outcome is unknown.
`YANAI_MOCK_COMMAND=1` makes the mock worker request one command.

The proposal's `outputs` are the files the parent expects the worker to change:
the plan the human reviews, not a limit. The worker may change any other
repository file the task needs (the protected files above excepted), commits
every pending change, and explains extra files in its `finish` explanation.

Agents search before they read: `grep` (an RE2 regular expression over the text
files in scope, at most 200 matches) and `list_files` (paths and sizes under a
directory or glob such as `deploy/**/*.yml`, at most 500) cost no model output
and never show protected or ignored files. `read_file` takes up to 10 paths per
call, because every turn resends the whole conversation, and
`start_line`/`end_line` read a range of a large file. A replan after observations starts
with the files earlier planning runs read, and the worker starts with its
existing expected files and the files the parent read, each with its sha256, so
neither pays read turns for them again. Workers change existing files with
`edit_file` (one exact, unique replacement) instead of rewriting them whole. Before each model call the harness
estimates the input from the provider's native token count for the previous turn
(as OpenRouter reports it) plus a byte bound on what was added since; once a
budget bucket has used more than 80% of its `max_tokens`, it first sends the same
request capped at one output token (a paid probe, recorded as its own attempt) to
get the exact prompt count. When the remaining budget or steps cannot cover this
turn and one more, the turn is the final one: the model is offered only its
terminal tool (`submit_proposal`, `submit_state_update` or `finish`), so a run ends
with a proposal, a state update or a `blocked` result instead of stalling. If that
final answer is invalid, the run stops with its work preserved. Size budgets
generously so the probe stays rare.

`status --json` includes observations; `status --attempts` reports unresolved
provider outcomes. Unknown billing/dispatch requires explicit reconciliation and,
where indicated, `--retry-unresolved`. `YANAI_MOCK=1` supplies synthetic tool calls;
it is not evidence of model quality.

## Compatibility and verification

Workspaces migrate to store schema 6 on first use. Migration is refused while a
previous binary's patch is unfinished; reconcile it with that binary first. Cycles
approved under contract revision 3 stay readable but cannot run: invalidate them
and plan again.
The target repository is not needed to inspect local status and recorded history.

```sh
go test -race -shuffle=on ./internal/... ./cmd/...
go vet ./internal/... ./cmd/...
go build ./cmd/yanai
```

Tests use temporary Git repositories and controlled providers, never a sibling
Yanai checkout. A controlled-provider ticket cycle runs real Python checks without a Go module.
The Go compatibility fixture exercises root and nested modules. Its
PostgreSQL subtest requires a disposable **trust-authenticated** local instance:

```sh
YANAI_TEST_ADMIN_URL='postgres://postgres@127.0.0.1:PORT/postgres?sslmode=disable'   go test -race -shuffle=on ./internal/executor -run TestNativeGenericProjectsWithRealChecks -v
```

That fixture uses a connection-local temporary table and asserts its query result.
It visibly skips the database scenario if the URL is absent. Package-scoped commands
exclude generated Go deliverables that may exist in an ignored local workspace.
