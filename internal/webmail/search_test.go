package webmail

import (
	"reflect"
	"testing"
	"time"

	"github.com/parisxmas/OxiMail/internal/store"
)

func TestParseSearchQuery(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want SearchQuery
	}{
		{name: "empty", raw: "", want: SearchQuery{}},
		{name: "whitespace only", raw: "   ", want: SearchQuery{}},
		{name: "bare term", raw: "report", want: SearchQuery{Bare: []string{"report"}}},
		{
			name: "from operator",
			raw:  "from:alice@x",
			want: SearchQuery{From: []string{"alice@x"}},
		},
		{
			name: "mixed operators + bare",
			raw:  "from:alice subject:foo report",
			want: SearchQuery{
				From:    []string{"alice"},
				Subject: []string{"foo"},
				Bare:    []string{"report"},
			},
		},
		{
			// Quoted strings keep embedded spaces — `from:"Alice Example"`
			// is a single token. Lowercase on the way in matches
			// case-insensitive substring intent.
			name: "quoted value",
			raw:  `from:"Alice Example" subject:"weekly report"`,
			want: SearchQuery{
				From:    []string{"alice example"},
				Subject: []string{"weekly report"},
			},
		},
		{
			name: "case-insensitive operator prefix",
			raw:  "FROM:alice SUBJECT:foo",
			want: SearchQuery{
				From:    []string{"alice"},
				Subject: []string{"foo"},
			},
		},
		{
			name: "has:attachment",
			raw:  "has:attachment",
			want: SearchQuery{HasAttachment: true},
		},
		{
			name: "has:attach short form",
			raw:  "has:attach",
			want: SearchQuery{HasAttachment: true},
		},
		{
			// has:<unknown> shouldn't silently drop — it falls through
			// to a bare term so the user sees an obvious zero-result
			// instead of "I typed has:foo and got my whole mailbox".
			name: "has unknown falls to bare",
			raw:  "has:foo",
			want: SearchQuery{Bare: []string{"has:foo"}},
		},
		{
			name: "before + after dash format",
			raw:  "after:2024-01-01 before:2024-12-31",
			want: SearchQuery{
				After:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				Before: time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "before slash format (gmail style)",
			raw:  "before:2024/06/15",
			want: SearchQuery{
				Before: time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC),
			},
		},
		{
			// Bad date → operator silently drops AND the original
			// token survives as bare. Better than a hard error: the
			// user sees results matching "2024" somewhere in the
			// body, and probably realises the format problem.
			name: "bad date falls back to bare",
			raw:  "before:notadate",
			want: SearchQuery{Bare: []string{"before:notadate"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseSearchQuery(c.raw)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("\n  got  %+v\n  want %+v", got, c.want)
			}
		})
	}
}

func TestSearchQueryEmpty(t *testing.T) {
	if !(SearchQuery{}).Empty() {
		t.Error("zero SearchQuery should be Empty()")
	}
	if (SearchQuery{Bare: []string{"x"}}).Empty() {
		t.Error("non-zero SearchQuery is not Empty()")
	}
}

func TestSearchMatches(t *testing.T) {
	// Build a message with known fields to drive the matcher.
	msg := &store.Message{
		Subject:    "Weekly report — Q3",
		FromAddr:   "Alice Example <alice@x.test>",
		ReceivedAt: "2024-06-15T12:00:00Z",
	}
	bodyText := "Hello team, here is the Q3 update with charts."
	toHeader := "team@x.test, bob@x.test"
	hasAttachment := true

	type fetchers struct {
		body, to string
		hasAtt   bool
	}
	defaultFetchers := fetchers{body: bodyText, to: toHeader, hasAtt: hasAttachment}

	cases := []struct {
		name  string
		query string
		f     fetchers
		want  bool
	}{
		// Empty matches everything — pages with no query should not
		// drop messages.
		{"empty matches", "", defaultFetchers, true},

		// Single-operator hits and misses.
		{"from hit", "from:alice", defaultFetchers, true},
		{"from miss", "from:bob", defaultFetchers, false},
		{"subject hit case-insensitive", "subject:WEEKLY", defaultFetchers, true},
		{"subject miss", "subject:budget", defaultFetchers, false},
		{"to hit", "to:bob", defaultFetchers, true},
		{"to miss", "to:carol", defaultFetchers, false},

		// has:attachment — true case + the false case where the
		// message has no attachment.
		{"has:attachment true", "has:attachment", defaultFetchers, true},
		{"has:attachment false", "has:attachment",
			fetchers{body: bodyText, to: toHeader, hasAtt: false}, false},

		// Date bounds. Message is dated 2024-06-15.
		{"after passes", "after:2024-01-01", defaultFetchers, true},
		{"after fails", "after:2024-07-01", defaultFetchers, false},
		{"before passes", "before:2024-12-31", defaultFetchers, true},
		{"before fails", "before:2024-05-01", defaultFetchers, false},
		// before:<the same day> must INCLUDE that day — gmail-style
		// boundary handling. The matcher compares to end-of-day.
		{"before inclusive of same day", "before:2024-06-15", defaultFetchers, true},

		// Bare term — searches subject + from + body.
		{"bare in subject", "report", defaultFetchers, true},
		{"bare in from", "alice", defaultFetchers, true},
		{"bare in body", "charts", defaultFetchers, true},
		{"bare nowhere", "elephant", defaultFetchers, false},

		// Multi-operator AND semantics.
		{"AND: from + subject hit", "from:alice subject:weekly", defaultFetchers, true},
		{"AND: from hit + subject miss", "from:alice subject:budget", defaultFetchers, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := ParseSearchQuery(c.query)
			fetchBody := func() string { return c.f.body }
			fetchTo := func() string { return c.f.to }
			fetchHasAtt := func() bool { return c.f.hasAtt }
			if got := q.matches(msg, fetchBody, fetchTo, fetchHasAtt); got != c.want {
				t.Errorf("query=%q: got %v, want %v", c.query, got, c.want)
			}
		})
	}
}

func TestSearchLazyFetch(t *testing.T) {
	// Metadata-only queries MUST NOT touch the body fetcher — the
	// blob store is the expensive side and a 1000-message inbox
	// running a from:alice search would tank the load time. This
	// test pins the contract: from: alone never invokes
	// fetchBodyText.
	msg := &store.Message{
		Subject:  "Anything",
		FromAddr: "alice@x.test",
	}
	bodyFetched := false
	q := ParseSearchQuery("from:alice")
	_ = q.matches(msg,
		func() string { bodyFetched = true; return "should not happen" },
		func() string { return "" },
		func() bool { return false },
	)
	if bodyFetched {
		t.Error("metadata-only query touched fetchBodyText — body fetch must stay lazy")
	}
}
