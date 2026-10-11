package handles

// Shared test helpers for the runtime-handles suite. These mirror the
// helpers that live with the root package's tests and in internal/trace;
// test code is not API surface, so the duplication is deliberate.

import (
	"context"
	"testing"

	"github.com/buchenberg/shepherd-kernel-go/internal/testvectors"
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

var reader = trace.ReadContext{ActorRef: "reader", VisibilityProfile: trace.VisibilityPayload}

func loadExecutionVectors(t *testing.T) testvectors.ExecutionVectorDoc {
	return testvectors.LoadExecutionVectors(t, "../../testdata/execution_vectors_v0.json")
}
