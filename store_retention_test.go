package trace

// Retention law tests for EvictOwnerPath / ReopenOwnerPath /
// CollectGarbage (bead 4y8, docs/retention-gc.md §4): the evicted path
// fails loudly, other paths' direct fact reads are byte-identical to
// before, surviving children render evicted parents as digest-only
// anchors, and GC reclaims exactly the unreachable.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestEvictedPathFailsLoudlyAndReopensAtOrdinalZero(t *testing.T) {
	store := newMemStore(t)
	receipt := appendDrafts(t, store, "intent:evict", "owner:evict",
		draft("step", Capture, map[string]any{"value": 1}),
		draft("step", Capture, map[string]any{"value": 2}),
	)

	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:evict", false); err != nil {
		t.Fatalf("EvictOwnerPath: %v", err)
	}

	if _, err := store.ReadOwnerPrefix(context.Background(), reader, "owner:evict", 99, ModeBoth); !errors.Is(err, ErrOwnerEvicted) {
		t.Fatalf("ReadOwnerPrefix after eviction: want ErrOwnerEvicted, got %v", err)
	}

	// Unknown paths still read as a successful empty slice; the loud
	// failure is reserved for evicted paths.
	slice, err := store.ReadOwnerPrefix(context.Background(), reader, "owner:never-existed", 0, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix on unknown path: %v", err)
	}
	if len(slice.FactIDs()) != 0 {
		t.Errorf("unknown path returned %d facts, want 0", len(slice.FactIDs()))
	}

	_, err = store.Append(context.Background(), trustedAppend, AppendBatch{
		AppendIntentID: "intent:after-evict",
		Groups:         []AppendGroup{{TraceOwnerID: "owner:evict", FactDrafts: []RecordDraft{draft("step", Capture, nil)}}},
	})
	if !errors.Is(err, ErrOwnerEvicted) {
		t.Fatalf("Append to evicted path: want ErrOwnerEvicted, got %v", err)
	}

	if err := store.ReopenOwnerPath(context.Background(), trustedAppend, "owner:evict"); err != nil {
		t.Fatalf("ReopenOwnerPath: %v", err)
	}

	fresh, err := store.Append(context.Background(), trustedAppend, AppendBatch{
		AppendIntentID: "intent:after-reopen",
		Groups:         []AppendGroup{{TraceOwnerID: "owner:evict", FactDrafts: []RecordDraft{draft("step", Capture, nil)}}},
	})
	if err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if rng := fresh.OwnerRanges["owner:evict"]; rng[0] != 0 {
		t.Errorf("first append after reopen starts at ordinal %d, want 0", rng[0])
	}

	slice, err = store.ReadOwnerPrefix(context.Background(), reader, "owner:evict", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix after reopen: %v", err)
	}
	if len(slice.FactIDs()) != 1 {
		t.Fatalf("reopened path has %d facts, want 1 (the pre-eviction facts are gone)", len(slice.FactIDs()))
	}
	if slice.FactIDs()[0] == receipt.FactIDs[0] {
		t.Error("reopened path leaked a pre-eviction fact id")
	}

	// Tombstone is one-shot: reopening a live path fails loudly.
	if err := store.ReopenOwnerPath(context.Background(), trustedAppend, "owner:evict"); err == nil {
		t.Error("ReopenOwnerPath on non-evicted path: want error, got nil")
	}
}

func TestCollectGarbageOnEmptyStoreNeverPanics(t *testing.T) {
	store := newMemStore(t)
	gc, err := store.CollectGarbage(context.Background(), trustedAppend)
	if err != nil {
		t.Fatalf("CollectGarbage on empty store: %v", err)
	}
	if gc.DeletedRecords != 0 || gc.DeletedWitnessEntries != 0 || gc.DeletedAppendIntents != 0 {
		t.Errorf("empty-store sweep reclaimed %+v; want a zero receipt", gc)
	}
}

func TestEvictRefusesUnknownPathAndWitnessPath(t *testing.T) {
	store := newMemStore(t)
	appendDrafts(t, store, "intent:seed", "owner:seed", draft("step", Capture, nil))

	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:none", false); err == nil {
		t.Error("evicting a never-existing path: want error, got nil")
	}
	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, witnessTraceOwnerID, true); err == nil {
		t.Error("evicting the kernel:witness path: want refusal, got nil")
	}
}

