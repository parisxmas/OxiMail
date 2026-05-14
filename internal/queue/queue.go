// Package queue is the outbound delivery queue: a background worker that
// picks up messages from the `outbound_queue` collection, attempts
// delivery to remote MX hosts, and reschedules failures with backoff.
//
// Queue state lives in OxiDB (`outbound_queue`), so delivery survives a
// restart; the message bodies it sends are read from the blob store.
package queue

import (
	"context"
	"log"
	"time"

	"github.com/parisxmas/OxiMail/internal/store"
)

// pollInterval is how often the worker scans for due deliveries.
const pollInterval = 30 * time.Second

// Queue is the outbound delivery worker.
type Queue struct {
	store *store.Store
}

// New builds the outbound queue worker.
func New(st *store.Store) *Queue {
	return &Queue{store: st}
}

// Start runs the delivery loop until `ctx` is cancelled, then returns.
//
// TODO: implement — claim due rows from `outbound_queue`, deliver over
// SMTP to each recipient's MX, and on failure reschedule with backoff
// (or bounce once retries are exhausted).
func (q *Queue) Start(ctx context.Context) error {
	log.Printf("queue: outbound worker started (stub — not yet implemented)")
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// TODO: scan outbound_queue for due deliveries and send.
		}
	}
}

// Stop signals the worker to wind down. Safe to call after Start has
// returned.
func (q *Queue) Stop() error {
	log.Print("queue: stopped")
	return nil
}
