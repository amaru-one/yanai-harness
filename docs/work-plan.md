# Engineer-led ticket workflow: work plan

An already-decided Markdown task produces an approved, bounded change in a
configured Git repository and machine-recorded validation evidence:

**Ticket → engineer plan → relevant specialists → human responses to every
observation → review → human approval → native execution and checks → awaiting review.**

One Markdown ticket starts one cycle. The engineer leads and owns implementation.
DB architect and designer are consulted only when the engineer records a reason.
Product Owner and interviews are retired from the active CLI. Existing engineer,
DB architect, and designer prompt files remain byte-for-byte unchanged, including
through workspace initialization and upgrades. Orchestration instructions live in
code; conflicts with local prompts must become observations.

Every observation, including advisory concerns, pauses progress. Human resolution
is distinct from execution approval. Changed requirements require a new ticket
revision and plan. Execution-time observations require a supplemental review and
approval of their responses under the original executable contract; they do not
reset confirmed patches or authorize wider scope.

Keep Go 1.26.6 for the harness itself, the project/CLI names, model selections,
durable storage,
budget accounting, approval contracts, repository writer lock, native executor,
patch prediction/recovery, and evidence gate. No UI implementation or MetaGPT
integration is included. The current delivery stops at awaiting_review; independent review remains unfinished in Step 9.
`git` is not a check command. `commit`, `merge`, `deploy`, and `destructive_db`
remain hard-refused.

## Focused delivery sequence

Each item has one purpose and must be independently reviewable. Checkboxes describe
verified exits, not merely code written. No PR is implied to have been published.

### 1. Replace the roadmap

- [x] Preserve the historical roadmap and its recorded limitations.
- [x] Document ticket intake, engineer leadership, selective consultation, prompt
  preservation, observation pauses, and human approval.

Exit: active requirements no longer depend on teacher interviews or the deleted
Yanai repository.

### 2. Generalize repository binding and checks

- [x] Require explicit Git repository binding and configured allowed paths. Go
  module validation is optional; root and nested Go modules remain supported.
- [x] Preserve identity, clean baselines, protected paths, writer locking, and
  fingerprints independent of narrowed model context. Compare predicted status
  bytes with the real Git oracle; retain rollback and crash-recovery coverage.
- [x] Support operator-configured executable allowlists and explicit check evidence
  rules across languages; retain the optional Go test/vet/build adapter. PostgreSQL
  is an explicit per-check prerequisite rather than universal.

Exit: approved native checks run without a Yanai checkout or a mandatory Go
module/toolchain. Language specifications stay in the existing local role prompts.

### 3. Durable ticket intake and observations

- [x] Parse the small Markdown format before model calls; snapshot its bytes as a
  hashed immutable artifact and trace tasks to original acceptance criteria.
- [x] Persist planning progress, model responses, observations, and human responses;
  resume without repeating recorded calls or bypassing unknown billing.
- [x] Retire public `analyze` and `discuss`, preserving historical records.

Exit: valid tickets create durable cycles; malformed tickets make no model calls;
observations survive restart and stop planning, approval, and execution.

### 4. Engineer-led orchestration

- [x] Use one engineer planning turn when no specialist is needed; selected
  specialists each receive one turn, followed by engineer consolidation.
- [x] Persist reasons for specialist selection and per-role provider accounting.
  Human-triggered continuation stays within cycle budgets.
- [x] Eliminate PO from active defaults and calls; protect the three local prompts
  from template replacement and upgrade sidecars.

Exit: controlled-provider tests demonstrate routing, immediate observation pauses,
replay without duplicate calls, and prompt integrity.

### 5. Approval, execution, and compatibility

- [x] Bind the new source and plan to the existing native executor and evidence gate.
- [x] Version executable contracts; old approvals cannot authorize the new workflow.
  Refuse schema upgrade while an old mutation remains unreconciled.
- [x] Require fresh human review/approval after execution observations; preserve the
  original patch journal and already-confirmed changes.
- [x] Keep local history readable without the former target; update help, examples,
  mocks, and disposable integration fixtures.