func TestEvictionRefusesPinnedFrontiersWithoutAck(t *testing.T) {
	store := newMemStore(t)
	receipt := appendDrafts(t, store, "intent:frontier", "owner:frontier",
		draft("step", Capture, nil),
	)
	if _, err := store.PublishCut(context.Background(), trustedAppend, FrontierSpec{
		FrontierID:         "frontier:pin",
		TargetTraceOwnerID: "owner:frontier",
		ThroughFactID:      receipt.FactIDs[0],
	}); err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:frontier", false); err == nil {
		t.Fatal("evicting a frontier-pinned path without ack: want error, got nil")
	}

	if _, err := store.ReadOwnerCutoff(context.Background(), "frontier:pin"); err != nil {
		t.Fatalf("ReadOwnerCutoff after refused eviction: %v", err)
	}

	eviction, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:frontier", true)
	if err != nil {
		t.Fatalf("evict with ack: %v", err)
	}
	if len(eviction.DeletedFrontierIDs) != 1 || eviction.DeletedFrontierIDs[0] != "frontier:pin" {
		t.Errorf("acked eviction frontier ids = %v, want [frontier:pin]", eviction.DeletedFrontierIDs)
	}

	if _, err := store.ReadOwnerCutoff(context.Background(), "frontier:pin"); !errors.Is(err, ErrUnknownFrontier) {
		t.Errorf("ReadOwnerCutoff after acked eviction: want ErrUnknownFrontier, got %v", err)
	}
}

func TestEvictionRefusesPinnedFrontierOnPublisherPath(t *testing.T) {
	store := newMemStore(t)
	receipt := appendDrafts(t, store, "intent:pub", "owner:target", draft("step", Capture, nil))
	if _, err := store.PublishCut(context.Background(), trustedAppend, FrontierSpec{
		FrontierID:         "frontier:pub",
		TargetTraceOwnerID: "owner:target",
		ThroughFactID:      receipt.FactIDs[0],
		PublisherOwnerID:   "owner:publisher",
	}); err != nil {
		t.Fatalf("PublishCut off-target: %v", err)
	}

	// The cut's record lives on the publisher path; evicting that path
	// without ack would break the read of the cut fact itself.
	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:publisher", false); err == nil {
		t.Fatal("evicting a publisher-pinned path without ack: want error, got nil")
	}
	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:publisher", true); err != nil {
		t.Fatalf("evict publisher with ack: %v", err)
	}

	// The target path is untouched by its publisher's eviction.
	if _, err := store.ReadOwnerCutoff(context.Background(), "frontier:pub"); !errors.Is(err, ErrUnknownFrontier) {
		t.Errorf("frontier was pinned only by the acked publisher path: want ErrUnknownFrontier, got %v", err)
	}
	slice, err := store.ReadOwnerPrefix(context.Background(), reader, "owner:target", 99, ModeBoth)
	if err != nil {
		t.Fatalf("target path read: %v", err)
	}
	if len(slice.FactIDs()) != 1 {
		t.Errorf("target path has %d facts, want 1", len(slice.FactIDs()))
	}
}

// Regression for the review's critical on the version gate: a database
// written before the retention change has no evicted_paths table and no
// marker, and must still open read-only and serve reads — the tombstone
// probe reads a missing table as not evicted.
func TestReadOnlyOpenOfPreRetentionSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-retention.trace.sqlite")
	store, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	expected := appendDrafts(t, store, "intent:legacy", "owner:legacy", draft("step", Capture, nil))
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Strip the retention bits to reconstruct the pre-retention schema.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec("DROP TABLE evicted_paths"); err != nil {
		t.Fatalf("drop evicted_paths: %v", err)
	}
	if _, err := raw.Exec("DELETE FROM meta WHERE key = 'schema:evicted_paths'"); err != nil {
		t.Fatalf("delete schema marker: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	viewer, err := OpenSQLiteTraceStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteTraceStoreReadOnly on pre-retention schema: %v", err)
	}
	defer viewer.Close(context.Background())

	slice, err := viewer.ReadOwnerPrefix(context.Background(), reader, "owner:legacy", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix on pre-retention database: %v", err)
	}
	if len(slice.FactIDs()) != 1 || slice.FactIDs()[0] != expected.FactIDs[0] {
		t.Errorf("pre-retention read returned %v, want [%s]", slice.FactIDs(), expected.FactIDs[0])
	}
}

