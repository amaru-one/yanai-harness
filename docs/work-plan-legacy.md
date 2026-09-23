# Yanai agent team: audited work plan

Audit date: 2026-09-19. Status: planned; implementation is not authorized by the creation of this document.

## Goal and purpose

Make `yanai-harness` a reliable software development team that turns evidence from real teacher interviews into approved, implemented, and tested improvements in the sibling `yanai` repository. Yanai is the first project through which we prove a recipe reusable by other software projects.

The intended cycle is:

**Teacher interview → traceable need → scope decision → implementation plan → human approval → changes in Yanai → automated checks and independent review → teacher feedback.**

Success means a working improvement with evidence behind it. Producing agent discussions or generated files alone does not complete the cycle.

Keep the Go harness, OpenRouter, and the four existing roles. Add independent review first and other specialists when a task requires them. Keep MetaGPT as an alternative execution backend to evaluate after the native workflow works.

## Boundaries

- Harness implementation belongs in `yanai-harness/`. Every application change produced for Yanai belongs in `yanai/`, initially `yanai/yanai-server/` and the specifications required by those changes.
- The application target is the existing sibling checkout, not a generated application inside the harness workspace.
- `yanai-ui/` is excluded from reading, generation, modification, and acceptance work in this stage. The designer remains a team member and can review teacher needs and interaction requirements without generating frontend files.
- Follow [Yanai's SPEC protocol](../../yanai/AGENTS.md). Read the applicable SPEC before code; update affected SPECs with implementation changes. Resolve contradictory contracts explicitly.
- Preserve existing uncommitted harness work. Evaluate and integrate it; do not reset or replace it wholesale.
- Create only files needed by approved tasks: implementation, configuration, meaningful tests, required SPECs, and necessary runtime state. Do not generate additional reports or documentation by default. Do not recreate `WORKFLOW.md` or the deleted backend `internal/integration/` directory. Put database integration tests alongside the packages they exercise.
- This document supersedes the implementation sequence in the previous proposal. It does not modify that historical document or declare its implementation steps complete.

## Audit: what exists and what remains unproven

The audit covers current working files, including uncommitted changes. `yanai` is clean at `2d9d41d`; `yanai-harness` is at `c703353` with modified tracked files and untracked workflow code. The harness commit alone does not reproduce the audited state.

| Finding | Local evidence | Consequence for the plan |
|---|---|---|
| The four-role CLI and OpenRouter integration already exist. | [CLI](../cmd/yanai/main.go), [configuration](../internal/config/config.go), [agent runner](../internal/team/agent.go) | Evolve the existing implementation. Preserve usable CLI behavior and role/model configuration. |
| Typed workflow contracts, SQLite storage, and artifact hashing exist, but the CLI execution path does not use them. | [Workflow package](../internal/workflow/workflow.go), [store](../internal/workflow/store.go), [current flow](../internal/team/flow.go) | Connect and strengthen these components before calling the workflow durable. Package tests are not proof of CLI integration. |
| `run` writes files under `cycles/.../entregables`, then marks a task `done`; it does not apply changes or run checks in Yanai. It can mark a task done when no files were extracted. | [Execute](../internal/team/flow.go) | Repository execution and verified completion are the central missing capabilities. |
| Approval hashes the plan text, context, and Git HEAD. Execution also trusts mutable task data in `state.json`; dirty files are not covered by HEAD. An invalid repository falls back to hashing its path string. | [Approval and repositoryBaseline](../internal/team/flow.go), [workspace state](../internal/ws/ws.go) | Bind approval to the actual executable contract and repository state; fail closed on invalid targets. |
| Four distinct non-change outcomes are collapsed into `sufficient`. The PO prompt still treats silence as non-use and insufficient evidence as sufficiency. | [Analyze](../internal/team/flow.go), [PO prompt](../internal/templates/files/prompts/product-owner.md) | Correct both control flow and prompts. Missing evidence must remain missing evidence. |
| Current templates are embedded from `internal/templates/files`; the original `internal/plantillas/archivos` paths are not the active template source. The default repository is still `../app-docente`. | [Template embedding](../internal/templates/templates.go), [active config](../internal/templates/files/yanai.config.json) | Fix the files the CLI actually uses and handle existing workspaces explicitly. |
| Context and prompts have drifted. The engineer prompt specifies ServeMux/database/sql; the server uses chi/pgx. Product inventory contains old migration-state and assessment-scale descriptions. | [Engineer prompt](../internal/templates/files/prompts/ingeniero.md), [product inventory](../internal/templates/files/context/producto.md), [server SPEC](../../yanai/yanai-server/SPEC.md) | Reconcile context before allowing agents to implement against it. |
| The migration SPEC now exists. Some backend contracts remain internally contradictory: criterion-score operations are documented alongside statements that no criterion-level operation exists. The root SPEC also describes `WithSystemTx` too broadly. | [Migration SPEC](../../yanai/yanai-server/migrations/SPEC.md), [repository SPEC](../../yanai/yanai-server/internal/repo/SPEC.md), [HTTP SPEC](../../yanai/yanai-server/internal/httpapi/SPEC.md), [DB SPEC](../../yanai/yanai-server/internal/db/SPEC.md) | Do not repeat the old “missing migration SPEC” diagnosis. Clarify domain terminology and intended behavior before editing those contracts. |
| A sampled student-list route resolves a teacher but does not check assignment to the requested class; its query scopes by class, while the standard RLS policy scopes by tenant. | [Middleware](../../yanai/yanai-server/internal/httpapi/middleware.go), [student handler](../../yanai/yanai-server/internal/httpapi/students.go), [student query](../../yanai/yanai-server/internal/repo/students.go), [SQL policy](../../yanai/yanai-server/migrations/00001_initial_schema.sql) | Prioritize a same-school, different-teacher authorization regression test. This is a static finding; it was not reproduced against a running database during this audit. |
| SQLite updates and events are separate operations; state-version fields do not enforce conditional transitions. Artifact publication checks existence before rename, leaving concurrent publication unresolved. | [Store](../internal/workflow/store.go), [artifact publication](../internal/workflow/artifacts.go) | Add transactional transitions, concurrency control, and crash recovery before real repository mutation. |
| Selected-file reads use lexical containment and ordinary file reads. Provider calls lack structured-output/tool contracts and persistent cost accounting. | [Repository reader](../internal/repoctx/repoctx.go), [provider client](../internal/openrouter/client.go) | Enforce boundaries in actual I/O and tool execution, and validate responses locally. |

### Checks completed for this audit

- [x] Read applicable backend SPECs and inspect the harness runtime, workflow scaffolding, active templates, and selected backend request/query paths.
- [x] Run `go test ./...`, `go vet ./...`, and `go build ./...` in both Go modules: all exited successfully.
- [x] Confirm harness tests currently reside in `internal/team` and `internal/workflow`; every backend package reports `[no test files]`.

No database was created, migrated, reset, or queried for this audit. No paid model calls or teacher pilot were performed. Database runtime correctness, deployed migration history, model availability, and interview-derived priorities remain unverified. The audit is targeted, not an exhaustive review of every SQL statement.

## Implementation order

Each step closes only when its exit condition is demonstrated. All implementation checkboxes start unchecked, including work with partial scaffolding already present.

Steps 3–7 may be developed against temporary repositories and mocked providers while Step 2 is completed. **Execution against the actual Yanai checkout in Step 8 requires Steps 1–7 to pass.** Unknown deployment history blocks destructive migration decisions, not harmless harness development.

### 1. Reconcile the project contract and preserve existing work

Location: both repositories' existing context, configuration, prompts, and SPECs. Owners: human product owner, Product Owner, DB Architect, Engineer.

- [x] Inventory the current harness diff and classify each partial implementation as reusable, incomplete, or obsolete. Preserve user changes and include accepted work in a reproducible revision through the normal development process.
- [x] Establish the active template source and an explicit migration policy for existing workspaces. Template extraction currently preserves existing files, so editing embedded prompts alone will not update users' workspaces.
- [x] Reconcile `alcance.md`, `producto.md`, and backend SPECs. Separate intended scope, observed implementation, tested behavior, and evidence of teacher use. Do not infer that interviews do not exist because they are absent from this checkout.
- [x] Resolve criterion scoring terminology with the human: distinguish judging evidence against a criterion from recording an official competency level. Correct contradictory specifications after the intended rule is settled; do not let either model opinion or current code silently decide product policy.
- [x] Align role prompts with chi, pgx, handwritten SQL, existing architecture, and current scope. Remove the unsupported silence/non-use inference and arbitrary frequency dismissal. Do not introduce offline synchronization or additional integrations simply because a prompt suggests them.

**Exit:** met on 2026-09-19. The contract is versioned in `yanai-harness`: this
document, the scope and product inventory in `internal/templates/files/context/`,
and the four role prompts. Workspaces created earlier are brought forward by the
template manifest rather than silently left behind.

Decisions taken: criterion scoring was rescoped with the human — a criterio never
receives a *nivel de logro*, but does carry a per-evidence mark in
`criterion_scores` (prose-only correction, no code change). Offline
synchronization, SIAGIE export and Svelte deliverables were removed from the
prompts as unsupported by scope or code.

Left explicitly unresolved, blocking only affected tickets:

- The criterion-score routes have **no client in `yanai-ui`** — server capability
  with no consumer. Build it or retire the routes; do not let a ticket decide it
  in passing.
- `yanai-ui`'s own SPECs still carry the superseded "a criterio is never graded"
  wording. Untouched on purpose: the UI is out of scope this stage.

Deliberately deferred to Step 3, though found here: the `../app-docente` default
in `yanai.config.json`, the literal `strings.Replace` that `init --repo` uses,
and `repositoryBaseline`'s fallback to hashing an invalid repository path.
Deferred to Step 4: giving each terminal verdict its own persisted state — Step 1
stopped the prompt teaching the collapse and stopped the CLI misreporting it, but
all four still share `PhaseSufficient`.

### 2. Establish a tested backend baseline

Location: `yanai/yanai-server`. Owners: Engineer and DB Architect; human resolves migration lifecycle questions.

- [x] Confirm which databases matter and whether any contain valuable data or applied older migration histories. Keep the current single-migration fresh-install path where appropriate; provide a forward upgrade path where required. Never infer authorization to reset an existing database.
- [x] Use a disposable PostgreSQL instance and deterministic populated fixtures: at least two tenants, two teachers in one tenant, classes, students, terms, competencies, criteria, and representative evidence. Keep fixtures synthetic.
- [x] Exercise the current migration and supported upgrade paths, permissions, constraints, and handwritten queries under the actual `yanai_app` role. Compilation alone is not acceptance for SQL changes.
- [x] Add focused integration tests in the existing `db`, `repo`, `httpapi`, and relevant worker packages. Cover login/session resolution, tenant isolation, same-tenant teacher authorization, roster/evidence reads, numeric/rubric scoring, score correction history, and explicit competency/conclusion writes.
- [x] Reproduce and fix the sampled class-authorization gap according to the agreed ownership/sharing contract. Check analogous resource routes rather than assuming tenant RLS enforces teacher ownership.
- [x] Exercise voice-note creation, transactional job enqueueing, transcription outcomes with a fake provider, and grade recomputation without altering official competency levels. Record the existing no-recovery behavior for abandoned `running` jobs; fix it before a selected increment depends on recovery.
  *Done except the fake provider, which was superseded by an explicit decision to keep `internal/stt` without a provider interface. Only the permanent-failure branch is reachable offline and it is covered; the untestable paths are recorded in that package's Open Questions. See the exit note.*
- [x] Reconcile relevant SQL, Go code, comments, and SPECs based on these results. Ensure database tests report skipped prerequisites visibly and cannot yield a false successful baseline.

**Exit:** met on 2026-09-20, at `yanai` `ffb672a` plus the two PRs below. The
suite runs against a throwaway PostgreSQL 16 created, migrated and dropped per
run, under the real `yanai_app` role, and CI runs it on every push with
`-race -shuffle=on`.

**Migration lifecycle, verified rather than inferred:** the local `yanai-pg`
database was checked and is empty — 44 tables at goose v1, zero rows anywhere,
confirmed as superuser so RLS was not hiding anything. `00001` is a squash and
the only migration there has ever been, so any database anywhere is either
empty or at v1: no older history can exist and no forward upgrade path is
needed. No deployed database was touched, and the tests never touch an existing
one — they create their own.

**The sampled authorization gap was systemic, not one route.** Every
`{classID}`/`{sessionID}`/`{studentID}`/`{assessmentID}`/`{voiceNoteID}`/`{scoreID}`
route was missing the check — about 30 of them. Any teacher could read any
colleague's roster and evidence, fetch any voice note's audio, discard a
colleague's recording, and assert official niveles de logro for classes they
had never taught. Closed behind one shared guard over `class_teachers`; the
assigned / same-school-unassigned / other-school matrix is under test, and the
tests were checked against the bug by neutralising the guard and watching 24 of
them go red.

Recorded, not fixed, as this step directs: an abandoned `running` job does not
merely get lost. `jobs_unique_key_idx` includes `'running'`, so with no reaper
a worker dying mid-job blocks that dedup key **permanently** — every later
`grade.recompute` for that student/class/term is silently dropped. Two tests
demonstrate it and the SPEC now says so.

Residual limitations, all explicit in the relevant SPECs:

- The transcription **success and transient-retry paths are untested** and
  cannot be tested as things stand: `VoiceNoteTranscribe` takes a concrete
  `*stt.Client` with a const endpoint. Only the permanent-failure branch is
  reachable offline. Keeping `internal/stt` free of a provider interface was a
  decision; the cost is recorded in its Open Questions.
- Voice-note creation over HTTP needs ffmpeg on `PATH` (installed in CI). The
  handler is tested below that layer.
- `go test` prints `ok` for an all-skipped package, so the loud skip is visible
  under `-v` and in CI logs, not in a bare `go test ./...`. CI always sets
  `YANAI_TEST_ADMIN_URL`, which is the actual guarantee against a false
  baseline.

Delivered as amaru-one/yanai#3 (the baseline) and #4 (the authorization fix,
which depends on it).

