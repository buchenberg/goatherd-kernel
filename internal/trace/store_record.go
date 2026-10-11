package trace

// Record-level persistence for the SQLite trace store: fact-row\n// insertion and idempotency, owner ordinals and commit sequences, and\n// the transaction-internal record reads the append path uses.

import (
	"context"
	"database/sql"

	"fmt"

	_ "modernc.org/sqlite"
)

func (s *SQLiteTraceStore) insertFactRow(gctx context.Context, fact Record, commitReceipt, appendIntentID string) error {
	if fact.View == nil {
		return &TraceStoreError{"path append requires record view metadata"}
	}
	if err := s.insertRecordIfMissing(gctx, fact, appendIntentID); err != nil {
		return err
	}

	_, err := s.db.ExecContext(gctx, `
		INSERT INTO path_entries(path_ref, path_ordinal, record_id, retained_context_ref, kind_label, append_intent_id, commit_receipt)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		fact.View.TraceOwnerID,
		fact.View.OwnerOrdinal,
		fact.Envelope.RecordID,
		fact.View.ContextRef,
		fact.View.KindLabel,
		appendIntentID,
		commitReceipt,
	)
	return err
}

func (s *SQLiteTraceStore) insertRecordIfMissing(gctx context.Context, fact Record, appendIntentID string) error {
	if fact.Envelope.RecordID != fact.Envelope.Digest {
		return &TraceStoreError{"record_id must equal digest"}
	}
	if s.factExists(gctx, fact.Envelope.RecordID) {
		existing, err := s.readRecord(gctx, fact.Envelope.RecordID)
		if err != nil {
			return err
		}
		if !recordContentMatches(existing, fact) {
			return &TraceStoreError{fmt.Sprintf("record id %q already names different content", fact.Envelope.RecordID)}
		}
		return nil
	}

	_, err := s.db.ExecContext(gctx, `
		INSERT INTO records(record_id, digest, schema_ref, mode, witness_ref, caused_by_json, body_json, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fact.Envelope.RecordID,
		fact.Envelope.Digest,
		fact.Envelope.SchemaRef,
		string(fact.Envelope.Mode),
		fact.Envelope.WitnessRef,
		stringToJSON(fact.Envelope.CausedByIDs),
		bodyToJSON(fact.Body),
		appendIntentID,
	)
	if err != nil {
		return err
	}

	for i, parent := range fact.Envelope.CausedByIDs {
		_, err := s.db.ExecContext(gctx,
			"INSERT INTO record_edges(parent_record_id, child_record_id, parent_position) VALUES (?, ?, ?)",
			parent, fact.Envelope.RecordID, i,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SQLiteTraceStore) factExists(gctx context.Context, factID string) bool {
	var count int
	s.db.QueryRowContext(gctx, "SELECT 1 FROM records WHERE record_id = ?", factID).Scan(&count)
	return count > 0
}

func (s *SQLiteTraceStore) nextOwnerOrdinal(gctx context.Context, ownerID string) (int, error) {
	var ordinal int
	err := s.db.QueryRowContext(gctx, "SELECT next_ordinal FROM owner_ordinals WHERE trace_owner_id = ?", ownerID).Scan(&ordinal)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query owner ordinal: %w", err)
	}
	return ordinal, nil
}

func (s *SQLiteTraceStore) nextCommitSeq(gctx context.Context) (int, error) {
	var seq int
	err := s.db.QueryRowContext(gctx, "SELECT value FROM meta WHERE key = 'next_commit_seq'").Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("query commit seq: %w", err)
	}
	return seq, nil
}

func scanRecord(row *sql.Row) (Record, error) {
	var r Record
	var causedByJSON, bodyJSON string
	var traceOwnerID, contextRef, kindLabel string
	var ownerOrdinal int

	err := row.Scan(
		&r.Envelope.RecordID, &r.Envelope.Digest, &r.Envelope.SchemaRef,
		&r.Envelope.Mode, &r.Envelope.WitnessRef, &causedByJSON, &bodyJSON,
		new(string), // append_intent_id
		&traceOwnerID, &ownerOrdinal, &contextRef, &kindLabel,
	)
	if err == sql.ErrNoRows {
		return Record{}, &UnknownFactError{"unknown fact"}
	}
	if err != nil {
		return Record{}, fmt.Errorf("scan record: %w", err)
	}

	r.Envelope.CausedByIDs = jsonToStrings(causedByJSON)
	body, err := bodyFromJSON(bodyJSON)
	if err != nil {
		return Record{}, fmt.Errorf("record %s: %w", r.Envelope.RecordID, err)
	}
	r.Body = body
	r.View = &RecordView{
		TraceOwnerID: traceOwnerID,
		OwnerOrdinal: ownerOrdinal,
		ContextRef:   contextRef,
		KindLabel:    kindLabel,
	}
	return r, nil
}

func factIDs(facts []Record) []string {
	ids := make([]string, len(facts))
	for i, f := range facts {
		ids[i] = f.Envelope.RecordID
	}
	return ids
}

func groupOwner(group AppendGroup) string {
	return group.TraceOwnerID
}

func (s *SQLiteTraceStore) readFactTx(tx *sql.Tx, factID string) (Record, error) {
	row := tx.QueryRow(`
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM records
		JOIN path_entries ON path_entries.record_id = records.record_id
		WHERE records.record_id = ?
		ORDER BY path_entries.path_ref ASC, path_entries.path_ordinal ASC
		LIMIT 1`, factID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readRecordTx(tx *sql.Tx, factID string) (Record, error) {
	row := tx.QueryRow(`
		SELECT records.*, '' AS trace_owner_id, -1 AS owner_ordinal,
		       '' AS retained_context_ref, '' AS kind_label
		FROM records WHERE records.record_id = ?`, factID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) factExistsTx(tx *sql.Tx, factID string) bool {
	var count int
	tx.QueryRow("SELECT 1 FROM records WHERE record_id = ?", factID).Scan(&count)
	return count > 0
}

func (s *SQLiteTraceStore) nextOwnerOrdinalTx(tx *sql.Tx, ownerID string) (int, error) {
	var ordinal int
	err := tx.QueryRow("SELECT next_ordinal FROM owner_ordinals WHERE trace_owner_id = ?", ownerID).Scan(&ordinal)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query owner ordinal: %w", err)
	}
	return ordinal, nil
}

func (s *SQLiteTraceStore) nextCommitSeqTx(tx *sql.Tx) (int, error) {
	var seq int
	err := tx.QueryRow("SELECT value FROM meta WHERE key = 'next_commit_seq'").Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("query commit seq: %w", err)
	}
	return seq, nil
}

func (s *SQLiteTraceStore) insertFactRowTx(tx *sql.Tx, fact Record, commitReceipt, appendIntentID string) error {
	if fact.View == nil {
		return &TraceStoreError{"path append requires record view metadata"}
	}
	if err := s.insertRecordIfMissingTx(tx, fact, appendIntentID); err != nil {
		return err
	}

	_, err := tx.Exec(`
		INSERT INTO path_entries(path_ref, path_ordinal, record_id, retained_context_ref, kind_label, append_intent_id, commit_receipt)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		fact.View.TraceOwnerID,
		fact.View.OwnerOrdinal,
		fact.Envelope.RecordID,
		fact.View.ContextRef,
		fact.View.KindLabel,
		appendIntentID,
		commitReceipt,
	)
	return err
}

func (s *SQLiteTraceStore) insertRecordIfMissingTx(tx *sql.Tx, fact Record, appendIntentID string) error {
	if fact.Envelope.RecordID != fact.Envelope.Digest {
		return &TraceStoreError{"record_id must equal digest"}
	}
	if s.factExistsTx(tx, fact.Envelope.RecordID) {
		existing, err := s.readRecordTx(tx, fact.Envelope.RecordID)
		if err != nil {
			return err
		}
		if !recordContentMatches(existing, fact) {
			return &TraceStoreError{fmt.Sprintf("record id %q already names different content", fact.Envelope.RecordID)}
		}
		return nil
	}

	_, err := tx.Exec(`
		INSERT INTO records(record_id, digest, schema_ref, mode, witness_ref, caused_by_json, body_json, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fact.Envelope.RecordID,
		fact.Envelope.Digest,
		fact.Envelope.SchemaRef,
		string(fact.Envelope.Mode),
		fact.Envelope.WitnessRef,
		stringToJSON(fact.Envelope.CausedByIDs),
		bodyToJSON(fact.Body),
		appendIntentID,
	)
	if err != nil {
		return err
	}

	for i, parent := range fact.Envelope.CausedByIDs {
		_, err := tx.Exec(
			"INSERT INTO record_edges(parent_record_id, child_record_id, parent_position) VALUES (?, ?, ?)",
			parent, fact.Envelope.RecordID, i,
		)
		if err != nil {
			return err
		}
	}

	return nil
}