func TestAppendIdempotentReplayAfterEviction(t *testing.T) {
	store := newMemStore(t)
	batch := AppendBatch{
		AppendIntentID: "intent:retry",
		Groups:         []AppendGroup{{TraceOwnerID: "owner:retry", FactDrafts: []RecordDraft{draft("step", Capture, nil)}}},
	}
	original, err := store.Append(context.Background(), trustedAppend, batch)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:retry", false); err != nil {
		t.Fatalf("EvictOwnerPath: %v", err)
	}

	replayed, err := store.Append(context.Background(), trustedAppend, batch)
	if err != nil {
		t.Fatalf("idempotent replay after eviction: %v", err)
	}
	if replayed.AppendIntentID != original.AppendIntentID || len(replayed.FactIDs) != len(original.FactIDs) {
		t.Errorf("replay returned %+v, want stored receipt %+v", replayed, original)
	}
}

// A closure read rooted at an unreachable record fails loudly instead of
// silently returning a shortened slice whose shape depends on GC timing.
func TestClosureRootOnEvictedPathFailsLoudly(t *testing.T) {
	store := newMemStore(t)
	root := appendDrafts(t, store, "intent:root", "owner:root", draft("step", Capture, nil))
	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:root", false); err != nil {
		t.Fatalf("EvictOwnerPath: %v", err)
	}
	if _, err := store.ReadCausalClosure(context.Background(), reader, []string{root.FactIDs[0]}, ModeBoth, "visible_only"); err == nil {
		t.Error("closure rooted at an evicted record: want error, got nil")
	}
}

// Pins the dedup case from the design's new-law wording: two owners share
// a content-addressed fact, one path is evicted — the survivor's reads are
// unchanged, because the record is still on the survivor's path. It does
// not vanish from every read, only from the evicted path's.
func TestSharedFactSurvivesSinglePathEviction(t *testing.T) {
	store := newMemStore(t)
	d := draft("step", Capture, map[string]any{"value": 1})
	appendDrafts(t, store, "intent:owner-a", "owner:a", d)
	second := appendDrafts(t, store, "intent:owner-b", "owner:b", d)

	before, err := store.ReadOwnerPrefix(context.Background(), reader, "owner:b", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix before eviction: %v", err)
	}

	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:a", false); err != nil {
		t.Fatalf("EvictOwnerPath owner:a: %v", err)
	}

	after, err := store.ReadOwnerPrefix(context.Background(), reader, "owner:b", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix after eviction: %v", err)
	}
	if len(after.FactIDs()) != len(before.FactIDs()) {
		t.Fatalf("survivor fact count changed: %d -> %d", len(before.FactIDs()), len(after.FactIDs()))
	}
	for i, id := range before.FactIDs() {
		if after.FactIDs()[i] != id {
			t.Errorf("survivor fact[%d] = %q, want %q", i, after.FactIDs()[i], id)
		}
	}
	fact, ok := after.FactsByID[second.FactIDs[0]].(Record)
	if !ok {
		t.Fatal("expected full Record on survivor path")
	}
	if fact.View.TraceOwnerID != "owner:b" {
		t.Errorf("survivor view trace_owner_id = %q, want owner:b", fact.View.TraceOwnerID)
	}
}

func TestEvictionRequiresInternalTrust(t *testing.T) {
	store := newMemStore(t)
	untrusted := AppendContext{ActorRef: "actor:random", TrustMode: "untrusted"}
	if _, err := store.EvictOwnerPath(context.Background(), untrusted, "owner:any", false); err == nil {
		t.Fatal("eviction without internal trust: want error, got nil")
	}
	if err := store.ReopenOwnerPath(context.Background(), untrusted, "owner:any"); err == nil {
		t.Fatal("reopen without internal trust: want error, got nil")
	}
	if _, err := store.CollectGarbage(context.Background(), untrusted); err == nil {
		t.Fatal("GC without internal trust: want error, got nil")
	}
}

