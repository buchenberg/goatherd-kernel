package trace

// Witness machinery of the SQLite trace store: witness plans, the\n// witness-support closure, chain validation to the root, and witness\n// record insertion.

import (
	"context"
	"database/sql"
	"encoding/json"

	"fmt"

	_ "modernc.org/sqlite"
)

type witnessPlan struct {
	recordID   string
	schemaRef  string
	kindLabel  string
	body       map[string]any
	witnessRef string
}

// witnessSupport accumulates the witness records a slice depends on, remembering
// the order each was first encountered.
//
// The order matters and is not incidental: it becomes Slice.WitnessAnchors, and
// Python builds the equivalent structure as a dict — insertion-ordered — then
// converts it to a tuple. Returning these records from a bare map range, as this
// did, made the anchor order differ on every read.
type witnessSupport struct {
	byID  map[string]Record
	order []string
}

func newWitnessSupport() *witnessSupport {
	return &witnessSupport{byID: make(map[string]Record)}
}

func (w *witnessSupport) add(rec Record) {
	id := rec.Envelope.RecordID
	if _, exists := w.byID[id]; !exists {
		w.order = append(w.order, id)
	}
	w.byID[id] = rec
}

func (w *witnessSupport) get(id string) (Record, bool) {
	rec, ok := w.byID[id]
	return rec, ok
}

// records returns the witnesses in first-seen order.
func (w *witnessSupport) records() []Record {
	out := make([]Record, 0, len(w.byID))
	for _, id := range w.order {
		if rec, ok := w.byID[id]; ok {
			out = append(out, rec)
		}
	}
	return out
}

func (s *SQLiteTraceStore) witnessSupportClosure(gctx context.Context, records []Record) ([]Record, error) {
	support := newWitnessSupport()
	validatedToRoot := make(map[string]bool)

	for _, record := range records {
		if record.Envelope.WitnessRef == "" && record.Envelope.SchemaRef != RootWitnessSchemaRef {
			return nil, &TraceStoreError{fmt.Sprintf("non-root record has empty witness_ref: %s", record.Envelope.RecordID)}
		}
	}

	for _, record := range records {
		if record.Envelope.WitnessRef != "" {
			if err := s.validateWitnessChain(gctx, record.Envelope.WitnessRef, support, validatedToRoot); err != nil {
				return nil, err
			}
		}
	}

	return support.records(), nil
}

func (s *SQLiteTraceStore) validateWitnessChain(gctx context.Context, startRef string, support *witnessSupport, validatedToRoot map[string]bool) error {
	seenInChain := make(map[string]bool)
	witnessRef := startRef

	for {
		if validatedToRoot[witnessRef] {
			return nil
		}
		if seenInChain[witnessRef] {
			return &TraceStoreError{fmt.Sprintf("witness chain cycle before root witness: %s", witnessRef)}
		}
		seenInChain[witnessRef] = true

		witness, ok := support.get(witnessRef)
		if !ok {
			var err error
			witness, err = s.readFact(gctx, witnessRef)
			if err != nil {
				return &TraceStoreError{fmt.Sprintf("witness ref does not resolve: %s", witnessRef)}
			}
			if witness.Envelope.SchemaRef != RootWitnessSchemaRef && witness.Envelope.SchemaRef != WitnessSchemaRef {
				return &TraceStoreError{fmt.Sprintf("witness_ref does not resolve to a witness record: %s", witnessRef)}
			}
			support.add(witness)
		}

		if witness.Envelope.SchemaRef != RootWitnessSchemaRef && witness.Envelope.SchemaRef != WitnessSchemaRef {
			return &TraceStoreError{fmt.Sprintf("witness_ref does not resolve to a witness record: %s", witnessRef)}
		}
		if witness.Envelope.SchemaRef == RootWitnessSchemaRef {
			if witness.Envelope.WitnessRef != RootWitnessRef {
				return &TraceStoreError{"root witness record must use the empty witness sentinel"}
			}
			for ref := range seenInChain {
				validatedToRoot[ref] = true
			}
			return nil
		}
		if witness.Envelope.WitnessRef == "" {
			return &TraceStoreError{fmt.Sprintf("non-root witness has empty witness_ref: %s", witnessRef)}
		}
		witnessRef = witness.Envelope.WitnessRef
	}
}

func (s *SQLiteTraceStore) insertWitnessRecordIfMissing(gctx context.Context, plan witnessPlan, appendIntentID string) error {
	if s.factExists(gctx, plan.recordID) {
		existing, err := s.readFact(gctx, plan.recordID)
		if err != nil {
			return err
		}
		if !witnessFactMatchesPlan(existing, plan) {
			return &TraceStoreError{fmt.Sprintf("witness record id %q already names a different witness", plan.recordID)}
		}
		return nil
	}

	ordinal, err := s.nextOwnerOrdinal(gctx, witnessTraceOwnerID)
	if err != nil {
		return err
	}

	fact := Record{
		Envelope: RecordEnvelope{
			RecordID:   plan.recordID,
			Digest:     plan.recordID,
			SchemaRef:  plan.schemaRef,
			Mode:       Capture,
			WitnessRef: plan.witnessRef,
		},
		Body: RecordBody{Payload: CopyMap(plan.body)},
		View: &RecordView{
			TraceOwnerID: witnessTraceOwnerID,
			OwnerOrdinal: ordinal,
			KindLabel:    plan.kindLabel,
		},
	}

	if err := s.insertFactRow(gctx, fact, fmt.Sprintf("witness:%s", plan.recordID), appendIntentID); err != nil {
		return err
	}

	_, err = s.db.ExecContext(gctx,
		`INSERT INTO owner_ordinals(trace_owner_id, next_ordinal) VALUES (?, ?)
		 ON CONFLICT(trace_owner_id) DO UPDATE SET next_ordinal = excluded.next_ordinal`,
		witnessTraceOwnerID, ordinal+1,
	)
	return err
}

