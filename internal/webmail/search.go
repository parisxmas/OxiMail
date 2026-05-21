package webmail

import (
	"strings"
	"time"

	"github.com/parisxmas/OxiMail/internal/store"
)

// SearchQuery is the parsed shape of the webmail search input. The
// SPA hands the raw user string over the wire (`?q=…`); the server
// parses gmail-style operators here and applies them per message.
//
// Semantics: every populated field is ANDed against the message. A
// term with no recognised prefix (e.g. a bare word) goes into Bare
// and matches the union of subject + from + body — the historical
// "search everything" behaviour. An empty SearchQuery matches every
// message (so q="" returns the full mailbox unchanged).
type SearchQuery struct {
	// From / To / Subject hold case-folded substrings extracted from
	// `from:`, `to:`, `subject:` operators. Each must appear in the
	// corresponding field of the message; multiple uses of the same
	// operator are ANDed (gmail uses OR; AND is easier to reason
	// about for a personal mailbox and we can revisit if anyone
	// needs the OR form).
	From    []string
	To      []string
	Subject []string

	// HasAttachment requires the message to expose at least one MIME
	// attachment. Triggered by `has:attachment` or `has:attach`.
	HasAttachment bool

	// Before / After bound the message's RFC 3339 internal date.
	// Triggered by `before:YYYY-MM-DD` and `after:YYYY-MM-DD`. We
	// also accept the gmail slash form (YYYY/MM/DD). Both bounds
	// are inclusive on the day at UTC midnight.
	Before time.Time
	After  time.Time

	// Bare holds free-text terms — anything that wasn't a recognised
	// operator. Each must match somewhere in subject / from / body
	// (substring, case-folded). The body match is what makes the
	// search input feel real; the existing matchesQuery code already
	// pulls the body when nothing matched the metadata.
	Bare []string
}

// Empty reports whether the query has no constraints — fast path so
// handleListMessages can skip every per-message check when the user
// hasn't typed anything.
func (q SearchQuery) Empty() bool {
	return len(q.From) == 0 && len(q.To) == 0 && len(q.Subject) == 0 &&
		!q.HasAttachment && q.Before.IsZero() && q.After.IsZero() &&
		len(q.Bare) == 0
}

// ParseSearchQuery tokenises the raw user input and extracts
// operators. Tokens are whitespace-separated; quoted strings keep
// embedded spaces (`from:"alice example"` is one token). Unrecognised
// operators stay as bare terms — so a future operator that we don't
// know about yet (or a user typo) still falls back to a substring
// search instead of being silently dropped.
func ParseSearchQuery(raw string) SearchQuery {
	tokens := tokenizeSearch(raw)
	var q SearchQuery
	for _, tok := range tokens {
		op, val := splitOp(tok)
		switch op {
		case "from":
			if val != "" {
				q.From = append(q.From, strings.ToLower(val))
			}
		case "to":
			if val != "" {
				q.To = append(q.To, strings.ToLower(val))
			}
		case "subject":
			if val != "" {
				q.Subject = append(q.Subject, strings.ToLower(val))
			}
		case "has":
			switch strings.ToLower(val) {
			case "attachment", "attach":
				q.HasAttachment = true
			default:
				// has:<unknown> — same fall-back rule as a bad
				// date: surface to the user as a bare term so
				// they see *some* effect (zero results) rather
				// than a silent "filter ignored".
				q.Bare = append(q.Bare, strings.ToLower(tok))
			}
		case "before":
			if t, ok := parseSearchDate(val); ok {
				q.Before = t
			} else {
				// Bad date — preserve the token as a bare term so
				// the user gets *some* feedback (zero results that
				// hint at the typo) instead of a silent drop that
				// looks like no filter was applied.
				q.Bare = append(q.Bare, strings.ToLower(tok))
			}
		case "after":
			if t, ok := parseSearchDate(val); ok {
				q.After = t
			} else {
				q.Bare = append(q.Bare, strings.ToLower(tok))
			}
		default:
			// Unknown prefix or bare word — fold to lowercase for the
			// substring matcher. We keep the original case-folded
			// form, including any "x:y" that didn't match a known op,
			// so `from:` typo'd as `frm:alice` still finds alice's
			// mail via the broad bare-term search.
			low := strings.ToLower(tok)
			if low != "" {
				q.Bare = append(q.Bare, low)
			}
		}
	}
	return q
}

