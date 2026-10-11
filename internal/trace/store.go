package trace

// SQLite trace store: the store type, construction, options, and the\n// error taxonomy. The append, read, frontier, witness, context, and record\n// seams live in their own files in this package.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"sync"

	_ "modernc.org/sqlite"
)

// ErrUnknownFrontier is the expected "no retained cutoff for this frontier id"
// condition, distinguished from real read faults so callers can retry it
// deliberately instead of spinning on every error.
var ErrUnknownFrontier = errors.New("unknown frontier")

// unknownFrontierError keeps the store's message shape while matching the
// sentinel through errors.Is.
type unknownFrontierError struct{ frontierID string }

func (e *unknownFrontierError) Error() string {
	return fmt.Sprintf("unknown frontier id: %q", e.frontierID)
}

func (e *unknownFrontierError) Is(target error) bool { return target == ErrUnknownFrontier }

// TraceStoreError is the base error for trace store law violations.
type TraceStoreError struct {
	msg string
}

func (e *TraceStoreError) Error() string { return e.msg }

// AppendIntentConflictError is raised when an append intent is retried with a different batch.
type AppendIntentConflictError struct {
	msg string
}

func (e *AppendIntentConflictError) Error() string { return e.msg }

// UnknownFactError is raised when a retained fact references a missing fact.
type UnknownFactError struct {
	msg string
}

func (e *UnknownFactError) Error() string { return e.msg }

const witnessTraceOwnerID = "kernel:witness"

// SQLiteTraceStore is the canonical TraceStore implementation backed by SQLite.
type SQLiteTraceStore struct {
	path string
	db   *sql.DB
	mu   sync.Mutex
	bus  *EffectBus // optional; nil = no real-time publishing
	// checkpointBlobLimit bounds the caller snapshot bytes persisted per
	// checkpoint (plan 05 §4). Zero means DefaultCheckpointBlobLimit.
	checkpointBlobLimit int64
}

// WithBus attaches an effect bus to the store. When set, every successful
// Append publishes an EffectEvent for each retained record. This enables
// real-time supervision without modifying the store's persistence behavior.
//
// The bus is optional — existing code that doesn't set a bus sees no change.
func (s *SQLiteTraceStore) WithBus(bus *EffectBus) *SQLiteTraceStore {
	s.bus = bus
	return s
}

// WithCheckpointBlobLimit bounds the opaque snapshot bytes persisted per
// checkpoint. A checkpoint whose snapshot exceeds the limit fails loudly at
// creation — the host externalizes large state instead of silently growing
// the trace database. Zero restores the default.
func (s *SQLiteTraceStore) WithCheckpointBlobLimit(limit int64) *SQLiteTraceStore {
	if limit <= 0 {
		limit = DefaultCheckpointBlobLimit
	}
	s.checkpointBlobLimit = limit
	return s
}

// DefaultCheckpointBlobLimit is the default per-checkpoint snapshot cap:
// 1 MiB, generous for serialized conversation history, small enough that a
// long session cannot balloon the trace database unnoticed.
const DefaultCheckpointBlobLimit = 1 << 20

// CheckpointBlobLimitOrDefault resolves the effective limit.
func (s *SQLiteTraceStore) CheckpointBlobLimitOrDefault() int64 {
	if s.checkpointBlobLimit > 0 {
		return s.checkpointBlobLimit
	}
	return DefaultCheckpointBlobLimit
}

// NewSQLiteTraceStore creates a new trace store at the given path.
// Use ":memory:" for an in-memory store.
func NewSQLiteTraceStore(path string) (*SQLiteTraceStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer

	// WAL mode allows concurrent readers with a single writer.
	// Busy timeout makes SQLite wait for the lock instead of returning
	// SQLITE_BUSY immediately when the middleware and tools write
	// concurrently.
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	s := &SQLiteTraceStore{path: path, db: db}
	if err := s.createSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *SQLiteTraceStore) Close(ctx context.Context) error {
	return s.db.Close()
}

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// q returns the transaction if non-nil, otherwise the raw db.
func (s *SQLiteTraceStore) q(tx *sql.Tx) queryer {
	if tx != nil {
		return tx
	}
	return s.db
}
