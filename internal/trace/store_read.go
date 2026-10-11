package trace

// The read path of the SQLite trace store: the public
// ReadFact/ReadOwnerPrefix/ReadCausalClosure API, slice building and
// visibility filtering, the by-schema scan recovery consumes, and the
// read-side authorization guards.

import (
	"context"
	"database/sql"

	"fmt"

	"strings"

	_ "modernc.org/sqlite"
)

// ReadFact reads one retained fact.
func (s *SQLiteTraceStore) ReadFact(ctx context.Context, auth ReadContext, factID string) (VisibleRecord, error) {
	opCtx := auth.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return nil, err
	}
	return s.readFact(ctx, factID)
}

// ReadOwnerPrefix reads all facts on an owner path up to and including the given ordinal.
func (s *SQLiteTraceStore) ReadOwnerPrefix(ctx context.Context, auth ReadContext, ownerID string, through int, modeFilter ModeFilter) (Slice, error) {
	opCtx := auth.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return Slice{}, err
	}
	if err := ensureModeFilter(modeFilter); err != nil {
		return Slice{}, err
	}

	var rows *sql.Rows
	var err error
	if ownerID == "" {
		rows, err = s.db.QueryContext(ctx,
			"SELECT record_id, path_ref, path_ordinal FROM path_entries WHERE path_ordinal <= ? ORDER BY path_ref, path_ordinal ASC",
			through,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			"SELECT record_id, path_ref, path_ordinal FROM path_entries WHERE path_ref = ? AND path_ordinal <= ? ORDER BY path_ordinal ASC",
			ownerID, through,
		)
	}
	if err != nil {
		return Slice{}, fmt.Errorf("query owner prefix: %w", err)
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

	return s.buildSlice(ctx, pathEntries, nil, opCtx.VisibilityProfile, modeFilter, true)
}

// ReadPathPrefix is the TraceStore protocol's path-addressed prefix read.
//
// In shepherd2 this is a pure alias for read_owner_prefix — the protocol declares
// it (facts.py:443) but the reference implementation's body is
// `return self.read_owner_prefix(read_context, trace_owner_id, through,
// mode_filter)`. It takes a trace owner id, not a free-form path, and applies no
// different addressing, so this delegates rather than reimplementing the query.
//
// It exists for protocol completeness: a caller written against the Python
// protocol can call either name and get the same slice.
func (s *SQLiteTraceStore) ReadPathPrefix(ctx context.Context, auth ReadContext, pathRef string, through int, modeFilter ModeFilter) (Slice, error) {
	return s.ReadOwnerPrefix(ctx, auth, pathRef, through, modeFilter)
}

// ReadCausalClosure reads the causal closure for one or more root facts.
func (s *SQLiteTraceStore) ReadCausalClosure(ctx context.Context, auth ReadContext, roots []string, modeFilter ModeFilter, closurePolicy string) (Slice, error) {
	opCtx := auth.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return Slice{}, err
	}
	if err := ensureModeFilter(modeFilter); err != nil {
		return Slice{}, err
	}
	if err := ensureClosurePolicy(closurePolicy); err != nil {
		return Slice{}, err
	}

	// BFS over causal edges
	pending := make([]string, len(roots))
	copy(pending, roots)
	seen := make(map[string]bool)
	for len(pending) > 0 {
		factID := pending[0]
		pending = pending[1:]
		if seen[factID] {
			continue
		}
		seen[factID] = true

		fact, err := s.readFact(ctx, factID)
		if err != nil {
			return Slice{}, err
		}
		for _, parent := range fact.GetEnvelope().CausedByIDs {
			if !seen[parent] {
				pending = append(pending, parent)
			}
		}
	}

	entries := s.canonicalFactOrder(ctx, seen)
	includeAnchors := closurePolicy == "include_external_anchors"
	return s.buildSlice(ctx, entries, nil, opCtx.VisibilityProfile, modeFilter, includeAnchors)
}

// FactCount returns the retained fact count for diagnostics.
func (s *SQLiteTraceStore) FactCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM records").Scan(&count)
	return count, err
}

