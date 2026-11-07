package trace

// Named durable blob persistence.
//
// The trace record carries identity; large opaque bytes — an execution
// snapshot, a diff, a serialized workspace — live beside it. A caller puts
// the bytes under a stable (owner, kind, key) address and keeps the returned
// digest in the fact that references them. Reads verify that digest, so a
// torn or edited row fails loudly instead of yielding plausible bytes.
//
// These are package functions taking the store, deliberately not methods on
// SQLiteTraceStore: the root package re-exports the store type, and a method
// would surface the blob plumbing in the public store API. As package
// functions they stay visible only to importers that ask for them.
//
// Relationship to checkpoint blobs. checkpoint_snapshots stays a typed,
// single-use, scope-keyed recovery row with its own enumeration, restore
// deletion, and per-checkpoint limit. Named blobs are opaque, caller-keyed,
// overwritable, and reachable only by exact address, with a different
// lifecycle. Migrating checkpoints onto this surface would change recovery
// for no gain, so the two live side by side: GC treats checkpoint payloads
// as a best-effort liveness source and named blobs as pure storage that
// confers no record liveness (a caller must keep the referencing fact alive
// through a normal path).

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrBlobNotFound is the expected "no blob at this address" condition,
// matched with errors.Is so a caller can distinguish a missing snapshot from
// a store fault.
var ErrBlobNotFound = errors.New("blob not found")

// ErrBlobTooLarge marks a PutBlob refused by the store's size limit. The
// bytes are never truncated or stored partially: a caller over the limit must
// externalize the payload or raise the limit deliberately.
var ErrBlobTooLarge = errors.New("blob over size limit")

// ErrBlobDigestMismatch marks a GetBlob whose stored digest does not match
// its bytes, or whose byte length does not match the recorded size. The
// caller gets no data: a corrupt blob must not be mistaken for content.
var ErrBlobDigestMismatch = errors.New("blob digest mismatch")

// DefaultBlobLimit is the default cap on one named blob: 8 MiB, large enough
// for a serialized workspace snapshot or a substantial diff, small enough
// that a runaway caller cannot silently balloon the trace database. Override
// with WithBlobLimit.
const DefaultBlobLimit int64 = 8 << 20

// WithBlobLimit bounds the bytes accepted by PutBlob. A blob over the limit
// fails loudly with ErrBlobTooLarge. Zero restores DefaultBlobLimit.
func (s *SQLiteTraceStore) WithBlobLimit(limit int64) *SQLiteTraceStore {
	if limit <= 0 {
		limit = DefaultBlobLimit
	}
	s.blobLimit = limit
	return s
}

// BlobLimitOrDefault resolves the store's effective named-blob limit,
// defaulting to DefaultBlobLimit.
func BlobLimitOrDefault(store *SQLiteTraceStore) int64 {
	if store.blobLimit > 0 {
		return store.blobLimit
	}
	return DefaultBlobLimit
}

