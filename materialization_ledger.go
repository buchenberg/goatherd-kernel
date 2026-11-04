package trace

// Completed-intent ledger for materialization (store-private table).
//
// The ledger write is the idempotency commit point for the materialize
// transition. These are package functions taking the store, deliberately
// not methods on SQLiteTraceStore: the root package re-exports the store
// type, and a method would surface ledger mutation in the public API.
//
// The ledger is deliberately opaque (d7c): the materialization vocabulary —
// outcome, result, receipt types — lives in the runtime's materialize
// orchestrator, which serializes its receipts to canonical receipt_json and
// stores the bytes here. The kernel compares request digests and decodes
// nothing; the JSON shape stays cross-readable with the Python reference by
// the orchestrator's serialization contract.

import (
	"context"
	"database/sql"
	"fmt"
)

// NewTraceStoreError constructs a TraceStoreError with the given message.
// Exported for the runtime layer, which raises store-shaped errors from
// outside this package.
func NewTraceStoreError(msg string) *TraceStoreError { return &TraceStoreError{msg: msg} }

// NewAppendIntentConflictError constructs an AppendIntentConflictError.
// Exported for the runtime layer.
func NewAppendIntentConflictError(msg string) *AppendIntentConflictError {
	return &AppendIntentConflictError{msg: msg}
}

// ReadCompletedMaterialization looks up the ledger for a materialize intent,
// returning the stored request digest and the receipt JSON exactly as the
// completer serialized it. The store lock is held only around the query,
// mirroring Python's _read_completed_intent; no lock is held across the
// substrate call or the capture append in the orchestrator.
func ReadCompletedMaterialization(store *SQLiteTraceStore, intentID string) (digest string, receiptJSON string, found bool, err error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	err = store.db.QueryRow(
		"SELECT request_digest, receipt_json FROM materialization_intents WHERE materialize_intent_id = ?",
		intentID,
	).Scan(&digest, &receiptJSON)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("query materialization intent: %w", err)
	}
	return digest, receiptJSON, true, nil
}

// RecordCompletedMaterialization writes the ledger entry — the idempotency
// commit point. receiptJSON is the serialized receipt in the completer's own
// vocabulary. The insert races only with a concurrent completion of the
// same intent; INSERT OR IGNORE plus a digest comparison decides replays the
// same way Python's IntegrityError branch does, without matching driver
// error strings.
func RecordCompletedMaterialization(store *SQLiteTraceStore, intentID, digest, receiptJSON string) error {
	store.mu.Lock()
	defer store.mu.Unlock()

	res, err := store.db.Exec(
		"INSERT OR IGNORE INTO materialization_intents(materialize_intent_id, request_digest, receipt_json) VALUES (?, ?, ?)",
		intentID, digest, receiptJSON,
	)
	if err != nil {
		return fmt.Errorf("record materialization intent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}

	// The row exists: this intent was completed concurrently. Same digest
	// means the concurrent receipt describes the same transition — the
	// store-level intent idempotency guarantees identical produced ids —
	// so replay is a silent success. Different content is a conflict.
	var existingDigest string
	err = store.db.QueryRow(
		"SELECT request_digest FROM materialization_intents WHERE materialize_intent_id = ?",
		intentID,
	).Scan(&existingDigest)
	if err != nil {
		return fmt.Errorf("re-read materialization intent: %w", err)
	}
	if existingDigest != digest {
		return &AppendIntentConflictError{
			fmt.Sprintf("materialize intent %q was already committed with different content", intentID),
		}
	}
	return nil
}

// DeleteMaterializationIntent removes a completed-materialization ledger row.
// It exists so tests can simulate the crash window between the capture append
// and the ledger write; it is test support for the runtime orchestrator, not
// a store operation.
func DeleteMaterializationIntent(gctx context.Context, store *SQLiteTraceStore, intentID string) error {
	_, err := store.db.ExecContext(gctx, `DELETE FROM materialization_intents WHERE materialize_intent_id = ?`, intentID)
	if err != nil {
		return fmt.Errorf("delete materialization intent: %w", err)
	}
	return nil
}
