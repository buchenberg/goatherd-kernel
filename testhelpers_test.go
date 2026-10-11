package shepherd

// Shared test helpers for the runtime-layer tests. These mirror the helpers
// that live with the trace store's own tests in internal/trace; test code is
// not API surface, so the duplication is deliberate.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/buchenberg/shepherd-kernel-go/internal/gittest"
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

// forkWorktreeChild forks an isolated worktree child of parent, provisions
// the worktree, and hands back the child and the worktree path the test
// "works" in. leaf names the worktree directory (no ':' — Windows paths).
// Mirrored from internal/scope's merge-gate suite; test code is not API
// surface, so the duplication is deliberate.
func forkWorktreeChild(t *testing.T, mgr *ScopeManager, parentID, childOwner, repo, leaf string) (*Scope, string) {
	t.Helper()
	wtPath := filepath.Join(t.TempDir(), leaf)
	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(context.Background(), SandboxSpec{}); err != nil {
		t.Fatalf("worktree create for %s: %v", childOwner, err)
	}
	child, err := mgr.ForkIsolated(parentID, childOwner, nil, sb)
	if err != nil {
		t.Fatalf("ForkIsolated %s: %v", childOwner, err)
	}
	return child, wtPath
}

func requireGit(t *testing.T) { gittest.RequireGit(t) }

func mustGit(t *testing.T, dir string, args ...string) { gittest.MustGit(t, dir, args...) }

func writeFile(t *testing.T, dir, name, content string) { gittest.WriteFile(t, dir, name, content) }

func worktreePaths(t *testing.T, repo string) []string { return gittest.WorktreePaths(t, repo) }

func readFile(t *testing.T, dir, name string) string { return gittest.ReadFile(t, dir, name) }

// readRepoFile reads a repo file, failing the test on error. Mirrored from
// internal/scope's settlement suite; test code is not API surface, so the
// duplication is deliberate.
func readRepoFile(t *testing.T, repo, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// findRecords reads an owner's trace and returns the records with the given
// schema ref, in record order. Mirrored from internal/scope's merge-gate
// suite; test code is not API surface, so the duplication is deliberate.
func findRecords(t *testing.T, store *SQLiteTraceStore, ownerID, schemaRef string) []Record {
	t.Helper()
	slice, err := store.ReadOwnerPrefix(context.Background(), TrustedReadContext, ownerID, 999, ModeBoth)
	if err != nil {
		t.Fatalf("read trace for %s: %v", ownerID, err)
	}
	var found []Record
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == schemaRef {
			found = append(found, rec.(Record))
		}
	}
	return found
}

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
