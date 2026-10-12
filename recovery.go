package shepherd

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	sandbox "github.com/buchenberg/shepherd-kernel-go/sandbox"
	trace "github.com/buchenberg/shepherd-kernel-go/trace"
)

// Recovery: rebuild the scope tree, checkpoints, and retained outputs from
// the durable trace after a process restart (plan 05 §4).
//
// The trace is the registry. Scope lifecycle events, checkpoint blobs, and
// settlement records are all durably recorded, so a restarted process loses
// only what was never recorded: conversation snapshots (deliberately opaque,
// deliberately not persisted beyond checkpoint blobs), timestamps on
// recovered outputs, and any root scope that never forked — a root with no
// fork record is invisible to the trace and must be re-created by the host.

// RecoverOptions configures scope recovery.
type RecoverOptions struct {
	// Sandbox re-adoption. Recovery calls it for every recovered root scope
	// and every isolated child, with the backend and durable address the
	// trace recorded (both empty for roots, which have no record; the path
	// empty for isolated children whose backend does not implement the
	// Addressed capability). Return (nil, nil) to recover the
	// scope without a sandbox — deliberate non-adoption, not an error: the
	// host uses it to orphan-clean worktrees it does not want back. An
	// error fails the recovery: half-adopted sandboxes are worse than none.
	//
	// Shared-sandbox children are never resolved: they inherit the parent's
	// re-adopted sandbox, exactly as they inherited it at fork time.
	//
	// The path is a normalized absolute address recorded at fork time
	// (plan 06, U2); a recorded non-absolute path fails the recovery
	// before the resolver sees it.
	Resolver func(scopeID, backend, path string) (sandbox.Sandbox, error)

	// Stats, when non-nil, receives the recovery's scan measurements:
	// records scanned per lifecycle schema and the total scan duration
	// (the scan phase — fork records, terminal states, checkpoint blobs,
	// sealed outputs — not the sandbox re-adoption). The recovery scan
	// is O(history) with no bound; hosts that run recovery at startup
	// hand this to their log so the cost is visible long before it
	// matters (plan 07, H5). The counts are written as their scans
	// complete — a recovery that fails partway reports what it had
	// counted, so an all-zero Stats after an error means the failure
	// preceded the first scan. ScanDuration is written only when the
	// scan phase completes.
	Stats *RecoveryStats
}

// RecoveryStats measures one recovery's scan phase. Every counter reports
// the raw records the scans read — the O(history) cost — not the derived
// state: terminal records are counted per record even when several fold
// into one owner's state, and corrupt checkpoint rows are counted even
// though they recover nothing.
type RecoveryStats struct {
	ForkRecords     int
	TerminalRecords int
	CheckpointBlobs int
	SealedOutputs   int
	// ScanDuration is the wall time of the whole scan phase.
	ScanDuration time.Duration
}

// resolve is the nil-safe form of opts.Resolver.
func (o RecoverOptions) resolve(scopeID, backend, path string) (sandbox.Sandbox, error) {
	if o.Resolver == nil {
		return nil, nil
	}
	return o.Resolver(scopeID, backend, path)
}

// forkEdge is one recovered fork: parent → child, with what the child needs.
type forkEdge struct {
	parentOwner string
	childOwner  string
	isolated    bool
	backend     string
	path        string
	baseline    trace.WorkspaceState
	forkPoint   string
}

