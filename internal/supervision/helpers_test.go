package supervision

// Shared test helpers for the supervision suite. These mirror the helpers
// that live with the root package's tests and in internal/scope; test code
// is not API surface, so the duplication is deliberate.

import (
	"context"
	"testing"

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
