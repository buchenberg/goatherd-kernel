# Design — owner-scoped eviction and garbage collection (bead `4y8`)

**Status:** design pass, revised through bot review round 2 (r3); this
revision (r4) closes the round-2 leftovers. It keeps the review provenance in
§8. Implementation is a separate change with its own law tests. This doc
matters for the v1.0 gate: the eviction API surface proposed here is part of
what the 9fc freeze would pin, so the design must be accepted *or consciously
deferred* before the freeze.

## 1. Drivers

The kernel is append-only (25 laws, `docs/law-coverage.md`) and exposes no
deletion of any kind. That is the right default for evidence, and it is why
retention stalled: a consumer that must honor a data-subject deletion (yaah's
`yaah-1e6g`, the privacy precondition for the persistence-convergence work)
needs at least one authorized, auditable way to make content unreachable and
eventually gone, without breaking the guarantees the other laws pin.

Today's consumer reality: yaah's `DeleteSession` removes its read model
(state.db rows) and the `trace_owners` mapping, while every kernel fact,
frontier, and intent row for that session survives forever. The
materialization-intent deletion added earlier is not reachable from yaah —
yaah's flows append records and publish cuts only; it never runs the
materialize orchestrator, so it creates no materialization intents.

## 2. Non-goals

- **No content rewrite, ever.** `records.digest = record_id` is content
  addressed; a "redacted" record with an edited body is a different record.
  The design removes reachability and storage, not history identity.
- **No partial-ordinal eviction in v1.** Evicting ordinals `[0, k)` while
  keeping `[k, …)` on the same owner path collides with reuse of the
  `owner_ordinals.next_ordinal` counter and changes what `ReadOwnerPrefix`
  returns for every surviving consumer of that path. v1 is whole-path eviction
  only; the owner path becomes empty and the counter resets. Partial windows
  are deferred until a consumer names one.
- **No deletion from any existing code path.** Append, read, frontier, and
  project keep their exact current behavior; eviction and GC are separate
  entry points, and the laws keep holding for every flow that does not call
  them.

## 3. API sketch

Two store methods plus a reopen path — the full frozen surface is three
methods. **Authorization is a checked predicate, not a parameter type**: an
`AppendContext` value is constructible by anyone in-process, so all three
enforce the same predicate the append path already relies on — `TrustMode ==
"internal"` or a presented `trusted:internal` witness, checked at entry
(mirroring `store_append.go`'s `ensureAppendAuthorized` shape). That
predicate is only as strong as the append gate itself: the kernel's
authorization model is in-process cooperative, and eviction is exactly as
forgeable as `TrustedAppendContext`'s append — no more. The design states
this rather than pretending a value-type check is an absolute boundary;
process-level isolation is the boundary that actually holds.

```go
// EvictOwnerPath removes an owner path's reachability: every path_entries row
// with path_ref = ownerID, the owner_ordinals row, a tombstone marking the
// path evicted, and — with the caller's ack — the frontier rows that pin
// this path as target OR as publisher. The facts themselves are untouched;
// garbage collection is a separate, explicit step. Evicting the internal
// kernel:witness path is refused unconditionally.
func (s *SQLiteTraceStore) EvictOwnerPath(ctx context.Context, auth AppendContext, ownerID string, ackDanglingFrontiers bool) (EvictionReceipt, error)

// ReopenOwnerPath deletes the eviction tombstone so the path can take
// appends again; the first append after reopening starts a fresh path at
// ordinal 0.
func (s *SQLiteTraceStore) ReopenOwnerPath(ctx context.Context, auth AppendContext, ownerID string) error

// CollectGarbage removes records no longer reachable from any surviving
// owner path, recorded frontier, or checkpoint snapshot — plus the causal
// parents those live records cite, the witnesses no live record's
// witness_ref cites, and the append intents left with zero referents.
// Pure reclamation; it changes no visible read result except the witness
// reclamation documented below.
func (s *SQLiteTraceStore) CollectGarbage(ctx context.Context, auth AppendContext) (GarbageReceipt, error)
```

`EvictOwnerPath` and `CollectGarbage` return receipts (counts plus the ids of
the invalidations they caused); `ReopenOwnerPath` returns only an error. The
caller — a privacy flow — must be able to report *what happened*.

### Eviction semantics

- `path_entries` rows for the owner are deleted in one transaction with the
  `owner_ordinals` row and a **tombstone** in a new `evicted_paths` table
  (`trace_owner_id PRIMARY KEY`, `evicted_at`). Without a tombstone, a
  missing path is indistinguishable from an empty one — `ReadOwnerPrefix`
  returns a successful empty slice for unknown paths today — and the
  "fails loudly" property would be an unfunded claim. Tombstoned paths:
  reads return `ErrOwnerEvicted` (not an empty slice), and appends to the
  path are refused until `ReopenOwnerPath(auth, ownerID)` deletes the
  tombstone, after which the next append starts a fresh path at ordinal 0.
  Schema note: `evicted_paths` joins `createSchema` as `CREATE TABLE IF NOT
  EXISTS` and the required-tables list in `validateExistingSchema` adopts it
  with a version gate, so databases opened before this change still open.
- **Frontiers — target *and* publisher sides.** `frontiers` pins
  `(target_trace_owner_id, through_owner_ordinal, through_fact_id)` and
  re-validates against the target path at read; the cut record itself
  (`created_by_fact_id`) is appended on the **publisher** path, which defaults
  to the target but can differ (`PublisherOwnerID`). Eviction therefore
  checks both: it refuses when a frontier pins the evicted path as *target*
  (read-time re-validation would dangle) or when the evicted path is the
  frontier's *publisher* (the cut's record would lose its only path entry,
  breaking the read of the cut fact itself) — unless the caller passes
  `ackDanglingFrontiers`, in which case those frontier rows are deleted with
  the path. Law 22 (`TestFrontierIsImmutable`) keeps passing for every
  frontier not evicted-with-ack.