// ScannedRecord is one record selected by schema ref, with the owner path it
// was read from and its decoded payload. Recovery is the caller: the
// lifecycle, checkpoint, and settlement schemas are open-map payloads, so
// records are the durable registry a restart rebuilds from.
type ScannedRecord struct {
	RecordID  string
	OwnerID   string
	SchemaRef string
	Payload   map[string]any
}

// ScanRecordsBySchema returns every retained record whose schema_ref is one
// of the given refs, each with its owner path (path_ref) and payload. A
// record retained on multiple paths appears once per path; lifecycle
// records are single-path by construction.
//
// It is a package function, deliberately not a method on SQLiteTraceStore:
// the root package re-exports the store type, and a method would surface
// this no-ReadContext scan in the public API. As an internal/ package
// function it stays invisible to importers outside the module.
func ScanRecordsBySchema(gctx context.Context, store *SQLiteTraceStore, schemaRefs ...string) ([]ScannedRecord, error) {
	if len(schemaRefs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(schemaRefs))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, 0, len(schemaRefs))
	for _, ref := range schemaRefs {
		args = append(args, ref)
	}

	rows, err := store.db.QueryContext(gctx, `
		SELECT records.record_id, path_entries.path_ref, records.schema_ref, records.body_json
		FROM records
		JOIN path_entries ON path_entries.record_id = records.record_id
		WHERE records.schema_ref IN (`+placeholders+`)
		ORDER BY path_entries.path_ref, path_entries.path_ordinal ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("scan records by schema: %w", err)
	}
	defer rows.Close()

	var out []ScannedRecord
	for rows.Next() {
		var rec ScannedRecord
		var bodyJSON string
		if err := rows.Scan(&rec.RecordID, &rec.OwnerID, &rec.SchemaRef, &bodyJSON); err != nil {
			return nil, fmt.Errorf("scan record row: %w", err)
		}
		// bodyFromJSON keeps the int/float distinction the canonical layer
		// depends on; plain Unmarshal would silently flatten numbers.
		body, err := bodyFromJSON(bodyJSON)
		if err != nil {
			return nil, fmt.Errorf("decode payload of record %s: %w", rec.RecordID, err)
		}
		rec.Payload = body.Payload
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ContextCount returns the retained context count for diagnostics.
func (s *SQLiteTraceStore) ContextCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM contexts").Scan(&count)
	return count, err
}

type pathEntry struct {
	recordID    string
	pathRef     string
	pathOrdinal int
}

func (s *SQLiteTraceStore) readFact(gctx context.Context, factID string) (Record, error) {
	row := s.db.QueryRowContext(gctx, `
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM records
		JOIN path_entries ON path_entries.record_id = records.record_id
		WHERE records.record_id = ?
		ORDER BY path_entries.path_ref ASC, path_entries.path_ordinal ASC
		LIMIT 1`, factID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readFactAtPath(gctx context.Context, factID, ownerID string, ordinal int) (Record, error) {
	row := s.db.QueryRowContext(gctx, `
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM path_entries
		JOIN records ON records.record_id = path_entries.record_id
		WHERE path_entries.record_id = ? AND path_entries.path_ref = ? AND path_entries.path_ordinal = ?`,
		factID, ownerID, ordinal)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readRecord(gctx context.Context, factID string) (Record, error) {
	row := s.db.QueryRowContext(gctx, `
		SELECT records.*, '' AS trace_owner_id, -1 AS owner_ordinal,
		       '' AS retained_context_ref, '' AS kind_label
		FROM records WHERE records.record_id = ?`, factID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) buildSlice(
	gctx context.Context,
	pathEntries []pathEntry,
	frontier *Frontier,
	visibility VisibilityProfile,
	modeFilter ModeFilter,
	includeExternalAnchors bool,
) (Slice, error) {
	// Filter by mode
	var loaded []struct {
		entry pathEntry
		fact  Record
	}
	for _, pe := range pathEntries {
		fact, err := s.readFactAtPath(gctx, pe.recordID, pe.pathRef, pe.pathOrdinal)
		if err != nil {
			return Slice{}, err
		}
		if modeMatches(fact, modeFilter) {
			loaded = append(loaded, struct {
				entry pathEntry
				fact  Record
			}{pe, fact})
		}
	}

	selected := make(map[string]bool)
	for _, l := range loaded {
		selected[l.entry.recordID] = true
	}

	// Owner paths and the three anchor collections are maps, but their order is
	// part of the read result. Python's TraceSlice holds all four as dicts and
	// emits them in insertion order — dict order is insertion order — so
	// fact_ids() and the anchor tuples are deterministic there. Go's map
	// iteration is randomised, so each map is paired with the order its keys were
	// first inserted and emitted through orderedValues. Iterating these maps
	// directly makes every read produce a different order: different from Python,
	// and different from the previous call in the same process.
	factsByID := make(map[string]VisibleRecord)
	contextsByID := make(map[string]RetainedContext)
	ownerPaths := make(map[string][]string)
	var ownerPathOrder []string
	var causalEdges [][2]string
	externalAnchors := make(map[string]ExternalAnchor)
	var externalAnchorOrder []string
	contextAnchors := make(map[string]ContextAnchor)
	var contextAnchorOrder []string
	witnessesByID := make(map[string]VisibleRecord)
	witnessAnchors := make(map[string]WitnessAnchor)
	var witnessAnchorOrder []string

	for _, l := range loaded {
		factID := l.entry.recordID
		visible := visibleFact(l.fact, visibility)
		if visible != nil {
			factsByID[factID] = visible
		}
		if _, seen := ownerPaths[l.entry.pathRef]; !seen {
			ownerPathOrder = append(ownerPathOrder, l.entry.pathRef)
		}
		ownerPaths[l.entry.pathRef] = append(ownerPaths[l.entry.pathRef], factID)

		for _, parent := range l.fact.Envelope.CausedByIDs {
			if selected[parent] {
				causalEdges = append(causalEdges, [2]string{parent, factID})
			} else if includeExternalAnchors {
				if _, exists := externalAnchors[parent]; !exists {
					externalAnchorOrder = append(externalAnchorOrder, parent)
					externalAnchors[parent] = s.anchorForFact(gctx, parent, "outside_frontier")
				}
			}
		}

		ctxID := ""
		if l.fact.View != nil {
			ctxID = l.fact.View.ContextRef
		}
		if ctxID != "" {
			if visibility == VisibilityShapeOnly {
				if _, exists := contextAnchors[ctxID]; !exists {
					contextAnchorOrder = append(contextAnchorOrder, ctxID)
					contextAnchors[ctxID] = ContextAnchor{
						ContextID:    ctxID,
						VisibleShape: map[string]any{"context_id": ctxID},
					}
				}
			} else {
				if _, exists := contextsByID[ctxID]; !exists {
					ctx, err := s.readContext(gctx, ctxID)
					if err == nil {
						contextsByID[ctxID] = ctx
					}
				}
			}
		}
	}

	// Witness support closure
	var loadedRecords []Record
	for _, l := range loaded {
		loadedRecords = append(loadedRecords, l.fact)
	}
	witnessSupport, err := s.witnessSupportClosure(gctx, loadedRecords)
	if err != nil {
		return Slice{}, err
	}
	for _, w := range witnessSupport {
		wRef := w.Envelope.RecordID
		visible := visibleFact(w, visibility)
		if visibility == VisibilityShapeOnly {
			if _, exists := witnessAnchors[wRef]; !exists {
				witnessAnchorOrder = append(witnessAnchorOrder, wRef)
				witnessAnchors[wRef] = witnessAnchor(w)
			}
		} else if visible != nil {
			witnessesByID[wRef] = visible
		}
	}

	return Slice{
		Frontier:          frontier,
		VisibilityProfile: visibility,
		ModeFilter:        modeFilter,
		FactsByID:         factsByID,
		ContextsByID:      contextsByID,
		OwnerPaths:        ownerPaths,
		OwnerPathOrder:    ownerPathOrder,
		CausalEdges:       causalEdges,
		ExternalAnchors:   orderedValues(externalAnchors, externalAnchorOrder),
		ContextAnchors:    orderedValues(contextAnchors, contextAnchorOrder),
		WitnessesByID:     witnessesByID,
		WitnessAnchors:    orderedValues(witnessAnchors, witnessAnchorOrder),
	}, nil
}

func (s *SQLiteTraceStore) anchorForFact(gctx context.Context, factID, hiddenReason string) ExternalAnchor {
	fact, err := s.readFact(gctx, factID)
	if err != nil {
		return ExternalAnchor{Ref: factID, AnchorKind: externalAnchorKindFact, HiddenReason: "unknown"}
	}
	kindLabel := ""
	if fact.View != nil {
		kindLabel = fact.View.KindLabel
	}
	traceOwnerID := ""
	ownerOrdinal := -1
	if fact.View != nil {
		traceOwnerID = fact.View.TraceOwnerID
		ownerOrdinal = fact.View.OwnerOrdinal
	}
	return ExternalAnchor{
		Ref:        factID,
		AnchorKind: externalAnchorKindFact,
		VisibleShape: map[string]any{
			"kind_label":     kindLabel,
			"schema_ref":     fact.Envelope.SchemaRef,
			"trace_owner_id": traceOwnerID,
			"owner_ordinal":  ownerOrdinal,
			"witness_ref":    fact.Envelope.WitnessRef,
		},
		HiddenReason: hiddenReason,
	}
}

func (s *SQLiteTraceStore) canonicalFactOrder(gctx context.Context, factIDs map[string]bool) []pathEntry {
	if len(factIDs) == 0 {
		return nil
	}

	ids := make([]any, 0, len(factIDs))
	for id := range factIDs {
		ids = append(ids, id)
	}
	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = "?"
	}

	query := fmt.Sprintf(`
		SELECT record_id, path_ref, path_ordinal FROM path_entries
		WHERE record_id IN (%s)
		ORDER BY path_ref ASC, path_ordinal ASC, record_id ASC`,
		strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(gctx, query, ids...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	// The entries are collected in the query's ORDER BY order, not through a
	// map: ranging a map discards the sort and randomizes the slice on every
	// call, which is what happened before this was pinned — a closure read
	// returned its facts in a different order on each process run, while a
	// single run always looked correct. A record reachable on several paths
	// keeps its first row, which the SQL order makes deterministic too.
	seen := make(map[string]bool)
	result := make([]pathEntry, 0, len(ids))
	for rows.Next() {
		var pe pathEntry
		if err := rows.Scan(&pe.recordID, &pe.pathRef, &pe.pathOrdinal); err != nil {
			continue
		}
		if !seen[pe.recordID] {
			seen[pe.recordID] = true
			result = append(result, pe)
		}
	}
	return result
}

func visibleFact(fact Record, visibility VisibilityProfile) VisibleRecord {
	switch visibility {
	case VisibilityPayload, VisibilityFullInternal:
		return fact
	case VisibilityShapeOnly:
		return RecordShape{
			Envelope:     fact.Envelope,
			View:         fact.View,
			HiddenReason: "payload_hidden",
		}
	default:
		return fact
	}
}

func modeMatches(fact Record, modeFilter ModeFilter) bool {
	switch modeFilter {
	case ModeBoth:
		return true
	case ModeDeclarationsOnly:
		return fact.Envelope.Mode == Declaration
	case ModeCapturesOnly:
		return fact.Envelope.Mode == Capture
	default:
		return true
	}
}

func ensureReadAuthorized(ctx OperationContext) error {
	if ctx.VisibilityProfile != VisibilityShapeOnly &&
		ctx.VisibilityProfile != VisibilityPayload &&
		ctx.VisibilityProfile != VisibilityFullInternal {
		return fmt.Errorf("unknown visibility profile: %s", ctx.VisibilityProfile)
	}
	if ctx.VisibilityProfile == VisibilityFullInternal {
		trusted := false
		for _, ref := range ctx.PresentedAuthorityRefs {
			if ref == "trusted:internal" {
				trusted = true
				break
			}
		}
		if !trusted {
			return &TraceStoreError{"full_internal reads require trusted internal witness"}
		}
	}
	return nil
}

func ensureModeFilter(f ModeFilter) error {
	if f != ModeBoth && f != ModeDeclarationsOnly && f != ModeCapturesOnly {
		return fmt.Errorf("unknown mode filter: %s", f)
	}
	return nil
}

func ensureClosurePolicy(p string) error {
	if p != "visible_only" && p != "include_external_anchors" {
		return fmt.Errorf("unknown closure policy: %s", p)
	}
	return nil
}
