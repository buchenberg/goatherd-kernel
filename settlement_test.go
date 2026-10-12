package shepherd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buchenberg/shepherd-kernel-go/internal/gittest"
	"github.com/buchenberg/shepherd-kernel-go/sandbox/git"
)

// TestSeal_FreezesChanges covers T3.5's Seal: registered output, classified
// changes frozen at seal time, sealed record on the child's path, and the
// no-baseline refusal.
func TestSeal_FreezesChanges(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:seal-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:seal-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}

	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if out.ID() == "" || out.ScopeID() != child.ID() {
		t.Errorf("output identity: %+v", out)
	}
	if len(out.Changes()) != 1 || out.Changes()[0].Path != "result.txt" || out.Changes()[0].Kind != ChangeCreate {
		t.Errorf("changes: %+v, want one create of result.txt", out.Changes())
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("state = %s, want unconsumed", out.State())
	}

	// Sealing does not merge: the scope stays active, the parent is untouched.
	if child.State() != ScopeActive {
		t.Errorf("child state = %s, want active after seal", child.State())
	}
	if fileExists(t, repo, "result.txt") {
		t.Error("sealing must not touch the parent workspace")
	}

	// Registry lookup paths.
	if got, ok := mgr.Output(out.ID()); !ok || got != out {
		t.Errorf("Output(%s) = %p, %v; want the sealed output", out.ID(), got, ok)
	}
	outs := mgr.OutputsForScope(child.ID())
	if len(outs) != 1 || outs[0] != out {
		t.Errorf("OutputsForScope: %+v, want the one output", outs)
	}

	// The sealed record is durable-trace, not advisory.
	sealed := findRecords(t, store, "set:seal-child", SchemaRunOutputSealed)
	if len(sealed) != 1 {
		t.Fatalf("sealed records: %d, want 1", len(sealed))
	}
	if fmt.Sprint(sealed[0].Body.Payload["output_id"]) != out.ID() {
		t.Errorf("sealed payload output_id = %v, want %s", sealed[0].Body.Payload["output_id"], out.ID())
	}

	// Later writes do not leak into the frozen output.
	if err := os.WriteFile(filepath.Join(wtPath, "extra.txt"), []byte("late"), 0o644); err != nil {
		t.Fatalf("write extra: %v", err)
	}
	if len(out.Changes()) != 1 {
		t.Errorf("changes after later writes: %d, want still 1 (frozen at seal)", len(out.Changes()))
	}

	// A scope without a fork baseline cannot seal.
	causalParent, _ := mgr.Create("set:seal-causal", nil)
	causalChild, err := mgr.Fork(causalParent.ID(), "set:seal-causal-child", nil)
	if err != nil {
		t.Fatalf("causal fork: %v", err)
	}
	if _, err := mgr.Seal(ctx, causalChild.ID()); err == nil {
		t.Error("Seal on a scope without a baseline should fail")
	}

	// An unregistered output (Scope.Seal directly) cannot settle.
	bare, err := child.Seal(ctx)
	if err != nil {
		t.Fatalf("Scope.Seal: %v", err)
	}
	if err := bare.Settle(ctx, SettleRelease); err == nil {
		t.Error("Settle on an unregistered output should fail")
	}
}

// TestSettle_SelectOnUnmovedParent covers the select verb end to end:
// fast-forward-only (strict digest equality), physical propagation, causal
// merge, worktree teardown, consume-once.
func TestSettle_SelectOnUnmovedParent(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:sel-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:sel-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if err := mgr.Settle(ctx, out.ID(), SettleSelect); err != nil {
		t.Fatalf("SettleSelect: %v", err)
	}
	if got := readRepoFile(t, repo, "result.txt"); got != "winner" {
		t.Errorf("parent result.txt = %q, want the winner's content", got)
	}
	if child.State() != ScopeMerged {
		t.Errorf("child state = %s, want merged", child.State())
	}
	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Errorf("child worktree should be destroyed after select, stat err = %v", statErr)
	}
	if out.State() != OutputSelected || out.Action() != SettleSelect {
		t.Errorf("output state/action = %s/%s, want selected", out.State(), out.Action())
	}

	// Consume-once.
	err = mgr.Settle(ctx, out.ID(), SettleSelect)
	if !errors.Is(err, ErrOutputConsumed) {
		t.Errorf("second settle: %v, want ErrOutputConsumed", err)
	}

	// The settled record cites the sealed one.
	if recs := findRecords(t, store, "set:sel-child", SchemaRunOutputSettled); len(recs) != 1 {
		t.Errorf("settled records: %d, want 1", len(recs))
	}
}

