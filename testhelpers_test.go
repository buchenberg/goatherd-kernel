package shepherd

// Shared test helpers for the runtime-layer tests. These mirror the helpers
// that live with the trace store's own tests in internal/trace; test code is
// not API surface, so the duplication is deliberate.

import (
	"context"
	"testing"

	"github.com/buchenberg/shepherd-kernel-go/internal/gittest"
	"github.com/buchenberg/shepherd-kernel-go/internal/testvectors"
)

func newMemStore(t *testing.T) *SQLiteTraceStore {
	t.Helper()
	store, err := NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return store
}

var trustedAppend = AppendContext{
	ActorRef:             "runtime:conformance",
	PresentedWitnessRefs: []string{"trusted:internal"},
	SchemaVersionSet:     "shepherd2-slice-a",
	TrustMode:            "internal",
}

var reader = ReadContext{ActorRef: "reader", VisibilityProfile: VisibilityPayload}

func draft(kind string, mode RecordMode, payload map[string]any, causedBy ...string) RecordDraft {
	return RecordDraft{
		KindLabel:       kind,
		Mode:            mode,
		SchemaRef:       "shepherd2.conformance." + kind + ".v1",
		Payload:         payload,
		CausedByFactIDs: causedBy,
	}
}

func newTestRepo(t *testing.T) string { return gittest.NewTestRepo(t) }

func requireGit(t *testing.T) { gittest.RequireGit(t) }

func mustGit(t *testing.T, dir string, args ...string) { gittest.MustGit(t, dir, args...) }

func writeFile(t *testing.T, dir, name, content string) { gittest.WriteFile(t, dir, name, content) }

func worktreePaths(t *testing.T, repo string) []string { return gittest.WorktreePaths(t, repo) }

func readFile(t *testing.T, dir, name string) string { return gittest.ReadFile(t, dir, name) }

// findRecords reads an owner's trace and returns the records with the given
// schema ref, in record order. Mirrored from internal/scope's merge-gate
// suite; test code is not API surface, so the duplication is deliberate.
func samePath(t *testing.T, a, b string) bool { return gittest.SamePath(t, a, b) }

func stagedFiles(t *testing.T, dir string) string { return gittest.StagedFiles(t, dir) }

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
