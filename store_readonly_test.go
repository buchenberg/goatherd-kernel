package trace

// Read-only store opening: the viewer's opener never writes, never creates
// schema, and refuses files that are not an existing trace database.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenSQLiteTraceStoreReadOnlyReadsExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.sqlite")
	store, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	receipt := appendDrafts(t, store, "intent:ro", "owner:ro",
		draft("step", Capture, map[string]any{"value": 1}))
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	ro, err := OpenSQLiteTraceStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteTraceStoreReadOnly: %v", err)
	}
	defer ro.Close(context.Background())

	count, err := ro.FactCount(context.Background())
	if err != nil {
		t.Fatalf("FactCount: %v", err)
	}
	if count == 0 {
		t.Error("read-only store sees no facts")
	}
	slice, err := ro.ReadOwnerPrefix(context.Background(), TrustedReadContext, "owner:ro", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	if _, ok := slice.FactsByID[receipt.FactIDs[0]]; !ok {
		t.Error("read-only store is missing the appended record")
	}
}

func TestOpenSQLiteTraceStoreReadOnlyRejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.sqlite")
	store, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	appendDrafts(t, store, "intent:ro", "owner:ro", draft("step", Capture, nil))
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	ro, err := OpenSQLiteTraceStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteTraceStoreReadOnly: %v", err)
	}
	defer ro.Close(context.Background())

	_, err = ro.Append(context.Background(), trustedAppend, AppendBatch{
		AppendIntentID: "intent:readonly",
		Groups:         []AppendGroup{{TraceOwnerID: "owner:ro", FactDrafts: []RecordDraft{draft("step", Capture, nil)}}},
	})
	if err == nil {
		t.Fatal("append through a read-only store should fail")
	}
	if !strings.Contains(err.Error(), "readonly") && !strings.Contains(err.Error(), "read-only") {
		t.Errorf("append error = %q, want a readonly-database failure", err)
	}
}

func TestOpenSQLiteTraceStoreReadOnlyRefusesNonTraceFiles(t *testing.T) {
	dir := t.TempDir()

	if _, err := OpenSQLiteTraceStoreReadOnly(filepath.Join(dir, "missing.sqlite")); err == nil {
		t.Error("missing file should fail the read-only open")
	}

	empty := filepath.Join(dir, "empty.sqlite")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	if _, err := OpenSQLiteTraceStoreReadOnly(empty); err == nil {
		t.Error("empty file should fail the schema check")
	}

	garbage := filepath.Join(dir, "garbage.sqlite")
	if err := os.WriteFile(garbage, []byte("not a database"), 0o644); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}
	if _, err := OpenSQLiteTraceStoreReadOnly(garbage); err == nil {
		t.Error("non-database file should fail the read-only open")
	}

	if _, err := OpenSQLiteTraceStoreReadOnly(":memory:"); err == nil {
		t.Error(":memory: should fail the read-only open")
	}

	// The refused opens must not have created the missing file.
	if _, err := os.Stat(filepath.Join(dir, "missing.sqlite")); !os.IsNotExist(err) {
		t.Error("read-only open created the file it was asked to open")
	}
}
