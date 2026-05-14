package store

import "fmt"

// Routed reports how Route dispatched a message.
type Routed struct {
	LocalCount  int // copies filed straight into local mailboxes
	QueuedCount int // recipients handed to the outbound queue for relay
}

// Route dispatches an outgoing message to its recipients: each address
// that resolves to a local mailbox is delivered there directly, and the
// rest are handed to the outbound queue for relay. It is the shared
// send-path behind the submission server and the webmail API.
//
// Route stops at the first error; recipients already delivered stay
// delivered, so a caller that retries may produce duplicates for them.
// TODO: per-recipient status (LMTP-style) to avoid that.
func (s *Store) Route(from string, recipients []string, in IncomingMessage) (Routed, error) {
	var routed Routed
	var remote []string

	for _, rcpt := range recipients {
		accts, err := s.ResolveRecipient(rcpt)
		if err != nil {
			return routed, fmt.Errorf("store: route to %q: %w", rcpt, err)
		}
		if len(accts) == 0 {
			remote = append(remote, rcpt) // not a local mailbox — relay it
			continue
		}
		for _, id := range accts {
			if _, err := s.Deliver(id, in); err != nil {
				return routed, fmt.Errorf("store: route: deliver to account %d: %w", id, err)
			}
			routed.LocalCount++
		}
	}

	if len(remote) > 0 {
		if _, err := s.Enqueue(from, remote, in.Raw); err != nil {
			return routed, fmt.Errorf("store: route: enqueue: %w", err)
		}
		routed.QueuedCount = len(remote)
	}
	return routed, nil
}