### 3. Bind the harness to the sibling Yanai repository

Location: harness configuration, CLI initialization, repository context, and execution preflight. Owner: Engineer.

- [x] Make the target explicit: for this project it resolves to `<parent>/yanai`, with the Go module at `yanai-server`. Remove the misleading `../app-docente` default and replace literal JSON string replacement with structured configuration updates.
- [x] Define path semantics: resolve `init --repo` from the invocation directory, then store a canonical location; resolve relative configured paths from the configuration file's directory. Invocation from another directory must not change the target.
- [x] Validate the actual Git root, expected module, allowed paths, and repository identity. Fail if the target is missing or points at the harness. Never substitute a hash of an invalid path for a source baseline.
- [x] Separate the harness workspace for engine state from the application repository for code changes. Exclude `yanai-ui`, secrets, Git internals, and unrelated files from model-visible context and application writes.
- [x] Require a clean target baseline before starting a mutating cycle and take a single-writer lock. Planning may inspect dirty state if labelled. Do not stash, discard, or overwrite user edits automatically.
- [x] Test sibling layout, different invocation directories, existing workspace configuration, wrong targets, symlink escapes, and dirty checkout behavior using temporary repositories.

**Exit:** met on 2026-09-20 for repository binding and execution preflight.
`init` recognizes the sibling layout and persists a validated canonical Git root;
relative configuration paths resolve from the workspace. Existing configuration
is updated structurally, preserving model choices and unknown fields. Invalid
legacy paths require an explicit `init --repo` rather than guessing a new target.

