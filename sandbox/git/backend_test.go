package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buchenberg/shepherd-kernel-go/internal/gittest"
	sbx "github.com/buchenberg/shepherd-kernel-go/sandbox"
	trace "github.com/buchenberg/shepherd-kernel-go/trace"
)

// samePath reports whether two paths refer to the same location, resolving
// symlinks so a temp-dir prefix difference (e.g. /var vs /private/var) does not
// produce a false mismatch.
func TestWorkspaceState_DigestIsOrderIndependent(t *testing.T) {
	a := trace.WorkspaceState{
		Backend:  "git",
		Revision: "abc",
		Data:     map[string]any{"head_sha": "abc", "stash_sha": "def"},
	}
	b := trace.WorkspaceState{
		Backend:  "git",
		Revision: "abc",
		Data:     map[string]any{"stash_sha": "def", "head_sha": "abc"},
	}

	da, err := a.Digest()
	if err != nil {
		t.Fatalf("digest a: %v", err)
	}
	db, err := b.Digest()
	if err != nil {
		t.Fatalf("digest b: %v", err)
	}
	if da != db {
		t.Errorf("digest must be independent of map iteration order: %s vs %s", da, db)
	}
	if !strings.HasPrefix(da, "sha256:") {
		t.Errorf("digest = %q, want a sha256: prefix", da)
	}
}

func TestWorkspaceState_DigestSensitivity(t *testing.T) {
	base := trace.WorkspaceState{
		Backend:  "git",
		Revision: "abc",
		Data:     map[string]any{"stash_sha": "def"},
	}
	baseDigest, err := base.Digest()
	if err != nil {
		t.Fatalf("base digest: %v", err)
	}

	cases := map[string]trace.WorkspaceState{
		"backend":  {Backend: "containerd", Revision: "abc", Data: map[string]any{"stash_sha": "def"}},
		"revision": {Backend: "git", Revision: "xyz", Data: map[string]any{"stash_sha": "def"}},
		"data":     {Backend: "git", Revision: "abc", Data: map[string]any{"stash_sha": "zzz"}},
		"extra":    {Backend: "git", Revision: "abc", Data: map[string]any{"stash_sha": "def", "extra": "1"}},
	}
	for name, ws := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := ws.Digest()
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if d == baseDigest {
				t.Errorf("digest must change when %s changes", name)
			}
		})
	}
}

func TestWorkspaceState_DigestEmptyState(t *testing.T) {
	if _, err := (trace.WorkspaceState{}).Digest(); err != nil {
		t.Fatalf("empty state digest: %v", err)
	}
}

func TestGitSandbox_Capabilities(t *testing.T) {
	repo := newTestRepo(t)

	inPlace := NewLocalGitSandbox(repo).Capabilities()
	if inPlace.Lifecycle {
		t.Error("in-place mode must not claim a real lifecycle")
	}
	if inPlace.Isolated {
		t.Error("in-place mode must not claim isolation")
	}
	if !inPlace.Diff {
		t.Error("git backend should support Diff")
	}
	if inPlace.Containment != trace.ContainUncontained {
		t.Errorf("in-place containment = %q, want %q", inPlace.Containment, trace.ContainUncontained)
	}
	if inPlace.Exec || inPlace.FileIO {
		t.Error("git backend must not claim Exec or FileIO")
	}

	worktree := NewWorktreeSandbox(repo, t.TempDir()).Capabilities()
	if !worktree.Lifecycle {
		t.Error("worktree mode should claim a real lifecycle")
	}
	if !worktree.Isolated {
		t.Error("worktree mode should claim isolation")
	}
	if worktree.Containment != trace.ContainBuffered {
		t.Errorf("worktree containment = %q, want %q", worktree.Containment, trace.ContainBuffered)
	}
}

func TestGitSandbox_UnsupportedMethods(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewLocalGitSandbox(repo)
	ctx := context.Background()

	if _, err := sb.Exec(ctx, sbx.ExecRequest{Command: "true"}); err != sbx.ErrUnsupported {
		t.Errorf("Exec err = %v, want sbx.ErrUnsupported", err)
	}
	if _, err := sb.ReadFile(ctx, "README.md"); err != sbx.ErrUnsupported {
		t.Errorf("ReadFile err = %v, want sbx.ErrUnsupported", err)
	}
	if err := sb.WriteFile(ctx, "x", nil, 0o644); err != sbx.ErrUnsupported {
		t.Errorf("WriteFile err = %v, want sbx.ErrUnsupported", err)
	}
}

func TestGitSandbox_InPlaceCreateValidatesRepo(t *testing.T) {
	ctx := context.Background()

	if err := NewLocalGitSandbox(t.TempDir()).Create(ctx, sbx.SandboxSpec{}); err == nil {
		t.Error("Create on a non-repository should fail")
	}
	if err := NewLocalGitSandbox(newTestRepo(t)).Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Errorf("Create on a repository should succeed: %v", err)
	}
}