// PutBlob stores data at (owner, kind, key) and returns its digest in the
// "sha256:<hex>" form the kernel uses elsewhere. A repeated put at the same
// address replaces the bytes and digest; created_at is preserved.
//
// owner is the trace owner id — the same namespace EvictOwnerPath uses — so
// eviction can reach the bytes. kind names the payload's role (for example a
// sealed snapshot or a diff), and key distinguishes blobs of that kind.
// Data whose length exceeds the store's blob limit fails with ErrBlobTooLarge
// and leaves any existing blob untouched. An owner with an eviction tombstone
// refuses new blobs with ErrOwnerEvicted, so a privacy flow cannot be undone
// by a later put.
func PutBlob(gctx context.Context, store *SQLiteTraceStore, owner, kind, key string, data []byte) (string, error) {
	if err := validateBlobAddress(owner, kind, key); err != nil {
		return "", err
	}
	if !store.hasNamedBlobs {
		return "", fmt.Errorf("blob %s/%s/%s: store has no named_blobs table; reopen read-write to create the schema", owner, kind, key)
	}
	if limit := BlobLimitOrDefault(store); int64(len(data)) > limit {
		return "", fmt.Errorf("blob %s/%s/%s: %d bytes over the store's %d-byte blob limit; externalize the payload or raise WithBlobLimit: %w",
			owner, kind, key, len(data), limit, ErrBlobTooLarge)
	}
	// A nil payload persists as an empty blob, matching the checkpoint
	// convention: "no bytes" is a value, not a missing row.
	if data == nil {
		data = []byte{}
	}
	digest := blobDigest(data)
	now := time.Now().UnixNano()

	// An evicted owner takes no new blobs: the tombstone is a privacy
	// boundary, and re-creating bytes under it would leave data that the
	// eviction surface (which refuses an already-evicted owner) can never
	// remove again. The check and the write share one transaction so an
	// eviction cannot slip between them.
	tx, err := store.db.BeginTx(gctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin blob transaction: %w", err)
	}
	defer tx.Rollback()
	if evicted, err := ownerEvicted(store.hasEvictedPaths, tx, owner); err != nil {
		return "", fmt.Errorf("check blob owner %s: %w", owner, err)
	} else if evicted {
		return "", &OwnerEvictedError{OwnerID: owner}
	}
	if _, err := tx.ExecContext(gctx, `
		INSERT INTO named_blobs
		(owner, kind, key, digest, size_bytes, data, created_at_unixnano, updated_at_unixnano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner, kind, key) DO UPDATE SET
			digest = excluded.digest,
			size_bytes = excluded.size_bytes,
			data = excluded.data,
			updated_at_unixnano = excluded.updated_at_unixnano`,
		owner, kind, key, digest, int64(len(data)), data, now, now); err != nil {
		return "", fmt.Errorf("put blob %s/%s/%s: %w", owner, kind, key, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit blob %s/%s/%s: %w", owner, kind, key, err)
	}
	return digest, nil
}

// GetBlob returns the bytes and digest stored at (owner, kind, key). The
// digest is recomputed and compared with the stored value, and the byte
// length with the recorded size; a mismatch fails with ErrBlobDigestMismatch
// and returns no data. An absent address fails with ErrBlobNotFound.
func GetBlob(gctx context.Context, store *SQLiteTraceStore, owner, kind, key string) ([]byte, string, error) {
	if err := validateBlobAddress(owner, kind, key); err != nil {
		return nil, "", err
	}
	if !store.hasNamedBlobs {
		// A pre-blob database predates the surface; the only honest answer
		// is that the address holds nothing, not a raw "no such table".
		return nil, "", fmt.Errorf("blob %s/%s/%s: %w", owner, kind, key, ErrBlobNotFound)
	}
	var digest string
	var size int64
	var data []byte
	err := store.db.QueryRowContext(gctx, `
		SELECT digest, size_bytes, data FROM named_blobs
		WHERE owner = ? AND kind = ? AND key = ?`, owner, kind, key).Scan(&digest, &size, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("blob %s/%s/%s: %w", owner, kind, key, ErrBlobNotFound)
	}
	if err != nil {
		return nil, "", fmt.Errorf("get blob %s/%s/%s: %w", owner, kind, key, err)
	}
	if int64(len(data)) != size {
		return nil, "", fmt.Errorf("blob %s/%s/%s: stored size %d, row holds %d bytes: %w",
			owner, kind, key, size, len(data), ErrBlobDigestMismatch)
	}
	computed := blobDigest(data)
	if computed != digest {
		return nil, "", fmt.Errorf("blob %s/%s/%s: stored digest %s, bytes hash to %s: %w",
			owner, kind, key, digest, computed, ErrBlobDigestMismatch)
	}
	return data, digest, nil
}

// DeleteBlob removes the blob at (owner, kind, key). Deleting an absent blob
// is a no-op so a cleanup can be retried; store faults still surface.
func DeleteBlob(gctx context.Context, store *SQLiteTraceStore, owner, kind, key string) error {
	if err := validateBlobAddress(owner, kind, key); err != nil {
		return err
	}
	if !store.hasNamedBlobs {
		return nil
	}
	if _, err := store.db.ExecContext(gctx,
		`DELETE FROM named_blobs WHERE owner = ? AND kind = ? AND key = ?`,
		owner, kind, key); err != nil {
		return fmt.Errorf("delete blob %s/%s/%s: %w", owner, kind, key, err)
	}
	return nil
}

// validateBlobAddress rejects an address that cannot be keyed. Empty parts
// are refused rather than defaulted: a typo must not silently store bytes
// under a namespace no reader will look in.
func validateBlobAddress(owner, kind, key string) error {
	switch {
	case owner == "":
		return fmt.Errorf("blob owner is required")
	case kind == "":
		return fmt.Errorf("blob kind is required")
	case key == "":
		return fmt.Errorf("blob key is required")
	}
	return nil
}

// blobDigest renders the kernel's sha256 digest form for opaque bytes.
func blobDigest(data []byte) string {
	return "sha256:" + hex.EncodeToString(sha256Sum(data))
}
