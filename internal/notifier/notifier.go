// Package notifier is a tiny process-wide pub/sub bus for "this mailbox
// changed" events. It is what makes IMAP IDLE see new mail dropped by
// another connection — SMTP delivery, the webmail compose endpoint,
// another IMAP session's APPEND, and so on.
//
// The contract is intentionally minimal:
//
//   - Publishers call Default.Notify(mailboxID). It never blocks and
//     never errors.
//   - Subscribers call Default.Subscribe(mailboxID), which returns a
//     *Subscription with a C() channel. Each notification non-blockingly
//     coalesces into a single pending wake-up on that channel — slow
//     subscribers do not back the publisher up, they just miss
//     intermediate events and catch up on the next read.
//   - When done, subscribers call Close().
package notifier

import "sync"

// Hub is a notification dispatcher. Use the package-level Default for
// in-process pub/sub; a fresh Hub is mostly useful in tests.
type Hub struct {
	mu   sync.Mutex
	subs map[uint64]map[*Subscription]struct{}
}

// Default is the process-wide hub used by the store and IMAP layers.
var Default = NewHub()

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: map[uint64]map[*Subscription]struct{}{}}
}

// Subscription is one subscriber's handle. Read from C() to receive
// wake-ups; call Close when done. The channel is buffered to one, so
// many notifications coalesce into one pending read.
type Subscription struct {
	hub       *Hub
	mailboxID uint64
	ch        chan struct{}
}

// C returns the channel that receives wake-ups. A receive means
// "something changed in the mailbox you subscribed to" — go ask the
// store what.
func (s *Subscription) C() <-chan struct{} {
	return s.ch
}

// Close removes the subscription. Safe to call more than once.
func (s *Subscription) Close() {
	if s.hub == nil {
		return
	}
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if set, ok := s.hub.subs[s.mailboxID]; ok {
		delete(set, s)
		if len(set) == 0 {
			delete(s.hub.subs, s.mailboxID)
		}
	}
	s.hub = nil
}

// Subscribe registers a new subscription for mailboxID.
func (h *Hub) Subscribe(mailboxID uint64) *Subscription {
	sub := &Subscription{hub: h, mailboxID: mailboxID, ch: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[mailboxID]
	if !ok {
		set = map[*Subscription]struct{}{}
		h.subs[mailboxID] = set
	}
	set[sub] = struct{}{}
	return sub
}

// Notify wakes every subscription on mailboxID. It does not block; if a
// subscriber's buffered channel is already full, the new notification
// is dropped (they will see the change on their next read anyway).
func (h *Hub) Notify(mailboxID uint64) {
	h.mu.Lock()
	subs := make([]*Subscription, 0, len(h.subs[mailboxID]))
	for sub := range h.subs[mailboxID] {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
}
