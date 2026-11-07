package trace

// SQLite trace store: the store type, construction, options, and the
// error taxonomy. The append, read, frontier, witness, context, and record
// seams live in their own files in this package.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
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
	// blobLimit bounds one named blob written by PutBlob. Zero means
	// DefaultBlobLimit.
	blobLimit int64
	// hasEvictedPaths records whether the evicted_paths table exists in
	// this database, fixed once at open: createSchema always creates it on
	// a read-write open, and the read-only opener verifies the schema. The
	// tombstone probe uses it to skip the sqlite_master check on hot paths.
	hasEvictedPaths bool
	// hasNamedBlobs records whether the named_blobs table exists, fixed at
	// open. A database written before named blobs opens read-only without
	// it (behind the schema marker gate); blob reads then report absence
	// rather than a raw "no such table" store fault.
	hasNamedBlobs bool
}

// WithBus attaches an effect bus to the store. When set, every successful
// Append publishes an EffectEvent for each retained record. This enables
// real-time supervision without modifying the store's persistence behavior.
//
// The bus is optional — existing code that doesn't set a bus sees no change.
//
// WithBus is not synchronized against Append: attach the bus as part of
// store construction, before the store serves its first append. Attaching
// while appends are in flight is a data race on the bus field.
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

// NewSQLiteTraceStore creates a new trace store at the given path.
// Use ":memory:" for an in-memory store.
func NewSQLiteTraceStore(path string) (*SQLiteTraceStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer

	// One pooled connection serializes every statement in-process: reads
	// queue behind an open write transaction at the pool, so in-process
	// SQLITE_BUSY never arises and the busy timeout below only governs
	// cross-process lock contention. WAL is still worth setting because it
	// is a persistent file property — cross-process readers (the trace UI's
	// read-only opener, the Python reference) get snapshot reads instead of
	// blocking on the writer's lock.
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

	s := &SQLiteTraceStore{path: path, db: db, hasEvictedPaths: true, hasNamedBlobs: true}
	if err := s.createSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return s, nil
}

// OpenSQLiteTraceStoreReadOnly opens an existing trace database for reading
// only. It is the viewer's opener: unlike NewSQLiteTraceStore it never
// writes — no journal-mode pragma runs and no schema is created — so a
// missing, empty, or non-trace file fails loudly instead of materializing
// a valid-looking empty trace. The database must already hold the full
// trace schema, which is validated without DDL.
//
// The path names a file, never ":memory:". Paths containing SQLite URI
// metacharacters ('?', '#') are percent-encoded, as the URI filename form
// requires.
func OpenSQLiteTraceStoreReadOnly(path string) (*SQLiteTraceStore, error) {
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("read-only open requires an existing database file, not %q", path)
	}
	dsn := "file:" + url.PathEscape(path) + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer

	// No journal-mode pragma: switching the journal mode writes to the
	// database file, which a read-only connection must not do. The busy
	// timeout and foreign-keys pragmas are connection-local and safe.
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	s := &SQLiteTraceStore{path: path, db: db}
	if err := s.validateExistingSchema(); err != nil {
		db.Close()
		return nil, err
	}
	// Evicted paths can only exist if the retention schema does; one probe
	// at open lets the tombstone checks skip the sqlite_master lookup.
	var present int
	if err := s.db.QueryRow("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'evicted_paths'").Scan(&present); err == nil {
		s.hasEvictedPaths = true
	}
	// A pre-blob database has no named_blobs table. Blob reads then report
	// absence instead of a raw SQL fault; writes still fail as read-only.
	if err := s.db.QueryRow("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'named_blobs'").Scan(&present); err == nil {
		s.hasNamedBlobs = true
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *SQLiteTraceStore) Close(ctx context.Context) error {
	return s.db.Close()
}
