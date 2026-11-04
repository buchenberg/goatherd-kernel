# Design — owner-scoped eviction and garbage collection (bead `4y8`)

**Status:** design pass (the deliverable the bead asks for). Implementation is a
separate change with its own law tests. This doc matters for the v1.0 gate: the
eviction API surface proposed here is part of what the 9fc freeze would pin, so
the design must be accepted *or consciously deferred* before the freeze.

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
- **No eviction without an explicit authenticated call.** Nothing in the
  append/read/frontier paths ever deletes; the laws stay intact for every
  existing code path.

## 3. API sketch

Two store methods, both trusted-context-gated (the same trust level the
`TrustedAppendContext` callers already use — the operation mutates the store,
so it is not reachable through read contexts):

```go
// EvictOwnerPath removes an owner path's reachability: every path_entries row
// with path_ref = ownerID, the owner_ordinals row, and — with the caller's
// ack — the frontier pins that reference the path. The facts themselves are
// untouched; garbage collection is a separate, explicit step.
func (s *SQLiteTraceStore) EvictOwnerPath(ctx context.Context, auth AppendContext, ownerID string, ackDanglingFrontiers bool) (EvictionReceipt, error)

// CollectGarbage removes records no longer reachable from any surviving
// owner path, recorded frontier, or checkpoint snapshot — plus the causal
// parents those live records cite — and the append intents that lost their
// last reference. Pure reclamation; it changes no visible read result.
func (s *SQLiteTraceStore) CollectGarbage(ctx context.Context, auth AppendContext) (GarbageReceipt, error)
```

Both return receipts (counts plus the ids of the invalidations they caused),
because the caller — a privacy flow — must be able to report *what happened*.

### Eviction semantics

- `path_entries` rows for the owner are deleted in one transaction with the
  `owner_ordinals` row. A fresh append to the same `trace_owner_id` starts a
  new path at ordinal 0; by construction nothing survives to collide with.
- **Frontiers.** `frontiers` pins `(target_trace_owner_id, through_owner_ordinal,
  through_fact_id)` and re-validates against the path at read. After eviction
  that re-validation fails; the design makes the failure explicit rather than
  silent: eviction refuses when a frontier still pins this path unless the
  caller passes `ackDanglingFrontiers`, in which case the frontier row is
  deleted with the path. The cut record itself survives as content — law 22
  (`TestFrontierIsImmutable`) keeps passing for every frontier not
  evicted-with-ack.
- **Checkpoint snapshots (`checkpoint_snapshots`) are out of eviction scope in
  v1.** They are scope-keyed, not owner-keyed; a scope-side checkpoint purge
  belongs to the caller that created them (same trust level, separate method
  when a consumer appears). They do participate in GC liveness, below.

### GC liveness rules

A record is **live** iff any of:

1. a surviving `path_entries` row references it;
2. a `frontiers` row references it (`through_fact_id` or `created_by_fact_id`);
3. a `checkpoint_snapshots` row embeds it (conservative: any snapshot whose
   `state_json` names the record id — snapshot content is a blob, so the
   check is a name scan, not a parse contract);
4. a **live** record names it in `caused_by_json` — the closure rule: dropping
   a parent a live child cites would corrupt the law-11/16-class invariants
   for that child's future reads.

Liveness is a fixpoint over `record_edges`/`caused_by_json` reached backward
from the seeded live set (children keep their parents). Everything else —
records, their `record_edges` rows, their `contexts` rows, and finally
`append_intents` rows that lost their last referencing row — is deleted. This
also resolves the standing "append_intents grow forever" item: the table
becomes refcounted by its referents, not a landfill.

Ordering inside the transaction: leaf intents last (they are FK targets from
`records`/`contexts`/`path_entries`), `record_edges` rows with their records,
and never a record still cited by a live one.

## 4. Law impact

- **Laws 21/22/23 (cut semantics, projection refusals):** unchanged. Every
  read surface keeps its semantics for every path not evicted; evicted paths
  fail loudly (path-not-found class) instead of returning a silently truncated
  prefix.
- **Law 25 (restart preserves records/cuts/paths):** unchanged for all
  non-eviction flows; eviction is a durable, transactional operation — the
  absence persists across restarts like any other committed state.
- **Laws 11/16/17 (closed witness support, out-of-cut parent anchors):** the
  closure rule in the liveness list is what keeps these honest — a record is
  never dropped while a live record cites it.
- **Law 12 (witness cycle detection):** GC runs on well-formed graphs by
  induction (eviction/GC only delete; deletion cannot create a cycle). A cycle
  found mid-sweep is a corruption event: abort, return an error — never
  garbage-collect through an invariant hole.
- **New law candidate (Go-side extension):** *an evicted path is unreadable
  and its facts vanish from every owner-path read, while any other path's read
  result is byte-identical to before.* Pinned against the dedup case
  (`TestContentAddressedFactSpansMultipleOwnerPaths`): two owners share a
  fact, evict one, the other's read does not change.

## 5. Caller wiring (yaah-1e6g on top)

yaah's session delete becomes a three-step flow:

1. collect the owner IDs to evict **before** the read-model cascade deletes
   `trace_owners` (the mapping is the only record of `sub-*` owner ids);
2. run the state.db cascade as today (`session_repo.go` `DeleteSession`);
3. call `EvictOwnerPath` for each owner, with `CollectGarbage` batched by the
   caller's retention policy, not per delete — the sweep is global, so
   per-delete sweeps are wasteful.

Failure ordering is load-bearing: kernel evictions happen **first**; the
read-model rows are deleted only after eviction succeeds. The reverse order
leaves the worst-case state on a crash — a "deleted" session whose content is
still fully readable from the trace.

## 6. Open questions (decisions the implementation needs)

1. **Ack-on-dangling-frontier vs always-refuse.** Proposed: refuse without the
   ack; privacy flows pass it deliberately when their policy says cuts die
   with the session.
2. **GC cadence.** A global sweep is cheap at yaah's scale and O(store) at
   large ones; a watermark or `since` hint can bound the work later. Defer
   until a second consumer exists.
3. **Upstream the new law?** The Python reference has no eviction; this design
   is a Go-side deliberate extension and must be recorded in the parity ledger
   as such, not as parity drift.
