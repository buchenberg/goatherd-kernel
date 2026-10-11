package trace

// The append path of the SQLite trace store: the public Append and\n// PreviewRecordIDs API, the in-transaction orchestration (appendInTx,\n// prepareAppend), and the batch validation, digest, and causal-parent\n// checks it runs.

import (
	"context"
	"database/sql"
	"encoding/json"

	"fmt"

	"time"

	_ "modernc.org/sqlite"
)

// Append appends a semantic batch or returns the prior receipt for its intent.
func (s *SQLiteTraceStore) Append(ctx context.Context, auth AppendContext, batch AppendBatch) (AppendReceipt, error) {
	opCtx := auth.ToOperationContext(OpAppend)
	if err := ensureAppendAuthorized(opCtx); err != nil {
		return AppendReceipt{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AppendReceipt{}, fmt.Errorf("begin transaction: %w", err)
	}

	receipt, isNew, err := s.appendInTx(tx, opCtx, batch)
	if err != nil {
		tx.Rollback()
		return AppendReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return AppendReceipt{}, fmt.Errorf("commit: %w", err)
	}

	// Publish effect events after successful commit. Only for new appends,
	// not idempotent retries — the store is the durable record, the bus is
	// ephemeral. Idempotent appends return existing receipts without
	// inserting new records, so publishing would be a no-op at best.
	if isNew {
		s.publishAppendEvents(batch, receipt)
	}

	return receipt, nil
}

// publishAppendEvents publishes an EffectEvent for each retained record
// in a successfully committed batch. Called after tx.Commit() so the bus
// never blocks or fails the persistence path.
func (s *SQLiteTraceStore) publishAppendEvents(batch AppendBatch, receipt AppendReceipt) {
	if s.bus == nil {
		return
	}

	now := time.Now()
	factIdx := 0
	for _, group := range batch.Groups {
		for _, draft := range group.FactDrafts {
			if factIdx >= len(receipt.FactIDs) {
				return
			}
			event := EffectEvent{
				RecordID:      receipt.FactIDs[factIdx],
				IntentID:      batch.AppendIntentID,
				TraceOwnerID:  group.TraceOwnerID,
				Mode:          draft.Mode,
				SchemaRef:     draft.SchemaRef,
				KindLabel:     draft.KindLabel,
				Payload:       draft.Payload,
				CausalParents: append(group.CausalParents, draft.CausedByFactIDs...),
				Timestamp:     now,
			}
			s.bus.Publish(event)
			factIdx++
		}
	}
}

// PreviewRecordIDs returns the durable record IDs this batch would allocate if committed.
func (s *SQLiteTraceStore) PreviewRecordIDs(ctx context.Context, auth AppendContext, batch AppendBatch) ([]string, error) {
	opCtx := auth.ToOperationContext(OpAppend)
	if err := ensureAppendAuthorized(opCtx); err != nil {
		return nil, err
	}
	if err := validateBatchShape(batch, opCtx); err != nil {
		return nil, err
	}
	return s.previewFactIDs(ctx, batch, opCtx)
}

func (s *SQLiteTraceStore) appendInTx(tx *sql.Tx, ctx OperationContext, batch AppendBatch) (AppendReceipt, bool, error) {
	if err := validateBatchShape(batch, ctx); err != nil {
		return AppendReceipt{}, false, err
	}
	batchDigest, err := batchDigest(batch, ctx)
	if err != nil {
		return AppendReceipt{}, false, err
	}

	// Check idempotency
	var existingDigest string
	var existingReceiptJSON string
	err = tx.QueryRow(
		"SELECT batch_digest, receipt_json FROM append_intents WHERE append_intent_id = ?",
		batch.AppendIntentID,
	).Scan(&existingDigest, &existingReceiptJSON)
	if err == nil {
		if existingDigest != batchDigest && existingDigest != legacyBatchDigest(batch, ctx) {
			return AppendReceipt{}, false, &AppendIntentConflictError{
				fmt.Sprintf("append intent %q was already committed with different content", batch.AppendIntentID),
			}
		}
		receipt, err := receiptFromJSON(existingReceiptJSON)
		return receipt, false, err // false = idempotent, no new records
	}
	if err != sql.ErrNoRows {
		return AppendReceipt{}, false, fmt.Errorf("query append intent: %w", err)
	}

	// Prepare append
	contexts, witnessPlans, facts, receipt, err := s.prepareAppend(tx, batch, ctx)
	if err != nil {
		return AppendReceipt{}, false, err
	}

	receiptJSON, err := receiptToJSON(receipt)
	if err != nil {
		return AppendReceipt{}, false, err
	}

	_, err = tx.Exec(
		"INSERT INTO append_intents(append_intent_id, batch_digest, receipt_json) VALUES (?, ?, ?)",
		batch.AppendIntentID, batchDigest, receiptJSON,
	)
	if err != nil {
		return AppendReceipt{}, false, fmt.Errorf("insert append intent: %w", err)
	}

	for _, ctx := range contexts {
		if err := s.insertContextTx(tx, ctx, batch.AppendIntentID); err != nil {
			return AppendReceipt{}, false, err
		}
	}

	for _, plan := range witnessPlans {
		if err := s.insertWitnessRecordIfMissingTx(tx, plan, batch.AppendIntentID); err != nil {
			return AppendReceipt{}, false, err
		}
	}

	for i, fact := range facts {
		if err := s.insertFactRowTx(tx, fact, receipt.CommitReceipts[i], batch.AppendIntentID); err != nil {
			return AppendReceipt{}, false, err
		}
	}

	for owner, rng := range receipt.OwnerRanges {
		_, err := tx.Exec(
			`INSERT INTO owner_ordinals(trace_owner_id, next_ordinal) VALUES (?, ?)
			 ON CONFLICT(trace_owner_id) DO UPDATE SET next_ordinal = excluded.next_ordinal`,
			owner, rng[1]+1,
		)
		if err != nil {
			return AppendReceipt{}, false, fmt.Errorf("update owner ordinal: %w", err)
		}
	}

	if len(receipt.CommitReceipts) > 0 {
		nextSeq := nextCommitSeq(receipt.CommitReceipts)
		if _, err := tx.Exec("UPDATE meta SET value = ? WHERE key = 'next_commit_seq'", fmt.Sprintf("%d", nextSeq)); err != nil {
			return AppendReceipt{}, false, fmt.Errorf("update commit seq: %w", err)
		}
	}

	return receipt, true, nil // true = new append
}

func (s *SQLiteTraceStore) prepareAppend(tx *sql.Tx, batch AppendBatch, ctx OperationContext) (
	contexts []RetainedContext,
	witnessPlans []witnessPlan,
	facts []Record,
	receipt AppendReceipt,
	err error,
) {
	stagedIDs := make(map[string]bool)
	localFactIDs := make(map[string]string)
	stagedNext := make(map[string]int)

	// Resolve starting ordinals
	for _, group := range batch.Groups {
		owner := groupOwner(group)
		if _, ok := stagedNext[owner]; !ok {
			ord, err := s.nextOwnerOrdinalTx(tx, owner)
			if err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}
			stagedNext[owner] = ord
		}
	}

	var commitReceipts []string
	var causalEdges [][2]string
	var contextReceipts []string
	ownerRanges := make(map[string][2]int)
	witnessPlanMap := make(map[string]witnessPlan)
	var witnessPlanOrder []string

	// addWitnessPlan registers a plan, keeping first-seen order. Ranging over the
	// map to build the insert list, as this did, made the witness records' owner
	// ordinals depend on Go map iteration — so two identical appends could allocate
	// different ordinals, and the witness owner path read back in a different order
	// every time. Python's dict is insertion-ordered, so it never does.
	addWitnessPlan := func(plan witnessPlan) {
		if _, ok := witnessPlanMap[plan.recordID]; !ok {
			witnessPlanOrder = append(witnessPlanOrder, plan.recordID)
		}
		witnessPlanMap[plan.recordID] = plan
	}

	nextCommit, err := s.nextCommitSeqTx(tx)
	if err != nil {
		return nil, nil, nil, AppendReceipt{}, err
	}

	for groupIndex, group := range batch.Groups {
		owner := groupOwner(group)
		drafts := group.FactDrafts
		if len(drafts) == 0 {
			continue
		}

		// Resolve context
		retCtx, err := s.resolveGroupContextTx(tx, batch.AppendIntentID, groupIndex, group, ctx)
		if err != nil {
			return nil, nil, nil, AppendReceipt{}, err
		}
		if !s.contextExistsTx(tx, retCtx.ContextID) {
			contexts = append(contexts, retCtx)
		}
		contextReceipts = append(contextReceipts, retCtx.ContextID)

		// Resolve witness.
		//
		// The root witness is registered before this group's ordinary one, and both
		// keep first-seen order below. Python holds these in a dict and uses
		// setdefault in that same order (trace_store.py:480-481), and the order is
		// load-bearing: witnesses are inserted in it and that assignment is what
		// gives them their owner ordinals.
		addWitnessPlan(rootWitnessPlan())

		wp, err := ordinaryWitnessPlan(retCtx, ctx)
		if err != nil {
			return nil, nil, nil, AppendReceipt{}, err
		}
		addWitnessPlan(wp)

		start := stagedNext[owner]
		ordinal := start

		for _, draft := range drafts {
			causedBy, err := resolvedCauses(group, draft, localFactIDs)
			if err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}
			if err := s.validateCausalParentsTx(tx, causedBy, stagedIDs); err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}

			commitReceipt := fmt.Sprintf("commit:%d", nextCommit+len(facts))
			schemaRef, err := resolveSchemaRef(draft, ctx)
			if err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}

			factID, err := RecordDigest(schemaRef, draft.Mode, draft.Payload, causedBy, wp.recordID)
			if err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}

			if draft.AppendLocalID != "" {
				localFactIDs[draft.AppendLocalID] = factID
			}

			envelope := RecordEnvelope{
				RecordID:    factID,
				Digest:      factID,
				SchemaRef:   schemaRef,
				Mode:        draft.Mode,
				WitnessRef:  wp.recordID,
				CausedByIDs: causedBy,
			}
			kindLabel := draft.KindLabel
			if kindLabel == "" {
				kindLabel = schemaRef
			}
			view := &RecordView{
				TraceOwnerID: owner,
				OwnerOrdinal: ordinal,
				ContextRef:   retCtx.ContextID,
				KindLabel:    kindLabel,
			}
			body := RecordBody{Payload: CopyMap(draft.Payload)}
			fact := Record{Envelope: envelope, Body: body, View: view}

			facts = append(facts, fact)
			commitReceipts = append(commitReceipts, commitReceipt)

			for _, parent := range causedBy {
				causalEdges = append(causalEdges, [2]string{parent, factID})
			}

			stagedIDs[factID] = true
			ordinal++
		}

		stagedNext[owner] = ordinal
		rng, ok := ownerRanges[owner]
		if !ok {
			ownerRanges[owner] = [2]int{start, ordinal - 1}
		} else {
			ownerRanges[owner] = [2]int{rng[0], ordinal - 1}
		}
	}

	// Collect witness plans in first-seen order, which becomes their insertion
	// order and therefore their owner ordinals.
	for _, id := range witnessPlanOrder {
		if wp, ok := witnessPlanMap[id]; ok {
			witnessPlans = append(witnessPlans, wp)
		}
	}

	receipt = AppendReceipt{
		AppendIntentID:  batch.AppendIntentID,
		FactIDs:         factIDs(facts),
		CommitReceipts:  commitReceipts,
		OwnerRanges:     ownerRanges,
		CausalEdges:     causalEdges,
		ContextReceipts: contextReceipts,
	}
	return contexts, witnessPlans, facts, receipt, nil
}

