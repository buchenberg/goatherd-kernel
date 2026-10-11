package shepherd_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	"github.com/buchenberg/shepherd-kernel-go/internal/gittest"
	"github.com/buchenberg/shepherd-kernel-go/internal/rttest"
	sandbox "github.com/buchenberg/shepherd-kernel-go/sandbox"
	"github.com/buchenberg/shepherd-kernel-go/sandbox/git"
	"github.com/buchenberg/shepherd-kernel-go/supervisor"
	trace "github.com/buchenberg/shepherd-kernel-go/trace"
)

// The three workspace-handle recipes from the reference (plan 04 §3), as
// executable documentation. They must pass on Windows (git worktree carrier)
// too — no OS assumptions beyond the git binary.

// TestExample_BestOfN: fork N isolated worktree scopes from one repo, each
// does work, seal all, inspect, select exactly one, discard the rest, and
// assert the parent ends up with only the winner's work.

// forkWorktreeChild forks an isolated worktree child of parent, provisions
// the worktree, and hands back the child and the worktree path the test
// "works" in. leaf names the worktree directory (no ':' — Windows paths).
// Local copy of the root package's test helper: this file is the external
// test package, and shared helpers that touch runtime types cannot live in
// rttest without an import cycle.
func forkWorktreeChild(t *testing.T, mgr *shepherd.ScopeManager, parentID, childOwner, repo, leaf string) (*shepherd.Scope, string) {
	t.Helper()
	wtPath := filepath.Join(t.TempDir(), leaf)
	sb := git.NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(context.Background(), sandbox.SandboxSpec{}); err != nil {
		t.Fatalf("worktree create for %s: %v", childOwner, err)
	}
	child, err := mgr.ForkIsolated(parentID, childOwner, nil, sb)
	if err != nil {
		t.Fatalf("ForkIsolated %s: %v", childOwner, err)
	}
	return child, wtPath
}

// findRecords returns the retained records on an owner path with the given
// schema ref. Local copy of the root package's test helper.
func findRecords(t *testing.T, store *trace.SQLiteTraceStore, ownerID, schemaRef string) []trace.Record {
	t.Helper()
	slice, err := store.ReadOwnerPrefix(context.Background(), trace.TrustedReadContext, ownerID, 999, trace.ModeBoth)
	if err != nil {
		t.Fatalf("read trace for %s: %v", ownerID, err)
	}
	var found []trace.Record
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == schemaRef {
			found = append(found, rec.(trace.Record))
		}
	}
	return found
}