Exit: an approved Markdown ticket creates the intended real project diff and
validation evidence within its limits, ending at `awaiting_review`, not `verified`.

## Unfinished legacy steps, adapted to the current app

These retain the original step numbers for traceability. Steps 7–8 supplied durable
artifacts and native execution; the focused sequence above changes their intake
and repository assumptions. Selective consultation does not complete Step 6;
fixtures do not complete the real-provider pilots in Steps 10–11. The historical
[roadmap](work-plan-legacy.md) records the former four-role/Yanai design and is not
the active specification.

### Step 6. Bounded handoffs and model capability contracts

- [ ] Define versioned, addressed handoffs for exactly engineer, DB architect, and
  designer; engineer owns implementation and records why each specialist is needed.
- [ ] Bound incoming context, requested documents, replies, retries, and cumulative
  cycle cost. Preserve durable responses and unknown-billing reconciliation.
- [ ] Validate configured models against the capabilities their assigned work needs;
  refuse unsupported configuration before dispatch, without introducing a PO role.
- [ ] Keep language/framework instructions in operator-owned template prompts and
  language checks in approved execution policy. Surface conflicts as observations.
- [ ] Verify handoff interruption/replay, malformed replies, budget exhaustion,
  omitted specialists, and every observation's mandatory human pause.

Exit: the three-role team exchanges bounded, validated, recoverable handoffs with
no hidden language assumptions or unapproved continuation.

### Step 9. Independent technical review and bounded repairs

- [ ] Define review of the exact applied diff, ticket criteria, and immutable check
  evidence, with a reviewer independent of the implementation turn. Start with
  human technical review; any automated review must use an appropriate retained
  role in fresh context, with explicit competence limits, not add a fourth role.
- [ ] Record structured findings and evidence references; distinguish implemented,
  verification_failed, revision_required, and verified outcomes. Passing checks
  alone must not certify acceptance, and reviewer prose cannot override failures.
- [ ] Implement bounded repairs under the approved scope and budgets; changed scope
  or new observations require human resolution and the appropriate fresh approval.
- [ ] Re-run affected checks and review the resulting exact diff after repairs;
  retain earlier findings and evidence through crashes and retries.
- [ ] Test unsupported approval, stale evidence, failed checks, repair exhaustion,
  reviewer independence, and recovery. Keep commit/merge/deploy/destructive_db refused.

Exit: independent technical review supports a machine-recorded verified outcome,
with human approval mandatory and no implicit publication permission.

### Step 10. First real Markdown-ticket pilot

- [ ] Select a small already-decided Markdown task in an available repository; set
  its language-specific template prompt, executable allowlist, and required checks.
- [ ] Obtain human approval of scope and an explicit paid OpenRouter budget; use
  the engineer-led three-role flow, consulting specialists only with recorded reasons.
- [ ] Exercise real planning, observations when present, approval, native changes,
  real checks (including disposable PostgreSQL where required), and Step 9 review.
- [ ] Record per-role/provider tokens and actual costs, wall time, repairs, human
  interventions, final diff, and criterion-level evidence. Report skips and unknowns.

Exit: a real-provider cycle meets its approved ticket criteria within recorded
limits. Controlled responses and synthetic fixtures are not this pilot's evidence.

### Step 11. Portability pilot in a different language

- [ ] Choose a second independent repository using a different language/toolchain.
- [ ] Configure repository scope, existing role prompts, installed tools, evidence
  rules, and budgets without changing orchestration code or introducing new roles.
- [ ] Run a paid, human-approved Markdown cycle through independent review, with
  real toolchain checks and any required disposable database assertions.
- [ ] Compare costs, role usage, interventions, failures, and acceptance evidence
  with Step 10; document actual portability limits and remaining gaps.

Exit: two real projects demonstrate configuration-driven portability across
languages. Passing Go and Python fixtures alone does not satisfy this exit.

## Validation standard

Run package tests, vet, build, and relevant concurrency suites with
`-race -shuffle=on`. Each new test must be checked by deliberately introducing its
bug, observing failure, reverting the mutation, and rerunning.