func (s *SQLiteTraceStore) previewFactIDs(gctx context.Context, batch AppendBatch, ctx OperationContext) ([]string, error) {
	// Use the same context resolution as prepareAppend
	localFactIDs := make(map[string]string)
	var factIDs []string

	for groupIndex, group := range batch.Groups {
		drafts := group.FactDrafts
		if len(drafts) == 0 {
			continue
		}

		// Use same context resolution as prepareAppend
		retCtx, err := s.resolveGroupContext(gctx, batch.AppendIntentID, groupIndex, group, ctx)
		if err != nil {
			return nil, err
		}
		wp, err := ordinaryWitnessPlan(retCtx, ctx)
		if err != nil {
			return nil, err
		}

		for _, draft := range drafts {
			causedBy, err := resolvedCauses(group, draft, localFactIDs)
			if err != nil {
				return nil, err
			}
			schemaRef, err := resolveSchemaRef(draft, ctx)
			if err != nil {
				return nil, err
			}
			factID, err := RecordDigest(schemaRef, draft.Mode, draft.Payload, causedBy, wp.recordID)
			if err != nil {
				return nil, err
			}
			if draft.AppendLocalID != "" {
				localFactIDs[draft.AppendLocalID] = factID
			}
			factIDs = append(factIDs, factID)
		}
	}
	return factIDs, nil
}

