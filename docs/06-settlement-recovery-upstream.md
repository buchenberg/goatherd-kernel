# Plan 06 — Settlement over dirty baselines & recoverable sandbox identity

Status: **shipped — v0.14.0, yaah re-pinned (PR #276)**. Two upstream gaps
surfaced by yaah's shepherd-kernel adoption (reviewed end-to-end after
the adoption completed; evidence cited per finding). Both were
kernel-owned: yaah worked around or fenced them, and the fixes landed
here. Phases A–C shipped in kernel PRs #53–#55 (v0.14.0); phase D — the
re-pin, the resolver's path parameter, the retirement of yaah's
worktree-record registry, and the restored dirty-fork regression test —
in yaah PR #276. The review process added two designs this plan had not
anticipated: sealed trees are pinned against garbage collection with
refs under `refs/shepherd/states/` (the trace store is invisible to
git's reachability traversal), and a same-tree different-HEAD parent is
deliberately a *moved* workspace — select's destructive reset would
discard the commit, so it routes to apply. Residual follow-ups recorded
below in the phases and non-goals: the pin release lifecycle, the
containerd capture identity, and the legacy-stash exposure.

---

## 1. Problem statement

### U1 — Settlement cannot deterministically settle a fork whose baseline has uncommitted content

Three independent defects compose into one dead end:

**U1a — captures of identical dirty trees never digest equal.**
`GitSandbox.Capture` (`sandbox/git/backend.go:359`) records
`Data: {head_sha, stash_sha}` where `stash_sha` is minted by
`git stash create` (`stashCreate`, backend.go:143) — a fresh commit with a
current timestamp per capture. `WorkspaceState.Digest()`
(`trace/workspace_state.go:26`) hashes the full `Data` map, so two captures
of a byte-identical dirty tree produce different digests. `Settle(Select)`'s
precondition — `digest(parent-now) == digest(baseline)` — therefore fails
deterministically for any dirty fork baseline, except when both captures
mint their stash commits within the same second. The `Settle` docstring
already concedes this ("a parent with uncommitted content at the fork point
has a capture that can never digest-match").

**U1b — Diff's base is the captured HEAD, not the captured state.**
`GitSandbox.Diff` (backend.go:433) stages the current tree into a scratch
index and runs `git diff --cached ws.Revision`, where `Revision` is the
captured **HEAD** — the recorded stash is ignored. Consequence: the
baseline's own uncommitted files appear as changes on *both* sides —
`Scope.Seal` freezes them into the output's change list, and
`Settle(Apply)`'s overlap guard reads them as parent drift — so a dirty
baseline always self-overlaps and refuses with `ErrApplyConflict`.

**U1c — the apply verb's delta is stash-keyed, and a tree-keyed delta must
be baseline-relative.** `Settle(Apply)` lands the sealed output through
`sandbox.DeltaApplier.ApplyDelta` (settlement.go:427), and
`GitSandbox.ApplyDelta` (backend.go:493) reads only
`delta.Data["stash_sha"]`, returning a silent no-op when it is empty —
"a clean capture: nothing to apply". Two consequences. First, dropping
`stash_sha` from new captures without reworking `ApplyDelta` would turn
every new sealed output into an empty-stash delta: `Settle(Apply)` would
report success and causally merge while physically applying nothing.
Second, the replacement cannot naively apply the child's full captured
tree: over a dirty baseline, `tree_sha` *includes* the baseline's
uncommitted content, so applying it as-is would re-land the baseline on
the parent (and read baseline files as conflicts whenever the parent
drifted on them). The output delta must be expressed **relative to the
fork baseline** — a three-way merge whose base is the baseline state, not
the child's full state and not HEAD.

**Net effect:** fork-and-choose over a fork point with uncommitted content
cannot settle at all, and any fix that only stabilizes the digest would
silently break the apply verb. Consumer evidence: yaah's C2 review
sessions — the flow's dirty-fork test flaked on same-second luck (PR #269,
macOS CI), the deterministic conflict surfaced on the same run, the test
was removed as untestable, and yaah's `chooseVariant` carries a comment
documenting the gap and telling users to commit before forking.

### U2 — Fork records carry no sandbox address, so isolated children are unrecoverable by the kernel's own registry

`forkWithBaseline` (`scope.go:250`) records `child_owner`, `isolated`,
`sandbox_backend`, and the baseline — nothing that addresses the substrate.
`RecoverScopes` passes only `(scopeID, backend)` to the
`RecoverOptions.Resolver`, so a consumer cannot re-adopt an isolated child's
substrate from the trace alone. yaah records its own
`yaah.worktree.provisioned.v1` facts on the child's owner path as a
workaround — a registry the kernel should own. The gap also forced yaah's
resolver to be conservative (strict git-registry verification before
adoption) because an unaddressed record can only be matched by path
existence, which is not ownership.

---

## 2. Design

### U1 — make the git WorkspaceState a content identity

The captured state should be *what the tree is*, not *which stash commit
the capture happened to mint*. Concretely:

**Capture** (`GitSandbox.Capture`): keep the scratch index, and derive the
tree identity from it with `git write-tree` — chosen explicitly over
`git rev-parse <stash_sha>^{tree}`, which would still require minting a
stash commit per capture and would keep the timestamp-identity problem this
plan removes. Record:

```
Data: { "head_sha": <HEAD>, "tree_sha": <deterministic tree> }
```

Drop `stash_sha` from new captures. For a clean tree the scratch index
equals HEAD's tree, so `tree_sha` degenerates to `HEAD^{tree}` — the design
is uniform, no special case. Both payload keys are content-stable: two
captures of an identical tree (clean or dirty) now digest equal, and
`Settle(Select)`'s precondition becomes a true content-equality check with
no settlement-code change. Digest equality holds for identical content at
the same HEAD: a capture after a *timeline* move (a commit, even one whose
tree matches the prior content) is a moved workspace by design — select's
destructive reset must never roll back a parent's real commit, so such a
parent routes to the apply verb, which merges and keeps the commit.

**Apply** (`applyState`): today it resets to `head_sha`, cleans, and
`git stash apply`s the minted commit. Replace the stash apply with a direct
restore of the captured tree via `git read-tree --reset -u <tree_sha>`.
The `-u` flag is load-bearing: it updates the worktree to match the new
index *and removes worktree files that the new tree does not contain* — a
file present in HEAD but deleted in the captured state must not survive.
The obvious alternatives have exactly that hole: `git checkout-index -a -f`
and `git restore --source=<tree_sha> -- .` only write entries present in
the tree, so a capture that contained a deletion would leave the deleted
file behind, and Apply would restore a superset of the captured state.
(The read-tree replaces the caller's index content too; Apply is already a
documented destructive reset — `ApplyWorkspace`'s contract is the
worktree's content, which the tests pin.)

Legacy states recorded before this change still carry `stash_sha`: keep the
stash-apply path for any state that has `stash_sha` and no `tree_sha`
(backward-compatible read; old traces must keep applying).

**ApplyDelta** (`GitSandbox.ApplyDelta`, U1c's fix): reworked from
"apply the minted stash commit" to a **baseline-relative three-way merge**.
The delta's data becomes:

```
{ "head_sha": <HEAD>, "tree_sha": <final tree>, "base_tree_sha": <baseline tree> }
```

`Seal` records `base_tree_sha` at seal time, from the fork baseline it
already holds: the baseline's `tree_sha` for new-style baselines; for a
legacy `stash_sha` baseline, `rev-parse <stash_sha>^{tree}` — and for a
legacy *clean* baseline, whose `stash_sha` is the empty string
(`stashCreate` returns "" for a clean tree), `rev-parse <head_sha>^{tree}`
instead, which is the same tree. Resolved while sealing either way, so the
delta never depends on a later `rev-parse` of a possibly-GC'd minted
commit. The sealed record's payload already carries both full states, so
recovery-rebuilt outputs inherit the pairing for free.

Recording the SHAs alone does not keep the objects: trees written by
`write-tree` are unreferenced, and git's reachability traversal cannot see
the trace store, so an explicit `git gc` between Seal and Settle could
otherwise prune exactly the objects a settlement needs. Seal therefore also
**pins** both trees with refs under `refs/shepherd/states/<sha>` — keyed by
the tree's own SHA, so identical content shares one pin and re-pinning is
idempotent. Pins live for the repository's life; a release lifecycle
(unpinning settled and discarded outputs) is a recorded follow-up gap. The
one residual exposure is the legacy dirty baseline itself: a pre-plan-06
minted stash may already be pruned when Seal resolves it, which fails Seal
with an actionable error — a fork over a pruned dirty baseline cannot
settle, at seal time rather than mid-settle.

Semantics: merge base = `base_tree_sha`, theirs = `tree_sha`, ours = the
parent worktree as it stands (uncommitted content included, via the same
scratch-index staging `Capture` and `Diff` already use). Baseline content
appears on neither side, so it never re-lands and never conflicts on its
own; only the child's actual changes are applied. Conflicts fail typed and
leave the parent untouched (the existing `ErrApplyConflict` contract at the
settlement layer, with the backend returning a plain error as today).
Special cases, stated so no implementation reintroduces the silent no-op:

- `tree_sha == base_tree_sha` — the child changed nothing; a true no-op.
- a delta with **no recognizable key at all** (neither `stash_sha` nor
  `tree_sha` present in `Data`) is **malformed** and returns an error.
  Today's code treats that shape as a clean no-op; that is precisely the
  silent-success failure this plan must close. Presence is checked with
  `v, ok := delta.Data[...]`, not by value: a legacy delta whose
  `stash_sha` key is *present but empty* is a legitimate legacy clean
  capture — a no-op, not an error — and must stay distinguishable from
  the malformed shape.
- a delta with `tree_sha` but no `base_tree_sha` is equally **malformed**:
  the baseline-relative merge has no base, and falling back to HEAD would
  reintroduce U1b's baseline self-overlap. It errors.
- a legacy `stash_sha`-carrying delta (empty or not) keeps the
  stash-apply path unchanged, with the empty case the no-op above.

Implementation candidate: `git merge-tree --write-tree
--merge-base=<base_tree_sha> <ours_tree> <tree_sha>` (git ≥ 2.38), where
`ours_tree` is `write-tree` over the parent's scratch index; the landing
step then writes the merged tree's added/modified paths *and removes*
paths present in `ours_tree` but absent from the merged tree — the same
deletion hazard Apply's `read-tree -u` closes. An acceptable fallback is an
ephemeral synthetic commit pair (`commit-tree` base, `commit-tree -p`
delta) fed to `git stash apply`; unlike `stash create` at capture time,
ephemeral minting at apply time never enters the recorded state, so it
cannot destabilize digests. The exact plumbing is an implementation
choice; the semantics above and the tests below are the contract.

**Diff** (`GitSandbox.Diff`): diff the scratch index against the captured
**full state** — `git diff --cached <tree_sha>` when `tree_sha` is present,
falling back to `Revision` (HEAD) for legacy states. This is U1b's fix:
`Seal`'s frozen change list and `Apply`'s overlap guard then compare
against what the baseline actually was, so a dirty baseline's own
uncommitted content appears on neither side.

**Contract promotion, scoped honestly.** "A capture of unchanged content
must be digest-stable" is the backend contract this plan establishes — and
it is currently violated by the containerd backend, which stamps each
capture with a sequence key (`shepherd/<id>/committed/<seq>`,
sandbox/containerd/sandbox.go:558-567) into both `Revision` and `Data`, so
every capture digests differently even over identical content. An audit
cannot establish the contract there. Scope it therefore: the git backend
satisfies it in Phase A and the contract is documented on the `Sandbox`
interface as the goal; the containerd capture-identity violation is
recorded as its own follow-up gap (alongside the already-recorded
re-adoptable-container-identity gap) and is explicitly out of scope here —
its sequence keys are also its retention mechanism, so reworking them is a
design task, not an audit finding.

**What deliberately does not change:** checkpoint staleness stays
revision-based (`markStaleCheckpoints` compares recorded HEAD against a
fresh capture's HEAD — the timeline position, by design, and its docstring
cites the old digest instability as rationale; update the rationale, not
the semantics). Settlement itself — the consume-once contract, the overlap
guard, the digest precondition, and the `settlement.go` call sites — is
untouched; with a stable digest and a baseline-relative `ApplyDelta` the
existing settlement code becomes correct as written. What changes is the
backend's state shape and the four workspace operations over it
(`Capture`, `Apply`, `ApplyDelta`, `Diff`). Update the conceded `Settle`
docstrings.

### U2 — record the address, hand it to the resolver

**Capability interface** (the `DeltaApplier` pattern — an optional
capability, not a kernel-wide breaking change):

```go
// Addressed is an optional Sandbox capability: the backend's durable
// address — the worktree path for a git worktree, the repository path
// for a materializer, the workspace path for a path-addressed container
// backend. (A backend whose identity is not a path must not implement
// this yet — recovery's absolute-path validation could not carry it —
// and stays a recorded follow-up.)
// Recovery records it in the fork payload and hands it back to the
// host's resolver, so an isolated child's substrate is re-adoptable
// from the trace alone.
//
// The address must be a normalized absolute path: a raw relative path
// persisted in a trace and replayed after a restart under a different
// working directory would address the wrong location.
type Addressed interface {
	Address() string
}
```

`GitSandbox` implements it (worktree path when isolated, repository path
otherwise). `NewWorktreeSandbox` today accepts relative paths and stores
them as given, so `Address()` normalizes at construction — `filepath.Abs`,
resolved eagerly, not at read time — and `RecoverScopes` validates that a
recorded `sandbox_path` is absolute before handing it to the resolver.

**Fork payload** (`forkWithBaseline`): when `isolated` and the sandbox
implements `Addressed`, record `sandbox_path` beside `sandbox_backend`.
The capability check cannot live in `forkWithBaseline` as currently
shaped — it receives only the backend *name* string, while the child
sandbox `sb` is in `ScopeManager.forkScope`'s hands. So the extraction
happens there: `forkScope` already derives `backend` from `sb`; it
additionally asserts the `Addressed` capability and passes the address
down as a new `forkWithBaseline` parameter, which records it. Recovery
(`scanForkRecords`) extracts it into the edge.

**Resolver signature** (`RecoverOptions.Resolver`):
`func(scopeID, backend, path string) (Sandbox, error)` — the path is empty
for roots and for isolated children whose backend does not implement
`Addressed`. It is *not* a shared-child discriminator: shared children are
never sent to the resolver at all — they inherit the parent's re-adopted
sandbox exactly as they inherited it at fork time, and recovery must keep
not calling the resolver for them. yaah's root-vs-worktree branching
currently keys off `backend == ""`, which is a proxy for "root"; the
explicit empty-path form is the same contract, now first-class. This is a
breaking change to `RecoverOptions`; the only consumer is yaah, coordinated
in Phase D.

---

## 3. Phases

### Phase A — git backend content identity (kernel)

- Capture records `{head_sha, tree_sha}` via `write-tree`; `stash_sha` no
  longer written.
- Apply restores from `tree_sha` with `read-tree --reset -u` (removes
  deleted entries); legacy `stash_sha`-only states keep the stash-apply
  path.
- `ApplyDelta` reworked to the baseline-relative three-way merge; `Seal`
  records `base_tree_sha`; legacy `stash_sha` deltas keep the stash path;
  a delta with neither key errors instead of silently no-opping.
- Diff bases on `tree_sha` (legacy fallback: `Revision`).
- Backend-contract doc on `Sandbox`, scoped per the contract-promotion
  note (containerd recorded as a known violation, not audited into
  compliance).
- **Tests** (law-coverage process, all three OSes in CI):
  - two captures of an identical *dirty* tree digest equal — repeated
    across a >1s gap to kill the same-second luck class outright;
  - capture/apply round-trip over a dirty tree, worktree-content pinned,
    **including a deletion**: a file present in HEAD but absent from the
    captured tree is gone after Apply (the superset-restore regression);
  - `ApplyDelta` over a dirty baseline: the child's changes **physically
    land** in the parent worktree (a canary assertion against the silent
    no-op), the baseline's own content is not re-applied or duplicated, a
    child deletion lands, and a conflicting delta refuses typed with the
    parent untouched;
  - `ApplyDelta` on a no-change delta (`tree_sha == base_tree_sha`) is a
    no-op; malformed deltas error — both the no-recognizable-key shape
    and the `tree_sha`-without-`base_tree_sha` shape — while a legacy
    clean-capture delta (`stash_sha` present but empty) stays a no-op;
  - legacy-state apply and apply-delta still work (fixtures with
    `stash_sha` payloads);
  - golden/store vectors updated for the new payload keys.

No API change. Own PR.

### Phase B — settlement verified over dirty baselines (kernel)

Expected: no production change; the Phase A fixes make the existing
settlement code correct. Update the conceded docstrings. Add the law tests:

- `Seal` over a dirty baseline: the frozen change list carries only the
  branch's changes;
- `Settle(Select)` over an unmoved dirty parent baseline: succeeds,
  deterministically — repeated to prove timing-independence;
- `Settle(Apply)` over path-disjoint parent drift with a dirty baseline:
  merges, drift survives;
- overlapping drift still refuses typed (`ErrApplyConflict`), unconsumed.

If the digest precondition misbehaves under the new state shape, the fix
lands here, not folded into Phase A. Own PR (may merge with A).

### Phase C — fork-record address + resolver signature (kernel)

- `Addressed` capability; `GitSandbox` implements it with a normalized
  absolute path.
- `forkScope` extracts the address from `sb` and passes it down;
  `forkWithBaseline` records `sandbox_path`; `scanForkRecords` extracts it.
- `Resolver` signature gains `path`; `RecoverScopes` passes it through and
  validates recorded paths are absolute.
- **Tests**: an isolated child recovers with its address handed to the
  resolver; a shared child inherits the parent's re-adopted sandbox with
  **no resolver call** (pinning the existing recovery contract, not
  contradicting it); a root resolves with an empty path; the recovery
  end-to-end over a crash-simulated trace re-adopts a worktree from the
  kernel record alone.

Breaking (`RecoverOptions`); version bump coordinated with yaah. Own PR.

### Phase D — yaah adaptation and re-pin (yaah repo)

- Pin the new kernel minor.
- The recovery resolver consumes `path` directly; the strict
  registered-worktree verification stays (it answers a different question:
  is the current *occupant* still ours).
- Stop writing `yaah.worktree.provisioned.v1` for new forks; keep reading
  them as a fallback for traces written before the bump (one transition
  note in the code), then delete the fallback at the next minor.
- Restore the dirty-fork choose regression test that C2 removed
  (`TestReviewSession_ChooseWithDirtyForkPoint`): fork from a dirty tree,
  choose, assert the settle lands and the dirty content survives — now
  stable on Linux, macOS, and Windows.
- Delete the known-gap comment in `chooseVariant`; update the yaah
  adoption plan's §8 kernel-gaps list.

### Release

One coordinated minor (v0.14.0): Phases A–C ship together so the digest
fix and the resolver break land once; yaah's Phase D re-pins in the same
window. The breaking `Resolver` signature is the only consumer-visible
incompatibility and its sole consumer is coordinated.

---

## 4. Risks

- **ApplyDelta merge plumbing** is the plan's highest-risk item: it
  replaces a well-trodden `stash apply` with an explicit three-way merge.
  `git merge-tree --write-tree --merge-base` requires git ≥ 2.38 (Oct
  2022) — verify CI floor, fall back to the ephemeral synthetic-commit
  pair if the floor is higher than we support. The landing step has the
  same deletion hazard as Apply; both are pinned by tests that assert
  content lands and deletions delete, never by index internals.
- **Apply rewrite semantics** (tree restore vs stash apply): the end state
  is identical, but index internals differ slightly. Mitigation: tests pin
  worktree content, never index internals.
- **Golden vector churn**: the payload key change (`stash_sha` → `tree_sha`,
  plus `base_tree_sha` on sealed outputs) touches recorded states. All
  vectors updated in Phase A; old traces are read-compatible by the legacy
  apply paths.
- **git plumbing portability**: `write-tree` and `read-tree --reset -u`
  are ancient stable plumbing — lower risk than the `stash create` behavior
  this replaces; `merge-tree --write-tree` is the youngest command
  involved and is risk-tracked above (the ApplyDelta landing step's write
  mechanism is an implementation choice already governed by the design's
  semantics, not a named dependency). CI runs all three OSes; the
  yaah experience (symlinked temp dirs, 8.3 names) says path
  *comparisons* are the portability hazard, and neither digest nor diff
  compares paths.
- **Scope creep into checkpoint staleness**: explicitly refused — revision
  semantics are deliberate and correct.

## 5. Acceptance criteria

- Two captures of an identical dirty tree digest equal, across seconds and
  OSes (law-pinned).
- A fork whose baseline has uncommitted content settles: Select when the
  parent is unmoved, Apply on path-disjoint drift, typed refusal on overlap
  — all deterministic, no timing.
- Sealed outputs over dirty baselines carry only the branch's changes.
- A settled Apply verb's content **physically lands** in the parent
  worktree — canary-pinned, so the silent-no-op class cannot regress.
- Apply restores the captured tree exactly, including deletions.
- Legacy `stash_sha` states still Apply and ApplyDelta.
- Isolated children's fork records carry their sandbox address, as a
  normalized absolute path; `RecoverScopes` hands it to the resolver;
  shared children keep inheriting without a resolver call; yaah re-adopts
  worktrees from the kernel record alone, with its workaround record
  retired.
- yaah's previously-removed dirty-fork regression test is back and green
  on all three CI platforms.

## 6. Non-goals

- No change to settlement's consume-once contract or the merge gate.
- No change to checkpoint staleness semantics (revision-based, deliberate).
- No new backends; no containerd work here — not its re-adoptable
  container identity (already recorded), and not its sequence-keyed
  capture identity, which this plan records as a known contract violation
  and a separate follow-up gap (its snapshot keys are also its retention
  mechanism; reworking them is its own design task).
- No materialization/substrate work (fenced per the yaah adoption plan's
  D7).
