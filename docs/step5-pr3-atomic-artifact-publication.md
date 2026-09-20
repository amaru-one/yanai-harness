# Step 5, PR 3 — Atomic artifact publication and startup reconciliation

## Context

PR 1 (#5, merged) built the workflow store. PR 2 (#6, open) made `workflow.db`
the CLI's authority: every command now goes through conditional transitions,
takes a workspace writer lock, reconciles abandoned ticket claims and
interrupted model-call attempts on startup, imports legacy cycles, and
replays `analyze`/`discuss`/`approve` idempotently.

One piece from the original Step 5 plan was explicitly deferred out of PR 2's
scope, stated in its own PR description:

> Artifact publication is still `Stat`-then-`Rename` (not yet atomic), and
> there's no startup reconciliation for it — that... is PR 3.

`internal/workflow/artifacts.go`'s `ArtifactStore` had been untouched since
before this work started. `grep -rn "ArtifactStore{" --include='*.go' .`
returned exactly one hit: its own test,
`TestArtifactStorePublishesImmutablyAndDetectsTampering` in `workflow_test.go`.
Nothing in `cmd/yanai` or `internal/team` called it — `Execute` writes
entregables candidates with plain `ws.WriteDocument`, unrelated to this type.
That meant PR 3 had no existing caller to preserve compatibility with, but
also that the defect was real and unexercised, exactly the kind of thing that
bites the first real caller (a later step) rather than anything before it:

```go
// artifacts.go:47-54, before PR 3
if _, err := os.Stat(dest); err == nil {
    return ArtifactRef{}, fmt.Errorf("artifact already exists: %s", path)
} else if !os.IsNotExist(err) {
    return ArtifactRef{}, err
}
if err := os.Rename(tmpName, dest); err != nil {
    return ArtifactRef{}, err
}
```

`Stat` then `Rename` is two steps, not one: two concurrent publishers can both
observe "not exists" and both proceed, and the second `Rename` silently wins
— exactly the race Steps 1–4's claim/attempt work already fixed for tickets
and model calls, still open here. There was also no link between what the
filesystem held and what SQLite believed: a crash between writing the file
and recording it would leave neither side able to tell what happened without
a human looking.

**Outcome:** publishing an artifact is atomic and race-safe, its hash is
bound to a stored row rather than trusted from the caller, and a process that
dies mid-publish leaves a state that startup reconciliation resolves —
consistent with claims and attempts — rather than an orphaned temp file or a
permanently stuck row.

## Decisions

| Decision | Choice |
|---|---|
| Scope | Fix and complete `ArtifactStore` itself, wired to `workflow.Store` for the pending/published bookkeeping. **Do not** reroute `Execute`'s entregables writes through it. Nothing used `ArtifactStore` for real work before this PR, so this completes a capability rather than changing existing behavior — rerouting entregables would have touched several already-passing tests for no requirement that named it. |
| Race resolution | At the **store** level, not the filesystem: `BeginArtifact` is a plain `INSERT` against `workflow_artifacts`' existing `PRIMARY KEY (project, cycle, ref_id)` and `UNIQUE (project, path)`. Two concurrent publishers of the same ref or path have exactly one `INSERT` succeed; the loser gets a constraint error before ever touching the filesystem. Same pattern PR 1's `ClaimTicket` already uses. |
| Filesystem atomicity | Replaced `Stat`-then-`Rename` with write-temp + `os.Link` (atomic `EEXIST` on a name collision) + `os.Remove` the temp name. A `Link` failure other than `EEXIST` (e.g. no hardlink support) falls back to `O_CREATE\|O_EXCL`. |
| Schema | **No migration needed.** `workflow_artifacts` (v1, PR 1) already had `state`, `sha256`, `published_at` as free-form columns with no `CHECK` constraint — `pending`/`published` join the `legacy` value PR 2 already writes. |
| Reconciliation | Folded into the existing `attachStore` in `cmd/yanai/main.go`, alongside `ExpireClaims`/`ReconcileAttempts` — one more thing every store-opening command resolves on startup, not a separate code path. |

## Scope boundaries

- No new migration, no new CLI flags, no change to `Execute`'s entregables
  writes or their existing tests.
- No orphan *adoption*: a file on disk with no matching row is reported, never
  silently turned into a row (same rule PR 1/2 apply to legacy import — never
  promote something unverified into looking authoritative).
- The backend repo (`yanai`) is untouched; this is entirely `yanai-harness`.

---

## 1. Store-side bookkeeping — new `internal/workflow/artifact_records.go`

Named apart from the existing `internal/workflow/artifacts.go` (filesystem
side) to keep "the DB row" and "the file" visually distinct, matching the
`store.go` / `claims.go` / `attempts.go` split from PR 1.

- [x] Create `artifact_records.go` with `ArtifactRecord` and the four store
      methods (`BeginArtifact`, `GetArtifact`, `PublishArtifact`,
      `PendingArtifacts`).

```go
type ArtifactRecord struct {
    Project, RefID, Path, SHA256, Version, State string
    Cycle                                        int
    CreatedAt, PublishedAt                       time.Time
}

func (s *Store) BeginArtifact(cycle int, ref ArtifactRef) (ArtifactRecord, error)
// INSERT ... state='pending'. A UNIQUE-constraint error on (project,cycle,ref_id)
// or (project,path) is surfaced as-is -- callers treat any error here as
// "already claimed by someone else, re-check GetArtifact".

func (s *Store) GetArtifact(cycle int, refID string) (ArtifactRecord, error)

func (s *Store) PublishArtifact(cycle int, refID, sha256 string) (ArtifactRecord, error)
// UPDATE ... SET state='published', published_at=now() WHERE state='pending'.
// RowsAffected != 1 is an error: either already published (benign, caller
// checks GetArtifact first) or something stranger.

func (s *Store) PendingArtifacts() ([]ArtifactRecord, error)
// All state='pending' rows for this project, across cycles -- reconciliation's
// input. Same collect-then-fetch shape as ReconcileAttempts to avoid the
// single-connection nested-query deadlock PR 1 already hit once.

func (s *Store) DropPendingArtifact(cycle int, refID string) error
// DELETE ... WHERE state='pending' -- guarded so it can never delete a
// published row by accident.
```

## 2. Filesystem side — rewrite `Publish`/`Read` in `internal/workflow/artifacts.go`

- [x] Rewrite `Publish`/`Read` to route through the store
      (`BeginArtifact`/`GetArtifact`/`PublishArtifact`).
- [x] Implement `linkIntoPlace` (write-temp + `os.Link` + `os.Remove`, with
      `EEXIST` hash-check and `O_CREATE|O_EXCL` fallback).

`Publish(store *Store, cycle int, ref ArtifactRef, content []byte) (ArtifactRef, error)`
resolves `ref.Path`, computes the actual hash, then looks up the existing
record: a `published` row with a matching hash is a benign replay (returned
as-is); a `published` row with a different hash is refused; a `pending` row
is finished (a previous attempt committed the row but died before linking or
publishing); no row means a fresh `BeginArtifact` — losing that race means
someone else is publishing this ref. Only after the row is committed does it
call `linkIntoPlace` and `PublishArtifact`.

`linkIntoPlace` writes `content` to a temp file beside `dest`, `os.Link(tmp,
dest)`, `os.Remove(tmp)`. On `Link`'s `EEXIST`, it hashes the existing file at
`dest`: matches → benign (another process's earlier attempt already placed
identical bytes; harmless to proceed), differs → error, never overwrite.
Falls back to `os.OpenFile(dest, O_CREATE|O_EXCL, ...)` when `Link` fails for
a reason other than `EEXIST` (e.g. cross-device or no hardlink support),
preserving the same all-or-nothing guarantee.

`Read` verifies against the **stored** record (`store.GetArtifact`), not a
caller-supplied `ArtifactRef` — closing the "trust the caller's hash" gap the
original Step 5 plan called out.

## 3. Reconciliation — extend `attachStore` in `cmd/yanai/main.go`

- [x] Wire `reconcileArtifacts` into `attachStore`, right after
      `store.ExpireClaims()` / `store.ReconcileAttempts()`.
- [x] Add the `DropPendingArtifact(cycle, refID) error` store method.

`reconcileArtifacts` resolves every pending artifact left by a process that
died between committing the row and finishing the publish:

- file present, hash matches → flip to published (the file made it, only the
  final store write didn't)
- file absent → drop the pending row (nothing to adopt)
- file present, hash mismatches → report and leave pending; never adopt

The workspace root (`w.Root`, the same base `ArtifactStore{Root: w.Root}`
would use) is what reconciliation reads against, since nothing publishes
anywhere else yet.

## 4. Tests

**`internal/workflow`** (`artifact_records_test.go`, plus the existing
`TestArtifactStorePublishesImmutablyAndDetectsTampering` updated in
`workflow_test.go`):

- [x] Concurrent `Publish` from parallel goroutines (same pattern as PR 1's
  `TestConcurrentClaimsProduceExactlyOneWinner`) for the same ref → exactly
  one winner, the rest get a clean error, no partial file left behind.
- [x] A benign replay (identical content, same ref) after a successful publish
  returns the same ref without error and without touching the file again.
- [x] A conflicting replay (different content, same ref or same path) is
  refused, both when the existing row is `published` and when it's `pending`
  with a file already on disk.
- [x] `Read` refuses when the on-disk file's hash no longer matches the stored
  record (tampering after the fact).
- [x] Reconciliation's three branches: pending+matching-file → published;
  pending+no-file → dropped; pending+mismatched-file → left pending, reported,
  never adopted.
- [x] The existing `TestArtifactStorePublishesImmutablyAndDetectsTampering` is
  updated for the new store-backed signature; its core assertion (a
  different-hash overwrite is refused) is unchanged.

**`cmd/yanai`** (`store_test.go`):

- [x] `TestArtifactReconciliationRunsThroughAttachStore` confirms
  `attachStore`'s reconciliation actually runs artifact reconciliation on a
  workspace with a hand-crafted pending row + matching file, through a real
  command invocation (`cmdStatus`), the same way PR 2's `store_test.go` tests
  reconciliation for claims/attempts through the CLI rather than only at the
  package level.

## 5. Close out Step 5

- [x] `docs/work-plan.md`: Step 5's boxes checked, its **Exit** note written
  (mirroring Steps 1–4's style), recording the "no migration needed" and
  "entregables not rerouted" scope notes for future steps to see.
- [x] `README.md`: the line *"El estado actual sigue en `state.json`; el
  almacenamiento SQLite aún no está conectado"* was false and is corrected.

## Verification

- [x] Automated:

```sh
cd /Users/amaru/yanai-context/yanai-harness

go build ./... && go vet ./... && gofmt -l .
go test ./... -count=1 -race -shuffle=on
make demo
```

- [x] Manual, by hand against a scratch workspace with `w.Store` attached:

```go
// simulate a crash between linking the file and marking it published
store.BeginArtifact(1, ref)
os.WriteFile(filepath.Join(root, ref.Path), content, 0o600) // the "file made it"
// no PublishArtifact call -- the "process" died here
```
then running any command (`yanai status`) confirms the row flips to
`published` without duplicating or corrupting the file; repeating without the
`os.WriteFile` step confirms the row is dropped instead.

## Delivery

Branch `feat/atomic-artifact-publication`, based on PR 2's tip
(`feat/store-backed-cli`) since PR 2 was still open when this branch was cut.

- [x] 1. `feat: bind artifact publication to conditional store records`
   (`artifact_records.go`, the rewritten `Publish`/`Read`)
- [x] 2. `test: cover concurrent, replayed and conflicting artifact publication`
- [x] 3. `feat: reconcile pending artifacts on startup`
   (`reconcileArtifacts` wired into `attachStore`)
- [x] 4. `test: cover artifact reconciliation through the CLI`
- [x] 5. `docs: close Step 5 and correct the README's SQLite claim`

Does not merge without review, same as PR 1 and PR 2.