// ordinaryWitnessPlan builds the witness a group's records cite.
//
// authority_refs comes from the *retained context's* capability_witness_refs, not
// from the append context's presented refs. Python does the same
// (trace_store.py `_ordinary_witness_plan`), and getting it wrong ties the witness
// id — and therefore every record id that cites it — to the caller's credentials:
// the same content appended by a different trusted caller produced a different
// record id, which Python does not do.
func ordinaryWitnessPlan(ctx RetainedContext, opCtx OperationContext) (witnessPlan, error) {
	body := map[string]any{
		"actor_ref":                 opCtx.ActorRef,
		"authority_refs":            NonNilStrings(ctx.CapabilityWitnessRefs),
		"active_binding_refs":       NonNilStrings(ctx.ActiveBindingRefs),
		"semantic_environment_refs": NonNilStrings(ctx.SemanticEnvironmentRefs),
		"visibility_policy_refs":    NonNilStrings(ctx.VisibilityPolicyRefs),
		"provenance_policy_refs":    []string{},
		"substrate_ref":             ctx.SubstrateRef,
		"containment":               string(ctx.Containment),
	}
	// The witness body is validated before it is digested, so a group context
	// with an empty substrate_ref or an unknown containment fails the append —
	// and the preview, which builds the same plan. Python validates in
	// _ordinary_witness_plan; without this the store would retain a witness
	// whose body fails validate_witness_body, and the record citing it would
	// digest over an input the kernel's own schema rejects.
	if err := ValidateWitnessBody(WitnessSchemaRef, body); err != nil {
		return witnessPlan{}, err
	}
	rootID := RootWitnessRecordIDMust()
	recordID, err := RecordDigest(WitnessSchemaRef, Capture, body, nil, rootID)
	if err != nil {
		return witnessPlan{}, fmt.Errorf("ordinary witness digest: %w", err)
	}
	return witnessPlan{
		recordID:   recordID,
		schemaRef:  WitnessSchemaRef,
		kindLabel:  "witness",
		body:       body,
		witnessRef: rootID,
	}, nil
}

func rootWitnessPlan() witnessPlan {
	body := RootWitnessBody()
	recordID := RootWitnessRecordIDMust()
	return witnessPlan{
		recordID:  recordID,
		schemaRef: RootWitnessSchemaRef,
		// Python uses "witness_root" (trace_store.py `_root_witness_plan`); this
		// said "root_witness", which is retained metadata a read would expose.
		kindLabel:  "witness_root",
		body:       body,
		witnessRef: RootWitnessRef,
	}
}

func witnessFactMatchesPlan(fact Record, plan witnessPlan) bool {
	return fact.Envelope.SchemaRef == plan.schemaRef &&
		fact.Envelope.Mode == Capture &&
		fact.Envelope.WitnessRef == plan.witnessRef
}

func recordContentMatches(a, b Record) bool {
	aJSON, _ := json.Marshal(map[string]any{
		"schema_ref":  a.Envelope.SchemaRef,
		"mode":        string(a.Envelope.Mode),
		"witness_ref": a.Envelope.WitnessRef,
		"caused_by":   a.Envelope.CausedByIDs,
		"body":        a.Body.Payload,
	})
	bJSON, _ := json.Marshal(map[string]any{
		"schema_ref":  b.Envelope.SchemaRef,
		"mode":        string(b.Envelope.Mode),
		"witness_ref": b.Envelope.WitnessRef,
		"caused_by":   b.Envelope.CausedByIDs,
		"body":        b.Body.Payload,
	})
	return string(aJSON) == string(bJSON)
}

func witnessAnchor(w Record) WitnessAnchor {
	return WitnessAnchor{
		WitnessRef: w.Envelope.RecordID,
		VisibleShape: map[string]any{
			"schema_ref":  w.Envelope.SchemaRef,
			"witness_ref": w.Envelope.WitnessRef,
		},
		HiddenReason: "hidden_by_visibility",
	}
}

func (s *SQLiteTraceStore) insertWitnessRecordIfMissingTx(tx *sql.Tx, plan witnessPlan, appendIntentID string) error {
	if s.factExistsTx(tx, plan.recordID) {
		existing, err := s.readFactTx(tx, plan.recordID)
		if err != nil {
			return err
		}
		if !witnessFactMatchesPlan(existing, plan) {
			return &TraceStoreError{fmt.Sprintf("witness record id %q already names a different witness", plan.recordID)}
		}
		return nil
	}

	ordinal, err := s.nextOwnerOrdinalTx(tx, witnessTraceOwnerID)
	if err != nil {
		return err
	}

	fact := Record{
		Envelope: RecordEnvelope{
			RecordID:   plan.recordID,
			Digest:     plan.recordID,
			SchemaRef:  plan.schemaRef,
			Mode:       Capture,
			WitnessRef: plan.witnessRef,
		},
		Body: RecordBody{Payload: CopyMap(plan.body)},
		View: &RecordView{
			TraceOwnerID: witnessTraceOwnerID,
			OwnerOrdinal: ordinal,
			KindLabel:    plan.kindLabel,
		},
	}

	if err := s.insertFactRowTx(tx, fact, fmt.Sprintf("witness:%s", plan.recordID), appendIntentID); err != nil {
		return err
	}

	_, err = tx.Exec(
		`INSERT INTO owner_ordinals(trace_owner_id, next_ordinal) VALUES (?, ?)
		 ON CONFLICT(trace_owner_id) DO UPDATE SET next_ordinal = excluded.next_ordinal`,
		witnessTraceOwnerID, ordinal+1,
	)
	return err
}