// readRepoFile reads a file from the test repo. Local copy of the root
// package's test helper.
func readRepoFile(t *testing.T, repo, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// fileExistsT reports whether a path exists in the test repo.
func fileExistsT(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func TestExample_BestOfN(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	repo := gittest.NewTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("bestofn", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	const attempts = 3
	outs := make([]*shepherd.RetainedOutput, 0, attempts)
	var children []*shepherd.Scope
	for i := 0; i < attempts; i++ {
		child, wtPath := forkWorktreeChild(t, mgr, parent.ID(),
			fmt.Sprintf("bestofn:c%d", i), repo, fmt.Sprintf("wt-%d", i))
		children = append(children, child)

		// "Does work": each attempt writes a distinct result file.
		if err := os.WriteFile(filepath.Join(wtPath, fmt.Sprintf("result-%d.txt", i)),
			[]byte(fmt.Sprintf("attempt %d", i)), 0o644); err != nil {
			t.Fatalf("attempt %d writes: %v", i, err)
		}

		out, err := mgr.Seal(ctx, child.ID())
		if err != nil {
			t.Fatalf("seal attempt %d: %v", i, err)
		}
		// The supervisor inspects each sealed output's frozen changes.
		if len(out.Changes()) != 1 || out.Changes()[0].Path != fmt.Sprintf("result-%d.txt", i) {
			t.Fatalf("attempt %d sealed changes: %+v, want exactly its result file", i, out.Changes())
		}
		outs = append(outs, out)
	}

	// Choose attempt 1: select its output, discard the losers.
	if err := mgr.Settle(ctx, outs[1].ID(), shepherd.SettleSelect); err != nil {
		t.Fatalf("select winner: %v", err)
	}
	for i, out := range outs {
		if i == 1 {
			continue
		}
		if err := mgr.Settle(ctx, out.ID(), shepherd.SettleDiscard); err != nil {
			t.Fatalf("discard loser %d: %v", i, err)
		}
	}

	// The parent workspace contains only the winner.
	if got := readRepoFile(t, repo, "result-1.txt"); got != "attempt 1" {
		t.Errorf("result-1.txt = %q, want the winner's content", got)
	}
	for _, i := range []int{0, 2} {
		if fileExistsT(t, repo, fmt.Sprintf("result-%d.txt", i)) {
			t.Errorf("loser %d leaked into the parent workspace", i)
		}
		if children[i].State() != shepherd.ScopeDiscarded {
			t.Errorf("loser %d state = %s, want discarded", i, children[i].State())
		}
	}
	if children[1].State() != shepherd.ScopeMerged {
		t.Errorf("winner state = %s, want merged", children[1].State())
	}

	// Consume-once: the winner cannot be settled again.
	if err := mgr.Settle(ctx, outs[1].ID(), shepherd.SettleSelect); !errors.Is(err, shepherd.ErrOutputConsumed) {
		t.Errorf("second settle of winner: %v, want shepherd.ErrOutputConsumed", err)
	}
}

// TestExample_RetryUntilAcceptable: loop fork → work → checkpoint →
// CommitMerge with a drafts-only reviewer → deny → discard → restore → retry
// with injected guidance, until approved. CheckCall's pre-tool deny and the
// merge gate's commit deny appear in one flow.
func TestExample_RetryUntilAcceptable(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	repo := gittest.NewTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("retry", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// The supervision surface: supervisor.DestructiveToolGuard denies calls before
	// execution; shepherd.DraftsOnlyReviewer denies merges at commit time.
	sup := supervisor.NewSupervisor(mgr, nil)
	sup.AddRule(supervisor.DestructiveToolGuard())
	reviewer := shepherd.DraftsOnlyReviewer("out")

	const maxAttempts = 3
	denied := 0
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// shepherd.Checkpoint before the attempt: a denial's rollback is the caller's
		// restore of the shared workspace.
		cp, err := mgr.CreateCheckpoint(ctx, parent.ID(), []byte("conversation"))
		if err != nil {
			t.Fatalf("attempt %d checkpoint: %v", attempt, err)
		}

		// The "model" first tries something destructive; the supervisor's
		// pre-execution guard denies it, and the model complies.
		badCall := trace.EffectEvent{
			TraceOwnerID: fmt.Sprintf("retry:%d", attempt),
			Mode:         trace.Declaration,
			SchemaRef:    "yaah.tool.bash.v1",
			KindLabel:    "bash",
			Payload:      map[string]any{"cmd": "rm -rf ."},
		}
		if iv := sup.CheckCall(badCall); iv == nil || iv.Type != supervisor.InterventionDeny {
			t.Fatalf("attempt %d: destructive call not denied pre-execution (iv=%+v)", attempt, iv)
		}

		// Fork a child that shares the parent's sandbox, carrying guidance
		// from the previous denial in its snapshot (the conversation state).
		guidance := "write your result under out/"
		if attempt == 1 {
			guidance = "do the task"
		}
		child, err := mgr.Fork(parent.ID(), fmt.Sprintf("retry:%d", attempt), guidance)
		if err != nil {
			t.Fatalf("attempt %d fork: %v", attempt, err)
		}

		// The child works directly in the shared tree. Early attempts write
		// outside out/; the last one complies with the guidance.
		target := "scratch.txt"
		if attempt == maxAttempts {
			target = "out/final.txt"
			if err := os.MkdirAll(filepath.Join(repo, "out"), 0o755); err != nil {
				t.Fatalf("mkdir out: %v", err)
			}
		}
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(target)),
			[]byte(fmt.Sprintf("attempt %d", attempt)), 0o644); err != nil {
			t.Fatalf("attempt %d write: %v", attempt, err)
		}

		lastErr = mgr.CommitMerge(ctx, child.ID(), reviewer)
		var deniedErr *shepherd.SupervisorDeniedError
		if errors.As(lastErr, &deniedErr) {
			denied++
			// Deny → child discarded (CommitMerge does it); the caller
			// restores the shared workspace from the checkpoint.
			if _, err := mgr.RestoreCheckpoint(ctx, cp.ID); err != nil {
				t.Fatalf("attempt %d restore: %v", attempt, err)
			}
			if fileExistsT(t, repo, filepath.FromSlash(target)) {
				t.Errorf("attempt %d: denied work survived the restore", attempt)
			}
			continue
		}
		if lastErr != nil {
			t.Fatalf("attempt %d CommitMerge: %v", attempt, lastErr)
		}
		break // approved and merged
	}

	if denied != maxAttempts-1 {
		t.Errorf("denials = %d, want %d", denied, maxAttempts-1)
	}
	if lastErr != nil {
		t.Fatalf("final attempt should have been approved, got: %v", lastErr)
	}
	if got := readRepoFile(t, repo, "out/final.txt"); got != fmt.Sprintf("attempt %d", maxAttempts) {
		t.Errorf("out/final.txt = %q, want the approved attempt's content", got)
	}
	if fileExistsT(t, repo, "scratch.txt") {
		t.Error("denied attempts' work must be gone from the parent tree")
	}

	// The trace carries the whole story: one denied decision per failed
	// attempt, one approval.
	decisions := findRecords(t, store, "retry", shepherd.SchemaSupervisorDecision)
	if len(decisions) != maxAttempts {
		t.Fatalf("decision records: %d, want %d", len(decisions), maxAttempts)
	}
	approved, refused := 0, 0
	for _, d := range decisions {
		if fmt.Sprint(d.Body.Payload["approved"]) == "true" {
			approved++
		} else {
			refused++
		}
	}
	if approved != 1 || refused != maxAttempts-1 {
		t.Errorf("decisions approved/refused = %d/%d, want 1/%d", approved, refused, maxAttempts-1)
	}
}

// TestExample_ApplyOntoMovedWorkspace: seal a child output; the parent
// advances on disjoint paths and shepherd.SettleApply succeeds; a second output
// overlapping the parent's concurrent change is refused with
// shepherd.ErrApplyConflict and stays unconsumed.
func TestExample_ApplyOntoMovedWorkspace(t *testing.T) {
	mgr := shepherd.NewScopeManager(rttest.NewMemStore(t))
	repo := gittest.NewTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("moved", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// Child 1's output: a feature file, disjoint from everything.
	child1, wt1 := forkWorktreeChild(t, mgr, parent.ID(), "moved:c1", repo, "wt1")
	if err := os.WriteFile(filepath.Join(wt1, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	out1, err := mgr.Seal(ctx, child1.ID())
	if err != nil {
		t.Fatalf("seal child 1: %v", err)
	}

	// Child 2's output: it will touch shared.txt, the same path the parent
	// is about to move.
	child2, wt2 := forkWorktreeChild(t, mgr, parent.ID(), "moved:c2", repo, "wt2")
	if err := os.WriteFile(filepath.Join(wt2, "shared.txt"), []byte("child's edit"), 0o644); err != nil {
		t.Fatalf("write shared: %v", err)
	}
	out2, err := mgr.Seal(ctx, child2.ID())
	if err != nil {
		t.Fatalf("seal child 2: %v", err)
	}

	// Meanwhile the parent advances: one disjoint path (parent.txt) and one
	// that overlaps child 2 (shared.txt).
	if err := os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("parent's own"), 0o644); err != nil {
		t.Fatalf("write parent.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("parent's edit"), 0o644); err != nil {
		t.Fatalf("write shared.txt: %v", err)
	}
	gittest.MustGit(t, repo, "add", "-A")
	gittest.MustGit(t, repo, "commit", "-m", "parent advances")

	// Disjoint output applies onto the moved workspace.
	if err := mgr.Settle(ctx, out1.ID(), shepherd.SettleApply); err != nil {
		t.Fatalf("apply disjoint output onto moved parent: %v", err)
	}
	if got := readRepoFile(t, repo, "feature.txt"); got != "feature" {
		t.Errorf("feature.txt = %q, want the child's content", got)
	}
	if got := readRepoFile(t, repo, "parent.txt"); got != "parent's own" {
		t.Errorf("parent.txt = %q, want the parent's own work preserved", got)
	}

	// Overlapping output is refused without consuming.
	err = mgr.Settle(ctx, out2.ID(), shepherd.SettleApply)
	if !errors.Is(err, shepherd.ErrApplyConflict) {
		t.Fatalf("overlapping apply: %v, want shepherd.ErrApplyConflict", err)
	}
	if !strings.Contains(err.Error(), "shared.txt") {
		t.Errorf("conflict text = %q, want it to name shared.txt", err.Error())
	}
	if out2.State() != shepherd.OutputUnconsumed {
		t.Errorf("conflicting output state = %s, want unconsumed", out2.State())
	}
	if got := readRepoFile(t, repo, "shared.txt"); got != "parent's edit" {
		t.Errorf("shared.txt = %q, want the parent's version untouched by the refusal", got)
	}

	// Clean up the refused output's scope.
	if err := mgr.Settle(ctx, out2.ID(), shepherd.SettleDiscard); err != nil {
		t.Fatalf("discard refused output: %v", err)
	}
}