Exercise actual PostgreSQL assertions against a disposable local instance; report
missing prerequisites as skips, never database acceptance. Distinguish provider
mocks, stub toolchains, real Go checks, and real database results. Do not claim paid
model behavior or a measured cost reduction without a separate real-provider run.

Use [the ticket template](ticket-template.md) and README for the operator flow.

## Verification record: original ticket-flow delivery

Implemented locally on 2026-09-23. No pull requests or commits were created.

- The full package suite passed with `go test -race -shuffle=on ./internal/... ./cmd/...`.
  The disposable PostgreSQL URL was supplied; the database fixture was not skipped.
- Follow-up race/shuffle checks covered the final ticket, initialization, workflow,
  and team changes. `go vet ./internal/... ./cmd/...` and a CLI build passed.
- Controlled-provider CLI scenarios cover engineer-only work, both selected
  specialists, advisory observation pauses, interrupted response/observation
  recovery, unknown billing reconciliation, execution observations and supplemental
  approval, refusal before approval, immutable evidence, and history without a target.
- The CLI acceptance fixture also runs a real Go test against the exact generated
  file. Separate native fixtures run real test/vet/build in root and nested modules;
  PostgreSQL 16 executes temporary-table inserts and an asserted query result.
- Thirteen deliberate behavioral mutations were detected and reverted: observation
  gating, fingerprint narrowing, prompt replacement, wrong task owner, ignored
  specialist pause, missing execution reapproval, library-build regression, wrong
  database result, unsafe upgrade, unreconciled billing, discarded response replay,
  lost observation recovery, and incorrect generated content in the real CLI check.
- The historical roadmap and the three embedded role prompts were compared with
  their original bytes. Local operator prompt files were not edited.

Provider behavior was exercised with controlled responses, not paid OpenRouter
calls. No model-quality result, cost-reduction percentage, arbitrary SQL migration
safety, or independent technical review is claimed. The native executor retains
its existing supported-patch limits and safe refusal/recovery behavior.

## Language-neutral executor follow-up

The executor now accepts explicitly configured runtime binaries, exact approved
arguments, and declared exit-code/output evidence rules. The Go adapter remains
optional. Initialization and context selection no longer impose a Go module or
file-extension default; template version 8 preserves all three role prompt files.

Validation performed on 2026-09-23:

- A controlled-provider Markdown cycle initialized a repository without `go.mod`,
  applied the approved document, and ran real Python unittest assertions against
  its contents. An invalid Go-binary setting did not affect this cycle.
- Real-runtime checks covered nonzero exits, missing success output, skipped-test
  failure patterns, credential isolation, executable invocation-path preservation,
  forbidden commands and shell aliases, and workspace-local executable refusal.
- A focused evidence test demonstrated that a configured non-Go `test` command is
  not parsed as Go output. This is parser coverage, not a real Rust-toolchain run.
- Eleven deliberate faults produced behavioral test failures and were reverted:
  missing success detection, ignored failure output, shell-alias acceptance,
  writable-tool acceptance, mandatory Go discovery, wrong generated content,
  Go-only context selection, Go parsing of other test output, discarded invocation
  paths, direct deployment executable acceptance, and workspace-local runtime aliases.
- Real Go root/nested-module and PostgreSQL temporary-table assertions passed.
  The three embedded role prompts still match their original bytes.
- The internal and complete CLI package suites passed with `-race -shuffle=on`.
  Vet and a CLI build passed. Combined runs exposed a legacy fixture flake:
  a temporary path produced an all-numeric source digest that the old interview
  redactor altered. The execution fixture now supplies a deterministic opaque
  source ID; production interview behavior was not changed.

The allowlist trusts installed runtimes and project test code; it is not process
isolation. Generic exit-code evidence proves only command success. Operator-chosen
output rules determine whether skipped or zero-test suites are rejected. These
fixtures do not complete legacy Steps 6, 9, 10, or 11 or demonstrate paid-model
quality, costs, or independent review.
