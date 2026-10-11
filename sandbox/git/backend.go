// Package git is the core module's git sandbox backend: the in-place
// materializer over the caller's repository and the detached-worktree
// isolated mode, both addressable through the sandbox package's registry
// as "git" (the backend self-registers on import). The Sandbox contract,
// the capability model, and OpenSandbox live in the parent sandbox
// package; this package owns git state only.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sbx "github.com/buchenberg/shepherd-kernel-go/sandbox"
	trace "github.com/buchenberg/shepherd-kernel-go/trace"
)

// gitTimeout bounds each git subprocess. A hung git process (e.g. waiting on a
// credential prompt or a dead NFS mount) must not block the caller
// indefinitely, even when the caller passes context.Background().
const gitTimeout = 30 * time.Second

// gitRunner executes git commands in a directory with a bounded timeout and
// terminal prompts disabled, returning trimmed stdout.
type gitRunner struct {
	dir string
}

// run executes a git command. The caller's context is honored for its values but
// NOT for cancellation: git is the substrate that keeps the workspace
// consistent, and aborting mid-operation leaves exactly the damage these
// methods exist to prevent (a staged index after `git add -A`, a half-applied
// reset, an abandoned worktree). Each subprocess is instead bounded by
// gitTimeout, which is the ceiling that actually protects the caller.
//
// Remote backends are free to honor cancellation; this backend deliberately
// does not. Terminal prompts are disabled so a command that would ask for
// credentials fails fast instead of hanging.
func (g *gitRunner) run(ctx context.Context, args ...string) (string, error) {
	return g.runWithEnv(ctx, nil, args...)
}

// runWithEnv is run with extra environment variables appended. It is how the
// scratch-index operations redirect git's index (GIT_INDEX_FILE) without ever
// touching the caller's.
func (g *gitRunner) runWithEnv(ctx context.Context, env []string, args ...string) (string, error) {
	out, _, err := g.runExit(ctx, env, args...)
	return out, err
}

// runExit is runWithEnv that also reports the process exit code. One git
// command uses a non-zero exit as protocol rather than failure —
// `git merge-tree --write-tree` exits 1 for merge conflicts, distinct from
// the >1 of a real error — so its caller must see the code. exitCode is -1
// when the process could not run at all.
func (g *gitRunner) runExit(ctx context.Context, env []string, args ...string) (string, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		code := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return "", code, fmt.Errorf("git %s: %w (stderr: %s)",
			strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), 0, nil
}

// indexEnv returns the environment that redirects git at a scratch index. It is
// defined once so the variable name and format cannot drift between the
// creation site and the commands that consume it.
func indexEnv(path string) []string {
	return []string{"GIT_INDEX_FILE=" + path}
}

// stagedIndex stages the full working tree — including untracked files — into a
// scratch index inside a private temp directory. The caller's real index is
// never touched, so Capture and Diff stay non-mutating even when the caller had
// staged changes before the call. The 0700 directory also keeps the staged
// snapshot unreadable to other local users; a bare file in a shared temp dir
// would not, because git rewrites the index through a 0644 lock-and-rename.
//
// The scratch index is seeded from the repository's real index when one exists,
// so git's stat cache survives and `git add -A` only hashes changed files.
// Otherwise it falls back to `git read-tree HEAD`, which also stages deletions
// correctly (an empty index would treat every tracked file as newly added).
// `git stash create --include-untracked` is NOT an alternative: that flag is
// silently ignored by the plumbing `git stash create` command (only
// `git stash push`/`save` honour it).
//
// Returns the scratch index path and a cleanup func the caller must always call.
func (g *gitRunner) stagedIndex(ctx context.Context) (string, func(), error) {
	dir, err := os.MkdirTemp("", "shepherd-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("create scratch index dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "index")

	env := indexEnv(path)
	if !g.seedScratchIndex(ctx, path) {
		if _, err := g.runWithEnv(ctx, env, "read-tree", "HEAD"); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("seed scratch index: %w", err)
		}
	}
	if _, err := g.runWithEnv(ctx, env, "add", "-A"); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("stage into scratch index: %w", err)
	}
	return path, cleanup, nil
}

