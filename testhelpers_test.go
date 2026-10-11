package shepherd

// Shared test helpers for the runtime-layer tests. These mirror the helpers
// that live with the trace store's own tests in internal/trace; test code is
// not API surface, so the duplication is deliberate.

import (
	"context"
	"testing"

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

type executionVectorDoc = testvectors.ExecutionVectorDoc

func loadExecutionVectors(t *testing.T) executionVectorDoc {
	return testvectors.LoadExecutionVectors(t, "testdata/execution_vectors_v0.json")
}
