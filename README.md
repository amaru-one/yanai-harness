# yanai-harness

A Go CLI for engineer-led development cycles over OpenRouter. Supply an
already-decided Markdown ticket; the engineer plans and implements it, consulting
the DB architect and designer only when needed. Every observation pauses for a
human response. Repository writes always require human approval.

The Product Owner and interview intake are retired. Historical cycles remain
readable. The engineer, DB architect, and designer's local Markdown prompts are
operator-owned and are never replaced during initialization or upgrade.

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
is an optional Go-specific validation setting, relative to the Git root. `--allow .` explicitly permits safe files throughout
the repository; narrower paths restrict context and generated writes. Git internals,
secrets, symlink escapes, ignored files, and the legacy `yanai-ui` boundary remain
protected. Fingerprints cover safe repository files independently of narrowed
context filters, plus the whole Git status and index.

Review `yanai.config.json`: the three model choices and prompt paths, allowed paths,
finite execution budgets, price bounds, and required checks. Budgets default to
zero deliberately: configure them before paid calls. Existing custom configuration
is preserved; rebinding does not silently widen an existing allowlist.

A check has an ID, working directory relative to the Git root (`.` is valid),
argument array, timeout, and optional PostgreSQL prerequisite. Existing Go checks
retain their built-in adapter:

```json
{"id":"unit","dir":".","args":["go","test","-race","-shuffle=on","./..."],"timeout_seconds":120,"requires_postgres":false}
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
tool directories and system binary directories, and the database URL only when
required. They do not inherit provider credentials. Provision dependencies first
and configure tools to keep generated caches outside the target. The optional Go
adapter supports validated `go test`, `go vet`, and `go build` with external caches;
`YANAI_GO_BINARY`, `YANAI_GO_CACHE`, and `YANAI_GO_MODCACHE` override its locations.
The tool name `go` is reserved for this compatibility adapter. A project without
Go checks does not need a Go toolchain at runtime.

Language/framework requirements belong in the operator's Markdown prompts under
`templates/`; the harness does not rewrite the engineer, DB architect, or designer
prompts. New configurations leave `repo.extensions` empty (no language filter);
set explicit extensions to narrow context for a project. Repository protections
and context byte limits still apply.

For a check marked `requires_postgres`, set `YANAI_TEST_ADMIN_URL` to a disposable
PostgreSQL URL with an explicit loopback host and port. Missing configuration
blocks the check; skipped tests do not establish acceptance. Checks without this
prerequisite can run without PostgreSQL.

## Ticket cycle

Use [the small Markdown template](docs/ticket-template.md): one `# Title`, a
`## Task` section, `## Acceptance criteria` containing `- ` bullets, and optional
`## Constraints`. No interview metadata or privacy-review ceremony is required.

```sh
export OPENROUTER_API_KEY=...
./yanai plan --ws /path/to/workspace task.md
./yanai status --ws /path/to/workspace
# For every observation, record your response:
./yanai resolve --ws /path/to/workspace --observation O-ID --note 'Your decision'
# Resume the same ticket after responding:
./yanai plan --ws /path/to/workspace task.md
./yanai review --ws /path/to/workspace
./yanai approve --ws /path/to/workspace --contract REVIEW_TOKEN
./yanai run --ws /path/to/workspace
```

An observation is always a pause, including advisory concerns. Resolving it is not
approval. If requirements change, revise the ticket and generate a new plan; revoke
an existing approval with `invalidate --note ...` first. Confirmed changes are never
automatically discarded. New execution requires a clean approved baseline.

Execution observations leave confirmed changes and evidence intact. Resolve them,
then run `review` and `approve` again before `run`. Supplemental approval authorizes
responses under the same task contract, not changes to its scope or patch baseline.

The final state is `awaiting_review`: real changes and machine-recorded checks are
available for human inspection. Independent technical review is not implemented.
No command commits, merges, deploys, or authorizes destructive database operations.

`status --json` includes observations; `status --attempts` reports unresolved
provider outcomes. Unknown billing/dispatch requires explicit reconciliation and,
where indicated, `--retry-unresolved`. Recorded planning responses are reused after
restart. `YANAI_MOCK=1` supplies synthetic responses; it is not evidence of model
quality. Existing local prompts may contain project-specific assumptions: agents
must raise conflicts rather than rewrite those files.

## Compatibility and verification

The active [work plan](docs/work-plan.md) replaces the
[historical Yanai roadmap](docs/work-plan-legacy.md). The store upgrades additively;
contract revision 3 rejects earlier approvals. Upgrade is refused if an older
workspace has an unresolved mutation: reconcile it with the previous binary first.
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
