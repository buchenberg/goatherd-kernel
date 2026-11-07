package trace

// Laws for the named durable blob surface (bead goatherd-kernel-vkg): exact
// address round-trips, digest verification on read, a loud size limit,
// namespace isolation, read-only behavior, the schema version gate, and
// eviction reaching the owner's bytes.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamedBlobRoundTrip(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	payload := []byte("sealed output snapshot\x00\x01\x02")

	digest, err := PutBlob(ctx, store, "owner:a", "snapshot", "proposal-1", payload)
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("digest = %q, want sha256:<64 hex>", digest)
	}
	if want := blobDigest(payload); digest != want {
		t.Errorf("digest = %q, want %q", digest, want)
	}

	got, gotDigest, err := GetBlob(ctx, store, "owner:a", "snapshot", "proposal-1")
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("bytes = %q, want %q", got, payload)
	}
	if gotDigest != digest {
		t.Errorf("returned digest = %q, want %q", gotDigest, digest)
	}

	// The same key under another owner or kind is a distinct address.
	if _, _, err := GetBlob(ctx, store, "owner:b", "snapshot", "proposal-1"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("other owner: want ErrBlobNotFound, got %v", err)
	}
	if _, _, err := GetBlob(ctx, store, "owner:a", "diff", "proposal-1"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("other kind: want ErrBlobNotFound, got %v", err)
	}
}

func TestNamedBlobOverwriteReplacesBytes(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()

	if _, err := PutBlob(ctx, store, "owner:a", "diff", "k", []byte("first")); err != nil {
		t.Fatalf("PutBlob first: %v", err)
	}
	if _, err := PutBlob(ctx, store, "owner:a", "diff", "k", []byte("second")); err != nil {
		t.Fatalf("PutBlob second: %v", err)
	}

	got, digest, err := GetBlob(ctx, store, "owner:a", "diff", "k")
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("bytes = %q, want %q", got, "second")
	}
	if digest != blobDigest([]byte("second")) {
		t.Errorf("digest = %q, want the replacement's digest", digest)
	}
}

func TestNamedBlobMissingFailsLoudly(t *testing.T) {
	store := newMemStore(t)
	if _, _, err := GetBlob(context.Background(), store, "owner:a", "snapshot", "gone"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("GetBlob missing: want ErrBlobNotFound, got %v", err)
	}
}

func TestNamedBlobSizeLimitNeverTruncates(t *testing.T) {
	store := newMemStore(t).WithBlobLimit(8)
	ctx := context.Background()

	if _, err := PutBlob(ctx, store, "owner:a", "diff", "k", []byte("original")); err != nil {
		t.Fatalf("PutBlob in-limit: %v", err)
	}
	_, err := PutBlob(ctx, store, "owner:a", "diff", "k", []byte("0123456789"))
	if !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("oversize PutBlob: want ErrBlobTooLarge, got %v", err)
	}

	// The refused write must leave the previous bytes intact, never a
	// truncated mix.
	got, _, err := GetBlob(ctx, store, "owner:a", "diff", "k")
	if err != nil {
		t.Fatalf("GetBlob after refused write: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("bytes after refused write = %q, want %q", got, "original")
	}

	// Exactly at the limit is allowed.
	if _, err := PutBlob(ctx, store, "owner:a", "diff", "edge", []byte("12345678")); err != nil {
		t.Errorf("exact-limit PutBlob: %v", err)
	}
}

