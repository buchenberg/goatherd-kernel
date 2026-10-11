package materialize

// Shared helpers for the materialize dispatch tests. These mirror the
// equivalents in the trace store's own test suite; test code is not API
// surface.

import (
	"context"
	"testing"

	"github.com/buchenberg/shepherd-kernel-go/internal/trace"
)

func newMemStore(t *testing.T) *trace.SQLiteTraceStore {
	t.Helper()
	store, err := trace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("trace.NewSQLiteTraceStore: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return store
}

var reader = trace.ReadContext{ActorRef: "reader", VisibilityProfile: trace.VisibilityPayload}

var trustedAppend = trace.AppendContext{
	ActorRef:             "runtime:conformance",
	PresentedWitnessRefs: []string{"trusted:internal"},
	SchemaVersionSet:     "shepherd2-slice-a",
	TrustMode:            "internal",
}
