package trace

// Frontier and cut machinery of the SQLite trace store: publish, resolve,\n// and the durable owner cutoff the runtime handles poll.

import (
	"context"
	"database/sql"
	"encoding/json"

	"fmt"

	"strconv"

	_ "modernc.org/sqlite"
)

// PublishFrontier publishes a retained owner-prefix frontier through the append path.
func (s *SQLiteTraceStore) PublishFrontier(ctx context.Context, auth AppendContext, spec FrontierSpec) (Frontier, error) {
	opCtx := auth.ToOperationContext(OpPublishCut)
	if err := ensureAppendAuthorized(opCtx); err != nil {
		return Frontier{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Frontier{}, fmt.Errorf("begin transaction: %w", err)
	}

	frontier, err := s.publishFrontierInTx(tx, opCtx, spec)
	if err != nil {
		tx.Rollback()
		return Frontier{}, err
	}

	if err := tx.Commit(); err != nil {
		return Frontier{}, fmt.Errorf("commit: %w", err)
	}
	return frontier, nil
}

// PublishCut is an alias for PublishFrontier.
func (s *SQLiteTraceStore) PublishCut(ctx context.Context, auth AppendContext, spec FrontierSpec) (Frontier, error) {
	return s.PublishFrontier(ctx, auth, spec)
}

// ResolveFrontier resolves a frontier into a graph-shaped trace slice.
func (s *SQLiteTraceStore) ResolveFrontier(ctx context.Context, auth ReadContext, frontierID string, modeFilter ModeFilter) (Slice, error) {
	opCtx := auth.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return Slice{}, err
	}
	if err := ensureModeFilter(modeFilter); err != nil {
		return Slice{}, err
	}

	frontier, err := s.readOwnerCutoff(ctx, frontierID)
	if err != nil {
		return Slice{}, err
	}

	through, err := s.readFactAtPath(ctx, frontier.ThroughFactID, frontier.TargetTraceOwnerID, frontier.ThroughOwnerOrdinal)
	if err != nil {
		return Slice{}, err
	}
	if through.View == nil || through.View.TraceOwnerID != frontier.TargetTraceOwnerID {
		return Slice{}, &TraceStoreError{"frontier through fact owner disagrees with target trace owner"}
	}
	if through.View.OwnerOrdinal != frontier.ThroughOwnerOrdinal {
		return Slice{}, &TraceStoreError{"frontier through fact ordinal changed"}
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT record_id, path_ref, path_ordinal FROM path_entries WHERE path_ref = ? AND path_ordinal <= ? ORDER BY path_ordinal ASC",
		frontier.TargetTraceOwnerID, frontier.ThroughOwnerOrdinal,
	)
	if err != nil {
		return Slice{}, fmt.Errorf("query frontier entries: %w", err)
	}
	defer rows.Close()

	var pathEntries []pathEntry
	for rows.Next() {
		var pe pathEntry
		if err := rows.Scan(&pe.recordID, &pe.pathRef, &pe.pathOrdinal); err != nil {
			return Slice{}, fmt.Errorf("scan path entry: %w", err)
		}
		pathEntries = append(pathEntries, pe)
	}

	return s.buildSlice(ctx, pathEntries, &frontier, opCtx.VisibilityProfile, modeFilter, true)
}

// ResolveCut is an alias for ResolveFrontier.
func (s *SQLiteTraceStore) ResolveCut(ctx context.Context, auth ReadContext, cutID string, modeFilter ModeFilter) (Slice, error) {
	return s.ResolveFrontier(ctx, auth, cutID, modeFilter)
}

// ReadOwnerCutoff reads a published frontier by ID.
func (s *SQLiteTraceStore) ReadOwnerCutoff(ctx context.Context, frontierID string) (Frontier, error) {
	return s.readOwnerCutoff(ctx, frontierID)
}