func TestNamedBlobNilPayloadIsEmptyBlob(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	digest, err := PutBlob(ctx, store, "owner:a", "snapshot", "empty", nil)
	if err != nil {
		t.Fatalf("PutBlob nil: %v", err)
	}
	if digest != blobDigest(nil) {
		t.Errorf("digest = %q, want the empty payload digest", digest)
	}
	got, _, err := GetBlob(ctx, store, "owner:a", "snapshot", "empty")
	if err != nil {
		t.Fatalf("GetBlob empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("bytes = %q, want empty", got)
	}
}

func TestNamedBlobDigestMismatchFailsLoudly(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	if _, err := PutBlob(ctx, store, "owner:a", "snapshot", "k", []byte("trusted")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	if _, err := store.db.Exec("UPDATE named_blobs SET data = ? WHERE owner = ? AND kind = ? AND key = ?",
		[]byte("tampered"), "owner:a", "snapshot", "k"); err != nil {
		t.Fatalf("tamper blob: %v", err)
	}
	got, digest, err := GetBlob(ctx, store, "owner:a", "snapshot", "k")
	if !errors.Is(err, ErrBlobDigestMismatch) {
		t.Fatalf("tampered GetBlob: want ErrBlobDigestMismatch, got %v (data=%q digest=%q)", err, got, digest)
	}
	if got != nil || digest != "" {
		t.Errorf("mismatched GetBlob returned data=%q digest=%q, want none", got, digest)
	}

	// A recorded size that disagrees with the row is also corruption.
	if _, err := store.db.Exec("UPDATE named_blobs SET data = data, size_bytes = 999 WHERE owner = ? AND kind = ? AND key = ?",
		"owner:a", "snapshot", "k"); err != nil {
		t.Fatalf("tamper size: %v", err)
	}
	if _, _, err := GetBlob(ctx, store, "owner:a", "snapshot", "k"); !errors.Is(err, ErrBlobDigestMismatch) {
		t.Fatalf("size-mismatched GetBlob: want ErrBlobDigestMismatch, got %v", err)
	}
}

func TestNamedBlobDeleteIsIdempotent(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	if _, err := PutBlob(ctx, store, "owner:a", "diff", "k", []byte("bytes")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := DeleteBlob(ctx, store, "owner:a", "diff", "k"); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}
	if _, _, err := GetBlob(ctx, store, "owner:a", "diff", "k"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("GetBlob after delete: want ErrBlobNotFound, got %v", err)
	}
	if err := DeleteBlob(ctx, store, "owner:a", "diff", "k"); err != nil {
		t.Errorf("repeated DeleteBlob: %v", err)
	}
}

func TestNamedBlobRequiresFullAddress(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	cases := []struct{ owner, kind, key string }{
		{"", "snapshot", "k"},
		{"owner:a", "", "k"},
		{"owner:a", "snapshot", ""},
	}
	for _, c := range cases {
		if _, err := PutBlob(ctx, store, c.owner, c.kind, c.key, []byte("x")); err == nil {
			t.Errorf("PutBlob(%q,%q,%q) succeeded, want an address error", c.owner, c.kind, c.key)
		}
		if _, _, err := GetBlob(ctx, store, c.owner, c.kind, c.key); err == nil {
			t.Errorf("GetBlob(%q,%q,%q) succeeded, want an address error", c.owner, c.kind, c.key)
		}
		if err := DeleteBlob(ctx, store, c.owner, c.kind, c.key); err == nil {
			t.Errorf("DeleteBlob(%q,%q,%q) succeeded, want an address error", c.owner, c.kind, c.key)
		}
	}
}

func TestNamedBlobSchemaVersionGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-blob.trace.sqlite")
	store, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	if _, err := PutBlob(context.Background(), store, "owner:a", "diff", "k", []byte("bytes")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Strip the blob bits to reconstruct a pre-blob database: it must still
	// open read-only, even though the table is gone.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec("DROP TABLE named_blobs"); err != nil {
		t.Fatalf("drop named_blobs: %v", err)
	}
	if _, err := raw.Exec("DELETE FROM meta WHERE key = ?", blobSchemaMarkerKey); err != nil {
		t.Fatalf("delete blob marker: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	viewer, err := OpenSQLiteTraceStoreReadOnly(path)
	if err != nil {
		t.Fatalf("read-only open of pre-blob schema: %v", err)
	}
	if _, _, err := GetBlob(context.Background(), viewer, "owner:a", "diff", "k"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("GetBlob on a pre-blob database: want ErrBlobNotFound, got %v", err)
	}
	if err := DeleteBlob(context.Background(), viewer, "owner:a", "diff", "k"); err != nil {
		t.Errorf("DeleteBlob on a pre-blob database: %v", err)
	}
	if err := viewer.Close(context.Background()); err != nil {
		t.Fatalf("close viewer: %v", err)
	}

	// A marker without its table is a broken database and must not open.
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	if _, err := raw.Exec("INSERT OR REPLACE INTO meta(key, value) VALUES (?, '1')", blobSchemaMarkerKey); err != nil {
		t.Fatalf("restore blob marker: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	if _, err := OpenSQLiteTraceStoreReadOnly(path); err == nil {
		t.Fatal("read-only open succeeded with the blob marker but no named_blobs table")
	}
}

func TestNamedBlobReadOnlyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob.trace.sqlite")
	store, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	if _, err := PutBlob(context.Background(), store, "owner:a", "snapshot", "k", []byte("bytes")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	viewer, err := OpenSQLiteTraceStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteTraceStoreReadOnly: %v", err)
	}
	defer viewer.Close(context.Background())

	got, _, err := GetBlob(context.Background(), viewer, "owner:a", "snapshot", "k")
	if err != nil {
		t.Fatalf("GetBlob read-only: %v", err)
	}
	if string(got) != "bytes" {
		t.Errorf("bytes = %q, want %q", got, "bytes")
	}
	if _, err := PutBlob(context.Background(), viewer, "owner:a", "snapshot", "k", []byte("more")); err == nil {
		t.Error("PutBlob on a read-only store succeeded, want a write refusal")
	}
}

func TestEvictOwnerPathAcceptsBlobOnlyOwner(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	if _, err := PutBlob(ctx, store, "owner:blobs-only", "snapshot", "k", []byte("bytes")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	receipt, err := store.EvictOwnerPath(ctx, trustedAppend, "owner:blobs-only", false)
	if err != nil {
		t.Fatalf("EvictOwnerPath on a blob-only owner: %v", err)
	}
	if receipt.DeletedBlobs != 1 {
		t.Errorf("DeletedBlobs = %d, want 1", receipt.DeletedBlobs)
	}
	if _, _, err := GetBlob(ctx, store, "owner:blobs-only", "snapshot", "k"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("blob survived eviction: %v", err)
	}
	// The tombstone is written, so a second eviction is refused rather than
	// re-deleting.
	if _, err := store.EvictOwnerPath(ctx, trustedAppend, "owner:blobs-only", false); !errors.Is(err, ErrOwnerEvicted) {
		t.Errorf("second eviction: want ErrOwnerEvicted, got %v", err)
	}
}

func TestPutBlobRefusesEvictedOwner(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	appendDrafts(t, store, "intent:put-evict", "owner:put-evict", draft("step", Capture, nil))
	if _, err := store.EvictOwnerPath(ctx, trustedAppend, "owner:put-evict", false); err != nil {
		t.Fatalf("EvictOwnerPath: %v", err)
	}

	if _, err := PutBlob(ctx, store, "owner:put-evict", "snapshot", "k", []byte("bytes")); !errors.Is(err, ErrOwnerEvicted) {
		t.Fatalf("PutBlob on an evicted owner: want ErrOwnerEvicted, got %v", err)
	}
	if _, _, err := GetBlob(ctx, store, "owner:put-evict", "snapshot", "k"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("refused PutBlob left bytes behind: %v", err)
	}
}

func TestEvictOwnerPathDeletesNamedBlobs(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	if _, err := PutBlob(ctx, store, "owner:evict", "snapshot", "k1", []byte("one")); err != nil {
		t.Fatalf("PutBlob owner:evict k1: %v", err)
	}
	if _, err := PutBlob(ctx, store, "owner:evict", "diff", "k2", []byte("two")); err != nil {
		t.Fatalf("PutBlob owner:evict k2: %v", err)
	}
	if _, err := PutBlob(ctx, store, "owner:keep", "snapshot", "k1", []byte("keep")); err != nil {
		t.Fatalf("PutBlob owner:keep: %v", err)
	}
	appendDrafts(t, store, "intent:evict", "owner:evict", draft("step", Capture, nil))

	receipt, err := store.EvictOwnerPath(ctx, trustedAppend, "owner:evict", false)
	if err != nil {
		t.Fatalf("EvictOwnerPath: %v", err)
	}
	if receipt.DeletedBlobs != 2 {
		t.Errorf("DeletedBlobs = %d, want 2", receipt.DeletedBlobs)
	}
	removed := []struct{ kind, key string }{{"snapshot", "k1"}, {"diff", "k2"}}
	for _, b := range removed {
		if _, _, err := GetBlob(ctx, store, "owner:evict", b.kind, b.key); !errors.Is(err, ErrBlobNotFound) {
			t.Errorf("evicted owner %s blob: want ErrBlobNotFound, got %v", b.kind, err)
		}
	}
	if _, _, err := GetBlob(ctx, store, "owner:keep", "snapshot", "k1"); err != nil {
		t.Errorf("other owner's blob was removed by eviction: %v", err)
	}
}