func validateBatchShape(batch AppendBatch, ctx OperationContext) error {
	if batch.AppendIntentID == "" {
		return fmt.Errorf("append_intent_id is required")
	}
	seenLocalRefs := make(map[string]bool)
	for _, group := range batch.Groups {
		if group.TraceOwnerID == "" {
			return fmt.Errorf("trace_owner_id is required")
		}
		for _, draft := range group.FactDrafts {
			if draft.Mode != Capture && draft.Mode != Declaration {
				return fmt.Errorf("fact mode must be 'capture' or 'declaration'")
			}
			if draft.AppendLocalID != "" {
				if seenLocalRefs[draft.AppendLocalID] {
					return fmt.Errorf("duplicate append-local id: %s", draft.AppendLocalID)
				}
				seenLocalRefs[draft.AppendLocalID] = true
			}
			if _, err := resolveSchemaRef(draft, ctx); err != nil {
				return err
			}
			if _, err := json.Marshal(draft.Payload); err != nil {
				return fmt.Errorf("payload not JSON-serializable: %w", err)
			}
		}
	}
	return nil
}

func resolvedCauses(group AppendGroup, draft RecordDraft, localFactIDs map[string]string) ([]string, error) {
	var local []string
	for _, ref := range draft.CausedByLocalRefs {
		factID, ok := localFactIDs[ref]
		if !ok {
			return nil, &TraceStoreError{fmt.Sprintf("append-local causal parent does not exist: %s", ref)}
		}
		local = append(local, factID)
	}

	// Duplicates across the merged parent list are rejected, not deduplicated:
	// the same parent cited at both the group and the draft level is a caller
	// error, and deduping it would change the parent tuple that the record
	// digest — and therefore the record id — is taken over. Python rejects it
	// with ValueError("duplicate causal parent"); so does this.
	seen := make(map[string]bool)
	var result []string
	addParent := func(id string) error {
		if seen[id] {
			return fmt.Errorf("duplicate causal parent: %s", id)
		}
		seen[id] = true
		result = append(result, id)
		return nil
	}
	for _, id := range group.CausalParents {
		if err := addParent(id); err != nil {
			return nil, err
		}
	}
	for _, id := range draft.CausedByFactIDs {
		if err := addParent(id); err != nil {
			return nil, err
		}
	}
	for _, id := range local {
		if err := addParent(id); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *SQLiteTraceStore) validateCausalParents(gctx context.Context, causedBy []string, stagedIDs map[string]bool) error {
	for _, id := range causedBy {
		if stagedIDs[id] {
			continue
		}
		if !s.factExists(gctx, id) {
			return &UnknownFactError{fmt.Sprintf("causal parent does not exist: %s", id)}
		}
	}
	return nil
}

func resolveSchemaRef(draft RecordDraft, ctx OperationContext) (string, error) {
	if draft.SchemaRef != "" {
		return draft.SchemaRef, nil
	}
	return "", fmt.Errorf("schema_ref is required")
}

// legacyBatchDigest reproduces the pre-correction digest algorithm, which
// marshalled Go structs with encoding/json.
//
// It exists only to read rows written before the algorithm was aligned with
// Python, and is never stored. Without it, a database created by an earlier
// release would see every retry of an already-committed intent as a *different*
// batch — because the stored digest cannot be recomputed — and return
// AppendIntentConflictError instead of the stored receipt. That would break
// idempotent retry, which is the property the check exists to provide.
//
// It is safe to keep: accepting either digest means an old row can be read, while
// a row written now can only match the corrected value. Retire it when no
// supported database predates the correction.
func legacyBatchDigest(batch AppendBatch, ctx OperationContext) string {
	data := map[string]any{
		"append_intent_id": batch.AppendIntentID,
		"groups":           batch.Groups,
		"actor_ref":        ctx.ActorRef,
		"authority_refs":   ctx.PresentedAuthorityRefs,
		"schema_env":       ctx.SchemaEnvironmentRef,
		"trust_mode":       ctx.TrustMode,
	}
	b, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256Sum(b))
}