- **Checkpoint snapshots (`checkpoint_snapshots`) are out of eviction scope in
  v1.** They are scope-keyed, not owner-keyed; a scope-side checkpoint purge
  belongs to the caller that created them (same trust level, separate method
  when a consumer appears). They do participate in GC liveness, below.

### GC liveness rules

A record is **live** iff any of:

1. a surviving `path_entries` row references it;
2. a `frontiers` row references it (`through_fact_id` or `created_by_fact_id`);
3. a `checkpoint_snapshots` row embeds it — scanned in the **`snapshot` BLOB
   cast to text**, *not* `state_json`: `state_json` holds the marshalled
   `WorkspaceState` (`backend`/`revision`/`data`), which carries no record
   ids; the opaque caller payload is the separate `snapshot` column, and
   `PersistCheckpointBlob` enforces no encoding on it — the scan is
   best-effort UTF-8 substring matching, not a contract. Two consequences:
   the receipt reports the scan methodology, and a caller whose blobs are
   binary-heavy must pin record ids itself (sidecar column or payload
   convention) if checkpoint-aware liveness must be exact;
4. a **live** record names it in `caused_by_json` — the closure rule: dropping
   a parent a live child cites would corrupt the law-11/16-class invariants
   for that child's future reads (boundaries of that rule below, §"Dependent
   reads");
5. a **live** record's `witness_ref` names it — witness reachability is a
   `witness_ref` chains-to-root property (`store_witness.go`
   `validateWitnessChain`), not a causal-parent property. Witness records
   also happen to live on the dedicated `kernel:witness` owner path (rule 1);
   the rule here makes the protection explicit instead of incidental.
   `RootWitnessRecordID`'s record is live unconditionally.

Liveness is a fixpoint over `record_edges`/`caused_by_json` and `witness_ref`
backward from the seeded live set (children and witnesses keep their
supporters). **Ordering inside the sweep:** all liveness is computed before
any deletion; the witness reclamation below then deletes a witness's
`kernel:witness` path entry **before** its record row — `path_entries`
FK-references `records`, so the path entry has to go first.

### Witness reclamation (the accumulation fix)