// Dependent reads: a surviving child's closure over an evicted parent
// degrades to a digest-only external anchor with an anchor kind naming
// the reason; it does not fail.
func TestSurvivingChildClosureRendersEvictedParentAsDigestAnchor(t *testing.T) {
	store := newMemStore(t)
	parent := appendDrafts(t, store, "intent:parent", "owner:parent", draft("step", Capture, map[string]any{"value": 1}))
	child := appendDrafts(t, store, "intent:child", "owner:child",
		draft("step", Capture, nil, parent.FactIDs[0]),
	)

	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:parent", false); err != nil {
		t.Fatalf("EvictOwnerPath: %v", err)
	}

	slice, err := store.ReadCausalClosure(context.Background(), reader, []string{child.FactIDs[0]}, ModeBoth, "include_external_anchors")
	if err != nil {
		t.Fatalf("ReadCausalClosure over evicted parent: %v", err)
	}
	if _, ok := slice.FactsByID[parent.FactIDs[0]]; ok {
		t.Error("evicted parent must not read as a slice fact")
	}
	var anchor *ExternalAnchor
	for i := range slice.ExternalAnchors {
		if slice.ExternalAnchors[i].Ref == parent.FactIDs[0] {
			anchor = &slice.ExternalAnchors[i]
			break
		}
	}
	if anchor == nil {
		t.Fatal("evicted parent anchor missing from closure read")
	}
	if anchor.AnchorKind != externalAnchorKindEvicted {
		t.Errorf("anchor kind = %q, want %q", anchor.AnchorKind, externalAnchorKindEvicted)
	}
	if anchor.VisibleShape["schema_ref"] == nil || anchor.VisibleShape["trace_owner_id"] != nil {
		t.Errorf("digest-only anchor shape = %v, want schema_ref/witness_ref only", anchor.VisibleShape)
	}
}

func TestCollectGarbageReclaimsUnreachableAndKeepsLive(t *testing.T) {
	store := newMemStore(t)

	// Each append carries its own retained context, so each gets its own
	// witness: an intent whose records all die takes its witness with it.
	deadGroup := func(intent, capRef string, drafts ...RecordDraft) AppendReceipt {
		t.Helper()
		receipt, err := store.Append(context.Background(), trustedAppend, AppendBatch{
			AppendIntentID: intent,
			Groups: []AppendGroup{{
				TraceOwnerID: "owner:dead",
				RetainedContext: &RetainedContext{
					ContextID:             "ctx:" + intent,
					CapabilityWitnessRefs: []string{capRef},
					SubstrateRef:          "sqlite.local.v1",
					Containment:           ContainContained,
				},
				FactDrafts: drafts,
			}},
		})
		if err != nil {
			t.Fatalf("Append(%s): %v", intent, err)
		}
		return receipt
	}

	// owner:dead batch A: two facts under one witness. p1 stays alive via a
	// surviving child; p2 is reclaimed as unreachable.
	deadA := deadGroup("intent:dead-a", "cap:dead-a",
		draft("step", Capture, map[string]any{"value": 1}),
		draft("step", Capture, map[string]any{"value": 2}),
	)
	p1, p2 := deadA.FactIDs[0], deadA.FactIDs[1]
	// owner:dead batch B: one fact under its own witness, fully unreachable.
	deadB := deadGroup("intent:dead-b", "cap:dead-b", draft("step", Capture, map[string]any{"value": 3}))
	s := deadB.FactIDs[0]
	// owner:live: an unrelated survivor plus a child citing p1 across paths.
	liveParent := appendDrafts(t, store, "intent:live", "owner:live", draft("step", Capture, map[string]any{"value": 9}))
	child := appendDrafts(t, store, "intent:child", "owner:live",
		draft("step", Capture, nil, p1),
	)

	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:dead", false); err != nil {
		t.Fatalf("EvictOwnerPath: %v", err)
	}

	factsBefore, err := store.FactCount(context.Background())
	if err != nil {
		t.Fatalf("FactCount: %v", err)
	}

	gc, err := store.CollectGarbage(context.Background(), trustedAppend)
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}

	// p1 is cited by the surviving child, so the closure rule keeps its
	// record row; p2, s, and the witness only dead records cite go. The
	// survivors are p1, liveParent, child, root, W_a (cited by p1) and the
	// default-context witness shared by liveParent/child.
	if got := factsBefore - gc.DeletedRecords; got != 6 {
		t.Errorf("records survived = %d, want 6", got)
	}
	if !recordRowExists(context.Background(), store, p1) {
		t.Error("evicted parent cited by a surviving child must survive GC")
	}
	if recordRowExists(context.Background(), store, p2) {
		t.Errorf("unreachable record %s must be reclaimed", p2)
	}
	if recordRowExists(context.Background(), store, s) {
		t.Errorf("unreachable record %s must be reclaimed", s)
	}
	if len(gc.ReclaimedWitnessIDs) == 0 {
		t.Error("dead owner's witnesses were never reclaimed; the accumulation fix did not run")
	}
	for _, w := range gc.ReclaimedWitnessIDs {
		if !witnessRecordIsReclaimed(context.Background(), store, w) {
			t.Errorf("witness %s record or its kernel:witness path entry survived", w)
		}
	}

	if !recordRowExists(context.Background(), store, liveParent.FactIDs[0]) {
		t.Error("unrelated live-path fact must survive GC")
	}

	// Surviving reads are intact: the live path keeps its own facts, and
	// the child's closure read still renders p1 through the digest-only
	// evicted anchor.
	prefix, err := store.ReadOwnerPrefix(context.Background(), reader, "owner:live", 99, ModeBoth)
	if err != nil {
		t.Fatalf("owner:live prefix read after GC: %v", err)
	}
	if len(prefix.FactIDs()) != 2 {
		t.Errorf("owner:live has %d facts after GC, want 2", len(prefix.FactIDs()))
	}
	slice, err := store.ReadCausalClosure(context.Background(), reader, []string{child.FactIDs[0]}, ModeBoth, "include_external_anchors")
	if err != nil {
		t.Fatalf("closure read after GC: %v", err)
	}
	if _, ok := slice.FactsByID[child.FactIDs[0]]; !ok {
		t.Error("surviving child missing from closure read after GC")
	}
	for i := range slice.ExternalAnchors {
		if slice.ExternalAnchors[i].Ref == p1 && slice.ExternalAnchors[i].AnchorKind != externalAnchorKindEvicted {
			t.Errorf("p1 anchor kind = %q, want %q", slice.ExternalAnchors[i].AnchorKind, externalAnchorKindEvicted)
		}
	}

	// Reclaimed witnesses stay reclaimed: a second sweep is a no-op.
	second, err := store.CollectGarbage(context.Background(), trustedAppend)
	if err != nil {
		t.Fatalf("second CollectGarbage: %v", err)
	}
	if second.DeletedRecords != 0 || second.DeletedWitnessEntries != 0 {
		t.Errorf("second sweep reclaimed %+v; GC must converge", second)
	}
}

