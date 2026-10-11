# Plan 06 — Settlement over dirty baselines & recoverable sandbox identity

Status: **proposed**. Two upstream gaps surfaced by yaah's shepherd-kernel
adoption (reviewed end-to-end after the adoption completed; evidence cited
per finding). Both are kernel-owned: yaah worked around or fenced them, but
the fixes belong here.

---

## 1. Problem statement

### U1 — Settlement cannot deterministically settle a fork whose baseline has uncommitted content

Two independent defects compose into one dead end:

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

**Net effect:** fork-and-choose over a fork point with uncommitted content
cannot settle at all. Consumer evidence: yaah's C2 review sessions — the
flow's dirty-fork test flaked on same-second luck (PR #269, macOS CI), the
deterministic conflict surfaced on the same run, the test was removed as
untestable, and yaah's `chooseVariant` carries a comment documenting the
gap and telling users to commit before forking.

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
tree identity from it — `git rev-parse <stash_sha>^{tree}`, or equivalently
`git write-tree` against the scratch index. Record:

```
Data: { "head_sha": <HEAD>, "tree_sha": <deterministic tree> }
```

Drop `stash_sha` from new captures. For a clean tree the scratch index
equals HEAD's tree, so `tree_sha` degenerates to `HEAD^{tree}` — the design
is uniform, no special case. Both payload keys are content-stable: two
captures of an identical tree (clean or dirty) now digest equal, and
`Settle(Select)`'s precondition becomes a true content-equality check with
no settlement-code change.

**Apply** (`applyState`): today it resets to `head_sha`, cleans, and
`git stash apply`s the minted commit. Replace the stash apply with a direct
restore of the captured tree — `git read-tree <tree_sha>` plus
`git checkout-index -a -f` (or `git restore --source=<tree_sha> -- .`).
The end state is identical — worktree equals the captured full tree — and
arguably more faithful, since the capture's scratch index *is* that tree.
Legacy states recorded before this change still carry `stash_sha`: keep the
stash-apply path for any state that has `stash_sha` and no `tree_sha`
(backward-compatible read; old traces must keep applying).

**Diff** (`GitSandbox.Diff`): diff the scratch index against the captured
**full state** — `git diff --cached <tree_sha>` when `tree_sha` is present,
falling back to `Revision` (HEAD) for legacy states. This is U1b's fix:
`Seal`'s frozen change list and `Apply`'s overlap guard then compare
against what the baseline actually was, so a dirty baseline's own
uncommitted content appears on neither side.

**Contract promotion.** "A capture of unchanged content must be
digest-stable" is a backend contract, not a git nicety: add it to the
`Sandbox` interface documentation and audit the containerd backend against
it (its snapshot keys should already be content-addressed; verify, don't
assume).

**What deliberately does not change:** checkpoint staleness stays
revision-based (`markStaleCheckpoints` compares recorded HEAD against a
fresh capture's HEAD — the timeline position, by design, and its docstring
cites the old digest instability as rationale; update the rationale, not
the semantics). Settlement's consume-once contract, the overlap guard, and
the digest-precondition code are untouched — with a stable digest they
become correct as written. Update the conceded `Settle` docstrings.

### U2 — record the address, hand it to the resolver

**Capability interface** (the `DeltaApplier` pattern — an optional
capability, not a kernel-wide breaking change):

```go
// Addressed is an optional Sandbox capability: the backend's durable
// address — the worktree path for a git worktree, the repository path
// for a materializer, a snapshot handle for snapshot-backed backends.
// Recovery records it in the fork payload and hands it back to the
// host's resolver, so an isolated child's substrate is re-adoptable
// from the trace alone.
type Addressed interface {
	Address() string
}
```

`GitSandbox` implements it (worktree path when isolated, repository path
otherwise).

**Fork payload** (`forkWithBaseline`): when `isolated` and the sandbox
implements `Addressed`, record `sandbox_path` beside `sandbox_backend`.
Recovery (`scanForkRecords`) extracts it into the edge.

**Resolver signature** (`RecoverOptions.Resolver`):
`func(scopeID, backend, path string) (Sandbox, error)` — the path is empty
for roots and unaddressed backends, exactly the discriminator hosts need
(yaah's root-vs-worktree branching currently keys off `backend == ""`, which
is a proxy for "root"; the explicit empty-path form is the same contract,
now first-class). This is a breaking change to `RecoverOptions`; the only
consumer is yaah, coordinated in Phase D.

---

## 3. Phases

### Phase A — git backend content identity (kernel)

- Capture records `{head_sha, tree_sha}`; `stash_sha` no longer written.
- Apply restores from `tree_sha`; legacy `stash_sha`-only states keep the
  stash-apply path.
- Diff bases on `tree_sha` (legacy fallback: `Revision`).
- Backend-contract doc on `Sandbox`; containerd audit note.
- **Tests** (law-coverage process, all three OSes in CI):
  - two captures of an identical *dirty* tree digest equal — repeated
    across a >1s gap to kill the same-second luck class outright;
  - capture/apply round-trip over a dirty tree, worktree-content pinned;
  - legacy-state apply still works (fixture with a `stash_sha` payload);
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

- `Addressed` capability; `GitSandbox` implements it.
- `forkWithBaseline` records `sandbox_path`; `scanForkRecords` extracts it.
- `Resolver` signature gains `path`; `RecoverScopes` passes it through.
- **Tests**: an isolated child recovers with its address handed to the
  resolver; a shared child resolves with an empty path; a root with an empty
  path; the recovery end-to-end over a crash-simulated trace re-adopts a
  worktree from the kernel record alone.

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

- **Apply rewrite semantics** (tree restore vs stash apply): the end state
  is identical, but index internals differ slightly. Mitigation: tests pin
  worktree content, never index internals.
- **Golden vector churn**: the payload key change (`stash_sha` → `tree_sha`)
  touches recorded states. All vectors updated in Phase A; old traces are
  read-compatible by the legacy apply path.
- **git plumbing portability**: `rev-parse <sha>^{tree}`, `write-tree`,
  `read-tree`, `checkout-index` are ancient stable plumbing — lower risk
  than the `stash create` behavior this replaces. CI runs all three OSes;
  the yaah experience (symlinked temp dirs, 8.3 names) says path
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
- Legacy `stash_sha` states still Apply.
- Isolated children's fork records carry their sandbox address;
  `RecoverScopes` hands it to the resolver; yaah re-adopts worktrees from
  the kernel record alone, with its workaround record retired.
- yaah's previously-removed dirty-fork regression test is back and green
  on all three CI platforms.

## 6. Non-goals

- No change to settlement's consume-once contract or the merge gate.
- No change to checkpoint staleness semantics (revision-based, deliberate).
- No new backends; no containerd identity work here (a re-adoptable
  container identity is a separate, recorded gap).
- No materialization/substrate work (fenced per the yaah adoption plan's
  D7).