// tokenizeSearch splits raw on whitespace but keeps double-quoted
// runs as a single token. Quotes are stripped from the result; the
// caller never sees them. Unterminated quotes are treated as if the
// closing quote were at end-of-input — easier than erroring on a
// half-typed search.
func tokenizeSearch(raw string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range raw {
		switch {
		case r == '"':
			inQuote = !inQuote
		case !inQuote && (r == ' ' || r == '\t'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// splitOp pulls the operator prefix out of a token. Returns
// ("", token) for bare words. The prefix lookup is case-insensitive
// because users type "From:" as often as "from:".
func splitOp(tok string) (op, val string) {
	idx := strings.IndexByte(tok, ':')
	if idx <= 0 || idx == len(tok)-1 {
		return "", tok
	}
	prefix := strings.ToLower(tok[:idx])
	switch prefix {
	case "from", "to", "subject", "has", "before", "after":
		return prefix, tok[idx+1:]
	}
	return "", tok
}

// parseSearchDate accepts YYYY-MM-DD and YYYY/MM/DD at UTC midnight.
// Anything else fails (and the operator falls through to a bare
// term so the user gets a sensible substring search instead of zero
// results).
func parseSearchDate(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", "2006/01/02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// matches reports whether the message satisfies the parsed query.
// fetchBodyText is a lazy fetcher returning the message's plain-text
// rendering — we only call it when no cheaper check can answer, so
// metadata-only searches stay off the blob store. fetchAttachments
// is similarly lazy and only invoked when has:attachment is set.
//
// AND across operator groups; within a group every term must also
// match (AND). Empty query short-circuits to true via Empty().
func (q SearchQuery) matches(
	m *store.Message,
	fetchBodyText func() string,
	fetchToHeader func() string,
	fetchHasAttachment func() bool,
) bool {
	if q.Empty() {
		return true
	}

	subjLow := strings.ToLower(m.Subject)
	fromLow := strings.ToLower(m.FromAddr)

	for _, needle := range q.From {
		if !strings.Contains(fromLow, needle) {
			return false
		}
	}
	for _, needle := range q.Subject {
		if !strings.Contains(subjLow, needle) {
			return false
		}
	}
	if len(q.To) > 0 {
		toLow := strings.ToLower(fetchToHeader())
		for _, needle := range q.To {
			if !strings.Contains(toLow, needle) {
				return false
			}
		}
	}
	if !q.After.IsZero() {
		t, ok := parseMsgDate(m)
		if !ok || t.Before(q.After) {
			return false
		}
	}
	if !q.Before.IsZero() {
		t, ok := parseMsgDate(m)
		// before:DATE is inclusive on DATE itself, so we compare
		// against end-of-day (24h after midnight).
		if !ok || !t.Before(q.Before.Add(24*time.Hour)) {
			return false
		}
	}
	if q.HasAttachment && !fetchHasAttachment() {
		return false
	}
	if len(q.Bare) > 0 {
		bodyLow := ""
		bodyFetched := false
		for _, needle := range q.Bare {
			if strings.Contains(subjLow, needle) || strings.Contains(fromLow, needle) {
				continue
			}
			if !bodyFetched {
				bodyLow = strings.ToLower(fetchBodyText())
				bodyFetched = true
			}
			if !strings.Contains(bodyLow, needle) {
				return false
			}
		}
	}
	return true
}

// parseMsgDate pulls a comparable time.Time out of the message's
// stored date string (RFC 3339, set by AppendMessage). Returns false
// on a malformed value — the caller treats that as a non-match.
func parseMsgDate(m *store.Message) (time.Time, bool) {
	if m.ReceivedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, m.ReceivedAt)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