func TestGitSandbox_InPlaceDestroyIsNoOp(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewLocalGitSandbox(repo)

	if err := sb.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// The user's repository must survive: an in-place sandbox never owns it.
	if !fileExists(t, repo, "README.md") {
		t.Error("Destroy must not delete the repository")
	}
}

// worktreePaths returns the paths git currently tracks as worktrees.
func TestWorktreeSandbox_CreateAndDestroy(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !fileExists(t, wtPath, "README.md") {
		t.Fatal("worktree should be checked out at HEAD")
	}

	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree directory should be removed, stat err=%v", err)
	}
	for _, p := range gittest.WorktreePaths(t, repo) {
		if gittest.SamePath(t, p, wtPath) {
			t.Errorf("worktree %s still registered after Destroy", p)
		}
	}
	// The repository must always survive.
	if !fileExists(t, repo, "README.md") {
		t.Error("Destroy must not delete the repository")
	}
}

func TestWorktreeSandbox_CreateRefusesNonEmptyPath(t *testing.T) {
	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	gittest.WriteFile(t, wtPath, "occupied.txt", "do not clobber")

	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(context.Background(), sbx.SandboxSpec{}); err == nil {
		t.Error("Create must refuse a non-empty worktree path")
	}
}

func TestWorktreeSandbox_RecoversFromStaleEntry(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate a crash: the directory disappears but git's administrative
	// entry under .git/worktrees survives.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}

	// Destroy must clean the stale entry rather than failing.
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy after stale removal: %v", err)
	}

	// And the same path must be reusable.
	if err := sb.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create after recovery: %v", err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })

	for _, p := range gittest.WorktreePaths(t, repo) {
		if gittest.SamePath(t, p, wtPath) {
			return
		}
	}
	t.Errorf("worktree %s not registered after recovery", wtPath)
}

func TestWorktreeSandbox_ApplySeedsForkState(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)

	// Dirty the parent tree: a modified tracked file and an untracked file.
	gittest.WriteFile(t, repo, "README.md", "# Fork point")
	gittest.WriteFile(t, repo, "untracked.txt", "also at fork point")

	main := NewLocalGitSandbox(repo)
	forkState, err := main.Capture(ctx)
	if err != nil {
		t.Fatalf("capture fork state: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	wt := NewWorktreeSandbox(repo, wtPath)
	if err := wt.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("worktree Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Destroy(context.Background()) })

	if err := wt.Apply(ctx, forkState); err != nil {
		t.Fatalf("worktree Apply: %v", err)
	}

	if got := gittest.ReadFile(t, wtPath, "README.md"); got != "# Fork point" {
		t.Errorf("worktree README.md = %q, want the parent's dirty content", got)
	}
	if got := gittest.ReadFile(t, wtPath, "untracked.txt"); got != "also at fork point" {
		t.Errorf("worktree untracked.txt = %q, want the parent's untracked file", got)
	}
}

func TestWorktreeSandbox_SiblingsAreIndependent(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)

	root := t.TempDir()
	wtA := NewWorktreeSandbox(repo, filepath.Join(root, "a"))
	wtB := NewWorktreeSandbox(repo, filepath.Join(root, "b"))
	if err := wtA.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	t.Cleanup(func() { _ = wtA.Destroy(context.Background()) })
	if err := wtB.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("create B: %v", err)
	}
	t.Cleanup(func() { _ = wtB.Destroy(context.Background()) })

	gittest.WriteFile(t, filepath.Join(root, "a"), "only-a.txt", "variant A")
	if fileExists(t, filepath.Join(root, "b"), "only-a.txt") {
		t.Error("a file written in worktree A must not appear in worktree B")
	}
	if fileExists(t, repo, "only-a.txt") {
		t.Error("a file written in worktree A must not appear in the main tree")
	}

	stateA, err := wtA.Capture(ctx)
	if err != nil {
		t.Fatalf("capture A: %v", err)
	}
	// Capturing A must not perturb B.
	if fileExists(t, filepath.Join(root, "b"), "only-a.txt") {
		t.Error("capturing A must not affect worktree B")
	}
	if _, err := wtB.Capture(ctx); err != nil {
		t.Fatalf("capture B: %v", err)
	}
	if stateA.Revision == "" {
		t.Error("captured state should carry a revision")
	}
}

