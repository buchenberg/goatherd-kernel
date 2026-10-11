package scope

// Shared test helpers for the scope-tree suite. These mirror the helpers
// that live with the root package's tests and in internal/sandbox; test
// code is not API surface, so the duplication is deliberate.

import (
	"context"
	"testing"

	"github.com/buchenberg/shepherd-kernel-go/internal/gittest"
	trace "github.com/buchenberg/shepherd-kernel-go/internal/trace"
)

func newMemStore(t *testing.T) *trace.SQLiteTraceStore {
	t.Helper()
	store, err := trace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return store
}

func newTestRepo(t *testing.T) string { return gittest.NewTestRepo(t) }

func mustGit(t *testing.T, dir string, args ...string) { gittest.MustGit(t, dir, args...) }

func writeFile(t *testing.T, dir, name, content string) { gittest.WriteFile(t, dir, name, content) }

func readFile(t *testing.T, dir, name string) string { return gittest.ReadFile(t, dir, name) }

func stagedFiles(t *testing.T, dir string) string { return gittest.StagedFiles(t, dir) }

func mustHead(t *testing.T, dir string) string {
	t.Helper()
	sha, err := gittest.HeadSHA(t, dir)
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return sha
}
