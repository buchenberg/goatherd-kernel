package trace

// Schema creation for the SQLite trace store: the tables, indices, and
// migrations the store methods assume.

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// expectedTables lists every table createSchema builds. A read-only open
// verifies all of them exist before handing out a store: a viewer pointed
// at a wrong file must fail loudly, not serve an empty trace of a session
// that never existed.
var expectedTables = []string{
	"meta",
	"append_intents",
	"materialization_intents",
	"contexts",
	"owner_ordinals",
	"records",
	"record_edges",
	"path_entries",
	"checkpoint_snapshots",
	"frontiers",
}

// retentionSchemaMarkerKey gates the evicted_paths table requirement: a
// database written before the retention change has no marker and no
// evicted_paths table, and both still open read-only. createSchema adds
// the marker on every read-write open.
const retentionSchemaMarkerKey = "schema:evicted_paths"

// blobSchemaMarkerKey gates the named_blobs table requirement the same way:
// a database written before named blobs has no marker and no table, and both
// still open read-only. createSchema adds the marker on every read-write
// open.
const blobSchemaMarkerKey = "schema:named_blobs"

// validateExistingSchema checks sqlite_master for the expected tables
// without running any DDL. It is the read-only counterpart of createSchema:
// the schema is verified, never created.
func (s *SQLiteTraceStore) validateExistingSchema() error {
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	defer rows.Close()

	have := make(map[string]bool, len(expectedTables))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan schema table name: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read schema: %w", err)
	}

	var missing []string
	for _, table := range expectedTables {
		if !have[table] {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("not a trace database: missing tables: %s", strings.Join(missing, ", "))
	}

	// Version gate for evicted_paths: databases created before the
	// retention change carry no marker in meta, so they still open
	// read-only without the table. Once createSchema has run once on a
	// read-write open, the table and marker exist together.
	var marker string
	err = s.db.QueryRow("SELECT value FROM meta WHERE key = ?", retentionSchemaMarkerKey).Scan(&marker)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read retention schema marker: %w", err)
	}
	if err == nil && !have["evicted_paths"] {
		return fmt.Errorf("not a trace database: missing table: evicted_paths")
	}

	// Same version gate for named blobs: a pre-blob database carries no
	// marker, so it opens without the table; once createSchema has run, the
	// marker and table exist together.
	err = s.db.QueryRow("SELECT value FROM meta WHERE key = ?", blobSchemaMarkerKey).Scan(&marker)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read blob schema marker: %w", err)
	}
	if err == nil && !have["named_blobs"] {
		return fmt.Errorf("not a trace database: missing table: named_blobs")
	}
	return nil
}

func (s *SQLiteTraceStore) createSchema() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		INSERT OR IGNORE INTO meta(key, value) VALUES ('next_commit_seq', '0');

		CREATE TABLE IF NOT EXISTS append_intents (
			append_intent_id TEXT PRIMARY KEY,
			batch_digest TEXT NOT NULL,
			receipt_json TEXT NOT NULL,
			committed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS materialization_intents (
			materialize_intent_id TEXT PRIMARY KEY,
			request_digest TEXT NOT NULL,
			receipt_json TEXT NOT NULL,
			completed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS contexts (
			context_id TEXT PRIMARY KEY,
			active_binding_refs_json TEXT NOT NULL,
			capability_witness_refs_json TEXT NOT NULL,
			semantic_environment_refs_json TEXT NOT NULL,
			visibility_policy_refs_json TEXT NOT NULL,
			substrate_ref TEXT NOT NULL,
			containment TEXT NOT NULL,
			append_intent_id TEXT NOT NULL,
			FOREIGN KEY(append_intent_id) REFERENCES append_intents(append_intent_id)
		);

		CREATE TABLE IF NOT EXISTS owner_ordinals (
			trace_owner_id TEXT PRIMARY KEY,
			next_ordinal INTEGER NOT NULL CHECK(next_ordinal >= 0)
		);

		CREATE TABLE IF NOT EXISTS records (
			record_id TEXT PRIMARY KEY,
			digest TEXT NOT NULL,
			schema_ref TEXT NOT NULL,
			mode TEXT NOT NULL,
			witness_ref TEXT NOT NULL,
			caused_by_json TEXT NOT NULL,
			body_json TEXT NOT NULL,
			append_intent_id TEXT NOT NULL,
			CHECK(record_id = digest),
			FOREIGN KEY(append_intent_id) REFERENCES append_intents(append_intent_id)
		);

		CREATE TABLE IF NOT EXISTS record_edges (
			parent_record_id TEXT NOT NULL,
			child_record_id TEXT NOT NULL,
			parent_position INTEGER NOT NULL CHECK(parent_position >= 0),
			PRIMARY KEY(child_record_id, parent_position),
			FOREIGN KEY(parent_record_id) REFERENCES records(record_id),
			FOREIGN KEY(child_record_id) REFERENCES records(record_id)
		);

		CREATE INDEX IF NOT EXISTS idx_record_edges_parent ON record_edges(parent_record_id);

		CREATE TABLE IF NOT EXISTS path_entries (
			path_ref TEXT NOT NULL,
			path_ordinal INTEGER NOT NULL CHECK(path_ordinal >= 0),
			record_id TEXT NOT NULL,
			retained_context_ref TEXT NOT NULL,
			kind_label TEXT NOT NULL,
			append_intent_id TEXT NOT NULL,
			commit_receipt TEXT NOT NULL UNIQUE,
			PRIMARY KEY(path_ref, path_ordinal),
			FOREIGN KEY(record_id) REFERENCES records(record_id),
			FOREIGN KEY(append_intent_id) REFERENCES append_intents(append_intent_id)
		);

		CREATE INDEX IF NOT EXISTS idx_path_entries_record ON path_entries(record_id);

		CREATE TABLE IF NOT EXISTS checkpoint_snapshots (
			checkpoint_id TEXT PRIMARY KEY,
			scope_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			state_json TEXT NOT NULL,
			snapshot BLOB NOT NULL,
			created_at_unixnano INTEGER NOT NULL
		);

		CREATE TABLE IF NOT EXISTS frontiers (
			frontier_id TEXT PRIMARY KEY,
			target_trace_owner_id TEXT NOT NULL,
			through_fact_id TEXT NOT NULL,
			through_owner_ordinal INTEGER NOT NULL CHECK(through_owner_ordinal >= 0),
			publisher_trace_owner_id TEXT,
			created_by_fact_id TEXT NOT NULL UNIQUE,
			append_intent_id TEXT NOT NULL UNIQUE,
			FOREIGN KEY(through_fact_id) REFERENCES records(record_id),
			FOREIGN KEY(created_by_fact_id) REFERENCES records(record_id),
			FOREIGN KEY(append_intent_id) REFERENCES append_intents(append_intent_id)
		);

		CREATE TABLE IF NOT EXISTS evicted_paths (
			trace_owner_id TEXT PRIMARY KEY,
			evicted_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS named_blobs (
			owner TEXT NOT NULL,
			kind TEXT NOT NULL,
			key TEXT NOT NULL,
			digest TEXT NOT NULL,
			size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
			data BLOB NOT NULL,
			created_at_unixnano INTEGER NOT NULL,
			updated_at_unixnano INTEGER NOT NULL,
			PRIMARY KEY (owner, kind, key)
		);
	`)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec("INSERT OR IGNORE INTO meta(key, value) VALUES (?, '1')", retentionSchemaMarkerKey); err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT OR IGNORE INTO meta(key, value) VALUES (?, '1')", blobSchemaMarkerKey)
	return err
}