Repository context and candidate output paths share a backend-only policy.
Planning labels dirty trees; `run` rejects them and holds a repository-wide OS
writer lock shared across workspaces/worktrees. The baseline binds the checkout
identity, HEAD, and cleanliness; it no longer falls back to hashing an invalid
path. Old plans need regeneration. Full contract/content approval remains Step 7.

The existing CLI source had been unintentionally excluded by the `yanai`
`.gitignore` pattern. This step anchors that rule to the root binary and tracks
`cmd/yanai` so the binding and its CLI tests are present in a fresh checkout.

Validation uses temporary sibling repositories, mock responses and a local fake
OpenRouter endpoint: path resolution, existing configuration, wrong targets,
symlinks, excluded reads/outputs, dirty trees, writer contention and process exit,
and a different checkout with the same HEAD. Local harness checks: `go test
./...`, `go test -race ./...`, `go vet ./...`, and `go build ./...`.

Step 8 still owns actual patch application: the current CLI stages candidates
for review and does not apply them to Yanai. No backend or UI files were changed.
As explicitly requested on 2026-09-20, VoiceNoteTranscribe testing and repair of
the currently broken CI are excluded. Local validation does not claim CI is green.

### 4. Make interview evidence and structured decisions part of the live flow

Location: harness intake, contracts, PO prompt, and CLI. Owners: Product Owner and Engineer.