// TestSettle_SelectRefusesMovedParent covers the fast-forward-only rule: a
// parent that advanced since the fork gets an actionable error pointing at
// apply, and the output is not consumed by the refusal.
func TestSettle_SelectRefusesMovedParent(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:move-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:move-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent advances after the seal.
	mustGit(t, repo, "commit", "--allow-empty", "-m", "advance")

	err = mgr.Settle(ctx, out.ID(), SettleSelect)
	if err == nil || !strings.Contains(err.Error(), "SettleApply") {
		t.Fatalf("select on moved parent: %v, want an error pointing at SettleApply", err)
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("refused select consumed the output: state = %s", out.State())
	}
	// The refusal is not consumption: apply still works on the moved parent.
	if err := mgr.Settle(ctx, out.ID(), SettleApply); err != nil {
		t.Fatalf("SettleApply after refused select: %v", err)
	}
}

// TestSettle_ApplyDisjointSucceeds covers the apply verb's happy path: the
// parent moved on disjoint paths, the sealed delta merges onto it.
func TestSettle_ApplyDisjointSucceeds(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:app-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:app-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("from child"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent advances on a disjoint path after the fork.
	if err := os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("from parent"), 0o644); err != nil {
		t.Fatalf("write parent.txt: %v", err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "parent work")

	if err := mgr.Settle(ctx, out.ID(), SettleApply); err != nil {
		t.Fatalf("SettleApply (disjoint): %v", err)
	}
	if got := readRepoFile(t, repo, "feature.txt"); got != "from child" {
		t.Errorf("feature.txt = %q, want the child's content on the moved parent", got)
	}
	if got := readRepoFile(t, repo, "parent.txt"); got != "from parent" {
		t.Errorf("parent.txt = %q, want the parent's own work preserved", got)
	}
	if child.State() != ScopeMerged {
		t.Errorf("child state = %s, want merged", child.State())
	}
	if out.State() != OutputApplied {
		t.Errorf("output state = %s, want applied", out.State())
	}
	if recs := findRecords(t, store, "set:app-child", SchemaRunOutputSettled); len(recs) != 1 {
		t.Errorf("settled records: %d, want 1", len(recs))
	}
}

// TestSettle_ApplyConflictKeepsUnconsumed covers the overlap guard: a path
// the parent changed since the baseline is refused with ErrApplyConflict,
// and the failure does not consume.
func TestSettle_ApplyConflictKeepsUnconsumed(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:conf-parent", git.NewLocalGitSandbox(repo))

	// The child will change shared.txt.
	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:conf-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "shared.txt"), []byte("child's version"), 0o644); err != nil {
		t.Fatalf("write shared: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent changes the same path after the fork.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("parent's version"), 0o644); err != nil {
		t.Fatalf("write shared: %v", err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "parent moves shared")

	err = mgr.Settle(ctx, out.ID(), SettleApply)
	if !errors.Is(err, ErrApplyConflict) {
		t.Fatalf("SettleApply (overlap): %v, want ErrApplyConflict", err)
	}
	if !strings.Contains(err.Error(), "shared.txt") {
		t.Errorf("conflict text = %q, want it to name the overlapping path", err.Error())
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("conflicting apply consumed the output: state = %s", out.State())
	}
	// The parent's version is untouched by the refusal.
	if got := readRepoFile(t, repo, "shared.txt"); got != "parent's version" {
		t.Errorf("shared.txt = %q, want the parent's version", got)
	}
	// Still unconsumed, the output can be discarded instead.
	if err := mgr.Settle(ctx, out.ID(), SettleDiscard); err != nil {
		t.Fatalf("SettleDiscard after conflict: %v", err)
	}
	if child.State() != ScopeDiscarded {
		t.Errorf("child state = %s, want discarded", child.State())
	}
}

// TestSettle_ReleaseAndUnknownAction covers release (mark consumed, no
// workspace change, no scope lifecycle change) and the unknown-action guard.
func TestSettle_ReleaseAndUnknownAction(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:rel-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:rel-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "kept.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if err := mgr.Settle(ctx, out.ID(), SettleRelease); err != nil {
		t.Fatalf("SettleRelease: %v", err)
	}
	if out.State() != OutputReleased {
		t.Errorf("state = %s, want released", out.State())
	}
	// Release changed nothing: the child is still active with its worktree.
	if child.State() != ScopeActive {
		t.Errorf("child state = %s, want active after release", child.State())
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Errorf("worktree must survive release: %v", statErr)
	}
	if fileExists(t, repo, "kept.txt") {
		t.Error("release must not touch the parent workspace")
	}
	// The caller still owns the scope's lifecycle: discard it now.
	if err := mgr.Discard(child.ID()); err != nil {
		t.Fatalf("Discard after release: %v", err)
	}

	// Unknown actions refuse without consuming.
	child2, _ := forkWorktreeChild(t, mgr, parent.ID(), "set:rel-child2", repo, "wt2")
	out2, err := mgr.Seal(ctx, child2.ID())
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	if err := mgr.Settle(ctx, out2.ID(), SettlementAction("teleport")); err == nil {
		t.Error("unknown action must be rejected")
	}
	if out2.State() != OutputUnconsumed {
		t.Errorf("unknown action consumed the output: state = %s", out2.State())
	}
}