// RecoverScopes rebuilds a ScopeManager from the durable trace: the scope
// tree and terminal states from lifecycle records, the checkpoint registry
// from the persisted blobs (with staleness detection against the re-adopted
// sandboxes), and retained outputs from sealed/settled records.
//
// What is and is not recoverable:
//
//   - Every scope that appears in a fork record — as parent or child — is
//     recovered, with its lifecycle state (merged/discarded children and
//     halted scopes included: terminal states are data too).
//   - A root scope that never forked left no record and is not recovered;
//     the host re-creates it. RecoverScopes returns the recovered roots so
//     the host can decide whether they are new or forgotten.
//   - Checkpoints recover from the checkpoint_snapshots table. A recovered
//     checkpoint whose recorded workspace revision no longer matches a
//     fresh capture of the re-adopted sandbox is marked CheckpointInvalid —
//     never silently restorable. Revision, not digest: the revision is the
//     timeline position, and that is what staleness means — a checkpoint
//     recorded at a HEAD the workspace has left is stale even if its
//     content would still digest equal. Sandboxes that cannot be captured
//     at recovery (no sandbox resolved, capture error) leave their
//     checkpoints Valid — restore re-validates against the live workspace
//     anyway.
//   - Retained outputs recover settle-able: the sealed payload carries the
//     full sealed and baseline states and the change list. A recovered
//     output's Diff is empty (the diff body was never recorded) and its
//     SealedAt/SettledAt are zero times — the records carry no timestamps.
//
// Sandboxes are never invented: the resolver decides what is re-adopted,
// and a recovery without a resolver rebuilds the causal tree only.
func RecoverScopes(store *trace.SQLiteTraceStore, opts RecoverOptions) (*ScopeManager, error) {
	ctx := context.Background()

	stats := opts.Stats
	scanStart := time.Now()
	edges, err := scanForkRecords(ctx, store)
	if err != nil {
		return nil, err
	}
	terminal, terminalRecords, err := scanTerminalStates(ctx, store)
	if err != nil {
		return nil, err
	}
	if stats != nil {
		stats.ForkRecords = len(edges)
		stats.TerminalRecords = terminalRecords
	}

	mgr := NewScopeManager(store)

	// The owner set: everyone who appears in a fork record.
	owners := make(map[string]bool)
	childOwners := make(map[string]bool)
	for _, e := range edges {
		owners[e.parentOwner] = true
		owners[e.childOwner] = true
		childOwners[e.childOwner] = true
	}

	scopes := make(map[string]*Scope, len(owners))
	for owner := range owners {
		state := ScopeActive
		if t, ok := terminal[owner]; ok {
			state = t
		}
		scope := &Scope{
			id:      fmt.Sprintf("scope:%s", owner),
			ownerID: owner,
			store:   store,
			state:   state,
		}
		scopes[owner] = scope
	}

	// Link the tree before anything else needs the parent pointers.
	for _, e := range edges {
		parent := scopes[e.parentOwner]
		child := scopes[e.childOwner]
		child.parent = parent
		child.forkPoint = e.forkPoint
		child.baseline = e.baseline
		parent.mu.Lock()
		parent.children = append(parent.children, child)
		parent.mu.Unlock()
	}

	// Register the tree, then load checkpoints and outputs — either can
	// introduce a scope the fork scan cannot see: a root that never forked
	// left no lifecycle record, but its checkpoint and sealed-output rows
	// prove it existed. Such scopes recover as bare active roots.
	mgr.mu.Lock()
	for _, scope := range scopes {
		mgr.scopes[scope.id] = scope
	}
	mgr.mu.Unlock()

	blobCount, err := recoverCheckpoints(ctx, mgr, store)
	if err != nil {
		return nil, err
	}
	if stats != nil {
		stats.CheckpointBlobs = blobCount
	}
	sealedCount, err := recoverOutputs(ctx, mgr, store)
	if err != nil {
		return nil, err
	}
	if stats != nil {
		stats.SealedOutputs = sealedCount
		stats.ScanDuration = time.Since(scanStart)
	}

	// Attach sandboxes: roots via the resolver (not owned, matching
	// Create), isolated children via the resolver (owned, matching
	// ForkIsolated), shared children inherit the parent's — including
	// bare roots introduced by checkpoint/output records.
	mgr.mu.RLock()
	all := make([]*Scope, 0, len(mgr.scopes))
	for _, scope := range mgr.scopes {
		all = append(all, scope)
	}
	mgr.mu.RUnlock()

	// Pass 1 resolves roots; pass 2 resolves isolated children and lets
	// shared children inherit — inheritance must see the parent's
	// re-adopted sandbox, so the order between the passes is not optional.
	roots := 0
	for _, scope := range all {
		if isChild := func() bool {
			scope.mu.Lock()
			defer scope.mu.Unlock()
			return scope.parent != nil
		}(); isChild {
			continue
		}
		sb, err := opts.resolve(scope.id, "", "")
		if err != nil {
			return nil, fmt.Errorf("recover scope %s: re-adopt sandbox: %w", scope.id, err)
		}
		if sb != nil {
			scope.WithSandbox(sb, false)
		}
		roots++
	}
	for _, scope := range all {
		if isChild := func() bool {
			scope.mu.Lock()
			defer scope.mu.Unlock()
			return scope.parent != nil
		}(); !isChild {
			continue
		}
		if edge := edgeFor(edges, scope.ownerID); edge != nil && edge.isolated {
			// A recorded address must be absolute: a relative path
			// replayed after a restart under a different working
			// directory would address the wrong location, and the
			// resolver cannot tell.
			if edge.path != "" && !filepath.IsAbs(edge.path) {
				return nil, fmt.Errorf("recover scope %s: recorded sandbox_path %q is not absolute", scope.id, edge.path)
			}
			sb, err := opts.resolve(scope.id, edge.backend, edge.path)
			if err != nil {
				return nil, fmt.Errorf("recover scope %s: re-adopt sandbox: %w", scope.id, err)
			}
			if sb != nil {
				scope.WithSandbox(sb, true)
			}
			continue
		}
		// A shared child inherits its parent's re-adopted sandbox, exactly
		// as it inherited the parent's at fork time; there is nothing to
		// resolve for it.
		if parent := scope.Parent(); parent != nil {
			scope.mu.Lock()
			scope.sandbox = parent.Sandbox()
			scope.mu.Unlock()
		}
	}

	// Staleness runs after re-adoption: it needs the sandboxes.
	if err := markStaleCheckpoints(ctx, mgr); err != nil {
		return nil, err
	}

	slog.Info("recovery: rebuilt scope registry from trace",
		"scopes", len(all), "roots", roots)
	return mgr, nil
}