func TestWorktreeSandbox_CaptureAppliesToMainTree(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)

	wtPath := filepath.Join(t.TempDir(), "wt")
	wt := NewWorktreeSandbox(repo, wtPath)
	if err := wt.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("worktree Create: %v", err)
	}

	// Speculative work happens only in the worktree.
	gittest.WriteFile(t, wtPath, "feature.txt", "from worktree")
	if fileExists(t, repo, "feature.txt") {
		t.Fatal("the main tree must not see worktree changes before Apply")
	}

	variant, err := wt.Capture(ctx)
	if err != nil {
		t.Fatalf("worktree Capture: %v", err)
	}

	// The worktree is torn down before the choice is made, so the state must
	// survive in the shared object store.
	if err := wt.Destroy(ctx); err != nil {
		t.Fatalf("worktree Destroy: %v", err)
	}

	main := NewLocalGitSandbox(repo)
	if err := main.Apply(ctx, variant); err != nil {
		t.Fatalf("apply variant to main tree: %v", err)
	}
	if got := gittest.ReadFile(t, repo, "feature.txt"); got != "from worktree" {
		t.Errorf("feature.txt = %q, want the variant's content", got)
	}
}

func TestWorktreeSandbox_CreateAtRepoPathRejected(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewWorktreeSandbox(repo, repo)
	if err := sb.Create(context.Background(), sbx.SandboxSpec{}); err == nil {
		t.Error("Create must reject a worktree path equal to the repository path")
	}
}

// TestWorktreeSandbox_DestroyRetriesOnBusyDirectory pins the Windows fallback:
// the worktree is removed even when `git worktree remove` cannot complete.
func TestWorktreeSandbox_DestroyRetriesOnBusyDirectory(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A lock file inside the worktree makes `git worktree remove` fail on
	// some platforms; the fallback must still leave nothing behind.
	gittest.WriteFile(t, wtPath, ".git-lock-probe", "x")
	time.Sleep(10 * time.Millisecond)

	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree should be gone, stat err=%v", err)
	}
	for _, p := range gittest.WorktreePaths(t, repo) {
		if gittest.SamePath(t, p, wtPath) {
			t.Errorf("worktree %s still registered after Destroy", p)
		}
	}
}

// TestGitSandbox_CapturePreservesCallerIndex pins the non-mutating contract when
// the caller had staged changes before Capture: the real index must be left
// exactly as it was, and the capture must still include untracked files.
func TestGitSandbox_CapturePreservesCallerIndex(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)

	// Caller stages a tracked-file edit and leaves an untracked file.
	gittest.WriteFile(t, repo, "README.md", "# Staged edit")
	mustGit(t, repo, "add", "README.md")
	gittest.WriteFile(t, repo, "untracked.txt", "untracked")
	stagedBefore := gittest.StagedFiles(t, repo)
	if stagedBefore != "README.md" {
		t.Fatalf("precondition: staged = %q, want %q", stagedBefore, "README.md")
	}

	sb := NewLocalGitSandbox(repo)
	ws, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if stagedAfter := gittest.StagedFiles(t, repo); stagedAfter != stagedBefore {
		t.Errorf("Capture mutated the caller's index: staged %q, want %q", stagedAfter, stagedBefore)
	}
	tree, _ := ws.Data["tree_sha"].(string)
	if tree == "" {
		t.Fatal("Capture did not record a tree for a dirty worktree")
	}
	if headTree := gittest.MustGit(t, repo, "rev-parse", "HEAD^{tree}"); tree == headTree {
		t.Errorf("dirty capture's tree = HEAD's tree %s; the staged edit and untracked file are missing from it", tree)
	}

	// The capture must include the untracked file: wipe the tree, apply, verify.
	mustGit(t, repo, "reset", "--hard")
	mustGit(t, repo, "clean", "-fd")
	if err := sb.Apply(ctx, ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "untracked.txt")); err != nil {
		t.Errorf("capture lost the untracked file: %v", err)
	}
}

// TestGitSandbox_DiffPreservesCallerIndex is the Diff counterpart: staging
// untracked files to render the diff must not disturb the caller's index either.
func TestGitSandbox_DiffPreservesCallerIndex(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	base := gittest.MustHead(t, repo)

	gittest.WriteFile(t, repo, "README.md", "# Staged edit")
	mustGit(t, repo, "add", "README.md")
	gittest.WriteFile(t, repo, "brand-new.txt", "untracked")
	stagedBefore := gittest.StagedFiles(t, repo)

	sb := NewLocalGitSandbox(repo)
	diff, files, err := sb.Diff(ctx, trace.WorkspaceState{Backend: "git", Revision: base}, 0)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "brand-new.txt") {
		t.Errorf("diff should include the untracked file:\n%s", diff)
	}
	seen := false
	for _, f := range files {
		if f == "brand-new.txt" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("changed files = %v, want brand-new.txt", files)
	}

	if stagedAfter := gittest.StagedFiles(t, repo); stagedAfter != stagedBefore {
		t.Errorf("Diff mutated the caller's index: staged %q, want %q", stagedAfter, stagedBefore)
	}
}

func newTestRepo(t *testing.T) string { return gittest.NewTestRepo(t) }

func requireGit(t *testing.T) { gittest.RequireGit(t) }

func mustGit(t *testing.T, dir string, args ...string) { gittest.MustGit(t, dir, args...) }

func fileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// --- Plan 06 Phase A: content identity and baseline-relative deltas ---

