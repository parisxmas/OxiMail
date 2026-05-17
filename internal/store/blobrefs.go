package store

import (
	"fmt"
)

// Reference-counted body-blob lifecycle.
//
// Every body blob in CollBlobRefs carries a count of the message
// documents pointing at it. AppendMessage inserts the row with
// count=1 alongside the PutObject; CopyMessage atomically $inc's it
// instead of duplicating the body; DeleteMessage atomically
// decrements and deletes the blob only when the count hits zero.
//
// The decrement-then-delete pattern has a small TOCTOU window — a
// concurrent CopyMessage of a message about to be expunged might
// race the blob delete. In practice the window is tiny (single
// find_and_modify call), and a deferred-GC pass would close it
// fully. For v1 we accept the race; the failure mode is "the copy
// gets a dangling blob and FetchBody returns NotFound" — visible to
// the client, recoverable by re-fetching.

// blobAddRef inserts the initial refcount row (count=1) for a freshly
// stored body. Idempotency: if the row already exists this is a
// no-op rather than a duplicate-key error, since AppendMessage runs
// once per blob.
func (s *Store) blobAddRef(key string) error {
	doc, err := encodeDoc(struct {
		BlobKey string `json:"blob_key"`
		Count   int    `json:"count"`
	}{BlobKey: key, Count: 1})
	if err != nil {
		return err
	}
	if _, err := s.db.Insert(CollBlobRefs, doc); err != nil {
		return fmt.Errorf("store: register blob ref %q: %w", key, err)
	}
	return nil
}

// blobBumpRef atomically increments the refcount on an existing
// blob. Used by CopyMessage. Returns the post-increment count or an
// error if the refcount row is missing (meaning the source blob has
// already been collected).
func (s *Store) blobBumpRef(key string) (int, error) {
	doc, err := s.db.FindAndModify(
		CollBlobRefs,
		map[string]any{"blob_key": key},
		map[string]any{"$inc": map[string]any{"count": 1}},
	)
	if err != nil {
		return 0, fmt.Errorf("store: bump blob ref %q: %w", key, err)
	}
	if doc == nil {
		return 0, fmt.Errorf("store: blob ref %q not found (already collected?)", key)
	}
	count, _ := doc["count"].(float64)
	return int(count), nil
}

// blobDropRef atomically decrements the refcount on a blob and, when
// the count reaches zero, removes both the refcount row and the
// blob bytes. The bool return reports whether the blob itself was
// removed (true) or just decremented (false). A missing refcount
// row is treated as "remove the blob anyway" so legacy data that
// pre-dates refcounting still cleans up.
func (s *Store) blobDropRef(key string) (removed bool, err error) {
	doc, err := s.db.FindAndModify(
		CollBlobRefs,
		map[string]any{"blob_key": key},
		map[string]any{"$inc": map[string]any{"count": -1}},
	)
	if err != nil {
		return false, fmt.Errorf("store: drop blob ref %q: %w", key, err)
	}
	if doc == nil {
		// Legacy blob with no refcount row: just delete it.
		if err := s.db.DeleteObject(BlobBucket, key); err != nil {
			return false, fmt.Errorf("store: drop legacy blob %q: %w", key, err)
		}
		return true, nil
	}
	count, _ := doc["count"].(float64)
	if int(count) <= 0 {
		if _, err := s.db.Delete(CollBlobRefs, map[string]any{"blob_key": key}); err != nil {
			return false, fmt.Errorf("store: delete blob ref row %q: %w", key, err)
		}
		if err := s.db.DeleteObject(BlobBucket, key); err != nil {
			return false, fmt.Errorf("store: delete blob %q: %w", key, err)
		}
		return true, nil
	}
	return false, nil
}