// ensureScope returns the manager's scope for ownerID, creating and
// registering a bare active root when it does not exist — the shape a
// never-forked root recovers as.
func ensureScope(mgr *ScopeManager, store *trace.SQLiteTraceStore, ownerID string) *Scope {
	scopeID := fmt.Sprintf("scope:%s", ownerID)
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if scope, ok := mgr.scopes[scopeID]; ok {
		return scope
	}
	scope := &Scope{
		id:      scopeID,
		ownerID: ownerID,
		store:   store,
		state:   ScopeActive,
	}
	mgr.scopes[scopeID] = scope
	return scope
}

// scanForkRecords reads every fork record into edges. The parent is the
// record's owner path; the child, isolation, backend, and full baseline
// state come from the payload (baseline entries older than the recovery
// payload keys decode empty, and those children simply cannot propose
// merges — the same refusal a pure-causal fork gets).
func scanForkRecords(ctx context.Context, store *trace.SQLiteTraceStore) ([]forkEdge, error) {
	records, err := trace.ScanRecordsBySchema(ctx, store, SchemaScopeForked)
	if err != nil {
		return nil, fmt.Errorf("scan fork records: %w", err)
	}

	var edges []forkEdge
	for _, rec := range records {
		childOwner, _ := rec.Payload["child_owner"].(string)
		if childOwner == "" {
			return nil, fmt.Errorf("fork record %s on %s has no child_owner", rec.RecordID, rec.OwnerID)
		}
		edge := forkEdge{
			parentOwner: rec.OwnerID,
			childOwner:  childOwner,
			forkPoint:   rec.RecordID,
			isolated:    rec.Payload["isolated"] == true,
		}
		edge.backend, _ = rec.Payload["sandbox_backend"].(string)
		edge.path, _ = rec.Payload["sandbox_path"].(string)
		if baselineRaw, ok := rec.Payload["baseline"].(map[string]any); ok {
			edge.baseline = workspaceStateFromPayload(baselineRaw)
		}
		edges = append(edges, edge)
	}
	return edges, nil
}