// commitFiles stages and commits the current worktree content, returning the
// commit's SHA. Baseline setup for the plan-06 tests: files are written and
// committed so trees are unambiguous.
func commitFiles(t *testing.T, repo, msg string) string {
	t.Helper()
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", msg)
	return gittest.MustGit(t, repo, "rev-parse", "HEAD")
}

// TestGitSandbox_CaptureDigestStableAcrossSeconds is plan 06 U1a's law test:
// two captures of a byte-identical dirty tree must digest equal regardless of
// when they run. The old stash-keyed state minted a fresh timestamped commit
// per capture, so equality held only within the same second — the sleep kills
// that luck class outright rather than leaving it to timing.
func TestGitSandbox_CaptureDigestStableAcrossSeconds(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	gittest.WriteFile(t, repo, "dirty.txt", "same content")
	sb := NewLocalGitSandbox(repo)

	first, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	second, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}

	if first.Data["tree_sha"] != second.Data["tree_sha"] {
		t.Errorf("tree_sha changed between identical captures: %v vs %v",
			first.Data["tree_sha"], second.Data["tree_sha"])
	}
	d1, err := first.Digest()
	if err != nil {
		t.Fatalf("digest first: %v", err)
	}
	d2, err := second.Digest()
	if err != nil {
		t.Fatalf("digest second: %v", err)
	}
	if d1 != d2 {
		t.Errorf("captures of an identical dirty tree digested differently: %s vs %s", d1, d2)
	}
}

// TestGitSandbox_ApplyRestoresExactTree is the deletion regression: a
// capture that held a deletion of a HEAD file must restore as a deletion.
// Writing the tree's entries alone (the checkout-index / git restore
// alternatives plan 06 rejected) would leave the deleted file behind and
// restore a superset of the captured state.
func TestGitSandbox_ApplyRestoresExactTree(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	gittest.WriteFile(t, repo, "a.txt", "one")
	gittest.WriteFile(t, repo, "b.txt", "two")
	commitFiles(t, repo, "base files")

	// The dirty captured state: modify a tracked file, delete another, add
	// an untracked one.
	gittest.WriteFile(t, repo, "a.txt", "modified")
	if err := os.Remove(filepath.Join(repo, "b.txt")); err != nil {
		t.Fatalf("remove b.txt: %v", err)
	}
	gittest.WriteFile(t, repo, "c.txt", "untracked")

	sb := NewLocalGitSandbox(repo)
	ws, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	mustGit(t, repo, "reset", "--hard")
	mustGit(t, repo, "clean", "-fd")
	if err := sb.Apply(ctx, ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := gittest.ReadFile(t, repo, "a.txt"); got != "modified" {
		t.Errorf("a.txt = %q, want the captured modification", got)
	}
	if gittest.FileExists(t, repo, "b.txt") {
		t.Error("b.txt survived Apply; a capture that deleted it must restore as a deletion, not a superset")
	}
	if !gittest.FileExists(t, repo, "c.txt") {
		t.Error("c.txt missing after Apply; the capture included the untracked file")
	}
}

// TestGitSandbox_DiffAgainstDirtyBaseline is plan 06 U1b's law test: Diff's
// base is the captured full state, so a dirty baseline's own uncommitted
// content appears in neither direction of the comparison.
func TestGitSandbox_DiffAgainstDirtyBaseline(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	gittest.WriteFile(t, repo, "base.txt", "committed")
	gittest.WriteFile(t, repo, "branch.txt", "original")
	commitFiles(t, repo, "base files")

	gittest.WriteFile(t, repo, "base.txt", "dirty")
	sb := NewLocalGitSandbox(repo)
	baseline, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture baseline: %v", err)
	}

	// An unmoved workspace diffs empty against the dirty baseline — the
	// baseline's own uncommitted content is on neither side.
	diff, files, err := sb.Diff(ctx, baseline, 0)
	if err != nil {
		t.Fatalf("Diff (unmoved): %v", err)
	}
	if diff != "" || len(files) != 0 {
		t.Errorf("unmoved worktree diffed against its own baseline: diff %q, files %v", diff, files)
	}

	// A real change lists only the changed path.
	gittest.WriteFile(t, repo, "branch.txt", "changed")
	diff, files, err = sb.Diff(ctx, baseline, 0)
	if err != nil {
		t.Fatalf("Diff (changed): %v", err)
	}
	if len(files) != 1 || files[0] != "branch.txt" {
		t.Errorf("changed files = %v, want only branch.txt (the baseline's own dirt must not appear)", files)
	}
	if strings.Contains(diff, "base.txt") {
		t.Errorf("diff includes the baseline's own uncommitted content:\n%s", diff)
	}
}