Witnesses are stored on the shared `kernel:witness` path (one path, all
sessions) and each references its creating append intent. Session evictions
never touch that path, so without a rule, witnesses and their intents
accumulate forever even when every session that cites them is gone. GC is
therefore allowed — as the one deliberate exception to "only eviction deletes
path entries" — to remove `kernel:witness` path entries whose witness record
no live record's `witness_ref` cites, together with that witness record. One
boundary on this: a witness's append intent is **not** exclusively the
witness's — witnesses are inserted under the *batch's* intent
(`insertWitnessRecordIfMissingTx(tx, plan, batch.AppendIntentID)`), shared
with that batch's ordinary records and contexts — so witness reclamation
never deletes intents directly; it deletes the witness row and its path
entry, and intent reclamation stays purely refcount-based (a leaf intent is
deleted only when its referent count across `records`/`contexts`/
`path_entries`/`frontiers` reaches zero). Every path **other than** the
witness path keeps its deletion-happens-only-via-eviction guarantee.

### Dependent reads (the `readFact` constraint)

`readFact` (`store_read.go:209–215`) inner-joins `path_entries`: a record
with zero path entries does not read — today's closure and anchor machinery
assumes reachability. Eviction breaks that assumption by design, so the
design pins the read behavior for evicted parents:

- A surviving child's causal-closure read over an evicted parent **degrades to
  a digest-only external anchor**, the same shape law 17 already defines for
  out-of-cut parents (`TestLawExternalAnchorPreservesOutOfCutParent`), with an
  anchor kind that names the reason `evicted`. The read does not fail.
- The implementation path is an outer-join record read used **wherever
  out-of-path records are rendered as anchors** — closure traversal *and*
  slice construction: `buildSlice` (which `ReadOwnerPrefix` and
  `ResolveFrontier` share) renders parent anchors through the same
  `readFact` join, so the degrade applies there too, not only to
  `ReadCausalClosure`. `ReadFact`'s public contract for reachable records is
  unchanged.
- Frontier validation against a `created_by_fact_id` on an evicted path is
  moot by the ack rule: the frontier row is deleted with the path before the
  validation could run. The ack exists precisely because this combination is
  not otherwise coherent.

This is the one place eviction genuinely changes a surviving owner's read
*presentation* (anchor metadata for a parent, not any fact content), and the
new law below states that boundary exactly.

### FK deletion order (implementer's checklist)

`append_intents` is FK-referenced by `records`, `contexts`, `path_entries`,
*and* `frontiers` (`store_schema.go:158`); `records` is FK-referenced by
`path_entries`, `record_edges`, and `frontiers`
(`through_fact_id`/`created_by_fact_id`); and foreign keys are enforced
(`PRAGMA foreign_keys = ON`, `store.go`). So the sweep deletes **children
before parents** in dependency order: surviving-referent-checked
`path_entries` rows → ack-deleted `frontiers` rows → orphaned `contexts` →
`record_edges` rows alongside their now-unreferenced `records` → leaf
`append_intents` (zero referents across all four). Never delete a record
still cited by a live one, and never a record a surviving path entry or
frontier still references.

## 4. Law impact

- **Laws 21/22/23 (cut semantics, projection refusals):** unchanged. Every
  read surface keeps its semantics for every path not evicted; evicted paths
  fail loudly (`ErrOwnerEvicted`) instead of returning a silently truncated
  prefix.
- **Law 25 (restart preserves records/cuts/paths):** unchanged for all
  non-eviction flows; eviction is a durable, transactional operation — the
  absence (and the tombstone) persist across restarts like any other
  committed state.
- **Laws 11/16/17 (closed witness support, out-of-cut parent anchors):** the
  `witness_ref` liveness rule keeps the support chain closed; the dependent
  reads section pins what a surviving child sees when a parent was evicted
  (a digest-only anchor — an extension of the law-17 shape, not a failure).
- **Law 12 (witness cycle detection):** GC runs on well-formed graphs by
  induction (eviction/GC only delete; deletion cannot create a cycle). A cycle
  found mid-sweep is a corruption event: abort, return an error — never
  garbage-collect through an invariant hole.
