package trace

// Retention for the SQLite trace store: owner-scoped eviction, path
// reopening, and garbage collection (bead 4y8, docs/retention-gc.md).
//
// The design pins three frozen-surface methods, the tombstone table, and
// the GC liveness fixpoint. No existing code path's behavior changes for
// any owner path that is not evicted; the only read-presentation change
// is a surviving child's digest-only anchor for an evicted parent.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	_ "modernc.org/sqlite"
)

// ErrOwnerEvicted is the expected "this owner path was evicted" condition,
// matched with errors.Is. Evicted paths fail loudly instead of reading as
// an empty slice, which is what an unknown path returns.
var ErrOwnerEvicted = errors.New("owner path evicted")

// OwnerEvictedError reports a refusal or read against an evicted owner
// path. It matches ErrOwnerEvicted through errors.Is.
type OwnerEvictedError struct{ OwnerID string }

func (e *OwnerEvictedError) Error() string {
	return fmt.Sprintf("owner path %q is evicted", e.OwnerID)
}

func (e *OwnerEvictedError) Is(target error) bool { return target == ErrOwnerEvicted }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// EvictionReceipt reports what EvictOwnerPath removed and which frontier
// invalidations the caller's ack authorized, so a privacy flow can state
// what happened.
type EvictionReceipt struct {
	OwnerID            string
	DeletedPathEntries int
	DeletedFrontierIDs []string
	// DeletedBlobs counts named blobs removed for the owner. Blob bytes can
	// carry workspace contents, so they are deleted with the path rather
	// than left for GC.
	DeletedBlobs int
}

// GarbageReceipt reports what CollectGarbage reclaimed. SnapshotScan
// names the liveness methodology for checkpoint snapshots: a record id
// embedded in the opaque payload keeps its record live only when the raw
// text contains it, so a caller whose payloads encode ids in any other
// shape (or split them across escapes) must pin record ids itself.
type GarbageReceipt struct {
	DeletedRecords        int
	DeletedRecordEdges    int
	DeletedWitnessEntries int
	ReclaimedWitnessIDs   []string
	DeletedContexts       int
	DeletedAppendIntents  int
	SnapshotScan          string
}

// OpEvict marks the eviction and GC entry points in the operation context.
// The authorization predicate is the append gate's; the kind is descriptive.
const OpEvict OperationKind = "evict"

// ensureEvictionAuthorized mirrors ensureAppendAuthorized: the kernel's
// authorization model is in-process cooperative, and eviction is exactly
// as forgeable as the append gate itself. This is stated by the design
// rather than presented as an absolute boundary.
func ensureEvictionAuthorized(ctx OperationContext) error {
	return ensureAppendAuthorized(ctx)
}

// rowQuerier covers *sql.DB and *sql.Tx for the shared tombstone probe.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// ownerEvicted reports whether the tombstone for ownerID exists. Databases
// written before the retention change have no evicted_paths table — the
// version gate in validateExistingSchema keeps those openable — and no
// tombstones, so a store opened without the table never probes it.
// Presence is cached on the store at open time.
func ownerEvicted(hasEvictedPaths bool, q rowQuerier, ownerID string) (bool, error) {
	if !hasEvictedPaths {
		return false, nil
	}
	var one int
	err := q.QueryRow("SELECT 1 FROM evicted_paths WHERE trace_owner_id = ?", ownerID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query eviction tombstone: %w", err)
	}
	return true, nil
}

// ownerPathEvictedTx reports whether the eviction tombstone for ownerID exists.
func (s *SQLiteTraceStore) ownerPathEvictedTx(tx *sql.Tx, ownerID string) (bool, error) {
	return ownerEvicted(s.hasEvictedPaths, tx, ownerID)
}