// applyDeltaFixture builds the dirty-baseline fork scenario the plan-06
// delta tests share: a repo whose committed files are base.txt, branch.txt,
// and doomed.txt, a dirty baseline (base.txt modified), the baseline state,
// and a child state that, relative to the baseline, modifies branch.txt,
// deletes doomed.txt, and adds created.txt. The worktree is left at the
// baseline when the helper returns.
func applyDeltaFixture(t *testing.T) (*GitSandbox, trace.WorkspaceState, trace.WorkspaceState) {
	t.Helper()
	ctx := context.Background()
	repo := newTestRepo(t)
	gittest.WriteFile(t, repo, "base.txt", "committed")
	gittest.WriteFile(t, repo, "branch.txt", "original")
	gittest.WriteFile(t, repo, "doomed.txt", "still here")
	commitFiles(t, repo, "base files")

	sb := NewLocalGitSandbox(repo)

	// The dirty fork baseline.
	gittest.WriteFile(t, repo, "base.txt", "dirty")
	baseline, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture baseline: %v", err)
	}

	// The child's work: baseline content plus its own changes.
	gittest.WriteFile(t, repo, "branch.txt", "from child")
	if err := os.Remove(filepath.Join(repo, "doomed.txt")); err != nil {
		t.Fatalf("remove doomed.txt: %v", err)
	}
	gittest.WriteFile(t, repo, "created.txt", "child file")
	child, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture child: %v", err)
	}
	child.Data["base_tree_sha"] = baseline.Data["tree_sha"]

	// Restore the worktree to the dirty baseline for the delta tests.
	if err := sb.Apply(ctx, baseline); err != nil {
		t.Fatalf("Apply baseline: %v", err)
	}
	if got := gittest.ReadFile(t, repo, "base.txt"); got != "dirty" {
		t.Fatalf("fixture setup: base.txt = %q, want the dirty baseline content", got)
	}
	return sb, baseline, child
}

// TestGitSandbox_ApplyDeltaIsBaselineRelative is the canary: over a dirty
// baseline, a settled delta's child changes physically land in the parent
// worktree, the baseline's own content is untouched, and a child deletion
// lands. The pre-plan-06 ApplyDelta silently no-opped on states it could
// not read — this test exists so that class cannot return unnoticed.
func TestGitSandbox_ApplyDeltaIsBaselineRelative(t *testing.T) {
	ctx := context.Background()
	sb, baseline, child := applyDeltaFixture(t)
	repo := sb.workDir()

	if err := sb.ApplyDelta(ctx, child); err != nil {
		t.Fatalf("ApplyDelta over dirty baseline: %v", err)
	}

	// The canary: the child's content physically landed.
	if got := gittest.ReadFile(t, repo, "branch.txt"); got != "from child" {
		t.Errorf("branch.txt = %q, want the child's content physically landed", got)
	}
	if !gittest.FileExists(t, repo, "created.txt") {
		t.Error("created.txt missing: ApplyDelta reported success but landed nothing (the silent no-op)")
	}
	if gittest.FileExists(t, repo, "doomed.txt") {
		t.Error("doomed.txt survived: the child's deletion must land")
	}
	// The baseline's own uncommitted content is untouched by the merge.
	if got := gittest.ReadFile(t, repo, "base.txt"); got != "dirty" {
		t.Errorf("base.txt = %q, want the baseline's own content left alone", got)
	}

	// Settle(Select) uses Apply on the same delta shape: the full tree,
	// including the baseline content, is restorable.
	if err := sb.Apply(ctx, child); err != nil {
		t.Fatalf("Apply of the child state: %v", err)
	}
	if got := gittest.ReadFile(t, repo, "base.txt"); got != "dirty" {
		t.Errorf("after Apply, base.txt = %q, want the baseline content restored with the delta", got)
	}
	_ = baseline
}

// TestGitSandbox_ApplyDeltaPreservesDisjointDrift covers the three-way
// property: parent drift on a path disjoint from the delta's changes
// survives the settle.
func TestGitSandbox_ApplyDeltaPreservesDisjointDrift(t *testing.T) {
	ctx := context.Background()
	sb, _, child := applyDeltaFixture(t)
	repo := sb.workDir()

	// Parent drift on a path the child never touched.
	gittest.WriteFile(t, repo, "base.txt", "parent version")

	if err := sb.ApplyDelta(ctx, child); err != nil {
		t.Fatalf("ApplyDelta with disjoint drift: %v", err)
	}
	if got := gittest.ReadFile(t, repo, "base.txt"); got != "parent version" {
		t.Errorf("base.txt = %q, want the parent's disjoint drift preserved", got)
	}
	if got := gittest.ReadFile(t, repo, "branch.txt"); got != "from child" {
		t.Errorf("branch.txt = %q, want the child's change landed", got)
	}
}

