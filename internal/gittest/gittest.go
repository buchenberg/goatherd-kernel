// Package gittest provides git fixtures shared by the sandbox component's
// tests and the runtime layer's tests: repo creation isolated from the
// developer's global git config, and a bounded git runner for setting up
// test state.
package gittest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitTestTimeout bounds each git subprocess started by this package.
const gitTestTimeout = 30 * time.Second

// RequireGit skips the test when the git binary is unavailable, producing a
// clear skip instead of a confusing failure mid-test.
func RequireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

// Run executes a git command in dir and fails the test on error.
func MustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(dir, args...)
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return out
}

// NewTestRepo creates a temp git repo with an initial commit, isolated from
// the developer's global and system git config (gpgsign, hooksPath, commit
// templates, etc.) so a host-machine setting can't break the tests.
func NewTestRepo(t *testing.T) string {
	t.Helper()
	RequireGit(t)

	dir := t.TempDir()

	// Point git at empty global/system config so inherited settings
	// (commit.gpgsign=true, core.hooksPath, commit.template, ...) don't
	// make the test commit fail in ways that look like product defects.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "gitconfig-system"))

	MustGit(t, dir, "init")
	MustGit(t, dir, "config", "user.email", "test@test.com")
	MustGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	MustGit(t, dir, "add", "-A")
	MustGit(t, dir, "commit", "-m", "init")
	return dir
}

// run executes one git command, bounded by gitTestTimeout, with terminal
// prompts disabled so a credential prompt fails fast instead of hanging.
func run(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTestTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Run executes one git command in dir and returns its trimmed stdout. It
// fails the test if the command errors.
func Run(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	return run(dir, args...)
}

// HeadSHA returns the current HEAD sha of the repo at dir.
func HeadSHA(t *testing.T, dir string) (string, error) {
	t.Helper()
	return run(dir, "rev-parse", "HEAD")
}

// FileExists reports whether a path exists under dir.
func FileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// WriteFile writes content to dir/name, creating parent directories.
func WriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// WorktreePaths lists the repo's worktree paths, porcelain format.
func WorktreePaths(t *testing.T, repo string) []string {
	t.Helper()
	out, err := run(repo, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, rest)
		}
	}
	return paths
}

// ReadFile reads and returns the contents of dir/name.
func ReadFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// SamePath reports whether a and b resolve to the same path.
func SamePath(t *testing.T, a, b string) bool {
	t.Helper()
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// StagedFiles returns the names staged in the index, one per line.
func StagedFiles(t *testing.T, dir string) string {
	t.Helper()
	out, err := run(dir, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	return out
}

// MustHead returns the current HEAD sha, failing the test on error.
func MustHead(t *testing.T, dir string) string {
	t.Helper()
	sha, err := run(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return sha
}