// batchDigest fingerprints an append batch for "same intent, different content"
// detection, matching shepherd2's `_batch_digest` field for field.
//
// It previously marshalled Go structs directly, so the JSON carried Go field names
// and declaration order rather than the sorted snake_case keys Python hashes. The
// two could never agree, and that matters beyond bookkeeping: the conflict check is
// part of the ABI, so a batch one store rejects as a conflicting reuse the other
// would accept.
func batchDigest(batch AppendBatch, ctx OperationContext) (string, error) {
	groups := make([]any, 0, len(batch.Groups))
	for _, g := range batch.Groups {
		drafts := make([]any, 0, len(g.FactDrafts))
		for _, d := range g.FactDrafts {
			schemaRef, err := resolveSchemaRef(d, ctx)
			if err != nil {
				return "", err
			}
			drafts = append(drafts, map[string]any{
				"append_local_id":      nullableString(d.AppendLocalID),
				"kind_label":           d.KindLabel,
				"mode":                 string(d.Mode),
				"schema_ref":           schemaRef,
				"payload":              d.Payload,
				"caused_by_fact_ids":   NonNilStrings(d.CausedByFactIDs),
				"caused_by_local_refs": NonNilStrings(d.CausedByLocalRefs),
			})
		}
		groups = append(groups, map[string]any{
			"trace_owner_id":   g.TraceOwnerID,
			"retained_context": contextDraftJSON(g.RetainedContext),
			"causal_parents":   NonNilStrings(g.CausalParents),
			"fact_drafts":      drafts,
		})
	}

	payload := map[string]any{
		"append_intent_id": batch.AppendIntentID,
		// Python's AppendBatch carries an atomicity field defaulting to "atomic";
		// the Go type only supports atomic batches, so it is fixed here.
		"atomicity":          "atomic",
		"actor_ref":          ctx.ActorRef,
		"schema_version_set": ctx.SchemaEnvironmentRef,
		"groups":             groups,
	}

	b, err := CanonicalJSONASCIIBytes(payload)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256Sum(b)), nil
}

func nextCommitSeq(receipts []string) int {
	maxSeq := 0
	for _, r := range receipts {
		var seq int
		fmt.Sscanf(r, "commit:%d", &seq)
		if seq >= maxSeq {
			maxSeq = seq + 1
		}
	}
	return maxSeq
}

func (s *SQLiteTraceStore) validateCausalParentsTx(tx *sql.Tx, causedBy []string, stagedIDs map[string]bool) error {
	for _, id := range causedBy {
		if stagedIDs[id] {
			continue
		}
		if !s.factExistsTx(tx, id) {
			return &UnknownFactError{fmt.Sprintf("causal parent does not exist: %s", id)}
		}
	}
	return nil
}

func ensureAppendAuthorized(ctx OperationContext) error {
	if ctx.TrustMode == "internal" {
		return nil
	}
	for _, ref := range ctx.PresentedAuthorityRefs {
		if ref == "trusted:internal" {
			return nil
		}
	}
	return &TraceStoreError{"Slice A append requires trusted internal witness"}
}