// TestGitSandbox_ApplyDeltaConflictRefusesAndLeavesUntouched covers the
// conflict contract: a delta whose changed path the parent also changed
// since the baseline fails, and the worktree keeps the parent's content.
func TestGitSandbox_ApplyDeltaConflictRefusesAndLeavesUntouched(t *testing.T) {
	ctx := context.Background()
	sb, _, child := applyDeltaFixture(t)
	repo := sb.workDir()

	gittest.WriteFile(t, repo, "branch.txt", "parent drift on the child's path")

	err := sb.ApplyDelta(ctx, child)
	if err == nil {
		t.Fatal("ApplyDelta on a conflicting path succeeded; want a typed refusal")
	}
	if got := gittest.ReadFile(t, repo, "branch.txt"); got != "parent drift on the child's path" {
		t.Errorf("branch.txt = %q after a refused delta; the parent's content must be untouched", got)
	}
	if !gittest.FileExists(t, repo, "doomed.txt") {
		t.Error("doomed.txt was removed by a refused delta; nothing may land on conflict")
	}
}

// TestGitSandbox_ApplyDeltaShapeRules pins the state-shape contract: the
// no-change delta is a no-op, the malformed shapes error (never a silent
// success), and the legacy clean capture stays a legitimate no-op.
func TestGitSandbox_ApplyDeltaShapeRules(t *testing.T) {
	ctx := context.Background()
	sb, baseline, child := applyDeltaFixture(t)
	repo := sb.workDir()

	// No-change delta: tree_sha == base_tree_sha.
	noChange := trace.WorkspaceState{Backend: "git", Revision: child.Revision,
		Data: map[string]any{"head_sha": child.Revision, "tree_sha": baseline.Data["tree_sha"], "base_tree_sha": baseline.Data["tree_sha"]}}
	if err := sb.ApplyDelta(ctx, noChange); err != nil {
		t.Fatalf("no-change delta: %v, want a no-op", err)
	}
	if got := gittest.ReadFile(t, repo, "branch.txt"); got != "original" {
		t.Errorf("branch.txt = %q after a no-change delta; the worktree must be untouched", got)
	}

	// Re-applying an already-landed delta is a no-op, not a conflict: the
	// merge resolves to the workspace's own content.
	if err := sb.ApplyDelta(ctx, child); err != nil {
		t.Fatalf("first landing: %v", err)
	}
	if err := sb.ApplyDelta(ctx, child); err != nil {
		t.Fatalf("second landing of the same delta: %v, want a no-op", err)
	}

	// Malformed: no recognizable key at all.
	malformed := trace.WorkspaceState{Backend: "git", Revision: child.Revision,
		Data: map[string]any{"head_sha": child.Revision}}
	if err := sb.ApplyDelta(ctx, malformed); err == nil {
		t.Error("delta with neither tree_sha nor stash_sha: want an error, got a silent success")
	}

	// Malformed: tree without a merge base.
	noBase := trace.WorkspaceState{Backend: "git", Revision: child.Revision,
		Data: map[string]any{"head_sha": child.Revision, "tree_sha": child.Data["tree_sha"]}}
	if err := sb.ApplyDelta(ctx, noBase); err == nil {
		t.Error("tree_sha without base_tree_sha: want an error, got a silent success")
	}

	// Legacy clean capture: the stash_sha key present but empty is a
	// legitimate no-op, not malformed.
	legacyClean := trace.WorkspaceState{Backend: "git", Revision: child.Revision,
		Data: map[string]any{"head_sha": child.Revision, "stash_sha": ""}}
	if err := sb.ApplyDelta(ctx, legacyClean); err != nil {
		t.Errorf("legacy clean delta: %v, want a no-op", err)
	}
}

// TestGitSandbox_ApplyDeltaLegacyStashPath covers a legacy delta recorded
// before tree_sha: it carries a minted stash_sha and must keep applying
// through the stash path.
func TestGitSandbox_ApplyDeltaLegacyStashPath(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	gittest.WriteFile(t, repo, "base.txt", "committed")
	commitFiles(t, repo, "base files")

	// Mint a legacy stash over dirty content. The dirt must be to a
	// tracked file: the plumbing `git stash create` ignores untracked
	// files, exactly as the scratch-index staging notes warn.
	gittest.WriteFile(t, repo, "base.txt", "legacy dirty")
	stash := gittest.MustGit(t, repo, "stash", "create")
	if stash == "" {
		t.Fatal("precondition: git stash create minted nothing for a dirty tree")
	}

	sb := NewLocalGitSandbox(repo)
	delta := trace.WorkspaceState{Backend: "git", Revision: gittest.MustHead(t, repo),
		Data: map[string]any{"head_sha": gittest.MustHead(t, repo), "stash_sha": stash}}

	// Reset to clean, then apply the legacy delta: the dirty content lands.
	mustGit(t, repo, "reset", "--hard")
	mustGit(t, repo, "clean", "-fd")
	if err := sb.ApplyDelta(ctx, delta); err != nil {
		t.Fatalf("ApplyDelta (legacy stash): %v", err)
	}
	if got := gittest.ReadFile(t, repo, "base.txt"); got != "legacy dirty" {
		t.Errorf("base.txt = %q, want the legacy stash's content landed", got)
	}
}