// seedScratchIndex copies the repository's real index to dst so the stat cache
// (and therefore `git add -A`'s ability to skip unchanged files) is preserved.
// It reports false when no readable index exists, leaving the caller to seed via
// `git read-tree HEAD`.
func (g *gitRunner) seedScratchIndex(ctx context.Context, dst string) bool {
	out, err := g.run(ctx, "rev-parse", "--git-path", "index")
	if err != nil || out == "" {
		return false
	}
	src := out
	if !filepath.IsAbs(src) {
		src = filepath.Join(g.dir, src)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	return os.WriteFile(dst, data, 0o600) == nil
}

// writeTree runs `git write-tree` against a scratch index and returns the
// tree SHA it spells — a content-derived identity: two indexes holding the
// same content write the same tree, with no timestamp or per-capture
// uniqueness in it. This is what makes captures of identical trees
// digest-stable (plan 06, U1a).
func (g *gitRunner) writeTree(ctx context.Context, index string) (string, error) {
	return g.runWithEnv(ctx, indexEnv(index), "write-tree")
}

// headSHA returns the current HEAD commit SHA.
func (g *gitRunner) headSHA(ctx context.Context) (string, error) {
	return g.run(ctx, "rev-parse", "HEAD")
}

// resetHardTo runs `git reset --hard <sha>`.
func (g *gitRunner) resetHardTo(ctx context.Context, sha string) error {
	_, err := g.run(ctx, "reset", "--hard", sha)
	return err
}

// cleanFiles runs `git clean -fd` — removes untracked files and directories.
func (g *gitRunner) cleanFiles(ctx context.Context) error {
	_, err := g.run(ctx, "clean", "-fd")
	return err
}

// stashApply runs `git stash apply <sha>`.
func (g *gitRunner) stashApply(ctx context.Context, sha string) error {
	_, err := g.run(ctx, "stash", "apply", sha)
	return err
}

// isRepo reports whether dir is inside a git working tree.
func (g *gitRunner) isRepo(ctx context.Context) bool {
	_, err := g.run(ctx, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// GitSandbox materializes a git working tree on the host filesystem.
//
// It has two modes, distinguished only by lifecycle:
//
//   - In-place (NewLocalGitSandbox): operates directly on the caller's
//     repository. It is a materializer, not an isolation boundary, so
//     Isolated is false and trace.Containment is trace.ContainUncontained. Create
//     validates that the path is a repository and Destroy is a no-op — a
//     sandbox must never delete a user's repository.
//
//   - Worktree (NewWorktreeSandbox): allocates a detached `git worktree` for
//     the scope. Create and Destroy manage real resources, Isolated is true,
//     and trace.Containment is trace.ContainBuffered: effects are held in a separate
//     checkout and only graduate when the caller applies that state to the
//     parent's in-place sandbox.
//
// Capture, Apply, and Diff are identical across modes and are shared: git
// worktrees share the object store, so a tree captured in a worktree can be
// applied or merged in the main tree of the same repository.
//
// Exec, ReadFile, and WriteFile return sbx.ErrUnsupported. Agent processes are the
// caller's concern; this backend owns git state only.
type GitSandbox struct {
	repoPath     string
	worktreePath string
	// address is the durable workspace address (plan 06, U2), normalized
	// to an absolute path at construction: the worktree path for an
	// isolated worktree, the repository path otherwise.
	address string
}

var _ sbx.Sandbox = (*GitSandbox)(nil)
var _ sbx.DeltaApplier = (*GitSandbox)(nil)
var _ sbx.DeltaBaseAnnotator = (*GitSandbox)(nil)
var _ sbx.Addressed = (*GitSandbox)(nil)

// absAddress normalizes a workspace path to an absolute one. A raw
// relative path persisted in a trace and replayed after a restart under a
// different working directory would address the wrong location, so the
// address is resolved eagerly, at construction — not at read time.
func absAddress(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// NewLocalGitSandbox returns an in-place sandbox over repoPath.
func NewLocalGitSandbox(repoPath string) *GitSandbox {
	return &GitSandbox{repoPath: repoPath, address: absAddress(repoPath)}
}

// NewWorktreeSandbox returns an isolated sandbox that checks out repoPath at a
// detached worktree in worktreePath. repoPath remains the source of truth for
// git administrative commands; worktreePath is where workspace operations run.
// worktreePath may be relative; the recorded address is normalized to an
// absolute path.
func NewWorktreeSandbox(repoPath, worktreePath string) *GitSandbox {
	return &GitSandbox{repoPath: repoPath, worktreePath: worktreePath, address: absAddress(worktreePath)}
}

// Address implements sbx.Addressed: the durable workspace address, a
// normalized absolute path — the worktree path when isolated, the
// repository path otherwise. Recovery records it in the fork payload and
// hands it back to the host's resolver, so an isolated child's substrate
// is re-adoptable from the trace alone.
func (g *GitSandbox) Address() string { return g.address }

// Backend identifies the backend.
func (g *GitSandbox) Backend() string { return "git" }

// isWorktree reports whether this sandbox owns a detached worktree.
func (g *GitSandbox) isWorktree() bool { return g.worktreePath != "" }

// workDir is the directory workspace operations run in.
func (g *GitSandbox) workDir() string {
	if g.isWorktree() {
		return g.worktreePath
	}
	return g.repoPath
}

// repo returns a runner bound to the repository (for administrative commands).
func (g *GitSandbox) repo() *gitRunner { return &gitRunner{dir: g.repoPath} }

// work returns a runner bound to the active workspace directory.
func (g *GitSandbox) work() *gitRunner { return &gitRunner{dir: g.workDir()} }

// Capabilities reports the git backend's support matrix.
func (g *GitSandbox) Capabilities() sbx.SandboxCapabilities {
	if g.isWorktree() {
		return sbx.SandboxCapabilities{
			Lifecycle:   true,
			Exec:        false,
			FileIO:      false,
			Diff:        true,
			Isolated:    true,
			Containment: trace.ContainBuffered,
		}
	}
	return sbx.SandboxCapabilities{
		Lifecycle:   false,
		Exec:        false,
		FileIO:      false,
		Diff:        true,
		Isolated:    false,
		Containment: trace.ContainUncontained,
	}
}

// Create validates the repository, and in worktree mode allocates the worktree.
//
// The worktree is created detached (`--detach`) so sibling variants cannot
// contend over a branch. Stale administrative entries from a previous crash are
// pruned first, which is a no-op when there is nothing to prune.
func (g *GitSandbox) Create(ctx context.Context, spec sbx.SandboxSpec) error {
	repo := g.repo()
	if !repo.isRepo(ctx) {
		return fmt.Errorf("git sandbox: %s is not a git repository", g.repoPath)
	}

	if !g.isWorktree() {
		return nil
	}

	if g.worktreePath == g.repoPath {
		return fmt.Errorf("git sandbox: worktree path must differ from the repository path")
	}

	if _, err := repo.run(ctx, "worktree", "prune"); err != nil {
		return fmt.Errorf("git sandbox: worktree prune: %w", err)
	}

	// Refuse to clobber an existing non-empty directory: it may be a live
	// worktree belonging to another scope, or unrelated user data.
	if entries, err := os.ReadDir(g.worktreePath); err == nil {
		if len(entries) > 0 {
			return fmt.Errorf("git sandbox: worktree path %s already exists and is not empty", g.worktreePath)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("git sandbox: inspect worktree path %s: %w", g.worktreePath, err)
	}

	if err := os.MkdirAll(filepath.Dir(g.worktreePath), 0o755); err != nil {
		return fmt.Errorf("git sandbox: create worktree parent: %w", err)
	}

	head, err := repo.headSHA(ctx)
	if err != nil {
		return fmt.Errorf("git sandbox: resolve HEAD: %w", err)
	}

	if _, err := repo.run(ctx, "worktree", "add", "--detach", g.worktreePath, head); err != nil {
		return fmt.Errorf("git sandbox: worktree add: %w", err)
	}
	return nil
}

// Destroy removes the worktree this sandbox allocated. In-place mode is a no-op
// so a scope can never delete the user's repository.
//
// On Windows a leftover handle (editor, shell, indexer, antivirus) can make
// `git worktree remove` fail; the directory is then removed directly and the
// administrative entry pruned so the worktree does not become unusable.
func (g *GitSandbox) Destroy(ctx context.Context) error {
	if !g.isWorktree() {
		return nil
	}

	repo := g.repo()
	if _, err := repo.run(ctx, "worktree", "remove", "--force", g.worktreePath); err != nil {
		// "git worktree remove" fails harmlessly when the worktree was
		// never created or is already gone, and os.RemoveAll is a no-op
		// then — an idempotent Destroy must not warn about it. Only skip
		// on the path-not-exist condition; any other stat failure (say
		// EACCES on the parent) must fall through to the removal attempt
		// rather than silently reporting success over a leftover tree.
		if _, statErr := os.Lstat(g.worktreePath); !os.IsNotExist(statErr) {
			if rmErr := os.RemoveAll(g.worktreePath); rmErr != nil {
				return fmt.Errorf("git sandbox: remove worktree %s: %w (git worktree remove also failed: %v)",
					g.worktreePath, rmErr, err)
			}
			if statErr != nil {
				return fmt.Errorf("git sandbox: inspect worktree %s: %w (git worktree remove failed: %v)",
					g.worktreePath, statErr, err)
			}
			slog.Warn("git sandbox: git worktree remove failed, removed with os.RemoveAll fallback",
				"worktree", g.worktreePath, "err", err)
		}
	}

	// Use a fresh context: cleanup must not be abandoned because the
	// caller's context expired mid-run.
	if _, err := repo.run(context.WithoutCancel(ctx), "worktree", "prune"); err != nil {
		return fmt.Errorf("git sandbox: prune after remove: %w", err)
	}
	return nil
}

// Capture records the current workspace state as a reusable trace.WorkspaceState.
//
// The working tree — including untracked files — is staged into a scratch index
// (never the caller's) and the index's tree is written out with
// `git write-tree`. The recorded tree_sha is the content itself, not a
// per-capture artifact: two captures of an identical tree at the same HEAD —
// clean or dirty — digest equal, which is the digest-stability contract on
// Sandbox.Capture and what Settle(Select)'s precondition needs (plan 06,
// U1a). A capture after the timeline moved (a commit, even one whose tree
// matches the prior content) records a different state by design. Nothing is
// committed and the working tree is unchanged, so a caller that had staged
// changes keeps them staged.
func (g *GitSandbox) Capture(ctx context.Context) (trace.WorkspaceState, error) {
	w := g.work()
	if !w.isRepo(ctx) {
		return trace.WorkspaceState{}, fmt.Errorf("git sandbox: %s is not a git working tree", g.workDir())
	}

	index, cleanupIndex, err := w.stagedIndex(ctx)
	if err != nil {
		return trace.WorkspaceState{}, fmt.Errorf("capture: %w", err)
	}
	defer cleanupIndex()

	treeSHA, err := w.writeTree(ctx, index)
	if err != nil {
		return trace.WorkspaceState{}, fmt.Errorf("capture: git write-tree: %w", err)
	}

	headSHA, err := w.headSHA(ctx)
	if err != nil {
		return trace.WorkspaceState{}, fmt.Errorf("capture: git rev-parse HEAD: %w", err)
	}

	return trace.WorkspaceState{
		Backend:  "git",
		Revision: headSHA,
		Data: map[string]any{
			"head_sha": headSHA,
			"tree_sha": treeSHA,
		},
	}, nil
}

// Apply resets the workspace to a captured state. The state is reusable: Apply
// consumes nothing, so the same value may be applied repeatedly (this is what
// fork-and-choose needs).
//
// Tree-identified states (tree_sha) are restored exactly, including
// deletions. Legacy states recorded before tree_sha still carry stash_sha and
// keep the stash-apply path, so old traces keep applying.
func (g *GitSandbox) Apply(ctx context.Context, ws trace.WorkspaceState) error {
	if ws.Backend != "git" {
		return fmt.Errorf("git sandbox: cannot apply state from backend %q", ws.Backend)
	}
	headSHA, _ := ws.Data["head_sha"].(string)
	if headSHA == "" {
		return fmt.Errorf("git sandbox: workspace state is missing head_sha")
	}
	if treeSHA, _ := ws.Data["tree_sha"].(string); treeSHA != "" {
		return g.applyTree(ctx, headSHA, treeSHA)
	}
	if _, ok := ws.Data["stash_sha"]; ok {
		stashSHA, _ := ws.Data["stash_sha"].(string)
		return g.applyState(ctx, headSHA, stashSHA)
	}
	return fmt.Errorf("git sandbox: workspace state has neither tree_sha nor stash_sha")
}

// applyState is the legacy destructive reset sequence for states recorded
// before tree_sha. It resets to the captured HEAD (rolling back commits made
// after the capture), removes untracked files, then re-applies the captured
// uncommitted changes from the minted stash commit.
func (g *GitSandbox) applyState(ctx context.Context, headSHA, stashSHA string) error {
	w := g.work()

	if err := w.resetHardTo(ctx, headSHA); err != nil {
		return fmt.Errorf("apply: git reset --hard %s: %w", headSHA, err)
	}

	if err := w.cleanFiles(ctx); err != nil {
		return fmt.Errorf("apply: git clean -fd: %w", err)
	}

	if stashSHA != "" {
		if err := w.stashApply(ctx, stashSHA); err != nil {
			return fmt.Errorf("apply: git stash apply %s: %w", stashSHA, err)
		}
	}
	return nil
}

// applyTree is the destructive reset sequence for tree-identified states:
// reset to the captured HEAD, remove untracked files, then restore the
// captured tree with `git read-tree --reset -u`. The -u is load-bearing: it
// updates the worktree to the new index AND removes worktree entries the
// tree does not contain, so a capture that held a deletion of a HEAD file
// lands as a deletion. Writing the tree's entries alone (checkout-index,
// git restore --source) would leave the deleted file behind and Apply
// would restore a superset of the captured state.
func (g *GitSandbox) applyTree(ctx context.Context, headSHA, treeSHA string) error {
	w := g.work()

	if err := w.resetHardTo(ctx, headSHA); err != nil {
		return fmt.Errorf("apply: git reset --hard %s: %w", headSHA, err)
	}

	if err := w.cleanFiles(ctx); err != nil {
		return fmt.Errorf("apply: git clean -fd: %w", err)
	}

	if _, err := w.run(ctx, "read-tree", "--reset", "-u", treeSHA); err != nil {
		return fmt.Errorf("apply: git read-tree --reset -u %s: %w", treeSHA, err)
	}
	return nil
}

// Diff returns the unified diff and changed file paths between ws and the
// current workspace.
//
// Untracked files are made visible through a scratch index, so Diff is
// non-mutating: it never touches the caller's index, matching Capture.
func (g *GitSandbox) Diff(ctx context.Context, ws trace.WorkspaceState, maxLines int) (string, []string, error) {
	if ws.Backend != "git" {
		return "", nil, fmt.Errorf("git sandbox: cannot diff state from backend %q", ws.Backend)
	}
	if ws.Revision == "" {
		return "", nil, fmt.Errorf("git sandbox: workspace state is missing a revision")
	}

	w := g.work()
	if !w.isRepo(ctx) {
		return "", nil, fmt.Errorf("git sandbox: %s is not a git working tree", g.workDir())
	}

	// The base is the captured full state, not its HEAD: a dirty baseline's
	// own uncommitted content must appear on neither side of a fork's diffs
	// (plan 06, U1b). tree_sha is the full state; Revision (the captured
	// HEAD) is the legacy fallback for states recorded before it.
	base := ws.Revision
	if treeSHA, _ := ws.Data["tree_sha"].(string); treeSHA != "" {
		base = treeSHA
	}

	index, cleanupIndex, err := w.stagedIndex(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("diff: %w", err)
	}
	defer cleanupIndex()

	env := indexEnv(index)
	names, err := w.runWithEnv(ctx, env, "diff", "--cached", "--name-only", base)
	if err != nil {
		return "", nil, fmt.Errorf("diff: git diff --name-only: %w", err)
	}
	var files []string
	if names != "" {
		files = strings.Split(names, "\n")
	}

	full, err := w.runWithEnv(ctx, env, "diff", "--cached", base)
	if err != nil {
		return "", nil, fmt.Errorf("diff: git diff: %w", err)
	}

	if maxLines > 0 {
		lines := strings.Split(full, "\n")
		if len(lines) > maxLines {
			full = strings.Join(lines[:maxLines], "\n") + "\n...[diff truncated]"
		}
	}

	return full, files, nil
}

// Exec is not implemented: the git backend owns git state, not processes.
func (g *GitSandbox) Exec(context.Context, sbx.ExecRequest) (sbx.ExecResult, error) {
	return sbx.ExecResult{}, sbx.ErrUnsupported
}

// ApplyDelta merges a captured state's changes onto the current workspace
// without resetting it — the three-way path for settlement's apply verb.
//
// The delta is baseline-relative (plan 06, U1c): its data carries
// base_tree_sha, the fork baseline's tree recorded by Seal, and tree_sha,
// the sealed output's full tree. The merge base is the baseline, so the
// baseline's own uncommitted content appears on neither side and only the
// branch's actual changes land. git worktrees share the object store, so
// trees captured in one worktree merge in any checkout of the same
// repository.
//
// Shape rules, so no implementation silently reintroduces the historical
// no-op bug (an empty stash_sha was treated as "clean capture, nothing to
// apply" even when the caller had handed it a state it could not read):
//
//   - tree_sha == base_tree_sha: the output changed nothing; a no-op.
//   - tree_sha without base_tree_sha: malformed — the merge has no base,
//     and falling back to HEAD would read the baseline's own content as
//     branch changes (U1b again). An error.
//   - neither key present: malformed. An error, never a silent success.
//   - a legacy stash_sha-carrying delta (recorded before tree_sha) keeps
//     the stash-apply path; an empty stash is a legacy clean capture, a
//     legitimate no-op — distinguished by key presence, not value.
//
// A conflicting delta fails and leaves the workspace untouched.
func (g *GitSandbox) ApplyDelta(ctx context.Context, delta trace.WorkspaceState) error {
	if delta.Backend != "git" {
		return fmt.Errorf("git sandbox: cannot apply delta from backend %q", delta.Backend)
	}
	treeSHA, _ := delta.Data["tree_sha"].(string)
	baseSHA, _ := delta.Data["base_tree_sha"].(string)
	if treeSHA != "" {
		if baseSHA == "" {
			return fmt.Errorf("git sandbox: apply delta: tree_sha without base_tree_sha: the delta has no merge base")
		}
		if treeSHA == baseSHA {
			return nil
		}
		return g.mergeTree(ctx, baseSHA, treeSHA)
	}
	if stashSHA, ok := delta.Data["stash_sha"].(string); ok {
		if stashSHA == "" {
			return nil // legacy clean capture: nothing to apply
		}
		w := g.work()
		if !w.isRepo(ctx) {
			return fmt.Errorf("git sandbox: %s is not a git working tree", g.workDir())
		}
		if err := w.stashApply(ctx, stashSHA); err != nil {
			return fmt.Errorf("git sandbox: apply delta %s: %w", stashSHA, err)
		}
		return nil
	}
	return fmt.Errorf("git sandbox: apply delta: state has neither tree_sha nor stash_sha")
}

// mergeTree lands a baseline-relative delta: a three-way merge whose base is
// the fork baseline's tree, whose theirs is the delta's tree, and whose ours
// is the current worktree (uncommitted content included, staged through the
// same scratch index Capture and Diff use). `git merge-tree --write-tree`
// computes the merged tree without touching any worktree, so a conflict
// fails before anything lands. The landing then materializes the merged
// tree through the scratch index (GIT_INDEX_FILE) with `read-tree --reset
// -u`: the caller's real index is untouched, while the worktree gains the
// merged content including removals — read-tree's -u removes entries the
// merge deleted, the same hazard Apply's restore closes.
func (g *GitSandbox) mergeTree(ctx context.Context, baseSHA, treeSHA string) error {
	w := g.work()
	if !w.isRepo(ctx) {
		return fmt.Errorf("git sandbox: %s is not a git working tree", g.workDir())
	}

	index, cleanupIndex, err := w.stagedIndex(ctx)
	if err != nil {
		return fmt.Errorf("apply delta: %w", err)
	}
	defer cleanupIndex()

	env := indexEnv(index)
	oursSHA, err := w.writeTree(ctx, index)
	if err != nil {
		return fmt.Errorf("apply delta: git write-tree: %w", err)
	}

	merged, code, err := w.runExit(ctx, nil, "merge-tree", "--write-tree", "--merge-base="+baseSHA, oursSHA, treeSHA)
	if err != nil {
		if code == 1 {
			return fmt.Errorf("git sandbox: apply delta: the delta conflicts with the current workspace; nothing was applied: %w", err)
		}
		return fmt.Errorf("git sandbox: apply delta: git merge-tree: %w", err)
	}
	mergedSHA := strings.SplitN(merged, "\n", 2)[0]
	if mergedSHA == "" {
		return fmt.Errorf("git sandbox: apply delta: git merge-tree wrote no tree")
	}
	if mergedSHA == oursSHA {
		// The merge resolved to the workspace's own content: the delta's
		// changes are already present. Nothing to land.
		return nil
	}

	if _, err := w.runWithEnv(ctx, env, "read-tree", "--reset", "-u", mergedSHA); err != nil {
		return fmt.Errorf("git sandbox: apply delta: land merged tree: %w", err)
	}
	return nil
}

// ReadFile is not implemented.
func (g *GitSandbox) ReadFile(context.Context, string) ([]byte, error) {
	return nil, sbx.ErrUnsupported
}

// WriteFile is not implemented.
func (g *GitSandbox) WriteFile(context.Context, string, []byte, fs.FileMode) error {
	return sbx.ErrUnsupported
}

// AnnotateDeltaBase implements sbx.DeltaBaseAnnotator: it resolves the fork
// baseline's tree identity, records it on a freshly captured delta as
// base_tree_sha, and pins both trees with refs (pinTree) so garbage
// collection cannot reclaim them between seal and settle.
//
// The baseline's own data decides the resolution: tree_sha for states
// recorded after plan 06; a legacy dirty baseline's stash_sha resolves the
// minted stash commit's tree; a legacy clean baseline (empty stash_sha —
// `git stash create` reports nothing for a clean tree) resolves the
// recorded HEAD's tree, which is the same content. A legacy dirty baseline
// whose minted stash has since been pruned fails here with an actionable
// error — the fork can no longer settle over its dirty baseline, at seal
// time rather than mid-settle.
func (g *GitSandbox) AnnotateDeltaBase(ctx context.Context, delta *trace.WorkspaceState, baseline trace.WorkspaceState) error {
	if delta == nil {
		return fmt.Errorf("git sandbox: annotate delta base: nil delta")
	}
	if delta.Backend != "git" || baseline.Backend != "git" {
		return fmt.Errorf("git sandbox: annotate delta base requires git states, got delta %q and baseline %q", delta.Backend, baseline.Backend)
	}
	w := g.work()
	if !w.isRepo(ctx) {
		return fmt.Errorf("git sandbox: %s is not a git working tree", g.workDir())
	}

	var baseSHA string
	if tree, _ := baseline.Data["tree_sha"].(string); tree != "" {
		baseSHA = tree
	} else if stash, _ := baseline.Data["stash_sha"].(string); stash != "" {
		out, err := w.run(ctx, "rev-parse", stash+"^{tree}")
		if err != nil {
			return fmt.Errorf("git sandbox: annotate delta base: the legacy baseline's minted stash %s is no longer readable (likely pruned by garbage collection); a fork over a pre-plan-06 dirty baseline cannot settle once its stash commit is gone: %w", stash, err)
		}
		baseSHA = out
	} else {
		head, _ := baseline.Data["head_sha"].(string)
		if head == "" {
			head = baseline.Revision
		}
		if head == "" {
			return fmt.Errorf("git sandbox: annotate delta base: baseline carries no tree, stash, or head identity")
		}
		out, err := w.run(ctx, "rev-parse", head+"^{tree}")
		if err != nil {
			return fmt.Errorf("git sandbox: annotate delta base: rev-parse %s^{tree}: %w", head, err)
		}
		baseSHA = out
	}

	// Both trees must survive until settle: pin them. The recorded SHAs
	// alone do not — git's reachability traversal cannot see the trace
	// store, and write-tree trees are otherwise unreferenced.
	if err := g.pinTree(ctx, baseSHA); err != nil {
		return fmt.Errorf("git sandbox: annotate delta base: pin the baseline tree: %w", err)
	}
	if tree, _ := delta.Data["tree_sha"].(string); tree != "" {
		if err := g.pinTree(ctx, tree); err != nil {
			return fmt.Errorf("git sandbox: annotate delta base: pin the delta tree: %w", err)
		}
	}

	if delta.Data == nil {
		delta.Data = map[string]any{}
	}
	delta.Data["base_tree_sha"] = baseSHA
	return nil
}

// stateRefPrefix is the ref namespace that anchors recorded state trees in
// git's reachability graph. Trees written by `git write-tree` are
// unreferenced: the trace store records their SHAs, but git's garbage
// collection traverses refs and the index, not SQLite — an explicit
// `git gc` between Seal and Settle could otherwise prune the very objects a
// settlement needs.
const stateRefPrefix = "refs/shepherd/states/"

// pinTree anchors a recorded state tree with a ref keyed by the tree's own
// SHA, so identical content shares one pin and re-pinning is idempotent.
// Pins live for the repository's life; a release lifecycle (unpinning
// settled and discarded outputs) is a recorded follow-up gap.
func (g *GitSandbox) pinTree(ctx context.Context, treeSHA string) error {
	w := g.work()
	if _, err := w.run(ctx, "update-ref", stateRefPrefix+treeSHA, treeSHA); err != nil {
		// Sibling seals can race on a shared pin: forks from the same
		// baseline resolve the identical tree, so concurrent Seals
		// update the same ref. The pin is idempotent and keyed by the
		// tree's own SHA — an existing ref with the right value is
		// success, whatever made the update fail.
		if got, rerr := w.run(ctx, "rev-parse", stateRefPrefix+treeSHA); rerr == nil && got == treeSHA {
			return nil
		}
		return fmt.Errorf("pin state tree %s: the tree is not readable (likely pruned by garbage collection) or the ref could not be updated: %w", treeSHA, err)
	}
	return nil
}

// --- OpenSandbox registration ---

// GitSandboxConfig is the git backend's OpenSandbox configuration. RepoPath
// is required; an empty WorktreePath opens the in-place materializer over
// the repository, a non-empty one opens a detached-worktree sandbox that
// Create provisions at that path.
type GitSandboxConfig struct {
	RepoPath     string
	WorktreePath string
}

func init() {
	sbx.RegisterSandbox("git", func(cfg any) (sbx.Sandbox, error) {
		c, ok := cfg.(GitSandboxConfig)
		if !ok {
			if cfg == nil {
				return nil, fmt.Errorf("git sandbox: OpenSandbox requires a GitSandboxConfig; nil is not one — a backend cannot be chosen by omission")
			}
			return nil, fmt.Errorf("git sandbox: OpenSandbox configuration is GitSandboxConfig, got %T", cfg)
		}
		if c.RepoPath == "" {
			return nil, fmt.Errorf("git sandbox: GitSandboxConfig.RepoPath is required")
		}
		if c.WorktreePath != "" {
			return NewWorktreeSandbox(c.RepoPath, c.WorktreePath), nil
		}
		return NewLocalGitSandbox(c.RepoPath), nil
	})
}