func (s *SQLiteTraceStore) publishFrontierInTx(tx *sql.Tx, ctx OperationContext, spec FrontierSpec) (Frontier, error) {
	if spec.FrontierID == "" {
		return Frontier{}, fmt.Errorf("frontier_id is required")
	}

	through, err := s.readLatestFactOnPathTx(tx, spec.ThroughFactID, spec.TargetTraceOwnerID)
	if err != nil {
		return Frontier{}, err
	}
	if through.View == nil || through.View.TraceOwnerID != spec.TargetTraceOwnerID {
		return Frontier{}, &TraceStoreError{"owner cutoff target disagrees with through fact owner"}
	}

	publisher := spec.PublisherOwnerID
	if publisher == "" {
		publisher = spec.TargetTraceOwnerID
	}
	throughOrdinal := through.View.OwnerOrdinal
	intent := spec.AppendIntentID
	if intent == "" {
		intent = fmt.Sprintf("frontier:%s", spec.FrontierID)
	}

	payload := map[string]any{
		"frontier_id":              spec.FrontierID,
		"target_trace_owner_id":    spec.TargetTraceOwnerID,
		"through_fact_id":          spec.ThroughFactID,
		"through_owner_ordinal":    throughOrdinal,
		"publisher_trace_owner_id": publisher,
	}

	causal := make([]string, 0, 1+len(spec.CausedBy))
	causal = append(causal, spec.ThroughFactID)
	causal = append(causal, spec.CausedBy...)

	retCtx := RetainedContext{
		ContextID:               fmt.Sprintf("ctx:%s:frontier", intent),
		CapabilityWitnessRefs:   ctx.PresentedAuthorityRefs,
		SemanticEnvironmentRefs: []string{ctx.SchemaEnvironmentRef},
		SubstrateRef:            "sqlite.local.v1",
		Containment:             ContainContained,
	}

	causedByFactIDs := make([]string, len(causal))
	copy(causedByFactIDs, causal)

	batch := AppendBatch{
		AppendIntentID: intent,
		Groups: []AppendGroup{{
			TraceOwnerID:    publisher,
			RetainedContext: &retCtx,
			CausalParents:   causedByFactIDs,
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: "shepherd2.frontier.owner_cutoff.v1",
				KindLabel: "frontier_published",
				Payload:   payload,
			}},
		}},
	}

	receipt, _, err := s.appendInTx(tx, ctx, batch)
	if err != nil {
		return Frontier{}, err
	}

	createdByFactID := receipt.FactIDs[0]
	frontier := Frontier{
		FrontierID:          spec.FrontierID,
		TargetTraceOwnerID:  spec.TargetTraceOwnerID,
		ThroughFactID:       spec.ThroughFactID,
		ThroughOwnerOrdinal: throughOrdinal,
		PublisherOwnerID:    publisher,
		CreatedByFactID:     createdByFactID,
	}

	// Check if frontier already exists
	existingFrontier, err := s.readOwnerCutoffTx(tx, spec.FrontierID)
	if err == nil {
		if existingFrontier.FrontierID != frontier.FrontierID ||
			existingFrontier.TargetTraceOwnerID != frontier.TargetTraceOwnerID ||
			existingFrontier.ThroughFactID != frontier.ThroughFactID ||
			existingFrontier.ThroughOwnerOrdinal != frontier.ThroughOwnerOrdinal {
			return Frontier{}, &TraceStoreError{fmt.Sprintf("frontier id %q already names a different cutoff", spec.FrontierID)}
		}
		return existingFrontier, nil
	}

	// Insert frontier
	_, err = tx.Exec(`
		INSERT INTO frontiers(frontier_id, target_trace_owner_id, through_fact_id, through_owner_ordinal,
			publisher_trace_owner_id, created_by_fact_id, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		frontier.FrontierID, frontier.TargetTraceOwnerID, frontier.ThroughFactID,
		frontier.ThroughOwnerOrdinal, frontier.PublisherOwnerID, frontier.CreatedByFactID, intent,
	)
	if err != nil {
		return Frontier{}, fmt.Errorf("insert frontier: %w", err)
	}

	return frontier, nil
}

func (s *SQLiteTraceStore) readOwnerCutoff(gctx context.Context, frontierID string) (Frontier, error) {
	row := s.db.QueryRowContext(gctx, "SELECT * FROM frontiers WHERE frontier_id = ?", frontierID)
	var f Frontier
	var publisherID, appendIntentID string
	err := row.Scan(&f.FrontierID, &f.TargetTraceOwnerID, &f.ThroughFactID,
		&f.ThroughOwnerOrdinal, &publisherID, &f.CreatedByFactID, &appendIntentID)
	if err == sql.ErrNoRows {
		return Frontier{}, &unknownFrontierError{frontierID: frontierID}
	}
	if err != nil {
		return Frontier{}, fmt.Errorf("scan frontier: %w", err)
	}
	f.PublisherOwnerID = publisherID

	// Verify against the retained frontier fact
	frontierFact, err := s.readFact(gctx, f.CreatedByFactID)
	if err != nil {
		return Frontier{}, err
	}

	payload := frontierFact.Body.Payload
	expected, err := frontierFromPayload(payload, f.CreatedByFactID)
	if err != nil {
		return Frontier{}, err
	}

	if f.FrontierID != expected.FrontierID ||
		f.TargetTraceOwnerID != expected.TargetTraceOwnerID ||
		f.ThroughFactID != expected.ThroughFactID ||
		f.ThroughOwnerOrdinal != expected.ThroughOwnerOrdinal {
		return Frontier{}, &TraceStoreError{"resolver index disagrees with retained frontier fact"}
	}

	return f, nil
}

// frontierFromPayload extracts a Frontier from a retained frontier fact's
// payload map. Returns an error if any required field is missing or has
// the wrong type — never silently uses a zero value.
func frontierFromPayload(payload map[string]any, createdByFactID string) (Frontier, error) {
	frontierID, ok := payload["frontier_id"].(string)
	if !ok || frontierID == "" {
		return Frontier{}, &TraceStoreError{"frontier fact missing or invalid frontier_id"}
	}
	targetOwner, ok := payload["target_trace_owner_id"].(string)
	if !ok || targetOwner == "" {
		return Frontier{}, &TraceStoreError{"frontier fact missing or invalid target_trace_owner_id"}
	}
	throughFact, ok := payload["through_fact_id"].(string)
	if !ok || throughFact == "" {
		return Frontier{}, &TraceStoreError{"frontier fact missing or invalid through_fact_id"}
	}
	throughOrdinal, err := intFromPayload(payload, "through_owner_ordinal")
	if err != nil {
		return Frontier{}, err
	}
	publisher, _ := payload["publisher_trace_owner_id"].(string) // optional

	return Frontier{
		FrontierID:          frontierID,
		TargetTraceOwnerID:  targetOwner,
		ThroughFactID:       throughFact,
		ThroughOwnerOrdinal: throughOrdinal,
		PublisherOwnerID:    publisher,
		CreatedByFactID:     createdByFactID,
	}, nil
}

// intFromPayload reads an integer field from a decoded payload.
//
// The value may be a json.Number, because bodies are decoded with UseNumber to
// preserve the int/float distinction the canonical writer needs, or a float64
// from a decoder that was not. Rejecting either would silently read an ordinal as
// zero.
func intFromPayload(payload map[string]any, key string) (int, error) {
	switch v := payload[key].(type) {
	case json.Number:
		n, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			return 0, &TraceStoreError{"frontier fact missing or invalid " + key}
		}
		return int(n), nil
	case float64:
		return int(v), nil
	default:
		return 0, &TraceStoreError{"frontier fact missing or invalid " + key}
	}
}

func (s *SQLiteTraceStore) readFrontierRow(gctx context.Context, frontierID string) (*sql.Row, error) {
	row := s.db.QueryRowContext(gctx, "SELECT * FROM frontiers WHERE frontier_id = ?", frontierID)
	return row, nil
}

func (s *SQLiteTraceStore) readLatestFactOnPath(gctx context.Context, factID, ownerID string) (Record, error) {
	row := s.db.QueryRowContext(gctx, `
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM path_entries
		JOIN records ON records.record_id = path_entries.record_id
		WHERE path_entries.record_id = ? AND path_entries.path_ref = ?
		ORDER BY path_entries.path_ordinal DESC LIMIT 1`, factID, ownerID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readOwnerCutoffTx(tx *sql.Tx, frontierID string) (Frontier, error) {
	row := tx.QueryRow("SELECT * FROM frontiers WHERE frontier_id = ?", frontierID)
	var f Frontier
	var publisherID, appendIntentID string
	err := row.Scan(&f.FrontierID, &f.TargetTraceOwnerID, &f.ThroughFactID,
		&f.ThroughOwnerOrdinal, &publisherID, &f.CreatedByFactID, &appendIntentID)
	if err == sql.ErrNoRows {
		return Frontier{}, &unknownFrontierError{frontierID: frontierID}
	}
	if err != nil {
		return Frontier{}, fmt.Errorf("scan frontier: %w", err)
	}
	f.PublisherOwnerID = publisherID
	return f, nil
}

func (s *SQLiteTraceStore) readLatestFactOnPathTx(tx *sql.Tx, factID, ownerID string) (Record, error) {
	row := tx.QueryRow(`
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM path_entries
		JOIN records ON records.record_id = path_entries.record_id
		WHERE path_entries.record_id = ? AND path_entries.path_ref = ?
		ORDER BY path_entries.path_ordinal DESC LIMIT 1`, factID, ownerID)
	return scanRecord(row)
}