- **New law candidate (Go-side extension):** *an evicted path reads as
  `ErrOwnerEvicted`; its facts vanish from the evicted path's reads; any
  other path's direct fact reads are byte-identical to before; a surviving
  child's closure read renders an evicted parent as a digest-only anchor.*
  Pinned against the dedup case
  (`TestContentAddressedFactSpansMultipleOwnerPaths`): two owners share a
  fact, evict one — the survivor's reads are unchanged (the record is still
  on the survivor's path; it does not vanish from *every* read, only from
  the evicted path's).

## 5. Caller wiring (yaah-1e6g on top)

yaah's session delete runs **kernel-first** — the order is load-bearing:

1. collect the owner IDs to evict **before** anything is deleted (the
   `trace_owners` mapping is the only record of `sub-*` owner ids);
2. call `EvictOwnerPath` for each owner; on the ack question, the session's
   own frontier cuts die with the session, so the deletion flow passes the
   ack for its own paths;
3. only after every eviction succeeded, run the state.db cascade
   (`session_repo.go` `DeleteSession` — it drops `trace_owners` last within
   its transaction, which preserves retry-ability the other way around);
   schedule `CollectGarbage` separately, batched by the retention policy —
   the sweep is global, so per-delete sweeps are wasteful.

A crash before step 3 completes leaves content evicted and the state model
intact (the safe direction); a crash after it leaves both gone. The reverse
order — read model first — would leave a "deleted" session whose content is
still fully readable from the trace, with the mapping needed to find it
already gone.

## 6. Decisions locked by this revision

1. **Tombstone table over empty-path semantics.** "Fails loudly" was not
   implementable without it: `ReadOwnerPrefix` returns a successful empty
   slice for unknown paths, and dropping `owner_ordinals` erased the only
   existence evidence. `evicted_paths` + `ErrOwnerEvicted` + explicit
   `ReopenOwnerPath`.
2. **Witness reclamation is in scope.** Without it, `kernel:witness` path
   entries and their intents accumulate forever regardless of session
   deletions — the stated retention problem would survive this design.
3. **Authorization predicate spelled out** (internal trust mode or the
   trusted internal witness, enforced at entry), not the parameter type.
4. **Snapshot liveness scans the `snapshot` BLOB** (cast to text), not
   `state_json`.
5. **Dependent reads degrade to digest-only anchors** rather than failing;
   the implementation carries an outer-join record read for closure
   traversal only.
6. **GC cadence: global, caller-batched.** A watermark/`since` bound is noted
   as a future tightening, deferred until a second consumer exists.
7. **Upstream status: deliberate Go-side extension.** The Python reference
   has no eviction; the implementation records it in the parity ledger as an
   extension, not as parity drift.

## 7. Review provenance

r2 absorbed the first review pass: the checkpoint liveness column mix-up
(`state_json` vs the `snapshot` BLOB), the missing `frontiers` referent in
the FK ordering, the nominal trust gate, the witness-reachability gap
(`witness_ref` liveness + `kernel:witness` reclamation), the `readFact`
inner-join dependent-read hazard, the empty-path-vs-loud-failure
contradiction (resolved by the tombstone), and the ordering contradiction in
the caller wiring.

r3 absorbed the second pass: FK order corrected (children before parents —
`path_entries`/`frontiers` reference `records`, so records were previously
deleted too early for an enforced-FK database); the trust gate's honest
scope — it is exactly as forgeable as append's own gate, and the doc says so
instead of implying an absolute boundary; witness-intent reclamation
narrowed to refcount-only (witnesses share the *batch's* intent with their
ordinary records — direct deletion could kill a live intent); the frontier
ack extended to the *publisher* path (a cut record appended off-target loses
its only path entry when the publisher is evicted); `ReopenOwnerPath` added
to the frozen-surface sketch; and the new-law wording narrowed to the
evicted path so it no longer contradicts the shared-fact dedup case.

r4 closed the round-2 leftovers: the snapshot scan is documented as
best-effort over an unencoded BLOB (with the sidecar-column escape hatch for
binary-heavy callers), witness reclamation states its FK order (path entry
before record), the anchor-degrade scope covers `buildSlice` (shared by
`ReadOwnerPrefix` and `ResolveFrontier`) in addition to closure traversal,
the receipts sentence names which method returns what, and this header.