- [x] Preserve interview source identity, date, revision/hash, and excerpt references; distinguish direct statements, interpretations, and unanswered questions. Redact personal identifiers before external model calls without losing local traceability.
- [x] Link each product proposal to evidence and versioned scope requirements. Permit explicitly labelled technical-enabler tickets for baseline or engine work without fabricating teacher demand.
- [x] Use validated structured proposals and tickets in `analyze` and `discuss`. Enforce supported schema versions, nonblank acceptance criteria, known role IDs, unique IDs, valid dependencies, allowed paths, and required inputs/outputs.
- [x] Give `NO_CHANGE_NEEDED`, `PROPOSE_CHANGE`, `NEEDS_EVIDENCE`, `OUT_OF_SCOPE`, and `BLOCKED_BY_BASELINE` distinct persisted states and CLI next actions. Missing evidence must never be reported as proof that the product is sufficient.
- [x] Replace regex parsing as the authority for new machine decisions. Keep human-readable rendering and deliberate legacy import support. Refuse ambiguous or invalid results after a bounded correction attempt.
- [x] Test absent and contradictory evidence, unsupported quotations, out-of-scope requests, malformed responses, duplicate/cyclic dependencies, and a valid technical-enabler ticket through the actual CLI path.

**Exit:** met on 2026-09-20. New cycles require `--privacy-reviewed`; retain source
hash, redacted revision, interview date when known, local source ID, numbered
excerpts, and the versioned scope snapshot. The original intake stays local with
0600 permissions; only the redacted public source is sent to the provider.

`analyze` and `discuss` now use version-1 JSON proposals and tickets as the
machine contract. The Markdown files are review projections and are not parsed
back into executable tasks. Exact citations, scope/input revisions, finding
types, ticket ownership, backend allowed paths, output declarations, dependency
graphs, attempts, and criteria are checked before persistence. Product tickets
must be grounded in citations; an explicit `--technical-enabler` input carries
engineering rationale instead of invented teacher demand.

The five outcomes have separate phases and CLI next actions. Empty evidence
becomes `NEEDS_EVIDENCE` without a provider call. Contradictions, invented
quotes, unknown references/roles, duplicate keys or IDs, cycles, invalid paths,
unsupported schema, and malformed output receive one bounded correction attempt;
failure leaves no executable proposal. Legacy cycles remain inspectable but are
read-only and must be explicitly re-imported into a new reviewed cycle.

Validation: `go test ./...`, `go test -race ./...`, `go vet ./...`, `go build
./...`, and `git diff --check` pass locally. Tests cover privacy/provenance,
redaction leakage, empty and contradictory evidence, each terminal outcome,
technical-enabler work, malformed/ambiguous provider output, legacy import, and
the live CLI path. VoiceNoteTranscribe tests and CI repair remain excluded by
the explicit project decision.

### 5. Connect durable state and recovery to the CLI

Location: harness workflow store, workspace adapter, and commands. Owner: Engineer.

- [x] Adopt one authoritative workflow store for proposals, tickets, approvals, attempts, usage, and events. Connect all commands to it; stop maintaining conflicting mutable JSON and SQLite authorities.
- [x] Define permitted state transitions and who may request them. Use transactional conditional updates with expected versions, event recording, and task claims. An LLM response cannot directly assign a successful terminal state.
- [x] Scope identities to project/cycle/ticket/revision; make duplicate commands idempotent and reject conflicting replay. Prevent two CLI processes from claiming the same work.
- [x] Add claim expiry and restart reconciliation. For an interrupted external side effect, inspect recorded and actual state before retrying; do not assume it never happened.
- [x] Make immutable runtime content publication reject concurrent overwrites and bind hashes to stored references. Recover orphaned publications or pending transitions after crashes; filesystem and SQLite operations are not one atomic transaction.
- [x] Version storage and import old cycles explicitly. Legacy staged deliverables remain historical/unverified; never promote old `done` records to technically verified work.
- [x] Test the CLI across process restart, concurrent claims, interrupted publication, stale transitions, replay, and legacy import. Unit tests of disconnected helpers do not close this step.