// EvictOwnerPath removes an owner path's reachability: every path_entries
// row with path_ref = ownerID, the owner_ordinals row, the owner's named
// blobs, a tombstone marking the path evicted, and — with the caller's ack —
// the frontier rows that pin this path as target OR as publisher. The facts
// themselves are untouched; garbage collection is a separate, explicit step.
// Evicting the internal kernel:witness path is refused unconditionally.
func (s *SQLiteTraceStore) EvictOwnerPath(ctx context.Context, auth AppendContext, ownerID string, ackDanglingFrontiers bool) (EvictionReceipt, error) {
	opCtx := auth.ToOperationContext(OpEvict)
	if err := ensureEvictionAuthorized(opCtx); err != nil {
		return EvictionReceipt{}, err
	}
	if ownerID == "" {
		return EvictionReceipt{}, fmt.Errorf("ownerID is required")
	}
	if ownerID == witnessTraceOwnerID {
		return EvictionReceipt{}, &TraceStoreError{fmt.Sprintf("evicting the internal %s path is refused", witnessTraceOwnerID)}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EvictionReceipt{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	if evicted, err := s.ownerPathEvictedTx(tx, ownerID); err != nil {
		return EvictionReceipt{}, err
	} else if evicted {
		return EvictionReceipt{}, &OwnerEvictedError{OwnerID: ownerID}
	}

	// An unknown path must not gain a tombstone, or a typo would blacklist a
	// path that never existed. An owner that holds only named blobs is not
	// unknown, though: its bytes (potentially workspace contents) must still
	// be deletable by a privacy flow.
	var entryCount int
	if err := tx.QueryRow("SELECT COUNT(*) FROM path_entries WHERE path_ref = ?", ownerID).Scan(&entryCount); err != nil {
		return EvictionReceipt{}, fmt.Errorf("count path entries: %w", err)
	}
	var ordinalCount int
	if err := tx.QueryRow("SELECT COUNT(*) FROM owner_ordinals WHERE trace_owner_id = ?", ownerID).Scan(&ordinalCount); err != nil {
		return EvictionReceipt{}, fmt.Errorf("count owner ordinals: %w", err)
	}
	var blobCount int
	if err := tx.QueryRow("SELECT COUNT(*) FROM named_blobs WHERE owner = ?", ownerID).Scan(&blobCount); err != nil {
		return EvictionReceipt{}, fmt.Errorf("count named blobs: %w", err)
	}
	if entryCount == 0 && ordinalCount == 0 && blobCount == 0 {
		return EvictionReceipt{}, &TraceStoreError{fmt.Sprintf("no such owner path: %s", ownerID)}
	}

	// Frontiers pin the path on both sides: as target (read-time
	// re-validation would dangle) and as publisher (the cut's record would
	// lose its only path entry). Refuse unless acked; acked frontiers are
	// deleted with the path.
	frontierRows, err := tx.Query(
		"SELECT frontier_id FROM frontiers WHERE target_trace_owner_id = ? OR publisher_trace_owner_id = ?",
		ownerID, ownerID,
	)
	if err != nil {
		return EvictionReceipt{}, fmt.Errorf("query pinning frontiers: %w", err)
	}
	var pinnedFrontierIDs []string
	for frontierRows.Next() {
		var id string
		if err := frontierRows.Scan(&id); err != nil {
			frontierRows.Close()
			return EvictionReceipt{}, fmt.Errorf("scan frontier id: %w", err)
		}
		pinnedFrontierIDs = append(pinnedFrontierIDs, id)
	}
	if err := frontierRows.Err(); err != nil {
		frontierRows.Close()
		return EvictionReceipt{}, fmt.Errorf("iterate pinning frontiers: %w", err)
	}
	frontierRows.Close()

	if len(pinnedFrontierIDs) > 0 && !ackDanglingFrontiers {
		return EvictionReceipt{}, &TraceStoreError{
			fmt.Sprintf("frontiers pin owner %q as target or publisher; pass ackDanglingFrontiers to evict anyway", ownerID),
		}
	}

	res, err := tx.Exec("DELETE FROM path_entries WHERE path_ref = ?", ownerID)
	if err != nil {
		return EvictionReceipt{}, fmt.Errorf("delete path entries: %w", err)
	}
	deletedEntries, err := res.RowsAffected()
	if err != nil {
		return EvictionReceipt{}, fmt.Errorf("count deleted path entries: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM owner_ordinals WHERE trace_owner_id = ?", ownerID); err != nil {
		return EvictionReceipt{}, fmt.Errorf("delete owner ordinals: %w", err)
	}
	blobRes, err := tx.Exec("DELETE FROM named_blobs WHERE owner = ?", ownerID)
	if err != nil {
		return EvictionReceipt{}, fmt.Errorf("delete named blobs: %w", err)
	}
	deletedBlobs, err := blobRes.RowsAffected()
	if err != nil {
		return EvictionReceipt{}, fmt.Errorf("count deleted named blobs: %w", err)
	}
	for _, id := range pinnedFrontierIDs {
		if _, err := tx.Exec("DELETE FROM frontiers WHERE frontier_id = ?", id); err != nil {
			return EvictionReceipt{}, fmt.Errorf("delete acked frontier: %w", err)
		}
	}
	if _, err := tx.Exec("INSERT INTO evicted_paths(trace_owner_id) VALUES (?)", ownerID); err != nil {
		return EvictionReceipt{}, fmt.Errorf("insert eviction tombstone: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return EvictionReceipt{}, fmt.Errorf("commit: %w", err)
	}
	return EvictionReceipt{
		OwnerID:            ownerID,
		DeletedPathEntries: int(deletedEntries),
		DeletedFrontierIDs: pinnedFrontierIDs,
		DeletedBlobs:       int(deletedBlobs),
	}, nil
}

// ReopenOwnerPath deletes the eviction tombstone so the path can take
// appends again; the first append after reopening starts a fresh path at
// ordinal 0.
func (s *SQLiteTraceStore) ReopenOwnerPath(ctx context.Context, auth AppendContext, ownerID string) error {
	opCtx := auth.ToOperationContext(OpEvict)
	if err := ensureEvictionAuthorized(opCtx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec("DELETE FROM evicted_paths WHERE trace_owner_id = ?", ownerID)
	if err != nil {
		return fmt.Errorf("delete eviction tombstone: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("count deleted tombstones: %w", err)
	}
	if deleted == 0 {
		return &TraceStoreError{fmt.Sprintf("owner path is not evicted: %s", ownerID)}
	}
	return tx.Commit()
}

// CollectGarbage removes records no longer reachable from any surviving
// owner path, recorded frontier, or checkpoint snapshot — plus the causal
// parents those live records cite, the witnesses no live record's
// witness_ref cites, and the append intents left with zero referents.
// Pure reclamation; it changes no visible read result except the witness
// reclamation the design pins.
func (s *SQLiteTraceStore) CollectGarbage(ctx context.Context, auth AppendContext) (GarbageReceipt, error) {
	opCtx := auth.ToOperationContext(OpEvict)
	if err := ensureEvictionAuthorized(opCtx); err != nil {
		return GarbageReceipt{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GarbageReceipt{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	receipt, err := s.collectGarbageInTx(tx)
	if err != nil {
		return GarbageReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return GarbageReceipt{}, fmt.Errorf("commit: %w", err)
	}
	return receipt, nil
}

func (s *SQLiteTraceStore) collectGarbageInTx(tx *sql.Tx) (GarbageReceipt, error) {
	receipt := GarbageReceipt{
		SnapshotScan: "best-effort scan for sha256:<64hex> record ids embedded in checkpoint_snapshots.snapshot payloads",
	}

	// The witness path is the one deliberate exception to "only eviction
	// deletes path entries": a kernel:witness entry confers no liveness on
	// its record (otherwise witnesses would accumulate forever). Every
	// other path's entry keeps its record fully live.
	rows, err := tx.Query("SELECT DISTINCT record_id FROM path_entries WHERE path_ref != ?", witnessTraceOwnerID)
	if err != nil {
		return GarbageReceipt{}, fmt.Errorf("seed live set from path entries: %w", err)
	}
	live := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return GarbageReceipt{}, fmt.Errorf("scan path entry record: %w", err)
		}
		live[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GarbageReceipt{}, fmt.Errorf("iterate path entries: %w", err)
	}

	var throughFactID, createdByFactID string
	frontierRows, err := tx.Query("SELECT through_fact_id, created_by_fact_id FROM frontiers")
	if err != nil {
		return GarbageReceipt{}, fmt.Errorf("seed live set from frontiers: %w", err)
	}
	for frontierRows.Next() {
		if err := frontierRows.Scan(&throughFactID, &createdByFactID); err != nil {
			frontierRows.Close()
			return GarbageReceipt{}, fmt.Errorf("scan frontier row: %w", err)
		}
		live[throughFactID] = true
		live[createdByFactID] = true
	}
	frontierRows.Close()
	if err := frontierRows.Err(); err != nil {
		return GarbageReceipt{}, fmt.Errorf("iterate frontiers: %w", err)
	}

	// The root witness is live unconditionally.
	live[RootWitnessRecordIDMust()] = true

	if err := markSnapshotLiveRecordsTx(tx, live, &receipt); err != nil {
		return GarbageReceipt{}, err
	}

	// Load the record graph once: the fixpoint below walks causal edges
	// backward and witness refs until nothing new goes live.
	recordWitness := make(map[string]string)
	rows, err = tx.Query("SELECT record_id, witness_ref FROM records")
	if err != nil {
		return GarbageReceipt{}, fmt.Errorf("load records for liveness: %w", err)
	}
	for rows.Next() {
		var id, witnessRef string
		if err := rows.Scan(&id, &witnessRef); err != nil {
			rows.Close()
			return GarbageReceipt{}, fmt.Errorf("scan record for liveness: %w", err)
		}
		recordWitness[id] = witnessRef
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GarbageReceipt{}, fmt.Errorf("iterate records: %w", err)
	}

	parentsOf := make(map[string][]string)
	rows, err = tx.Query("SELECT child_record_id, parent_record_id FROM record_edges")
	if err != nil {
		return GarbageReceipt{}, fmt.Errorf("load causal edges for liveness: %w", err)
	}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			rows.Close()
			return GarbageReceipt{}, fmt.Errorf("scan causal edge: %w", err)
		}
		parentsOf[child] = append(parentsOf[child], parent)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GarbageReceipt{}, fmt.Errorf("iterate causal edges: %w", err)
	}

	// Liveness fixpoint over a worklist: children and witnesses keep their
	// supporters. Each record enters the worklist at most once, so this is
	// O(V+E) over the causal and witness edges rather than one full live-set
	// scan per closure level.
	worklist := make([]string, 0, len(live))
	for id := range live {
		worklist = append(worklist, id)
	}
	for len(worklist) > 0 {
		id := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]
		if witnessRef := recordWitness[id]; witnessRef != "" && witnessRef != RootWitnessRef && !live[witnessRef] {
			live[witnessRef] = true
			worklist = append(worklist, witnessRef)
		}
		for _, parent := range parentsOf[id] {
			if !live[parent] {
				live[parent] = true
				worklist = append(worklist, parent)
			}
		}
	}

	// GC runs on well-formed graphs by induction (eviction/GC only delete;
	// deletion cannot create a cycle). A witness cycle among surviving
	// records is a corruption event: abort, never collect through an
	// invariant hole.
	if err := checkWitnessCycles(recordWitness, live); err != nil {
		return GarbageReceipt{}, err
	}

	dead := make([]string, 0, maxInt(0, len(recordWitness)-len(live)))
	for id := range recordWitness {
		if !live[id] {
			dead = append(dead, id)
		}
	}

	// The sweep deletes children before parents, per the FK dependency
	// order: path_entries and record_edges reference records, records and
	// contexts and path_entries and frontiers reference append_intents.
	if len(dead) > 0 {
		// Temp tables live on the pooled connection: drop-then-create makes
		// the sweep safe even if a previous run's cleanup was skipped.
		if _, err := tx.Exec("DROP TABLE IF EXISTS temp.dead_records"); err != nil {
			return GarbageReceipt{}, fmt.Errorf("clear staged dead records: %w", err)
		}
		if _, err := tx.Exec("CREATE TEMP TABLE IF NOT EXISTS dead_records(record_id TEXT PRIMARY KEY)"); err != nil {
			return GarbageReceipt{}, fmt.Errorf("stage dead records: %w", err)
		}
		defer tx.Exec("DROP TABLE IF EXISTS temp.dead_records")
		for _, id := range dead {
			if _, err := tx.Exec("INSERT INTO temp.dead_records(record_id) VALUES (?)", id); err != nil {
				return GarbageReceipt{}, fmt.Errorf("stage dead record id: %w", err)
			}
		}

		// Witness reclamation deletes the kernel:witness path entry before
		// its record row: path_entries FK-references records.
		witnessRows, err := tx.Query(
			"SELECT record_id FROM path_entries WHERE path_ref = ? AND record_id IN (SELECT record_id FROM dead_records)",
			witnessTraceOwnerID,
		)
		if err != nil {
			return GarbageReceipt{}, fmt.Errorf("query reclaimed witness entries: %w", err)
		}
		for witnessRows.Next() {
			var id string
			if err := witnessRows.Scan(&id); err != nil {
				witnessRows.Close()
				return GarbageReceipt{}, fmt.Errorf("scan reclaimed witness id: %w", err)
			}
			receipt.ReclaimedWitnessIDs = append(receipt.ReclaimedWitnessIDs, id)
		}
		witnessRows.Close()
		if err := witnessRows.Err(); err != nil {
			return GarbageReceipt{}, fmt.Errorf("iterate reclaimed witness entries: %w", err)
		}

		res, err := tx.Exec("DELETE FROM path_entries WHERE record_id IN (SELECT record_id FROM dead_records)")
		if err != nil {
			return GarbageReceipt{}, fmt.Errorf("delete reclaimed path entries: %w", err)
		}
		deletedEntries, _ := res.RowsAffected()
		receipt.DeletedWitnessEntries = int(deletedEntries)

		res, err = tx.Exec("DELETE FROM record_edges WHERE parent_record_id IN (SELECT record_id FROM dead_records) OR child_record_id IN (SELECT record_id FROM dead_records)")
		if err != nil {
			return GarbageReceipt{}, fmt.Errorf("delete edges of dead records: %w", err)
		}
		deletedEdges, _ := res.RowsAffected()
		receipt.DeletedRecordEdges = int(deletedEdges)

		if _, err := tx.Exec("DELETE FROM records WHERE record_id IN (SELECT record_id FROM dead_records)"); err != nil {
			return GarbageReceipt{}, fmt.Errorf("delete dead records: %w", err)
		}
	}

	// Orphaned contexts are no longer referenced by any surviving path entry.
	res, err := tx.Exec("DELETE FROM contexts WHERE context_id NOT IN (SELECT retained_context_ref FROM path_entries)")
	if err != nil {
		return GarbageReceipt{}, fmt.Errorf("delete orphaned contexts: %w", err)
	}
	deletedContexts, _ := res.RowsAffected()
	receipt.DeletedContexts = int(deletedContexts)

	// Leaf append intents: zero referents across records, contexts,
	// path_entries, and frontiers. Witness reclamation never deletes
	// intents directly — witnesses share the batch's intent with their
	// ordinary records, so reclamation stays purely refcount-based.
	res, err = tx.Exec(`DELETE FROM append_intents WHERE append_intent_id NOT IN (
		SELECT append_intent_id FROM records
		UNION SELECT append_intent_id FROM contexts
		UNION SELECT append_intent_id FROM path_entries
		UNION SELECT append_intent_id FROM frontiers)`)
	if err != nil {
		return GarbageReceipt{}, fmt.Errorf("delete leaf append intents: %w", err)
	}
	deletedIntents, _ := res.RowsAffected()
	receipt.DeletedAppendIntents = int(deletedIntents)

	receipt.DeletedRecords = len(dead)
	return receipt, nil
}

// sha256RecordIDPattern matches the kernel's record ids, which CanonicalDigest
// always renders as "sha256:<64 lowercase hex>". The snapshot scan extracts
// the sha256-shaped record ids found in each payload.
var sha256RecordIDPattern = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// markSnapshotLiveRecordsTx extends the live set with records whose ids
// appear in any checkpoint snapshot's opaque payload. The snapshot column
// carries the caller's encoded payload with no contract on it, so the scan
// is best-effort: one pass per snapshot extracting sha256-shaped ids, blind
// to ids an encoding splits across escapes. A caller whose blobs are
// binary-heavy must pin record ids itself (sidecar column or payload
// convention) if checkpoint-aware liveness must be exact.
func markSnapshotLiveRecordsTx(tx *sql.Tx, live map[string]bool, receipt *GarbageReceipt) error {
	rows, err := tx.Query("SELECT CAST(snapshot AS TEXT) FROM checkpoint_snapshots")
	if err != nil {
		return fmt.Errorf("scan checkpoint snapshots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return fmt.Errorf("scan checkpoint snapshot text: %w", err)
		}
		for _, id := range sha256RecordIDPattern.FindAllString(text, -1) {
			live[id] = true
		}
	}
	return rows.Err()
}

// checkWitnessCycles walks every surviving record's witness chain to the
// root. A chain that revisits a witness before reaching the root witness —
// or cites a record row that does not exist — is corruption: GC aborts
// rather than collecting through an invariant hole.
func checkWitnessCycles(recordWitness map[string]string, live map[string]bool) error {
	validated := make(map[string]bool)
	for id := range live {
		ref := recordWitness[id]
		if ref == "" || ref == RootWitnessRef || validated[ref] {
			continue
		}
		seen := make(map[string]bool)
		for ref != "" && ref != RootWitnessRef {
			if seen[ref] {
				return &TraceStoreError{fmt.Sprintf("witness chain cycle before root witness: %s", ref)}
			}
			if _, ok := recordWitness[ref]; !ok {
				return &TraceStoreError{fmt.Sprintf("witness ref does not resolve: %s", ref)}
			}
			seen[ref] = true
			ref = recordWitness[ref]
		}
		for seenRef := range seen {
			validated[seenRef] = true
		}
	}
	return nil
}