// TestSettle_DiscardDestroysChild covers the discard verb: parent untouched,
// child discarded, owned worktree destroyed, sealed state still inspectable.
func TestSettle_DiscardDestroysChild(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:disc-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:disc-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "loser.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if err := mgr.Settle(ctx, out.ID(), SettleDiscard); err != nil {
		t.Fatalf("SettleDiscard: %v", err)
	}
	if child.State() != ScopeDiscarded {
		t.Errorf("child state = %s, want discarded", child.State())
	}
	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Errorf("discarded child's worktree should be destroyed, stat err = %v", statErr)
	}
	if fileExists(t, repo, "loser.txt") {
		t.Error("discard must not touch the parent workspace")
	}
	// The sealed state is retained as a record.
	if out.State() != OutputDiscarded {
		t.Errorf("output state = %s, want discarded", out.State())
	}
}

// readRepoFile reads a file from the repo working tree, failing the test
// when it is missing.
func readRepoFile(t *testing.T, repo, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestSeal_RecordsDeltaBase pins plan 06's seal-time annotation: the sealed
// record's state carries base_tree_sha — the fork baseline's tree, resolved
// while its objects are certainly reachable — so a recovered output can
// ApplyDelta without resolving a legacy identity that garbage collection may
// have reclaimed.
func TestSeal_RecordsDeltaBase(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:sealbase-parent", git.NewLocalGitSandbox(repo))

	// Dirty the fork baseline so its tree differs from HEAD's tree — the
	// annotation must carry the dirty state's identity, not HEAD's.
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:sealbase-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	if _, err := mgr.Seal(ctx, child.ID()); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The baseline tree as the parent's sandbox captures it.
	parentSb := git.NewLocalGitSandbox(repo)
	baseline, err := parentSb.Capture(ctx)
	if err != nil {
		t.Fatalf("capture parent baseline: %v", err)
	}

	sealed := findRecords(t, store, "set:sealbase-child", SchemaRunOutputSealed)
	if len(sealed) != 1 {
		t.Fatalf("sealed records: %d, want 1", len(sealed))
	}
	payload := sealed[0].Body.Payload
	state, _ := payload["state"].(map[string]any)
	if state == nil {
		t.Fatalf("sealed payload has no state: %v", payload)
	}
	data, _ := state["data"].(map[string]any)
	if data == nil {
		t.Fatalf("sealed payload state has no data: %v", state)
	}
	base, _ := data["base_tree_sha"].(string)
	if base == "" {
		t.Fatal("sealed state carries no base_tree_sha; ApplyDelta at settle time would have no merge base")
	}
	if want, _ := baseline.Data["tree_sha"].(string); base != want {
		t.Errorf("base_tree_sha = %s, want the fork baseline's tree %s", base, want)
	}
}

// TestSettle_SameTreeMovedHeadRoutesToApply pins the timeline-move routing:
// a parent whose content is unchanged but that committed since the fork
// (here: it committed the baseline's own dirt) is a *moved* workspace —
// select refuses, because its reset would discard the commit — and apply
// merges, keeping the commit. This is deliberate: same-tree-different-HEAD
// captures digest differently because the timeline position is part of
// the state, and select's destructive path must never roll back a parent's
// real commit.
func TestSettle_SameTreeMovedHeadRoutesToApply(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:sametree-parent", git.NewLocalGitSandbox(repo))

	// A dirty fork baseline: uncommitted content on a tracked file.
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:sametree-child", repo, "wt")
	// A git worktree checks out HEAD, so the baseline's uncommitted content
	// must be carried into the child the way a provisioning layer would;
	// otherwise the child's delta would read the baseline dirt as its own
	// deletion.
	if err := os.WriteFile(filepath.Join(wtPath, "base.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write inherited baseline content: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent commits the baseline's dirt: content unchanged, timeline
	// moved.
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "parent commits the dirt")
	movedHead := gittest.MustHead(t, repo)

	err = mgr.Settle(ctx, out.ID(), SettleSelect)
	if err == nil || !strings.Contains(err.Error(), "SettleApply") {
		t.Fatalf("select over a moved timeline: %v, want a refusal pointing at SettleApply", err)
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("refused select consumed the output: %s", out.State())
	}

	if err := mgr.Settle(ctx, out.ID(), SettleApply); err != nil {
		t.Fatalf("SettleApply over the moved parent: %v", err)
	}
	if got := readRepoFile(t, repo, "result.txt"); got != "winner" {
		t.Errorf("result.txt = %q, want the child's content landed", got)
	}
	// The parent's commit survives the apply: the merge never rolls back
	// the parent's timeline.
	if got := gittest.ReadFile(t, repo, "base.txt"); got != "dirty" {
		t.Errorf("base.txt = %q, want the committed content intact", got)
	}
	if head := gittest.MustHead(t, repo); head != movedHead {
		t.Errorf("HEAD moved during apply: %s, want the parent's commit %s intact", head, movedHead)
	}
	if out.State() != OutputApplied {
		t.Errorf("output state = %s, want applied", out.State())
	}
}

// --- Plan 06 Phase B: settlement over dirty baselines ---

// dirtyBaselineFork forks a worktree child from a parent whose workspace
// carries uncommitted content — plan 06's scenario. The baseline dirt
// (README.md modified, uncommitted) is written back into the child
// worktree the way a provisioning layer would, because a git worktree
// checks out HEAD, not the dirt; without that, the child's delta would
// read the baseline's own content as its own deletion. The parent keeps
// its uncommitted content when the helper returns.
func dirtyBaselineFork(t *testing.T, mgr *ScopeManager, parentID, childOwner, repo, leaf string) (*Scope, string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatalf("dirty the baseline: %v", err)
	}
	child, wtPath := forkWorktreeChild(t, mgr, parentID, childOwner, repo, leaf)
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatalf("carry the baseline dirt into the child: %v", err)
	}
	return child, wtPath
}

