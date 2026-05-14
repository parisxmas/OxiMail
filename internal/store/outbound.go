package store

import (
	"fmt"
	"time"

	"github.com/parisxmas/OxiDB/go/oxidb"
)

// Outbound queue statuses.
const (
	// OutboundQueued — the message is eligible for a delivery attempt now.
	OutboundQueued = "queued"
	// OutboundDeferred — a previous attempt temp-failed; not eligible
	// again before NextRetryAt.
	OutboundDeferred = "deferred"
)

// OutboundMessage is a message awaiting delivery to one or more remote
// hosts. Recipients is the set still to deliver: as the queue worker
// delivers (or permanently abandons) recipients it shrinks the list,
// and once it empties the row — and the body blob — are removed.
type OutboundMessage struct {
	ID          uint64   `json:"_id,omitempty"`
	From        string   `json:"from"`          // envelope sender
	Recipients  []string `json:"recipients"`    // remote recipients still to deliver
	BodyBlob    string   `json:"body_blob"`     // blob-store key for the raw RFC 5322 message
	SizeBytes   int64    `json:"size_bytes"`
	Status      string   `json:"status"`        // OutboundQueued | OutboundDeferred
	Attempts    int      `json:"attempts"`      // delivery attempts made so far
	NextRetryAt string   `json:"next_retry_at"` // RFC3339; not eligible before this time
	LastError   string   `json:"last_error"`    // most recent failure, for operators
	CreatedAt   string   `json:"created_at"`
}

// Enqueue stores a message for outbound delivery: the raw body goes to
// the blob store, then a queue document is inserted, due immediately.
func (s *Store) Enqueue(from string, recipients []string, raw []byte) (*OutboundMessage, error) {
	key, err := newBlobKey()
	if err != nil {
		return nil, err
	}
	if _, err := s.db.PutObject(BlobBucket, key, raw, "message/rfc822", nil); err != nil {
		return nil, fmt.Errorf("store: enqueue: store body: %w", err)
	}

	now := nowRFC3339()
	m := &OutboundMessage{
		From:        from,
		Recipients:  recipients,
		BodyBlob:    key,
		SizeBytes:   int64(len(raw)),
		Status:      OutboundQueued,
		NextRetryAt: now,
		CreatedAt:   now,
	}
	doc, err := encodeDoc(m)
	if err != nil {
		_ = s.db.DeleteObject(BlobBucket, key)
		return nil, err
	}
	resp, err := s.db.Insert(CollOutboundQueue, doc)
	if err != nil {
		_ = s.db.DeleteObject(BlobBucket, key) // no orphan blob
		return nil, fmt.Errorf("store: enqueue: insert: %w", err)
	}
	if m.ID, err = insertedID(resp); err != nil {
		_ = s.db.DeleteObject(BlobBucket, key)
		return nil, err
	}
	return m, nil
}

// ListDueOutbound returns up to `limit` queued messages whose retry time
// has arrived, oldest retry time first.
func (s *Store) ListDueOutbound(limit int) ([]OutboundMessage, error) {
	rows, err := s.db.Find(
		CollOutboundQueue,
		map[string]any{"next_retry_at": map[string]any{"$lte": nowRFC3339()}},
		&oxidb.FindOptions{
			Sort:  map[string]any{"next_retry_at": 1},
			Limit: &limit,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("store: list due outbound: %w", err)
	}
	out := make([]OutboundMessage, 0, len(rows))
	for _, r := range rows {
		var m OutboundMessage
		if err := decodeDoc(r, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// GetOutbound looks a queued message up by its OxiDB id. It is mainly
// for operators and tests inspecting the queue.
func (s *Store) GetOutbound(id uint64) (*OutboundMessage, error) {
	row, err := s.db.FindOne(CollOutboundQueue, map[string]any{"_id": id})
	if err != nil {
		return nil, fmt.Errorf("store: get outbound %d: %w", id, err)
	}
	if row == nil {
		return nil, ErrNotFound
	}
	var m OutboundMessage
	if err := decodeDoc(row, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// FetchOutboundBody reads a queued message's raw RFC 5322 body from the
// blob store.
func (s *Store) FetchOutboundBody(m *OutboundMessage) ([]byte, error) {
	body, _, err := s.db.GetObject(BlobBucket, m.BodyBlob)
	if err != nil {
		return nil, fmt.Errorf("store: fetch outbound body of %d (blob %q): %w", m.ID, m.BodyBlob, err)
	}
	return body, nil
}

// DeferOutbound reschedules a partially- or un-delivered message: the
// recipient list is replaced with the ones still pending, the attempt
// count and retry time are bumped, and the last error is recorded.
func (s *Store) DeferOutbound(id uint64, remaining []string, attempts int, nextRetry time.Time, lastErr string) error {
	doc, err := s.db.FindAndModify(
		CollOutboundQueue,
		map[string]any{"_id": id},
		map[string]any{"$set": map[string]any{
			"recipients":    remaining,
			"attempts":      attempts,
			"next_retry_at": nextRetry.UTC().Format(time.RFC3339),
			"last_error":    lastErr,
			"status":        OutboundDeferred,
		}},
	)
	if err != nil {
		return fmt.Errorf("store: defer outbound %d: %w", id, err)
	}
	if doc == nil {
		return ErrNotFound
	}
	return nil
}

// CompleteOutbound removes a queued message and its body blob, once
// every recipient has been delivered or permanently abandoned. The
// document goes first: a surviving document pointing at a missing blob
// would be retried forever, whereas an orphaned blob is only wasted
// disk.
func (s *Store) CompleteOutbound(m *OutboundMessage) error {
	if _, err := s.db.Delete(CollOutboundQueue, map[string]any{"_id": m.ID}); err != nil {
		return fmt.Errorf("store: complete outbound %d: %w", m.ID, err)
	}
	if err := s.db.DeleteObject(BlobBucket, m.BodyBlob); err != nil {
		return fmt.Errorf("store: complete outbound %d: remove body %q: %w", m.ID, m.BodyBlob, err)
	}
	return nil
}
