package shepherd

// Shared test helpers for the runtime-layer tests. The fixtures live in
// internal/rttest so the external test packages (shepherd_test, task_test,
// supervisor_test) share them; these wrappers keep the root package's
// in-package tests unchanged. Test code is not API surface.

import (
	"testing"

	trace "github.com/buchenberg/shepherd-kernel-go/trace"

	"github.com/buchenberg/shepherd-kernel-go/internal/gittest"
	"github.com/buchenberg/shepherd-kernel-go/internal/rttest"
	"github.com/buchenberg/shepherd-kernel-go/internal/testvectors"
)

func newMemStore(t *testing.T) *trace.SQLiteTraceStore { return rttest.NewMemStore(t) }

var trustedAppend = rttest.TrustedAppend

var reader = rttest.Reader

func draft(kind string, mode trace.RecordMode, payload map[string]any, causedBy ...string) trace.RecordDraft {
	return rttest.Draft(kind, mode, payload, causedBy...)
}

func newTestRepo(t *testing.T) string { return gittest.NewTestRepo(t) }

func requireGit(t *testing.T) { gittest.RequireGit(t) }

func mustGit(t *testing.T, dir string, args ...string) { gittest.MustGit(t, dir, args...) }

func writeFile(t *testing.T, dir, name, content string) { gittest.WriteFile(t, dir, name, content) }

func worktreePaths(t *testing.T, repo string) []string { return gittest.WorktreePaths(t, repo) }

func readFile(t *testing.T, dir, name string) string { return gittest.ReadFile(t, dir, name) }

func samePath(t *testing.T, a, b string) bool { return gittest.SamePath(t, a, b) }

// stagedFiles returns the staged file list, used to pin the non-mutating
// contract of Capture and Diff.
func stagedFiles(t *testing.T, dir string) string { return gittest.StagedFiles(t, dir) }

// mustHead returns the current HEAD SHA, failing the test on error.
func mustHead(t *testing.T, dir string) string {
	t.Helper()
	sha, err := gittest.HeadSHA(t, dir)
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return sha
}

func loadExecutionVectors(t *testing.T) testvectors.ExecutionVectorDoc {
	return testvectors.LoadExecutionVectors(t, "testdata/execution_vectors_v0.json")
}