// recordRowExists probes the records table directly: an evicted-but-cited
// parent has no path entries, so the public read intentionally cannot see
// it.
func recordRowExists(ctx context.Context, store *SQLiteTraceStore, recordID string) bool {
	var one int
	store.db.QueryRowContext(ctx, "SELECT 1 FROM records WHERE record_id = ?", recordID).Scan(&one)
	return one == 1
}

func witnessRecordIsReclaimed(ctx context.Context, store *SQLiteTraceStore, witnessID string) bool {
	var entries, records int
	store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM path_entries WHERE record_id = ? AND path_ref = ?", witnessID, witnessTraceOwnerID).Scan(&entries)
	store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM records WHERE record_id = ?", witnessID).Scan(&records)
	return entries == 0 && records == 0
}

// GC liveness rule 3: records embedded in a checkpoint snapshot BLOB stay
// live even after their path is evicted; deleting the checkpoint frees
// them on the next sweep.
func TestCollectGarbageKeepsSnapshotReferencedRecords(t *testing.T) {
	store := newMemStore(t)
	receipt := appendDrafts(t, store, "intent:snap", "owner:snap", draft("state", Capture, map[string]any{"value": 1}))
	factID := receipt.FactIDs[0]

	payload := map[string]any{"retained_fact_id": factID}
	blob, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal snapshot payload: %v", err)
	}
	if err := PersistCheckpointBlob(context.Background(), store, "ckpt:1", "scope:1", 1,
		WorkspaceState{Backend: "git", Data: map[string]any{}}, blob, time.Now()); err != nil {
		t.Fatalf("PersistCheckpointBlob: %v", err)
	}

	if _, err := store.EvictOwnerPath(context.Background(), trustedAppend, "owner:snap", false); err != nil {
		t.Fatalf("EvictOwnerPath: %v", err)
	}

	gc, err := store.CollectGarbage(context.Background(), trustedAppend)
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if gc.DeletedRecords != 0 {
		t.Errorf("snapshot-referenced record was reclaimed (deleted %d records)", gc.DeletedRecords)
	}

	if err := DeleteCheckpointBlob(context.Background(), store, "ckpt:1"); err != nil {
		t.Fatalf("DeleteCheckpointBlob: %v", err)
	}
	gc, err = store.CollectGarbage(context.Background(), trustedAppend)
	if err != nil {
		t.Fatalf("CollectGarbage after checkpoint delete: %v", err)
	}
	// The record and its now-uncited witness both go.
	if gc.DeletedRecords != 2 {
		t.Errorf("after checkpoint delete, sweep deleted %d records, want 2", gc.DeletedRecords)
	}
}
