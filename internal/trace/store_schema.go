package trace

// Schema creation for the SQLite trace store: the tables, indices, and
// migrations the store methods assume.

import (
	_ "modernc.org/sqlite"
)

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
	`)
	return err
}
