package store

import (
	"fmt"
	"sort"
)

// ExpungeRecord is one row in the expunge log: a UID that vanished
// from a mailbox at a known mod-sequence (RFC 7162 §3.2 QRESYNC).
type ExpungeRecord struct {
	ID        uint64 `json:"_id,omitempty"`
	MailboxID uint64 `json:"mailbox_id"`
	UID       uint32 `json:"uid"`
	ModSeq    uint64 `json:"modseq"`
}

// recordExpunge inserts one expunge-log row. The log lets a future
// QRESYNC SELECT compute the "what UIDs disappeared since
// mod-sequence M" answer without re-asking other sessions or
// reconstructing state from a journal.
func (s *Store) recordExpunge(accountID, mailboxID uint64, uid uint32, modSeq uint64) error {
	rec := &ExpungeRecord{
		MailboxID: mailboxID,
		UID:       uid,
		ModSeq:    modSeq,
	}
	doc, err := encodeDoc(rec)
	if err != nil {
		return err
	}
	if _, err := s.db.Insert(ExpungeLogColl(accountID), doc); err != nil {
		return fmt.Errorf("store: record expunge of UID %d in mailbox %d: %w", uid, mailboxID, err)
	}
	return nil
}

// ExpungedSince returns the UIDs that have been expunged from a
// mailbox with mod-sequence greater than sinceModSeq, ordered by UID.
// Used by QRESYNC SELECT to populate VANISHED (EARLIER).
func (s *Store) ExpungedSince(accountID, mailboxID uint64, sinceModSeq uint64) ([]uint32, error) {
	rows, err := s.db.Find(ExpungeLogColl(accountID), map[string]any{"mailbox_id": mailboxID}, nil)
	if err != nil {
		if isMissingCollection(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list expunges for mailbox %d: %w", mailboxID, err)
	}
	out := make([]uint32, 0, len(rows))
	for _, r := range rows {
		var rec ExpungeRecord
		if err := decodeDoc(r, &rec); err != nil {
			return nil, err
		}
		if rec.ModSeq > sinceModSeq {
			out = append(out, rec.UID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