// scanTerminalStates folds merged/discarded/halt records into a per-owner
// terminal state. A child merged then somehow discarded cannot happen (the
// state machine rejects it), so a collision is a corrupt trace and fails
// the recovery loudly.
func scanTerminalStates(ctx context.Context, store *trace.SQLiteTraceStore) (map[string]ScopeState, int, error) {
	records, err := trace.ScanRecordsBySchema(ctx, store, SchemaScopeMerged, SchemaScopeDiscarded, SchemaSupervisorHalt)
	if err != nil {
		return nil, 0, fmt.Errorf("scan terminal records: %w", err)
	}

	// The raw record count rides beside the folded states: the stats
	// surface reports the scan's O(history) cost, and several records
	// can fold into one owner's terminal state.
	rawRecords := len(records)
	states := make(map[string]ScopeState)
	for _, rec := range records {
		var owner string
		var state ScopeState
		switch rec.SchemaRef {
		case SchemaSupervisorHalt:
			// Halt is recorded on the halted scope's own path.
			owner = rec.OwnerID
			state = ScopeDiscarded
		case SchemaScopeMerged, SchemaScopeDiscarded:
			// Merges and discards are recorded on the parent's path; the
			// subject is the child_owner in the payload.
			child, _ := rec.Payload["child_owner"].(string)
			owner = child
			if rec.SchemaRef == SchemaScopeMerged {
				state = ScopeMerged
			} else {
				state = ScopeDiscarded
			}
		}
		if owner == "" {
			continue
		}
		if prior, ok := states[owner]; ok && prior != state {
			return nil, 0, fmt.Errorf("owner %s is recorded both %s and %s: the trace is inconsistent", owner, prior, state)
		}
		states[owner] = state
	}
	return states, rawRecords, nil
}

// edgeFor returns the edge that made owner a child, or nil.
func edgeFor(edges []forkEdge, childOwner string) *forkEdge {
	for i := range edges {
		if edges[i].childOwner == childOwner {
			return &edges[i]
		}
	}
	return nil
}

// recoverCheckpoints rebuilds the checkpoint registry from the persisted
// blobs. A checkpoint whose scope is unknown to the fork scan introduces a
// bare root scope: the blob proves the scope existed even though it never
// left a lifecycle record.
func recoverCheckpoints(ctx context.Context, mgr *ScopeManager, store *trace.SQLiteTraceStore) (int, error) {
	blobs, corrupt, err := trace.LoadCheckpointBlobs(ctx, store)
	if err != nil {
		return 0, fmt.Errorf("recover checkpoints: %w", err)
	}
	for _, id := range corrupt {
		slog.Warn("recovery: skipping checkpoint with undecodable state row", "checkpoint", id)
	}

	for _, b := range blobs {
		ensureScope(mgr, store, strings.TrimPrefix(b.ScopeID, "scope:"))

		cp := &Checkpoint{
			ID:        b.CheckpointID,
			Seq:       b.Seq,
			ScopeID:   b.ScopeID,
			Workspace: b.State,
			Snapshot:  b.Snapshot,
			CreatedAt: b.CreatedAt,
			State:     CheckpointValid,
		}

		mgr.mu.Lock()
		if existing, exists := mgr.checkpoints[cp.ID]; exists {
			mgr.mu.Unlock()
			return 0, fmt.Errorf("recovered checkpoint ID collision: %s (scopes %s, %s)", cp.ID, existing.ScopeID, cp.ScopeID)
		}
		mgr.checkpoints[cp.ID] = cp
		mgr.mu.Unlock()
	}
	// The corrupt rows are part of the scan's cost even though they
	// recover nothing; the stats surface reports what was read.
	return len(blobs) + len(corrupt), nil
}

// markStaleCheckpoints invalidates recovered checkpoints whose recorded
// revision no longer matches a fresh capture of the re-adopted sandbox —
// the never-silently-restorable rule (plan 05 section 4). Revision, not
// digest: the revision is the timeline position, and that is the
// staleness question — content equality (digest-stable since plan 06's
// content identity) is a different question, and a checkpoint whose
// content still matches but whose timeline advanced is stale anyway. A
// scope without a re-adopted sandbox leaves its checkpoints Valid —
// restore re-validates against the live workspace anyway.
func markStaleCheckpoints(ctx context.Context, mgr *ScopeManager) error {
	mgr.mu.RLock()
	type pair struct {
		cp    *Checkpoint
		scope *Scope
	}
	var pairs []pair
	for _, cp := range mgr.checkpoints {
		if scope, ok := mgr.scopes[cp.ScopeID]; ok {
			pairs = append(pairs, pair{cp, scope})
		}
	}
	mgr.mu.RUnlock()

	for _, p := range pairs {
		p.scope.mu.RLock()
		sb := p.scope.sandbox
		p.scope.mu.RUnlock()
		if sb == nil {
			continue
		}
		now, err := sb.Capture(ctx)
		if err != nil {
			slog.Warn("recovery: could not capture workspace for checkpoint staleness check",
				"checkpoint", p.cp.ID, "err", err)
			continue
		}
		p.cp.mu.Lock()
		if p.cp.State == CheckpointValid && now.Revision != p.cp.Workspace.Revision {
			p.cp.State = CheckpointInvalid
		}
		p.cp.mu.Unlock()
	}
	return nil
}

