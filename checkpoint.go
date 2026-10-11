package shepherd

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	sandbox "github.com/buchenberg/shepherd-kernel-go/sandbox"
	trace "github.com/buchenberg/shepherd-kernel-go/trace"

	"fmt"
	"log/slog"

	"sync"
	"sync/atomic"
	"time"
)

// Schema refs for checkpoint trace recording. Version 2 carries
// backend-neutral fields (the v1 payload embedded git-specific stash_sha and
// head_sha keys).
const (
	SchemaCheckpointCreated  = "shepherd.checkpoint.created.v2"
	SchemaCheckpointRestored = "shepherd.checkpoint.restored.v2"
)

// nextCheckpointSeq is a process-wide monotonic sequence used for checkpoint,
// workspace-event, and scope-lifecycle identity and ordering. Wall-clock time is
// intentionally NOT used per call: time.Now().UnixNano() can return the same
// value for concurrent callers, and equal CreatedAt values would leave ordering
// ambiguous (map iteration order).
//
// The counter starts from a per-process random base rather than 0. Its values
// feed durable AppendIntentIDs (checkpoint, restore, workspace, and scope
// lifecycle), and Append treats a repeated intent ID as an idempotent retry or a
// conflict. Starting from 0 would let a restarted process regenerate IDs the
// previous process already committed, silently skipping records or failing hard.
var nextCheckpointSeq atomic.Uint64

func init() {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Randomness is unavailable only in pathological environments; wall
		// clock still keeps the base unique across restarts in practice.
		nextCheckpointSeq.Store(uint64(time.Now().UnixNano()))
		return
	}
	nextCheckpointSeq.Store(binary.BigEndian.Uint64(b[:]))
}

// CheckpointState tracks whether a checkpoint is usable.
type CheckpointState string

const (
	// CheckpointValid means the checkpoint can be restored.
	CheckpointValid CheckpointState = "valid"
	// CheckpointUsed means the checkpoint has already been restored.
	CheckpointUsed CheckpointState = "used"
	// CheckpointInvalid means a restore failed and the checkpoint is unusable.
	CheckpointInvalid CheckpointState = "invalid"
)

// Checkpoint is a scope-owned, single-use capture of workspace state plus an
// opaque caller-provided snapshot.
//
// The workspace state is backend-neutral: a git stash/HEAD pair, a container
// snapshot key, or anything else the scope's Sandbox understands. The snapshot
// is opaque to the kernel — typically JSON-marshaled conversation history from
// the agent loop — and the caller knows its concrete type.
//
// Checkpoints are single-use: RestoreCheckpoint marks them as used and releases
// the snapshot. Restore is guarded by mu so concurrent restores of the same
// checkpoint cannot both claim the valid state.
type Checkpoint struct {
	// mu guards State and Snapshot across concurrent restore attempts.
	mu sync.Mutex

	// ID is a unique checkpoint identifier, derived from a monotonic sequence
	// rather than wall-clock time so concurrent checkpoints cannot collide.
	ID string
	// Seq is the process-wide monotonic creation order. It is the authoritative
	// ordering key (CreatedAt is informational and can tie).
	Seq uint64
	// ScopeID is the scope this checkpoint belongs to.
	ScopeID string
	// Workspace is the captured backend-neutral workspace state.
	Workspace trace.WorkspaceState
	// Snapshot is the opaque caller-provided state (typically conversation
	// history). Released after a successful restore.
	Snapshot []byte
	// CreatedAt is when the checkpoint was taken.
	CreatedAt time.Time
	// State tracks usability.
	State CheckpointState
}

