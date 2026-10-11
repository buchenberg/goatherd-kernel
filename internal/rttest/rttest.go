// Package rttest holds the runtime layer's shared test helpers: the
// in-memory store fixture and the trusted append/read contexts the
// conformance suite uses. Test support is not API surface; the package
// exists so the runtime layer's external test packages (shepherd_test,
// task_test, supervisor_test) can share the same fixtures the root
// package's in-package tests use.
package rttest

import (
	"context"
	"testing"

	trace "github.com/buchenberg/shepherd-kernel-go/trace"
)

// NewMemStore returns a t.Cleanup-managed in-memory trace store.
func NewMemStore(t *testing.T) *trace.SQLiteTraceStore {
	t.Helper()
	store, err := trace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("trace.NewSQLiteTraceStore: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return store
}

// TrustedAppend is the trusted internal append context the runtime layer's
// tests record through.
var TrustedAppend = trace.AppendContext{
	ActorRef:             "runtime:conformance",
	PresentedWitnessRefs: []string{"trusted:internal"},
	SchemaVersionSet:     "shepherd2-slice-a",
	TrustMode:            "internal",
}

// Reader is the standard payload-visibility read context.
var Reader = trace.ReadContext{ActorRef: "reader", VisibilityProfile: trace.VisibilityPayload}

// Draft builds a conformance-shaped record draft.
func Draft(kind string, mode trace.RecordMode, payload map[string]any, causedBy ...string) trace.RecordDraft {
	return trace.RecordDraft{
		KindLabel:       kind,
		Mode:            mode,
		SchemaRef:       "shepherd2.conformance." + kind + ".v1",
		Payload:         payload,
		CausedByFactIDs: causedBy,
	}
}