// TestGitSandbox_AnnotateDeltaBase pins Seal's seal-time resolution: the
// baseline's tree identity is recorded on the delta while the baseline's
// objects are certainly reachable — new-style baselines directly, a legacy
// stash baseline through the minted commit, a legacy clean baseline through
// HEAD's tree.
func TestGitSandbox_AnnotateDeltaBase(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	gittest.WriteFile(t, repo, "base.txt", "committed")
	commitFiles(t, repo, "base files")
	sb := NewLocalGitSandbox(repo)

	headTree := gittest.MustGit(t, repo, "rev-parse", "HEAD^{tree}")

	// New-style baseline: the tree is recorded directly.
	gittest.WriteFile(t, repo, "base.txt", "dirty")
	baseline, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture baseline: %v", err)
	}
	delta, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture delta: %v", err)
	}
	if err := sb.AnnotateDeltaBase(ctx, &delta, baseline); err != nil {
		t.Fatalf("AnnotateDeltaBase (new-style): %v", err)
	}
	if got, _ := delta.Data["base_tree_sha"].(string); got != baseline.Data["tree_sha"] {
		t.Errorf("base_tree_sha = %q, want the baseline's tree %v", got, baseline.Data["tree_sha"])
	}

	// Legacy dirty baseline: the minted stash's tree. The dirt is to a
	// tracked file — plumbing `git stash create` ignores untracked files.
	mustGit(t, repo, "reset", "--hard")
	gittest.WriteFile(t, repo, "base.txt", "legacy dirty")
	legacyStash := gittest.MustGit(t, repo, "stash", "create")
	stashTree := gittest.MustGit(t, repo, "rev-parse", legacyStash+"^{tree}")
	legacyDirty := trace.WorkspaceState{Backend: "git", Revision: gittest.MustHead(t, repo),
		Data: map[string]any{"head_sha": gittest.MustHead(t, repo), "stash_sha": legacyStash}}
	delta, err = sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture delta (legacy): %v", err)
	}
	if err := sb.AnnotateDeltaBase(ctx, &delta, legacyDirty); err != nil {
		t.Fatalf("AnnotateDeltaBase (legacy dirty): %v", err)
	}
	if got, _ := delta.Data["base_tree_sha"].(string); got != stashTree {
		t.Errorf("base_tree_sha = %q, want the stash's tree %s", got, stashTree)
	}

	// Legacy clean baseline: HEAD's tree — the same content, no stash minted.
	legacyClean := trace.WorkspaceState{Backend: "git", Revision: gittest.MustHead(t, repo),
		Data: map[string]any{"head_sha": gittest.MustHead(t, repo), "stash_sha": ""}}
	delta, err = sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture delta (legacy clean): %v", err)
	}
	if err := sb.AnnotateDeltaBase(ctx, &delta, legacyClean); err != nil {
		t.Fatalf("AnnotateDeltaBase (legacy clean): %v", err)
	}
	if got, _ := delta.Data["base_tree_sha"].(string); got != headTree {
		t.Errorf("base_tree_sha = %q, want HEAD's tree %s for a legacy clean baseline", got, headTree)
	}

	// Malformed inputs refuse rather than record a wrong base.
	if err := sb.AnnotateDeltaBase(ctx, nil, baseline); err == nil {
		t.Error("nil delta: want an error")
	}
	foreign := trace.WorkspaceState{Backend: "containerd", Data: map[string]any{}}
	if err := sb.AnnotateDeltaBase(ctx, &delta, foreign); err == nil {
		t.Error("non-git baseline: want an error")
	}
}

// TestGitSandbox_AnnotateDeltaBaseLegacyStashPruned covers the migration
// path's failure mode: a scope forked from a pre-plan-06 dirty baseline
// whose minted stash commit has since been pruned. The annotation must
// return an actionable error naming the stash — a fork over a dirty
// baseline cannot settle once its stash is gone — not a raw rev-parse
// failure, and never a silently wrong base.
func TestGitSandbox_AnnotateDeltaBaseLegacyStashPruned(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	gittest.WriteFile(t, repo, "base.txt", "committed")
	commitFiles(t, repo, "base files")

	gittest.WriteFile(t, repo, "base.txt", "legacy dirty")
	stash := gittest.MustGit(t, repo, "stash", "create")
	if stash == "" {
		t.Fatal("precondition: git stash create minted nothing for a dirty tree")
	}
	mustGit(t, repo, "reset", "--hard")
	mustGit(t, repo, "gc", "--prune=now")

	// The pruned stash is genuinely unreadable now.
	if out, err := gittest.Run(t, repo, "rev-parse", stash+"^{tree}"); err == nil {
		t.Fatalf("precondition: the stash commit survived gc (rev-parse returned %q); the fixture cannot exercise the pruned path", out)
	}

	sb := NewLocalGitSandbox(repo)
	delta, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture delta: %v", err)
	}
	legacyDirty := trace.WorkspaceState{Backend: "git", Revision: gittest.MustHead(t, repo),
		Data: map[string]any{"head_sha": gittest.MustHead(t, repo), "stash_sha": stash}}

	err = sb.AnnotateDeltaBase(ctx, &delta, legacyDirty)
	if err == nil {
		t.Fatal("annotation over a pruned legacy stash succeeded; want an error — a wrong base would mis-merge")
	}
	if !strings.Contains(err.Error(), stash) {
		t.Errorf("error does not name the pruned stash %s: %v", stash, err)
	}
	if !strings.Contains(err.Error(), "garbage collection") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// TestGitSandbox_AnnotateDeltaBasePinsTrees pins the anti-GC contract: the