// CreateCheckpoint captures the scope's workspace state plus the caller's
// snapshot.
//
// The workspace capture is delegated to the scope's Sandbox, so this works
// unchanged for an in-place git tree, a detached worktree, or a future
// container backend. A scope created without a sandbox returns sandbox.ErrNoSandbox.
//
// The snapshot is persisted durably into the trace database's
// checkpoint_snapshots table (bounded by the store's checkpoint blob limit;
// an oversized snapshot fails the checkpoint rather than silently ballooning
// the file), which is what makes a checkpoint restorable after a process
// restart via RecoverScopes. The "checkpoint.created" trace record remains
// advisory.
func (s *Scope) CreateCheckpoint(ctx context.Context, snapshot []byte) (*Checkpoint, error) {
	s.mu.RLock()
	state := s.state
	sb := s.sandbox
	ownerID := s.ownerID
	scopeID := s.id
	s.mu.RUnlock()

	if state != ScopeActive {
		return nil, fmt.Errorf("cannot checkpoint scope %s in state %s", scopeID, state)
	}
	if sb == nil {
		return nil, sandbox.ErrNoSandbox
	}

	ws, err := sb.Capture(ctx)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: capture workspace: %w", err)
	}

	seq := nextCheckpointSeq.Add(1)
	cp := &Checkpoint{
		ID:        fmt.Sprintf("cp:%s:%d", ownerID, seq),
		Seq:       seq,
		ScopeID:   scopeID,
		Workspace: ws,
		Snapshot:  snapshot,
		CreatedAt: time.Now(),
		State:     CheckpointValid,
	}

	if limit := trace.CheckpointBlobLimitOrDefault(s.store); int64(len(snapshot)) > limit {
		return nil, fmt.Errorf("checkpoint %s: snapshot is %d bytes, over the store's %d-byte checkpoint blob limit; store large state externally and pass a reference instead", cp.ID, len(snapshot), limit)
	}
	if err := trace.PersistCheckpointBlob(ctx, s.store, cp.ID, cp.ScopeID, cp.Seq, cp.Workspace, cp.Snapshot, cp.CreatedAt); err != nil {
		return nil, fmt.Errorf("checkpoint %s: %w", cp.ID, err)
	}

	digest, _ := ws.Digest()
	payload := map[string]any{
		"checkpoint_id": cp.ID,
		"backend":       ws.Backend,
		"revision":      ws.Revision,
		"state_digest":  digest,
		"has_snapshot":  len(snapshot) > 0,
	}
	_, err = s.store.Append(ctx, trace.TrustedAppendContext, trace.AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:checkpoint:%d", ownerID, seq),
		Groups: []trace.AppendGroup{{
			TraceOwnerID: ownerID,
			FactDrafts: []trace.RecordDraft{{
				Mode:      trace.Declaration,
				SchemaRef: SchemaCheckpointCreated,
				KindLabel: "checkpoint:created",
				Payload:   payload,
			}},
		}},
	})
	if err != nil {
		slog.Warn("checkpoint: record create in trace failed (advisory)", "err", err)
	}

	return cp, nil
}

// RestoreCheckpoint reverts the workspace to the checkpointed state and returns
// the stored snapshot for the caller to deserialize and restore into its agent
// loop's context manager.
//
// The workspace restore is delegated to the scope's Sandbox. The checkpoint's
// snapshot field is released after a successful restore, and the checkpoint is
// marked used — calling Restore twice returns an error.
//
// The single-use transition is race-safe: cp.mu is held for the entire
// validate → apply → mark-used sequence, so concurrent restore attempts cannot
// both claim the valid state.
//
// Records a "checkpoint.restored" capture in the scope's trace (advisory).
func (s *Scope) RestoreCheckpoint(ctx context.Context, cp *Checkpoint) ([]byte, error) {
	if cp == nil {
		return nil, fmt.Errorf("restore: nil checkpoint")
	}

	s.mu.RLock()
	state := s.state
	sb := s.sandbox
	ownerID := s.ownerID
	scopeID := s.id
	s.mu.RUnlock()

	if state != ScopeActive {
		return nil, fmt.Errorf("cannot restore in scope %s state %s", scopeID, state)
	}
	if sb == nil {
		return nil, sandbox.ErrNoSandbox
	}
	if cp.ScopeID != scopeID {
		return nil, fmt.Errorf("checkpoint %s belongs to scope %s, not %s",
			cp.ID, cp.ScopeID, scopeID)
	}

	// Claim the checkpoint exclusively for this restore.
	cp.mu.Lock()
	defer cp.mu.Unlock()

	if cp.State != CheckpointValid {
		return nil, fmt.Errorf("checkpoint %s is %s, cannot restore", cp.ID, cp.State)
	}

	if err := sb.Apply(ctx, cp.Workspace); err != nil {
		cp.State = CheckpointInvalid
		return nil, fmt.Errorf("restore: apply workspace: %w", err)
	}

	// Release the snapshot and mark used (under cp.mu).
	snapshot := cp.Snapshot
	cp.Snapshot = nil
	cp.State = CheckpointUsed

	// The persisted blob is consumed with the checkpoint: a restored
	// checkpoint is single-use and must not resurface as restorable in a
	// later recovery. Advisory for the same reason the restore record is.
	if err := trace.DeleteCheckpointBlob(ctx, s.store, cp.ID); err != nil {
		slog.Warn("checkpoint: delete persisted snapshot failed (advisory)", "checkpoint", cp.ID, "err", err)
	}

	digest, _ := cp.Workspace.Digest()
	_, err := s.store.Append(ctx, trace.TrustedAppendContext, trace.AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:restore:%d", ownerID, cp.Seq),
		Groups: []trace.AppendGroup{{
			TraceOwnerID: ownerID,
			FactDrafts: []trace.RecordDraft{{
				Mode:      trace.Capture,
				SchemaRef: SchemaCheckpointRestored,
				KindLabel: "checkpoint:restored",
				Payload: map[string]any{
					"checkpoint_id": cp.ID,
					"backend":       cp.Workspace.Backend,
					"revision":      cp.Workspace.Revision,
					"state_digest":  digest,
				},
			}},
		}},
	})
	if err != nil {
		slog.Warn("checkpoint: record restore in trace failed (advisory)", "err", err)
	}

	return snapshot, nil
}