// TestSeal_OverDirtyBaselineFreezesOnlyBranchChanges is the U1b law at the
// settlement layer: a seal over a dirty baseline freezes only the branch's
// own changes. The baseline's uncommitted content appears on neither side,
// so it is neither in the frozen change list nor in the overlap guard's
// view of parent drift.
func TestSeal_OverDirtyBaselineFreezesOnlyBranchChanges(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:dseal-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := dirtyBaselineFork(t, mgr, parent.ID(), "set:dseal-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}

	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal over a dirty baseline: %v", err)
	}
	changes := out.Changes()
	if len(changes) != 1 || changes[0].Path != "result.txt" || changes[0].Kind != ChangeCreate {
		t.Errorf("frozen changes = %+v, want only result.txt created; the baseline's own dirt must not be frozen in", changes)
	}
	for _, c := range changes {
		if c.Path == "README.md" {
			t.Errorf("the baseline's uncommitted content was frozen into the sealed change list: %+v", changes)
		}
	}
}

// TestSettle_SelectOverUnmovedDirtyParentIsDeterministic is the U1a law at
// the settlement layer: select over an unmoved dirty parent succeeds,
// deterministically. The pre-plan-06 state could pass only when both
// captures minted their stash commits within the same second; the
// repetition with a >1s gap between fork and settle kills that luck class
// outright rather than leaving the pass to timing.
func TestSettle_SelectOverUnmovedDirtyParentIsDeterministic(t *testing.T) {
	for i := 0; i < 3; i++ {
		t.Run(fmt.Sprintf("iteration-%d", i), func(t *testing.T) {
			mgr := NewScopeManager(newMemStore(t))
			repo := newTestRepo(t)
			ctx := context.Background()
			parent, _ := mgr.Create(fmt.Sprintf("set:dsel-parent-%d", i), git.NewLocalGitSandbox(repo))

			child, wtPath := dirtyBaselineFork(t, mgr, parent.ID(), fmt.Sprintf("set:dsel-child-%d", i), repo, "wt")
			if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("winner"), 0o644); err != nil {
				t.Fatalf("write result: %v", err)
			}
			out, err := mgr.Seal(ctx, child.ID())
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}

			// The same-second luck class: the baseline capture happened at
			// fork time; this gap pushes the parent's settle-time capture
			// into a different timestamp window.
			time.Sleep(1100 * time.Millisecond)

			if err := mgr.Settle(ctx, out.ID(), SettleSelect); err != nil {
				t.Fatalf("SettleSelect over an unmoved dirty parent: %v", err)
			}
			if got := readRepoFile(t, repo, "result.txt"); got != "winner" {
				t.Errorf("result.txt = %q, want the child's content landed", got)
			}
			// The baseline's uncommitted content survives the select.
			if got := readRepoFile(t, repo, "README.md"); got != "uncommitted" {
				t.Errorf("README.md = %q, want the baseline's uncommitted content preserved", got)
			}
			if child.State() != ScopeMerged {
				t.Errorf("child state = %s, want merged", child.State())
			}
			if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
				t.Errorf("child worktree should be destroyed after select, stat err = %v", err)
			}
		})
	}
}