// baseline and delta trees recorded at seal survive an explicit prune, so a
// gc between Seal and Settle cannot reclaim the objects a settlement needs.
func TestGitSandbox_AnnotateDeltaBasePinsTrees(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	gittest.WriteFile(t, repo, "base.txt", "committed")
	commitFiles(t, repo, "base files")

	sb := NewLocalGitSandbox(repo)
	gittest.WriteFile(t, repo, "base.txt", "dirty")
	baseline, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture baseline: %v", err)
	}
	gittest.WriteFile(t, repo, "branch.txt", "child")
	delta, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture delta: %v", err)
	}

	if err := sb.AnnotateDeltaBase(ctx, &delta, baseline); err != nil {
		t.Fatalf("AnnotateDeltaBase: %v", err)
	}
	mustGit(t, repo, "gc", "--prune=now")

	// Both trees are still readable, and the delta still applies after
	// the prune.
	base, _ := delta.Data["base_tree_sha"].(string)
	if _, err := gittest.Run(t, repo, "rev-parse", base+"^{tree}"); err != nil {
		t.Errorf("baseline tree %s did not survive gc: %v", base, err)
	}
	if tree, _ := delta.Data["tree_sha"].(string); tree != "" {
		if _, err := gittest.Run(t, repo, "rev-parse", tree); err != nil {
			t.Errorf("delta tree %s did not survive gc: %v", tree, err)
		}
	}
	mustGit(t, repo, "reset", "--hard")
	mustGit(t, repo, "clean", "-fd")
	gittest.WriteFile(t, repo, "base.txt", "dirty")
	if err := sb.ApplyDelta(ctx, delta); err != nil {
		t.Fatalf("ApplyDelta after gc: %v", err)
	}
	if got := gittest.ReadFile(t, repo, "branch.txt"); got != "child" {
		t.Errorf("branch.txt = %q after a post-gc settle; the pinned delta must still land", got)
	}
}

// TestGitSandbox_PinTreeIdempotentWhenRefUnwritable pins the concurrent-seal
// fallback deterministically: when the update-ref cannot proceed — here a
// read-only ref namespace, the same failure surface a sibling seal holding
// the ref lock produces — an existing ref that already holds the right tree
// is success, not a spurious Seal failure. The race itself is
// scheduler-dependent, so the test forces the failure branch; chmod cannot
// make the ref unwritable on Windows, so it is skipped there.
func TestGitSandbox_PinTreeIdempotentWhenRefUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod cannot make the ref unwritable on Windows")
	}
	ctx := context.Background()
	repo := newTestRepo(t)
	gittest.WriteFile(t, repo, "base.txt", "committed")
	commitFiles(t, repo, "base files")
	sb := NewLocalGitSandbox(repo)

	baseline, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture baseline: %v", err)
	}
	tree, _ := baseline.Data["tree_sha"].(string)
	if tree == "" {
		t.Fatal("capture recorded no tree_sha")
	}

	// Pin once, then make the ref namespace unwritable so the next
	// update-ref fails.
	if err := sb.pinTree(ctx, tree); err != nil {
		t.Fatalf("pinTree (first): %v", err)
	}
	refPath := filepath.Join(repo, ".git", "refs", "shepherd", "states", tree)
	if _, err := os.Stat(refPath); err != nil {
		t.Fatalf("the pin's ref file %s: %v", refPath, err)
	}
	dir := filepath.Dir(refPath)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod ref dir: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	if err := os.Chmod(refPath, 0o444); err != nil {
		t.Fatalf("chmod ref file: %v", err)
	}

	// Sanity: an update-ref really cannot proceed now.
	if _, err := gittest.Run(t, repo, "update-ref", "refs/shepherd/states/"+tree, tree); err == nil {
		t.Skip("update-ref succeeded despite the unwritable ref; the environment runs privileged and cannot exercise the fallback")
	}

	if err := sb.pinTree(ctx, tree); err != nil {
		t.Errorf("pinTree with a correct existing ref failed: %v; a sibling's identical pin must be success", err)
	}
}
