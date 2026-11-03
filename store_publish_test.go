package trace

import (
	"context"
	"slices"
	"testing"
	"time"
)

// The bus must mirror the durable record: a published EffectEvent's causal
// parents are the retained envelope's resolved parent list, and only fresh
// appends publish — idempotent replays and frontier republishes do not.

func waitForEvent(t *testing.T, ch <-chan EffectEvent) EffectEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for bus event")
		return EffectEvent{}
	}
}

func assertNoEvent(t *testing.T, ch <-chan EffectEvent) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected bus event: record %s", ev.RecordID)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishEventsResolveLocalCausalRefs(t *testing.T) {
	store := newMemStore(t)
	bus := NewEffectBus(0)
	defer bus.Close()
	store.WithBus(bus)
	ch := bus.Subscribe("test")

	// One batch: draft "a" (no parents), draft "b" citing "a" by append-local
	// ref, draft "c" citing the external parent by fact id. The durable
	// envelope of "b" carries the resolved record id; the event must too —
	// publishing the pre-resolution draft refs would drop the local parent
	// from the event entirely.
	receipt := appendDrafts(t, store, "publish:local-refs", "agent:main",
		RecordDraft{
			KindLabel:     "a",
			Mode:          Declaration,
			SchemaRef:     "shepherd2.conformance.a.v1",
			Payload:       map[string]any{"n": 1},
			AppendLocalID: "a",
		},
		RecordDraft{
			KindLabel:         "b",
			Mode:              Capture,
			SchemaRef:         "shepherd2.conformance.b.v1",
			Payload:           map[string]any{"n": 2},
			CausedByLocalRefs: []string{"a"},
		},
	)

	evA := waitForEvent(t, ch)
	if evA.RecordID != receipt.FactIDs[0] || len(evA.CausalParents) != 0 {
		t.Fatalf("event a: record %s parents %v, want %s with no parents", evA.RecordID, evA.CausalParents, receipt.FactIDs[0])
	}
	evB := waitForEvent(t, ch)
	if evB.RecordID != receipt.FactIDs[1] {
		t.Fatalf("event b: record %s, want %s", evB.RecordID, receipt.FactIDs[1])
	}

	retained, err := store.readFact(context.Background(), receipt.FactIDs[1])
	if err != nil {
		t.Fatalf("readFact: %v", err)
	}
	if !slices.Equal(evB.CausalParents, retained.Envelope.CausedByIDs) {
		t.Fatalf("event b parents %v != retained envelope parents %v", evB.CausalParents, retained.Envelope.CausedByIDs)
	}
	if len(evB.CausalParents) != 1 || evB.CausalParents[0] != receipt.FactIDs[0] {
		t.Fatalf("event b parents %v, want the resolved record id %s", evB.CausalParents, receipt.FactIDs[0])
	}

	// Idempotent replay of the same intent: the receipt is returned, nothing
	// is written, and no event is published.
	if _, err := store.Append(context.Background(), trustedAppend, AppendBatch{
		AppendIntentID: "publish:local-refs",
		Groups: []AppendGroup{{TraceOwnerID: "agent:main", FactDrafts: []RecordDraft{
			{
				KindLabel:     "a",
				Mode:          Declaration,
				SchemaRef:     "shepherd2.conformance.a.v1",
				Payload:       map[string]any{"n": 1},
				AppendLocalID: "a",
			},
			{
				KindLabel:         "b",
				Mode:              Capture,
				SchemaRef:         "shepherd2.conformance.b.v1",
				Payload:           map[string]any{"n": 2},
				CausedByLocalRefs: []string{"a"},
			},
		}}},
	}); err != nil {
		t.Fatalf("idempotent Append: %v", err)
	}
	assertNoEvent(t, ch)
}

func TestPublishFrontierPublishesFactOnce(t *testing.T) {
	store := newMemStore(t)
	bus := NewEffectBus(0)
	defer bus.Close()
	store.WithBus(bus)
	ch := bus.Subscribe("test")

	parent := appendDrafts(t, store, "publish:frontier:seed", "agent:main",
		draft("seed", Capture, map[string]any{"step": 1}))
	waitForEvent(t, ch) // drain the seed append's event

	factID := parent.FactIDs[0]
	spec := FrontierSpec{
		FrontierID:         "f-publish-once",
		TargetTraceOwnerID: "agent:main",
		ThroughFactID:      factID,
	}
	frontier, err := store.PublishFrontier(context.Background(), trustedAppend, spec)
	if err != nil {
		t.Fatalf("PublishFrontier: %v", err)
	}

	// The frontier fact reaches the bus like any appended record, and its
	// causal parents name the through fact.
	ev := waitForEvent(t, ch)
	if ev.SchemaRef != "shepherd2.frontier.owner_cutoff.v1" || ev.KindLabel != "frontier_published" {
		t.Fatalf("frontier event schema/label = %s/%s, want shepherd2.frontier.owner_cutoff.v1/frontier_published", ev.SchemaRef, ev.KindLabel)
	}
	if ev.TraceOwnerID != "agent:main" {
		t.Fatalf("frontier event owner = %s, want agent:main", ev.TraceOwnerID)
	}
	if !slices.Equal(ev.CausalParents, []string{factID}) {
		t.Fatalf("frontier event parents %v, want [%s]", ev.CausalParents, factID)
	}

	retained, err := store.readFact(context.Background(), frontier.CreatedByFactID)
	if err != nil {
		t.Fatalf("readFact(frontier fact): %v", err)
	}
	if !slices.Equal(ev.CausalParents, retained.Envelope.CausedByIDs) {
		t.Fatalf("frontier event parents %v != retained envelope parents %v", ev.CausalParents, retained.Envelope.CausedByIDs)
	}

	// Republishing the same frontier is an idempotent replay: no new event.
	if _, err := store.PublishFrontier(context.Background(), trustedAppend, spec); err != nil {
		t.Fatalf("idempotent PublishFrontier: %v", err)
	}
	assertNoEvent(t, ch)
}
