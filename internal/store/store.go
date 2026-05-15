// Package store is the mail server's data layer, backed entirely by
// OxiDB: document collections for the relational-ish metadata, the blob
// store for message bodies, and OxiMem for ephemeral state.
//
// Collection layout (canonical metadata):
//
//	domains          one document per hosted domain
//	accounts         one document per mailbox account (login + quota)
//	aliases          address -> account(s) forwarding
//	mailboxes        IMAP folders; each carries uidnext / uidvalidity
//	messages         one document per message: metadata, IMAP flags, and
//	                 a body-blob key (the RFC 5322 body lives in the blob
//	                 store, not in the document)
//	outbound_queue   messages awaiting delivery, with a retry schedule
//
// Per-message IMAP flags are a field on the `messages` document, not a
// separate collection: the document is small (the body is a blob ref),
// so a flag change is a cheap single-document update.
//
// Entity identity is OxiDB's auto-assigned `_id` (a u64) — `insert`
// overwrites any client-supplied `_id` — so references between entities
// (mailbox -> account, message -> mailbox) are stored as u64 fields.
//
// OxiDB gives no foreign keys or cascading deletes, so referential
// cleanup (removing an account's mailboxes, messages, and body blobs) is
// this layer's responsibility — see DeleteAccount.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/parisxmas/OxiDB/go/oxidb"
)

// Collection names — the single source of truth for the schema. Every
// query in this package refers to a collection through one of these.
const (
	CollDomains       = "domains"
	CollAccounts      = "accounts"
	CollAliases       = "aliases"
	CollMailboxes     = "mailboxes"
	CollMessages      = "messages"
	CollOutboundQueue = "outbound_queue"
	CollVacations     = "vacations"
)

// BlobBucket is the OxiDB blob-store bucket holding raw RFC 5322 message
// bodies. PutObject auto-creates it; EnsureSchema also creates it
// explicitly.
const BlobBucket = "message-bodies"

// ErrNotFound is returned by the Get* operations when no document
// matches. Callers should test it with errors.Is.
var ErrNotFound = errors.New("store: not found")

// ErrAuthFailed is returned by Authenticate when an address is unknown,
// inactive, or the password does not match. The cause is deliberately
// not distinguished, so a caller cannot use it to probe which addresses
// exist.
var ErrAuthFailed = errors.New("store: authentication failed")

// Store is the handle every component uses to reach OxiDB.
type Store struct {
	db *oxidb.Client
}

// Open connects to OxiDB.
//
// TODO: swap the single connection for the oxidb connection pool once
// the concurrent hot paths (delivery, IMAP fetch) exist.
func Open(host string, port int) (*Store, error) {
	db, err := oxidb.Connect(host, port, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("store: connect to OxiDB %s:%d: %w", host, port, err)
	}
	return &Store{db: db}, nil
}

// Close releases the OxiDB connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// -----------------------------------------------------------------------
// Document <-> struct helpers
// -----------------------------------------------------------------------

// encodeDoc turns an entity struct into the map[string]any OxiDB's client
// expects, via a JSON round-trip (struct json tags are the schema).
func encodeDoc(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: encode document: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("store: encode document: %w", err)
	}
	return m, nil
}

// decodeDoc fills the entity struct `v` from an OxiDB document map.
func decodeDoc(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: decode document: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("store: decode document: %w", err)
	}
	return nil
}

// insertedID extracts the `_id` OxiDB assigned, from an insert response
// (which is `{"id": <u64>}`).
func insertedID(resp map[string]any) (uint64, error) {
	if v, ok := resp["id"].(float64); ok && v >= 0 {
		return uint64(v), nil
	}
	return 0, fmt.Errorf("store: insert returned no id: %v", resp)
}

// nowRFC3339 is the canonical timestamp format for stored documents.
// OxiDB auto-detects RFC 3339 strings and indexes them as epoch millis,
// so date fields stay range-queryable.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