// TestSettle_ApplyOverDirtyBaselineWithDisjointDrift is the U1b + U1c law
// at the settlement layer: a dirty baseline with path-disjoint parent drift
// merges — the drift survives, the baseline's own content never conflicts
// with itself, and the child's changes land on the parent.
func TestSettle_ApplyOverDirtyBaselineWithDisjointDrift(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:dapp-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := dirtyBaselineFork(t, mgr, parent.ID(), "set:dapp-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent drifts on a path disjoint from the output's changes,
	// keeping its own uncommitted baseline content.
	if err := os.WriteFile(filepath.Join(repo, "drift.txt"), []byte("parent drift"), 0o644); err != nil {
		t.Fatalf("write drift: %v", err)
	}

	if err := mgr.Settle(ctx, out.ID(), SettleApply); err != nil {
		t.Fatalf("SettleApply over a dirty baseline with disjoint drift: %v", err)
	}
	if got := readRepoFile(t, repo, "result.txt"); got != "winner" {
		t.Errorf("result.txt = %q, want the child's change landed", got)
	}
	if got := readRepoFile(t, repo, "drift.txt"); got != "parent drift" {
		t.Errorf("drift.txt = %q, want the parent's disjoint drift preserved", got)
	}
	if got := readRepoFile(t, repo, "README.md"); got != "uncommitted" {
		t.Errorf("README.md = %q, want the baseline's uncommitted content untouched", got)
	}
}

// TestSettle_ApplyOverlapOverDirtyBaselineRefusesTyped covers the overlap
// guard over a dirty baseline: a parent that changed a path the sealed
// output also changes refuses with ErrApplyConflict and consumes nothing.
// The baseline's own dirt is not an overlap — only a genuine path conflict
// refuses.
func TestSettle_ApplyOverlapOverDirtyBaselineRefusesTyped(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:dconf-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := dirtyBaselineFork(t, mgr, parent.ID(), "set:dconf-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("from child"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent drifts onto the output's own path.
	if err := os.WriteFile(filepath.Join(repo, "result.txt"), []byte("from parent"), 0o644); err != nil {
		t.Fatalf("write overlapping drift: %v", err)
	}

	err = mgr.Settle(ctx, out.ID(), SettleApply)
	if !errors.Is(err, ErrApplyConflict) {
		t.Fatalf("SettleApply over an overlapping path: %v, want ErrApplyConflict", err)
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("refused apply consumed the output: %s", out.State())
	}
	if got := readRepoFile(t, repo, "result.txt"); got != "from parent" {
		t.Errorf("result.txt = %q after a refused apply; the parent's content must be untouched", got)
	}
	if got := readRepoFile(t, repo, "README.md"); got != "uncommitted" {
		t.Errorf("README.md = %q after a refused apply; the baseline's uncommitted content must be untouched", got)
	}

	// The refused output is still settle-able: release ends it cleanly.
	if err := mgr.Settle(ctx, out.ID(), SettleRelease); err != nil {
		t.Fatalf("SettleRelease after a refused apply: %v", err)
	}
}

// TestSettle_PhysicalEffectLandsWhenMergeUnrecords pins the typed
// partial-settlement contract (plan 07, H3): a settle whose physical
// effect lands but whose causal merge fails to record reports
// ErrSettleEffectLanded, the work stands anyway, and the output stays
// unconsumed — the caller can end its lifecycle instead of guessing
// whether a retry would re-apply an effect that already happened.
func TestSettle_PhysicalEffectLandsWhenMergeUnrecords(t *testing.T) {
	store, _ := fileStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:partial-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:partial-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("landed anyway"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The causal merge records to the store; closing it makes the
	// recording fail while the physical verbs — capture, apply — touch
	// only git. This is the exact partial-settlement window.
	if err := store.Close(ctx); err != nil {
		t.Fatalf("close store: %v", err)
	}

	err = mgr.Settle(ctx, out.ID(), SettleSelect)
	if !errors.Is(err, ErrSettleEffectLanded) {
		t.Fatalf("settle with an unrecordable merge: %v, want an error wrapping ErrSettleEffectLanded", err)
	}
	if got := readRepoFile(t, repo, "result.txt"); got != "landed anyway" {
		t.Errorf("result.txt = %q; the physical effect must land despite the recording failure", got)
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("output state = %s, want unconsumed — the caller must be able to end the lifecycle", out.State())
	}

}

// TestSettle_ApplyPhysicalEffectLandsWhenMergeUnrecords is the apply verb's
// half of the typed partial-settlement contract: with the parent moved on
// a disjoint path, the three-way merge lands the child's change, the
// causal-merge recording fails, and the error wraps
// ErrSettleEffectLanded with the drift preserved and the output
// unconsumed.
func TestSettle_ApplyPhysicalEffectLandsWhenMergeUnrecords(t *testing.T) {
	store, _ := fileStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:partial-apply-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:partial-apply-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("landed anyway"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent moves on a path disjoint from the output's changes, so
	// the settle takes the three-way apply path.
	if err := os.WriteFile(filepath.Join(repo, "drift.txt"), []byte("parent drift"), 0o644); err != nil {
		t.Fatalf("write drift: %v", err)
	}

	if err := store.Close(ctx); err != nil {
		t.Fatalf("close store: %v", err)
	}

	err = mgr.Settle(ctx, out.ID(), SettleApply)
	if !errors.Is(err, ErrSettleEffectLanded) {
		t.Fatalf("apply with an unrecordable merge: %v, want an error wrapping ErrSettleEffectLanded", err)
	}
	if got := readRepoFile(t, repo, "result.txt"); got != "landed anyway" {
		t.Errorf("result.txt = %q; the merged change must land despite the recording failure", got)
	}
	if got := readRepoFile(t, repo, "drift.txt"); got != "parent drift" {
		t.Errorf("drift.txt = %q; the parent's disjoint drift must survive the landed merge", got)
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("output state = %s, want unconsumed", out.State())
	}
}
