package imap

import (
	"bytes"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"

	"github.com/parisxmas/OxiMail/internal/store"
)

// search runs an IMAP SEARCH over the mailbox snapshot. Criteria that
// need the message body — HEADER, BODY, TEXT, SENTSINCE/SENTBEFORE —
// read it from the blob store; the rest match the snapshot metadata
// alone.
//
// TODO: back BODY / TEXT search with OxiDB's full-text index instead of
// fetching and scanning every body.
func (m *selectedMailbox) search(kind imapserver.NumKind, criteria *imap.SearchCriteria) *imap.SearchData {
	m.resolveCriteria(criteria)

	var (
		data   imap.SearchData
		seqSet imap.SeqSet
		uidSet imap.UIDSet
	)
	for i := range m.msgs {
		msg := &m.msgs[i]
		seqNum := m.session.EncodeSeqNum(uint32(i) + 1)
		if !m.matches(msg, seqNum, criteria) {
			continue
		}
		uidSet.AddNum(imap.UID(msg.UID))

		var num uint32
		switch kind {
		case imapserver.NumKindSeq:
			if seqNum == 0 {
				continue // expunged from this session's view
			}
			seqSet.AddNum(seqNum)
			num = seqNum
		case imapserver.NumKindUID:
			num = msg.UID
		}
		if data.Min == 0 || num < data.Min {
			data.Min = num
		}
		if data.Max == 0 || num > data.Max {
			data.Max = num
		}
		data.Count++
	}

	switch kind {
	case imapserver.NumKindSeq:
		data.All = seqSet
	case imapserver.NumKindUID:
		data.All = uidSet
	}
	return &data
}

// resolveCriteria rewrites the "*" wildcard (and "n:*" ranges) in every
// sequence-number and UID set the criteria carries, recursively.
func (m *selectedMailbox) resolveCriteria(c *imap.SearchCriteria) {
	for i := range c.SeqNum {
		if set, ok := m.staticNumSet(c.SeqNum[i]).(imap.SeqSet); ok {
			c.SeqNum[i] = set
		}
	}
	for i := range c.UID {
		if set, ok := m.staticNumSet(c.UID[i]).(imap.UIDSet); ok {
			c.UID[i] = set
		}
	}
	for i := range c.Not {
		m.resolveCriteria(&c.Not[i])
	}
	for i := range c.Or {
		m.resolveCriteria(&c.Or[i][0])
		m.resolveCriteria(&c.Or[i][1])
	}
}

// matches reports whether one message satisfies the search criteria.
// seqNum is the message's sequence number as this session sees it.
func (m *selectedMailbox) matches(msg *store.Message, seqNum uint32, c *imap.SearchCriteria) bool {
	for _, set := range c.SeqNum {
		if seqNum == 0 || !set.Contains(seqNum) {
			return false
		}
	}
	for _, set := range c.UID {
		if !set.Contains(imap.UID(msg.UID)) {
			return false
		}
	}

	if !withinDate(parseTime(msg.InternalDate), c.Since, c.Before) {
		return false
	}

	for _, flag := range c.Flag {
		if !hasFlag(msg.Flags, string(flag)) {
			return false
		}
	}
	for _, flag := range c.NotFlag {
		if hasFlag(msg.Flags, string(flag)) {
			return false
		}
	}

	if c.Larger != 0 && msg.SizeBytes <= c.Larger {
		return false
	}
	if c.Smaller != 0 && msg.SizeBytes >= c.Smaller {
		return false
	}

	// HEADER / BODY / TEXT / SENT* all need the raw message.
	if len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 ||
		!c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		raw, err := m.store.FetchBody(msg)
		if err != nil {
			return false // body unreadable — cannot satisfy a body criterion
		}
		if !matchBodyCriteria(raw, c) {
			return false
		}
	}

	for i := range c.Not {
		if m.matches(msg, seqNum, &c.Not[i]) {
			return false
		}
	}
	for _, or := range c.Or {
		if !m.matches(msg, seqNum, &or[0]) && !m.matches(msg, seqNum, &or[1]) {
			return false
		}
	}
	return true
}

// matchBodyCriteria checks the criteria that require the raw message:
// the SENT date range, header-field substrings, and BODY / TEXT search.
func matchBodyCriteria(raw []byte, c *imap.SearchCriteria) bool {
	if !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		h := gomail.Header{Header: readEntity(raw).Header}
		sent, err := h.Date()
		if err != nil || !withinDate(sent, c.SentSince, c.SentBefore) {
			return false
		}
	}
	for _, hf := range c.Header {
		if !matchHeaderFields(readEntity(raw).Header.FieldsByKey(hf.Key), hf.Value) {
			return false
		}
	}
	for _, text := range c.Text {
		if !matchEntity(readEntity(raw), text, true) {
			return false
		}
	}
	for _, body := range c.Body {
		if !matchEntity(readEntity(raw), body, false) {
			return false
		}
	}
	return true
}

// readEntity parses raw into a go-message Entity. A message that will
// not parse yields an empty entity, so callers can treat it uniformly.
func readEntity(raw []byte) *gomessage.Entity {
	ent, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil || ent == nil {
		ent, _ = gomessage.New(gomessage.Header{}, bytes.NewReader(nil))
	}
	return ent
}

// withinDate reports whether t falls in the [since, before) range. Per
// RFC 3501, only the date is compared — the time and zone are ignored.
func withinDate(t, since, before time.Time) bool {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !before.IsZero() && !t.Before(before) {
		return false
	}
	return true
}

// matchHeaderFields reports whether any of the header fields contains
// pattern (case-insensitive). An empty pattern matches if the field is
// present at all.
func matchHeaderFields(fields gomessage.HeaderFields, pattern string) bool {
	if pattern == "" {
		return fields.Len() > 0
	}
	pattern = strings.ToLower(pattern)
	for fields.Next() {
		v, _ := fields.Text()
		if strings.Contains(strings.ToLower(v), pattern) {
			return true
		}
	}
	return false
}

// matchEntity reports whether pattern appears in the entity — in a text
// part's decoded body, and (when includeHeader is set) in its headers.
// It recurses into multipart messages.
func matchEntity(e *gomessage.Entity, pattern string, includeHeader bool) bool {
	if pattern == "" {
		return true
	}
	if includeHeader && matchHeaderFields(e.Header.Fields(), pattern) {
		return true
	}
	if mr := e.MultipartReader(); mr != nil {
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return false
			}
			if matchEntity(part, pattern, includeHeader) {
				return true
			}
		}
		return false
	}
	t, _, err := e.Header.ContentType()
	if err != nil {
		return false
	}
	if !strings.HasPrefix(t, "text/") && !strings.HasPrefix(t, "message/") {
		return false
	}
	buf, err := io.ReadAll(e.Body)
	if err != nil {
		return false
	}
	return bytes.Contains(bytes.ToLower(buf), bytes.ToLower([]byte(pattern)))
}
