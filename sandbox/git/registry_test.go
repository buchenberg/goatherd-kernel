// The git backend's registry integration: the backend self-registers on
// import, and OpenSandbox constructs it from GitSandboxConfig exactly as a
// host would. These live with the backend (not in the sandbox package) so
// the registry-mechanics tests stay independent of which backends happen to
// be linked in.
package git_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buchenberg/shepherd-kernel-go/internal/gittest"
	sbx "github.com/buchenberg/shepherd-kernel-go/sandbox"
	"github.com/buchenberg/shepherd-kernel-go/sandbox/git"
)

func TestGitBackendIsRegistered(t *testing.T) {
	backends := sbx.SandboxBackends()
	found := false
	for _, b := range backends {
		if b == "git" {
			found = true
		}
	}
	if !found {
		t.Fatalf("SandboxBackends() = %v, want the git backend", backends)
	}
}

func TestOpenSandbox_GitInPlace(t *testing.T) {
	repo := gittest.NewTestRepo(t)

	sb, err := sbx.OpenSandbox("git", git.GitSandboxConfig{RepoPath: repo})
	if err != nil {
		t.Fatalf("OpenSandbox (in-place): %v", err)
	}
	if sb.Backend() != "git" {
		t.Errorf("Backend() = %q, want git", sb.Backend())
	}
	caps := sb.Capabilities()
	if caps.Isolated || caps.Lifecycle {
		t.Errorf("in-place capabilities = %+v, want not isolated, no lifecycle", caps)
	}
	// In-place Create validates the repository and does not provision.
	if err := sb.Create(context.Background(), sbx.SandboxSpec{}); err != nil {
		t.Errorf("Create over the existing repo: %v", err)
	}
	if _, err := sb.Capture(context.Background()); err != nil {
		t.Errorf("Capture: %v", err)
	}
}

func TestOpenSandbox_GitWorktreeIsUnprovisioned(t *testing.T) {
	repo := gittest.NewTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	sb, err := sbx.OpenSandbox("git", git.GitSandboxConfig{RepoPath: repo, WorktreePath: wtPath})
	if err != nil {
		t.Fatalf("OpenSandbox (worktree): %v", err)
	}
	if !sb.Capabilities().Isolated {
		t.Error("worktree sandbox must report Isolated")
	}
	// OpenSandbox never provisions: the worktree does not exist until
	// Create, which is the lifecycle contract.
	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Errorf("OpenSandbox must not provision; worktree stat err = %v", statErr)
	}
	if err := sb.Create(context.Background(), sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create provisions the worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Errorf("provisioned worktree should hold the repo's files: %v", err)
	}
	if err := sb.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func TestOpenSandbox_GitConfigValidation(t *testing.T) {
	if _, err := sbx.OpenSandbox("git", nil); err == nil || !strings.Contains(err.Error(), "GitSandboxConfig") {
		t.Errorf("nil config: %v, want it to demand a GitSandboxConfig", err)
	}
	if _, err := sbx.OpenSandbox("git", map[string]any{"repo": "x"}); err == nil || !strings.Contains(err.Error(), "map[") {
		t.Errorf("wrong config type: %v, want it to name the offending type", err)
	}
	if _, err := sbx.OpenSandbox("git", git.GitSandboxConfig{}); err == nil || !strings.Contains(err.Error(), "RepoPath is required") {
		t.Errorf("empty RepoPath: %v, want a required-field error", err)
	}
}
