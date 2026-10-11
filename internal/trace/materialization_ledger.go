package trace

// Completed-intent ledger for materialization (store-private table).
//
// The ledger write is the idempotency commit point for the materialize
// transition. These are package functions taking the store, deliberately
// not methods on SQLiteTraceStore: the root package re-exports the store
// type, and a method would surface ledger mutation in the public API. As
// internal/ package functions they stay invisible to importers outside
// the module.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// materializationReceiptJSON serializes a receipt for the ledger the way
// Python's _receipt_to_json does: sorted keys, compact separators, the
// ASCII flavour.
func materializationReceiptJSON(receipt MaterializationReceipt) (string, error) {
	body, err := CanonicalJSONASCIIBytes(map[string]any{
		"outcome":             string(receipt.Outcome),
		"substrate_ref":       receipt.SubstrateRef,
		"target_record_ids":   NonNilStrings(receipt.TargetRecordIDs),
		"produced_record_ids": NonNilStrings(receipt.ProducedRecordIDs),
		"failure_reason":      receipt.FailureReason,
		"world_side_anchors":  NonNilAnchors(receipt.WorldSideAnchors),
	})
	if err != nil {
		return "", fmt.Errorf("materialization receipt encode: %w", err)
	}
	return string(body), nil
}

func materializationReceiptFromJSON(payload string) (MaterializationReceipt, error) {
	var stored struct {
		Outcome           string           `json:"outcome"`
		SubstrateRef      string           `json:"substrate_ref"`
		TargetRecordIDs   []string         `json:"target_record_ids"`
		ProducedRecordIDs []string         `json:"produced_record_ids"`
		FailureReason     string           `json:"failure_reason"`
		WorldSideAnchors  []map[string]any `json:"world_side_anchors"`
	}
	// UseNumber, matching retained-body reads: without it a numeric anchor
	// value would decode as float64 while a fresh receipt (and Python's
	// json.loads) holds the exact number, so the replay comparison in the
	// orchestrator would silently stop holding.
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&stored); err != nil {
		return MaterializationReceipt{}, fmt.Errorf("materialization receipt decode: %w", err)
	}
	return MaterializationReceipt{
		Outcome:           MaterializationOutcome(stored.Outcome),
		SubstrateRef:      stored.SubstrateRef,
		TargetRecordIDs:   NonNilStrings(stored.TargetRecordIDs),
		ProducedRecordIDs: NonNilStrings(stored.ProducedRecordIDs),
		FailureReason:     stored.FailureReason,
		WorldSideAnchors:  NonNilAnchors(stored.WorldSideAnchors),
	}, nil
}

// NonNilAnchors returns an empty (non-nil) slice for nil input, so a receipt
// without anchors serializes as [] rather than null, matching Python's tuple.
func NonNilAnchors(anchors []map[string]any) []map[string]any {
	if anchors == nil {
		return []map[string]any{}
	}
	return anchors
}

// NewTraceStoreError constructs a TraceStoreError with the given message.
// Exported for the runtime layer, which raises store-shaped errors from
// outside this package.
func NewTraceStoreError(msg string) *TraceStoreError { return &TraceStoreError{msg: msg} }

// NewAppendIntentConflictError constructs an AppendIntentConflictError.
// Exported for the runtime layer.
func NewAppendIntentConflictError(msg string) *AppendIntentConflictError {
	return &AppendIntentConflictError{msg: msg}
}

// ReadCompletedMaterialization looks up the ledger for a materialize intent.
// The store lock is held only around the query, mirroring Python's
// _read_completed_intent; no lock is held across the substrate call or the
// capture append in the orchestrator.
func ReadCompletedMaterialization(store *SQLiteTraceStore, intentID string) (string, MaterializationReceipt, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var digest, receiptJSON string
	err := store.db.QueryRow(
		"SELECT request_digest, receipt_json FROM materialization_intents WHERE materialize_intent_id = ?",
		intentID,
	).Scan(&digest, &receiptJSON)
	if err == sql.ErrNoRows {
		return "", MaterializationReceipt{}, false, nil
	}
	if err != nil {
		return "", MaterializationReceipt{}, false, fmt.Errorf("query materialization intent: %w", err)
	}
	receipt, err := materializationReceiptFromJSON(receiptJSON)
	if err != nil {
		return "", MaterializationReceipt{}, false, err
	}
	return digest, receipt, true, nil
}

// RecordCompletedMaterialization writes the ledger entry — the idempotency
// commit point. The insert races only with a concurrent completion of the
// same intent; INSERT OR IGNORE plus a digest comparison decides replays the
// same way Python's IntegrityError branch does, without matching driver
// error strings.
func RecordCompletedMaterialization(store *SQLiteTraceStore, intentID, digest string, receipt MaterializationReceipt) error {
	receiptJSON, err := materializationReceiptJSON(receipt)
	if err != nil {
		return err
	}

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
// and the ledger write; internal/ keeps it invisible outside the module.
func DeleteMaterializationIntent(gctx context.Context, store *SQLiteTraceStore, intentID string) error {
	_, err := store.db.ExecContext(gctx, `DELETE FROM materialization_intents WHERE materialize_intent_id = ?`, intentID)
	if err != nil {
		return fmt.Errorf("delete materialization intent: %w", err)
	}
	return nil
}