**Exit:** met across three PRs against `yanai-harness`. PR 1 (#5, merged)
built the durable store: ticket claims with lease-based expiry, an attempt
ledger, and conditional transitions. PR 2 (#6) made `workflow.db` the CLI's
authority — every command goes through conditional transitions, takes a
workspace writer lock, reconciles abandoned claims and interrupted
model-call attempts on startup, imports legacy cycles, and replays
`analyze`/`discuss`/`approve` idempotently. PR 3 closed the one piece PR 2
explicitly deferred: `ArtifactStore.Publish` was `Stat`-then-`Rename`, a
real (if unexercised, since nothing called it yet) race, with no link
between what the filesystem held and what SQLite believed. It now binds
publication to `workflow_artifacts` via `BeginArtifact`/`PublishArtifact` —
a plain `INSERT` against the table's existing primary key decides any race,
the same pattern `ClaimTicket` already used — replaces the rename with
write-temp + `os.Link` (atomic on a name collision), and folds artifact
reconciliation into `attachStore` alongside claim/attempt reconciliation: a
pending row with a matching file on disk is published, one with no file is
dropped, and one with a mismatched file is left pending and reported —
never adopted, the same rule legacy import already followed.

**No migration was needed for PR 3.** `workflow_artifacts` (added in PR 1)
already had `state`/`sha256`/`published_at` as free-form columns with no
`CHECK` constraint, so `pending`/`published` simply joined the `legacy`
value PR 2 already wrote there.

**Entregables writes were not rerouted through `ArtifactStore`.** Nothing
in `cmd/yanai` or `internal/team` called `ArtifactStore` before PR 3 —
`Execute` writes entregables candidates with plain `ws.WriteDocument`,
unrelated to this type — so PR 3 completed a capability rather than
changing existing behavior. Routing entregables through it is future work
for whichever step first needs atomic, store-backed publication for real
deliverables.

### 6. Introduce bounded handoffs and extensible roles

Location: harness role registry, prompts, context assembly, and provider adapter. Owners: Engineer and Product Owner.

- [ ] Preserve `product-owner`, `arquitecto-bd`, `ingeniero`, and `disenador`, including configurable OpenRouter model mappings. Add `revisor` as the first additional role; permit an optional software architect or domain researcher when justified by the ticket.
- [ ] Use exact configured IDs and explicit capabilities instead of fuzzy role-name normalization. An added software architect must not accidentally become the DB architect.
- [ ] Give agents the relevant evidence, scope clauses, required SPECs, ticket, dependency outputs, and current diff. Replace the ever-growing shared transcript with addressed, versioned handoffs and bounded clarification requests.
- [ ] Read required SPECs in full before relevant code. If context limits prevent that, narrow the task or fetch in additional bounded steps; do not silently truncate a governing contract.
- [ ] Separate model proposals from deterministic engine authority. Roles can propose revisions and raise conflicts; only validated engine transitions, authorized tools, and human approval advance execution.
- [ ] Preflight the configured OpenRouter models and needed capabilities at implementation time. Validate all responses locally; handle refusal, empty content, truncation, and unsupported parameters explicitly. Record any explicitly configured fallback.
- [ ] Keep `status`, approval inspection, and other local commands usable without a live provider or API key. Test routing and provider failures with fakes.

**Exit:** additional roles work without changing the orchestrator's decision logic, and handoffs remain bounded and traceable.

### 7. Bind human approval to executable work and limits

Location: harness approval, preflight, and accounting. Owners: Engineer; human approves plans.

- [x] Present a concrete reviewable plan: evidence or technical-enabler rationale, ticket graph, allowed paths, expected changes, required tests, dependencies, and execution limits.
- [x] Bind approval to canonical ticket data, scope and contract revisions, repository identity, base commit/content, and execution policy. Changing mutable state outside the approved contract must not authorize different work.
- [x] Track the engine's own successive patch states so an authorized change does not invalidate itself. Stop on unexpected external edits, changed acceptance criteria, or changes outside the approved scope.
- [x] Define configurable token/cost, elapsed-time, model-call, and repair limits. Reserve budget before calls and persist attempts/usage, including retries and failed or truncated responses. Treat unknown cost as unknown, never zero.
- [x] Allow bounded fixes within approved scope without repeated permission requests. Require revised approval for expanded scope or contract changes; keep commit, merge, deployment, and destructive database actions under their explicit project authorization policy.
- [x] Test edited task data, changed source without a new commit, tampered inputs, invalid repository identity, exhausted budget, and an unauthorized attempt to set `verified`.

**Exit:** met locally on 2026-09-20. Approval now binds a versioned execution
contract to canonical ticket rows, evidence/scope and provenance, prompt/model
settings, required checks, policy, repository identity, index and content hashes.
The approval record, active contract, conditional phase change and event commit
in one SQLite transaction. `review` exposes the complete contract; `approve
--contract HASH` can pin the exact reviewed revision. `policy --note` and
`invalidate --note` explicitly revise authorization without clearing consumption.

SQLite schema v3 records cycle-wide reservations, actual provider usage/cost,
HTTP retries, billing reconciliation, bounded repair attempts and active work
leases. Configuration requires finite explicit limits and model price bounds.
Missing billing/usage blocks paid calls across the workspace; `reconcile-attempt`
records confirmed figures and provenance. `--retry-unresolved` acknowledges risk
but cannot erase spending or bypass billing reconciliation. Failed analysis resumes
its existing cycle instead of opening a fresh budget. Human review time is excluded;
abandoned active intervals are charged conservatively through their lease expiry.

The engine records exact intended and confirmed patch successors. Temporary-repo
CLI tests prove authorized successors preserve approval and subsequent external
edits invalidate it. This is the accounting/authorization seam for Step 8, not a
repository patch executor. Candidate responses and files now use immutable,
store-bound publication; candidate readiness commits with its artifact reference.
Inputs are hash-checked before reuse. `response_recorded`, `candidate_ready` and
`awaiting_execution` remain the vocabulary; no actor can set `verified` yet.

Validation: full harness `go test ./...`, `go test -race ./...`, `go vet ./...`,
`go build ./...`, and `git diff --check`. Regression coverage includes stale and
tampered contracts/inputs, changed source without a commit, wrong checkouts,
concurrent reservations, unknown billing, retry/truncation accounting, restart and
migration, exhausted limits, request deadlines, authorized patch successors and
forbidden verification transitions. Step 6 remains deferred by explicit decision.
VoiceNoteTranscribe testing, CI repair, UI work and real application patch execution
were not included. Required project checks are approved data until Step 8 can run
them through controlled tools.

### 8. Implement real changes in Yanai through controlled tools

Location: harness executor; output in the existing `yanai` checkout. Owner: Engineer. Depends on Steps 1–7.

- [x] Add a small execution interface for repository reads/searches, patch application, allowed checks, and diff inspection. Implement the native backend first; preserve a narrow seam for a future alternative backend.
- [x] Execute on the approved Yanai branch/checkout with one writer. Set command working directories explicitly, including `yanai/yanai-server` for Go checks. Apply changes to that checkout rather than leaving the only implementation in `entregables`.
- [x] Enforce filesystem and process boundaries at tool execution: reject escapes and symlink redirection, protect unrelated files, constrain environment/network access, and avoid exposing credentials to model-generated commands. Prompt instructions alone are insufficient.
- [x] Support a bounded edit → check → inspect failure → repair loop. Require full source for edited regions; do not replace complete files from truncated context.
- [x] Record actual patch hashes, source snapshots, commands, exit codes, and outputs as necessary engine evidence. Compare the resulting diff with approved paths and outputs before advancing state.
- [x] Reconcile interruptions around patch application using pre/post content. Never blindly reapply a patch or erase unrelated edits. Empty output is not successful implementation; legitimate no-change outcomes require a distinct explanation and validation.
- [x] Prove the executor first on a temporary sibling layout, then with one small approved backend task in real Yanai. Check that the expected diff exists in `yanai`, that checks ran there, and that no application copy was generated inside the harness.

**Exit:** an approved task produces the intended real Yanai diff and machine-recorded validation evidence within its limits.

**First increment (2026-09-21): native executor foundation only, by explicit
scope agreement.** `internal/executor` supplies complete reads, literal search,
full-source conditional edits, diff inspection, and approved checks. It reuses
the repository path policy and writer lock, immutable artifact publication, and
`PreparePatch`/`ReconcilePatch`. Source journals are durable before patch intent;
atomic destination writes are followed by actual snapshot comparison. Recovery
adopts an exact completed patch, rolls back a partial batch, and refuses to
overwrite a third state. No workflow schema migration is needed. The module now
requires Go 1.26.6 for the rooted filesystem operations.

The status prediction was compared with real Git porcelain bytes, including
tracked/untracked ordering, deletion, spaces, tabs, UTF-8, and returning a file
to HEAD. It uses the broad backend fingerprint, not narrowed context paths.
Unsupported Git behavior that changes the predicted status fails the post-write
comparison and triggers rollback. Tests exercise actual child-process exit at
journal publication, intent, staged bytes, partial application, and complete
application; an executable-bit prediction mismatch exercises rollback too.

Checks accept only approved IDs for `go test`, `go vet`, and `go build`, with
restricted arguments, explicit directories, readonly modules, offline module
resolution, a fresh environment, bounded output, timeout/process-group cleanup,
and immutable start/result evidence. Tests require an operator-supplied
loopback disposable PostgreSQL URL. This is not a sandbox for hostile Go code:
tests still run with the host user's filesystem and network privileges. The
writer lock coordinates harness writers, not arbitrary concurrent host writers;
observed external edits block reconciliation. Unexpected check writes are
preserved and block further execution, not automatically deleted. A command
start without a finished record blocks further patches/checks: after a harness
crash its subprocess may still be running. Operator reconciliation of that
condition is deferred to the orchestration increment; it is never auto-retried.

Validation: the full harness source suite passed with `go test -race
-shuffle=on ./...`, plus `go vet ./...` and `go build ./...`, in a clean temporary
source export. Running `./...` in the working checkout also discovers the
pre-existing ignored candidate Go files in `yanai-workspace/entregables` and
fails on their application imports; those files were preserved. The executor's
seven new test functions were checked with 17 deliberate production mutations,
including omission of the database environment variable and killing only a
check's parent process. Each produced a failing assertion and was reverted.
The final native runner also passed `go test -v -race -shuffle=on -count=1
./...`, vet, and build in the real clean Yanai backend against local PostgreSQL;
the integration test rejected skipped tests and verified an unchanged repository
snapshot. This validates real check execution, not real application patching.

At the end of that first increment the remaining six checkboxes were still
open: `run` staged candidates, and no repair loop, budget integration, new
ticket transitions or approved real application patch was claimed.

**Second increment (2026-09-21): `run` drives the executor.** Approval now
binds an execution contract revision that names the backend, the candidate wire
format and the exact root, worktree, HEAD and branch the diff lands in; a
revision-1 contract is refused before any model call or write, so an approval
recorded when `run` only staged candidates can never authorize writing. A
durable execution round, keyed by project, cycle, contract, ticket revision and
round, records the complete inputs, the model attempt and raw response, the
validated candidate, the expected pre-state, the patch receipt, the confirmed
repository hash, every check run with the state it tested, and the outcome.
Rounds advance forward only under a conditional version.

`implemented` and `no_change_reported` have no edge in the ticket transition
table at all: they are reachable only through `MarkImplemented` and
`MarkNoChange`, which verify the round's evidence and commit the ticket's
status in the same transaction. `MarkImplemented` requires a confirmed patch
that differs from the pre-state, a published manifest, and every approved check
passing against exactly the confirmed state. Only `implemented` satisfies a
dependency — a staged candidate and a reported no-change do not — and a cycle
whose tickets all reached an outcome, with at least one real change and the
approved checks passing against the final state, enters `awaiting_review`.

`run`'s order is: workspace lock and artifact reconciliation (already done when
the store attached), contract-revision and checkout-identity validation,
executor open with pending-patch recovery, approval and state recheck, durable
round resume, and only then a model call. Patch state is deliberately not read
before recovery, because a pending patch is what recovery exists to resolve,
and no outer repository lock is held while the executor opens its own.

Execution context is complete or the round stops: the full current source and
mode of every declared output, explicit absence for a new file, `AGENTS.md` and
the `SPEC.md` of every directory on the way to each output, the model's own
selection of supporting files read whole, and the ticket, dependency and
repository data. An oversized context is an error; a supporting file that is
too large, denied or absent is named with its reason rather than dropped
silently. A SPEC.md is created only when it is an approved output of the
ticket.

Candidates are strict JSON: schema version, `change` or `no_change`, an
explanation, and exactly one entry per declared output carrying complete
source, a deletion or an explicit `unchanged`. Unknown fields, duplicate keys
or paths, trailing content, truncation, undeclared or omitted outputs,
unencodable source and effective no-ops are all refused, and a refusal is a
rejected response, never a narrowed application. Legacy `=== ARCHIVO ===`
extraction remains for reading history; `run` no longer writes an application
mirror under `entregables`.

Failed checks keep the recorded diff in the checkout and feed the repair round
the real failure output and the actual diff. Repairs are bounded by the
ticket's `max_attempts` and the cycle's repair budget together, charged before
a replacement round opens. A resumed candidate or a confirmed patch costs no
new generation. A timeout, a cancellation, truncated evidence or a check that
wrote to the repository is an unknown outcome, not a verdict: the round keeps
its confirmed patch, execution stops for a person, and a later run re-runs the
checks rather than regenerating a patch that is already applied. A test check
refuses to run without a disposable loopback PostgreSQL URL, and a zero exit
code is not acceptance — skipped tests, a package that executed no tests and
output that fabricates success are all failures.

Validation: the full harness suite passed with `go vet ./...`, `go build
./...` and `go test -race -shuffle=on -count=1 ./...` in a clean temporary
source export, plus `git diff --check`. Thirty deliberate production mutations
were applied one at a time against the new tests — including promoting a stale
staged candidate, letting a no-change satisfy a dependency, reaching
`implemented` through a generic status update, accepting a check that ran
against another state, summarizing preimages instead of supplying them whole,
inheriting the operator's environment into a check, treating a timeout as a
verdict, discarding a confirmed patch on an unknown outcome, skipping the
evidence re-read, and writing an application mirror again. Each produced a
failing assertion and was reverted; the three that initially survived were
answered by strengthening the test, not by weakening the mutation.

The store migrates to schema v4, which only adds `workflow_rounds`; no
existing row is rewritten. This was exercised against the project's real
workspace, whose cycle was approved and staged under the previous flow: it
stays inspectable through `status`, its staged candidates are untouched, and
`run` refuses it with the contract-revision message that names the remedy
rather than applying anything.

Crash coverage is split deliberately: `internal/executor` exercises real child
processes exiting at journal publication, patch intent, staged bytes, partial
and complete application, and the orchestration tests resume from the durable
round after an external edit interrupts application and after a check produces
an untrustworthy outcome, proving neither regenerates nor reapplies.

**Real-Yanai acceptance (2026-09-21).** A fresh technical-enabler cycle was
analyzed, discussed, reviewed and approved through the real CLI against the
real `yanai` checkout, and `yanai run` applied its diff there:
`yanai-server/internal/grading/compute_test.go` gained
`TestDropLowestNNamesExactlyWhichScoresWereDroppedAndKept`, which pins the
exact dropped score id, the exact counted ids, their disjointness and the
unchanged 0.90 result, and that folder's `SPEC.md` records the new coverage.
Production behavior is unchanged: `compute.go` was not touched. The three
approved checks ran in `yanai/yanai-server` against a disposable loopback
PostgreSQL — `go test -v -race -shuffle=on -count=1 ./...`, `go vet ./...`,
`go build ./...` — all passing against exactly the applied state, with the
recorded verbose evidence showing the database packages running, the new
regression running and nothing skipped. The regression was mutation-tested in
the real repository by making `compute.go` record the highest scores as dropped
while still dropping the lowest from the mean: the pre-existing
`TestDropLowestN` passed and the new one failed on both the identity and the
disjointness, and the mutation was reverted. A rerun changed nothing, a human
file dropped into the checkout stopped the run and was preserved untouched, no
application copy was produced in the harness, and the approved diff is left
uncommitted for review.

The demonstration used a local deterministic provider in place of OpenRouter,
so the contract, human approval gate, executor, checks and evidence are real
while the model's text is fixed and reviewable. A live-provider run is not
claimed.

**Limitations carried forward.** This is not a sandbox for hostile Go code:
approved checks still run with the host user's filesystem and network
privileges. The writer lock coordinates harness writers, not arbitrary
concurrent host writers. A check that starts without a finished record still
blocks further patches and checks until an operator reconciles it; the harness
never auto-retries it and never kills a process from a persisted PID.
`commit`, `merge`, `deploy`, `destructive_db` and `yanai-ui` remain out of
scope, and independent technical review is Step 9.

### 9. Gate completion on independent technical review

Location: harness review/scheduler; validation in Yanai. Owners: Reviewer, Engineer, and relevant contract owner.

- [ ] Distinguish `implemented`, `verification_failed`, `revision_required`, `verified`, and, where applicable, `integrated`. Retire the equivalence between “model replied” and “done.”
- [ ] Have `revisor` inspect the exact diff, relevant SPECs, evidence, and acceptance criteria independently of the implementer's success claim. The trusted runner executes required checks and records results; reviewer prose cannot override failure.
- [ ] Bind verification to the exact content tested. Later changes invalidate affected verification and dependent results. Schedule dependents in graph order only when their required outputs have passed the appropriate gate.
- [ ] Route defects to the implementation owner and contract ambiguities to their owner. Bound repair attempts; do not let the reviewer weaken criteria to make a change pass.
- [ ] Test a broken SQL query, same-tenant authorization failure, empty output, forged test-success text, failed required test, and changed diff after review. Each must prevent verified completion.
- [ ] Report technical verification separately from commit/merge, deployment, and teacher acceptance. None is implied by another.

**Exit:** the actual CLI refuses false completion, and independent review accepts a real tested backend change.

### 10. Complete one increment selected from real interviews

Location: harness cycle and `yanai/yanai-server`. Owners: human interviewer, Product Owner, core specialists, Reviewer.

- [ ] Select one concrete teacher problem from supplied, redacted interview evidence. Let the human choose between defensible priorities; do not preselect a new observation entity merely because it appears in the scope document.
- [ ] Translate the need into the smallest useful backend increment and explicit acceptance criteria. Preserve the difference between existing voice transcription and any proposed structured observations or generated conclusions.
- [ ] Run the complete evidence → plan → approval → implementation → verification cycle using the actual configured provider within an agreed budget. Mocks prove mechanics, not model effectiveness.
- [ ] If the increment involves generated conclusions, verify evidence support, teacher edits, draft handling, and explicit approval before official recording; generation must not silently assert an official competency level.
- [ ] Measure verified task completion, failures, repair attempts, cost, elapsed time, and human interventions. Distinguish technical readiness from measured teacher benefit.
- [ ] Return results to the interviewer and capture follow-up evidence for the next cycle. Keep mobile usability and full teacher acceptance pending until the later UI stage makes those tests possible.

**Exit:** one interview-supported backend increment exists in Yanai with traceable approval and passing checks. Any outstanding user-facing acceptance is clearly identified.

### 11. Prove the recipe on a second project

Location: harness configuration/adapters and a separately authorized target repository. Owners: Engineer and Product Owner.

- [ ] Extract only the boundaries demonstrated by Yanai: project scope, evidence intake, repository/tool policy, role/model mapping, contracts, checks, budgets, and approval rules. Keep MINEDU assumptions in Yanai's project configuration and prompts.
- [ ] Run a small unrelated software task through a second configured repository with its own acceptance checks. It must not require changes to the generic scheduler or evidence/approval rules.
- [ ] Confirm project isolation: approvals, file paths, state, and outputs from one project cannot affect another.
- [ ] Update the existing harness README with the proven onboarding recipe: configure project → load scope/evidence → verify baseline → approve bounded plan → execute/review → collect feedback. Avoid introducing a parallel documentation system.

**Exit:** a second project completes a verified task through configuration and existing adapters, demonstrating reuse beyond Yanai.

### 12. Evaluate MetaGPT later against the working native backend

Status: deferred experiment, not a prerequisite for Steps 1–11.

- [ ] Establish a fixed evaluation set from observed needs and failures: valid increment, missing evidence, out-of-scope request, SQL defect, authorization defect, stale approval, interrupted execution, and budget exhaustion.
- [ ] First compare the native team with a simpler engineer-plus-review configuration under comparable tasks, models, budgets, and acceptance gates. Confirm that extra roles improve outcomes enough to justify their cost.
- [ ] Then evaluate a bounded MetaGPT adapter using the same tickets, repository permissions, approval rules, and verification contract. Reassess its then-current capabilities; do not rely on the old proposal's static inspection.
- [ ] Compare verified completion, human intervention, cost, latency, recovery, and maintenance effort over repeated runs where practical. Adopt an alternative only if measured benefits justify its operational cost.

**Exit:** an evidence-based keep/adopt/defer decision. The native harness continues to work regardless of the experiment's outcome.

## Completion checklist for the first Yanai milestone

- [ ] A real interview excerpt can be traced to a scoped, human-approved ticket.
- [ ] The live harness uses validated contracts and durable workflow state.
- [ ] The approved task changes the sibling `yanai` repository directly.
- [ ] Database behavior is tested against populated disposable PostgreSQL under the application role.
- [ ] Independent review and machine-run checks determine technical completion.
- [ ] Restart, stale approval, wrong target, invalid paths, and budget exhaustion cannot become successful execution.
- [ ] The four original roles remain available; extra roles have explicit responsibilities.
- [ ] No `yanai-ui` changes or unsolicited documentation artifacts are produced.
- [ ] Teacher acceptance and measured benefit remain distinct from backend test results.

Steps 1–10 deliver this first milestone. Step 11 proves portability. Step 12 informs a later execution-backend choice.
