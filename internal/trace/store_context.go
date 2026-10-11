package trace

// Retained-context management for the SQLite trace store: group-context
// resolution, persistence, and the context ids and digests the reference
// store allocates the same way.

import (
	"context"
	"database/sql"
	"encoding/json"

	"fmt"

	"strconv"

	_ "modernc.org/sqlite"
)

func (s *SQLiteTraceStore) resolveGroupContext(
	gctx context.Context, appendIntentID string, groupIndex int, group AppendGroup, ctx OperationContext,
) (RetainedContext, error) {
	if group.RetainedContext != nil && group.RetainedContext.ContextID != "" {
		// Check if this is a reuse request
		if group.RetainedContext.SubstrateRef == "" {
			// It's a reuse reference
			return s.readContext(gctx, group.RetainedContext.ContextID)
		}
	}

	draft := defaultContextDraft()
	if group.RetainedContext != nil {
		draft = *group.RetainedContext
	}

	payload := contextPayload(draft, ctx)
	contextID, err := contextIDFor(appendIntentID, groupIndex, payload)
	if err != nil {
		return RetainedContext{}, err
	}

	newCtx := RetainedContext{
		ContextID:               contextID,
		ActiveBindingRefs:       payload.ActiveBindingRefs,
		CapabilityWitnessRefs:   payload.CapabilityWitnessRefs,
		SemanticEnvironmentRefs: payload.SemanticEnvironmentRefs,
		VisibilityPolicyRefs:    payload.VisibilityPolicyRefs,
		SubstrateRef:            payload.SubstrateRef,
		Containment:             payload.Containment,
	}

	existing, err := s.readContext(gctx, contextID)
	if err == nil {
		if !contextEqual(existing, newCtx) {
			return RetainedContext{}, &TraceStoreError{fmt.Sprintf("context id %q already names a different context", contextID)}
		}
		return existing, nil
	}

	return newCtx, nil
}

func (s *SQLiteTraceStore) contextExists(gctx context.Context, contextID string) bool {
	var count int
	s.db.QueryRowContext(gctx, "SELECT 1 FROM contexts WHERE context_id = ?", contextID).Scan(&count)
	return count > 0
}

func (s *SQLiteTraceStore) readContext(gctx context.Context, contextID string) (RetainedContext, error) {
	row := s.db.QueryRowContext(gctx, "SELECT * FROM contexts WHERE context_id = ?", contextID)
	var ctx RetainedContext
	var activeJSON, capJSON, semJSON, visJSON string
	err := row.Scan(&ctx.ContextID, &activeJSON, &capJSON, &semJSON, &visJSON,
		&ctx.SubstrateRef, &ctx.Containment, new(string))
	if err == sql.ErrNoRows {
		return RetainedContext{}, &TraceStoreError{fmt.Sprintf("unknown context id: %s", contextID)}
	}
	if err != nil {
		return RetainedContext{}, fmt.Errorf("scan context: %w", err)
	}
	ctx.ActiveBindingRefs = jsonToStrings(activeJSON)
	ctx.CapabilityWitnessRefs = jsonToStrings(capJSON)
	ctx.SemanticEnvironmentRefs = jsonToStrings(semJSON)
	ctx.VisibilityPolicyRefs = jsonToStrings(visJSON)
	return ctx, nil
}