// recoverOutputs rebuilds the retained-output registry from sealed and
// settled records. Sealed payloads carry the full states and change list,
// so a recovered output is settle-able; settled records mark the terminal
// ones.
func recoverOutputs(ctx context.Context, mgr *ScopeManager, store *trace.SQLiteTraceStore) (int, error) {
	sealed, err := trace.ScanRecordsBySchema(ctx, store, SchemaRunOutputSealed)
	if err != nil {
		return 0, fmt.Errorf("recover outputs: scan sealed records: %w", err)
	}
	settled, err := trace.ScanRecordsBySchema(ctx, store, SchemaRunOutputSettled)
	if err != nil {
		return 0, fmt.Errorf("recover outputs: scan settled records: %w", err)
	}

	terminals := make(map[string]string) // output id -> action
	for _, rec := range settled {
		id, _ := rec.Payload["output_id"].(string)
		action, _ := rec.Payload["action"].(string)
		if id != "" {
			terminals[id] = action
		}
	}

	for _, rec := range sealed {
		id, _ := rec.Payload["output_id"].(string)
		if id == "" {
			continue
		}
		scopeID, _ := rec.Payload["scope"].(string)
		if owner := strings.TrimPrefix(scopeID, "scope:"); owner != "" {
			ensureScope(mgr, store, owner)
		}

		out := &RetainedOutput{
			id:      id,
			scopeID: scopeID,
			state:   OutputUnconsumed,
		}
		if stateRaw, ok := rec.Payload["state"].(map[string]any); ok {
			out.workspace = workspaceStateFromPayload(stateRaw)
		}
		if baseRaw, ok := rec.Payload["baseline"].(map[string]any); ok {
			out.baseline = workspaceStateFromPayload(baseRaw)
		}
		if changesRaw, ok := rec.Payload["changes"].([]any); ok {
			for _, cRaw := range changesRaw {
				c, ok := cRaw.(map[string]any)
				if !ok {
					continue
				}
				path, _ := c["path"].(string)
				kind, _ := c["kind"].(string)
				out.changes = append(out.changes, ProposedChange{Path: path, Kind: kind})
			}
		}
		out.sealedFactID = rec.RecordID

		if action, ok := terminals[id]; ok {
			terminal, err := stateFor(SettlementAction(action))
			if err != nil {
				slog.Warn("recovery: unknown settlement action on record; output left unconsumed",
					"output", id, "action", action)
			} else {
				out.state = terminal
				out.action = SettlementAction(action)
			}
		}

		mgr.mu.Lock()
		if existing, exists := mgr.outputs[out.id]; exists {
			mgr.mu.Unlock()
			return 0, fmt.Errorf("recovered output ID collision: %s (scopes %s, %s)", out.id, existing.scopeID, out.scopeID)
		}
		out.mgr = mgr
		mgr.outputs[out.id] = out
		mgr.mu.Unlock()
	}
	return len(sealed), nil
}

// workspaceStateFromPayload decodes a state encoded by the lifecycle
// payloads ({"backend", "revision", "data"}).
func workspaceStateFromPayload(raw map[string]any) trace.WorkspaceState {
	ws := trace.WorkspaceState{}
	ws.Backend, _ = raw["backend"].(string)
	ws.Revision, _ = raw["revision"].(string)
	if data, ok := raw["data"].(map[string]any); ok {
		ws.Data = data
	}
	if ws.Backend == "" {
		return trace.WorkspaceState{}
	}
	return ws
}
