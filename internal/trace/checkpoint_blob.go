package trace

// Checkpoint blob persistence (plan 05 §4).
//
// The trace record carries the checkpoint's identity; the blob table carries
// what a restart actually needs: the full WorkspaceState (head/stash for
// git, snapshot keys for containerd) and the opaque caller snapshot. The
// table lives in the trace database — one file to back up, one place the
// recovery pass reads.
//
// The methods are exported because the runtime layer (scope checkpoints and
// recovery) lives outside this package; internal/ keeps them invisible to
// importers outside the module.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RecoveredCheckpointBlob is one persisted checkpoint row as recovery
// receives it.
type RecoveredCheckpointBlob struct {
	CheckpointID string
	ScopeID      string
	Seq          uint64
	State        WorkspaceState
	Snapshot     []byte
	CreatedAt    time.Time
}

// PersistCheckpointBlob stores a checkpoint's restorable halves. It is the
// hard-fail half of durability: a checkpoint that cannot be persisted must
// not be handed out as restorable.
func (s *SQLiteTraceStore) PersistCheckpointBlob(gctx context.Context, checkpointID, scopeID string, seq uint64, ws WorkspaceState, snapshot []byte, createdAt time.Time) error {
	stateJSON, err := json.Marshal(map[string]any{
		"backend":  ws.Backend,
		"revision": ws.Revision,
		"data":     ws.Data,
	})
	if err != nil {
		return fmt.Errorf("marshal checkpoint state: %w", err)
	}
	// A nil snapshot persists as an empty blob: the column is NOT NULL so a
	// checkpoint-without-conversation-state cannot be silently dropped by
	// the schema.
	if snapshot == nil {
		snapshot = []byte{}
	}
	_, err = s.db.ExecContext(gctx, `
		INSERT OR REPLACE INTO checkpoint_snapshots
		(checkpoint_id, scope_id, seq, state_json, snapshot, created_at_unixnano)
		VALUES (?, ?, ?, ?, ?, ?)`,
		checkpointID, scopeID, int64(seq), string(stateJSON), snapshot, createdAt.UnixNano())
	if err != nil {
		return fmt.Errorf("persist checkpoint snapshot: %w", err)
	}
	return nil
}

// LoadCheckpointBlobs returns every persisted checkpoint, oldest first by
// creation time. Rows whose state JSON cannot be decoded are skipped with
// their IDs collected — a corrupt row must not fail recovery of the rest.
func (s *SQLiteTraceStore) LoadCheckpointBlobs(gctx context.Context) ([]RecoveredCheckpointBlob, []string, error) {
	rows, err := s.db.QueryContext(gctx, `
		SELECT checkpoint_id, scope_id, seq, state_json, snapshot, created_at_unixnano
		FROM checkpoint_snapshots ORDER BY created_at_unixnano ASC`)
	if err != nil {
		return nil, nil, fmt.Errorf("load checkpoint snapshots: %w", err)
	}
	defer rows.Close()

	var blobs []RecoveredCheckpointBlob
	var corrupt []string
	for rows.Next() {
		var b RecoveredCheckpointBlob
		var stateJSON string
		var createdAtUnix int64
		var seq int64
		if err := rows.Scan(&b.CheckpointID, &b.ScopeID, &seq, &stateJSON, &b.Snapshot, &createdAtUnix); err != nil {
			return nil, nil, fmt.Errorf("scan checkpoint snapshot: %w", err)
		}
		var state struct {
			Backend  string         `json:"backend"`
			Revision string         `json:"revision"`
			Data     map[string]any `json:"data"`
		}
		// UseNumber keeps the int/float distinction the canonical layer
		// depends on -- plain Unmarshal flattens numbers to float64.
		dec := json.NewDecoder(strings.NewReader(stateJSON))
		dec.UseNumber()
		if err := dec.Decode(&state); err != nil {
			corrupt = append(corrupt, b.CheckpointID)
			continue
		}
		b.State = WorkspaceState{Backend: state.Backend, Revision: state.Revision, Data: state.Data}
		b.Seq = uint64(seq) // stored as int64: the sequence counter is a random-based uint64
		b.CreatedAt = time.Unix(0, createdAtUnix)
		blobs = append(blobs, b)
	}
	return blobs, corrupt, rows.Err()
}

// DeleteCheckpointBlob removes a persisted checkpoint. Restore deletes its
// blob on success: a restored checkpoint is single-use and must not be
// offered as restorable again by a later recovery.
func (s *SQLiteTraceStore) DeleteCheckpointBlob(gctx context.Context, checkpointID string) error {
	_, err := s.db.ExecContext(gctx, `DELETE FROM checkpoint_snapshots WHERE checkpoint_id = ?`, checkpointID)
	if err != nil {
		return fmt.Errorf("delete checkpoint snapshot: %w", err)
	}
	return nil
}