func (s *SQLiteTraceStore) insertContext(gctx context.Context, rc RetainedContext, appendIntentID string) error {
	_, err := s.db.ExecContext(gctx, `
		INSERT INTO contexts(context_id, active_binding_refs_json, capability_witness_refs_json,
			semantic_environment_refs_json, visibility_policy_refs_json, substrate_ref, containment, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rc.ContextID,
		stringToJSON(rc.ActiveBindingRefs),
		stringToJSON(rc.CapabilityWitnessRefs),
		stringToJSON(rc.SemanticEnvironmentRefs),
		stringToJSON(rc.VisibilityPolicyRefs),
		rc.SubstrateRef,
		string(rc.Containment),
		appendIntentID,
	)
	return err
}

func defaultContextDraft() RetainedContext {
	return RetainedContext{
		SubstrateRef: "sqlite.local.v1",
		Containment:  ContainContained,
	}
}

func contextPayload(draft RetainedContext, ctx OperationContext) RetainedContext {
	semEnv := append([]string{}, draft.SemanticEnvironmentRefs...)
	semEnv = append(semEnv, ctx.SchemaEnvironmentRef)
	semEnv = deduplicate(semEnv)

	return RetainedContext{
		ActiveBindingRefs:       append([]string{}, draft.ActiveBindingRefs...),
		CapabilityWitnessRefs:   append([]string{}, draft.CapabilityWitnessRefs...),
		SemanticEnvironmentRefs: semEnv,
		VisibilityPolicyRefs:    append([]string{}, draft.VisibilityPolicyRefs...),
		SubstrateRef:            draft.SubstrateRef,
		Containment:             draft.Containment,
	}
}

// contextIDFor derives a retained context id the way shepherd2's `_context_id`
// does:
//
//	digest = sha256(f"{intent}\0{group_index}\0{json}").hexdigest()
//	return f"context:{digest[:32]}"
//
// where `json` is the context payload on its own — not wrapped in an object — and
// is encoded with ensure_ascii=True. All three details were wrong before:
//
//   - the input is a NUL-joined *string*, not a JSON object of intent/group/payload;
//   - the result is the first 32 hex characters with a "context:" prefix, not all
//     64 with "ctx:";
//   - the encoder is the ASCII flavour, because `_json_dumps` omits ensure_ascii.
//
// With an ASCII-only payload the last point is invisible, which is why this
// survived: the ids were simply always different, and nothing compared them.
func contextIDFor(appendIntentID string, groupIndex int, payload RetainedContext) (string, error) {
	body, err := CanonicalJSONASCIIBytes(map[string]any{
		"active_binding_refs":       payload.ActiveBindingRefs,
		"capability_witness_refs":   payload.CapabilityWitnessRefs,
		"semantic_environment_refs": payload.SemanticEnvironmentRefs,
		"visibility_policy_refs":    payload.VisibilityPolicyRefs,
		"substrate_ref":             payload.SubstrateRef,
		"containment":               string(payload.Containment),
	})
	if err != nil {
		return "", fmt.Errorf("containerd context id: %w", err)
	}
	joined := append([]byte(appendIntentID), 0)
	joined = strconv.AppendInt(joined, int64(groupIndex), 10)
	joined = append(joined, 0)
	joined = append(joined, body...)

	digest := fmt.Sprintf("%x", sha256Sum(joined))
	return "context:" + digest[:32], nil
}

func contextEqual(a, b RetainedContext) bool {
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return string(aJSON) == string(bJSON)
}

// contextDraftJSON renders a group's retained-context reference the way Python's
// `_context_draft_to_json` does, including `RetainedContextDraft`'s defaults.
//
// Python distinguishes three cases in `_context_draft_for_group` — an explicit
// draft, a reuse reference by context id, or nothing at all — and the "nothing"
// case still carries the defaults (substrate_ref "sqlite.local.v1", containment
// "contained"). Go represents all three with one pointer, so which one it is has to
// be inferred the same way the rest of the store infers it: an id with no substrate
// ref is a reuse reference.
func contextDraftJSON(rc *RetainedContext) map[string]any {
	defaults := defaultContextDraft()
	draft := map[string]any{
		"active_binding_refs":       []string{},
		"capability_witness_refs":   []string{},
		"semantic_environment_refs": []string{},
		"visibility_policy_refs":    []string{},
		"substrate_ref":             defaults.SubstrateRef,
		"containment":               string(defaults.Containment),
		"reuse_context_id":          nil,
	}
	if rc == nil {
		return draft
	}
	if rc.ContextID != "" && rc.SubstrateRef == "" {
		draft["reuse_context_id"] = rc.ContextID
		return draft
	}
	draft["active_binding_refs"] = NonNilStrings(rc.ActiveBindingRefs)
	draft["capability_witness_refs"] = NonNilStrings(rc.CapabilityWitnessRefs)
	draft["semantic_environment_refs"] = NonNilStrings(rc.SemanticEnvironmentRefs)
	draft["visibility_policy_refs"] = NonNilStrings(rc.VisibilityPolicyRefs)
	draft["substrate_ref"] = rc.SubstrateRef
	draft["containment"] = string(rc.Containment)
	return draft
}

func (s *SQLiteTraceStore) resolveGroupContextTx(
	tx *sql.Tx, appendIntentID string, groupIndex int, group AppendGroup, ctx OperationContext,
) (RetainedContext, error) {
	if group.RetainedContext != nil && group.RetainedContext.ContextID != "" {
		if group.RetainedContext.SubstrateRef == "" {
			return s.readContextTx(tx, group.RetainedContext.ContextID)
		}
	}

	draft := defaultContextDraft()
	if group.RetainedContext != nil {
		draft = *group.RetainedContext
	}

	payload := contextPayload(draft, ctx)
	contextID, err := contextIDFor(appendIntentID, groupIndex, payload)
	if err != nil {
		return RetainedContext{}, err
	}

	newCtx := RetainedContext{
		ContextID:               contextID,
		ActiveBindingRefs:       payload.ActiveBindingRefs,
		CapabilityWitnessRefs:   payload.CapabilityWitnessRefs,
		SemanticEnvironmentRefs: payload.SemanticEnvironmentRefs,
		VisibilityPolicyRefs:    payload.VisibilityPolicyRefs,
		SubstrateRef:            payload.SubstrateRef,
		Containment:             payload.Containment,
	}

	existing, err := s.readContextTx(tx, contextID)
	if err == nil {
		if !contextEqual(existing, newCtx) {
			return RetainedContext{}, &TraceStoreError{fmt.Sprintf("context id %q already names a different context", contextID)}
		}
		return existing, nil
	}

	return newCtx, nil
}

func (s *SQLiteTraceStore) contextExistsTx(tx *sql.Tx, contextID string) bool {
	var count int
	tx.QueryRow("SELECT 1 FROM contexts WHERE context_id = ?", contextID).Scan(&count)
	return count > 0
}

func (s *SQLiteTraceStore) readContextTx(tx *sql.Tx, contextID string) (RetainedContext, error) {
	row := tx.QueryRow("SELECT * FROM contexts WHERE context_id = ?", contextID)
	var ctx RetainedContext
	var activeJSON, capJSON, semJSON, visJSON string
	err := row.Scan(&ctx.ContextID, &activeJSON, &capJSON, &semJSON, &visJSON,
		&ctx.SubstrateRef, &ctx.Containment, new(string))
	if err == sql.ErrNoRows {
		return RetainedContext{}, &TraceStoreError{fmt.Sprintf("unknown context id: %s", contextID)}
	}
	if err != nil {
		return RetainedContext{}, fmt.Errorf("scan context: %w", err)
	}
	ctx.ActiveBindingRefs = jsonToStrings(activeJSON)
	ctx.CapabilityWitnessRefs = jsonToStrings(capJSON)
	ctx.SemanticEnvironmentRefs = jsonToStrings(semJSON)
	ctx.VisibilityPolicyRefs = jsonToStrings(visJSON)
	return ctx, nil
}

func (s *SQLiteTraceStore) insertContextTx(tx *sql.Tx, ctx RetainedContext, appendIntentID string) error {
	_, err := tx.Exec(`
		INSERT INTO contexts(context_id, active_binding_refs_json, capability_witness_refs_json,
			semantic_environment_refs_json, visibility_policy_refs_json, substrate_ref, containment, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ctx.ContextID,
		stringToJSON(ctx.ActiveBindingRefs),
		stringToJSON(ctx.CapabilityWitnessRefs),
		stringToJSON(ctx.SemanticEnvironmentRefs),
		stringToJSON(ctx.VisibilityPolicyRefs),
		ctx.SubstrateRef,
		string(ctx.Containment),
		appendIntentID,
	)
	return err
}
