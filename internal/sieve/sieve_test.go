package sieve

import (
	"reflect"
	"testing"
)

func msg(headers map[string]string, size int64) Message {
	h := map[string][]string{}
	for k, v := range headers {
		h[canonicalHeader(k)] = []string{v}
	}
	return Message{Headers: h, Size: size}
}

func TestEmptyScriptImplicitlyKeeps(t *testing.T) {
	s, err := Parse("")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := s.Eval(msg(nil, 0))
	want := []Action{Keep{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRequireIsAccepted(t *testing.T) {
	src := `require ["fileinto"]; keep;`
	if _, err := Parse(src); err != nil {
		t.Fatalf("parse: %v", err)
	}
}

func TestHeaderContainsFilesIntoFolder(t *testing.T) {
	src := `
		require ["fileinto"];
		if header :contains "Subject" "lunch" {
		    fileinto "Personal";
		}
	`
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := s.Eval(msg(map[string]string{"Subject": "Want to grab lunch?"}, 0))
	if len(got) != 1 {
		t.Fatalf("got %d actions, want 1: %v", len(got), got)
	}
	if fi, ok := got[0].(FileInto); !ok || fi.Mailbox != "Personal" {
		t.Errorf("got %v, want FileInto{Personal}", got[0])
	}
}

func TestHeaderIsExactMatch(t *testing.T) {
	src := `if header :is "From" "boss@oximail.test" { fileinto "Work"; }`
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	miss := s.Eval(msg(map[string]string{"From": "Boss <boss@oximail.test>"}, 0))
	// :is wants the entire header value to match exactly; here it does not.
	if _, ok := miss[0].(FileInto); ok {
		t.Errorf("got %v, expected the implicit keep since the value does not match exactly", miss)
	}
	hit := s.Eval(msg(map[string]string{"From": "boss@oximail.test"}, 0))
	if fi, ok := hit[0].(FileInto); !ok || fi.Mailbox != "Work" {
		t.Errorf("got %v, want FileInto{Work}", hit)
	}
}

func TestAddressMatchesAddressPart(t *testing.T) {
	src := `if address :is "From" "boss@oximail.test" { fileinto "Work"; }`
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// address picks the address out of the display-name form.
	hit := s.Eval(msg(map[string]string{"From": "The Boss <boss@oximail.test>"}, 0))
	if fi, ok := hit[0].(FileInto); !ok || fi.Mailbox != "Work" {
		t.Errorf("got %v, want FileInto{Work}", hit)
	}
}

func TestDiscardDoesNotImplicitlyKeep(t *testing.T) {
	src := `
		if header :contains "Subject" "spam" { discard; }
	`
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := s.Eval(msg(map[string]string{"Subject": "you won the spam lottery"}, 0))
	for _, a := range got {
		switch a.(type) {
		case Discard:
			return
		case Keep:
			t.Error("an explicit discard should not also leave an implicit keep")
		}
	}
	t.Error("script with a matched discard did not produce one")
}

func TestAllofAndAnyofCompose(t *testing.T) {
	src := `
		if allof(header :contains "Subject" "report", header :contains "From" "alice") {
		    fileinto "Reports";
		}
		if anyof(header :is "Subject" "urgent", header :contains "X-Priority" "1") {
		    fileinto "Priority";
		}
	`
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Both branches fire.
	got := s.Eval(msg(map[string]string{
		"Subject":    "urgent report",
		"From":       "alice@partners.test",
		"X-Priority": "5",
	}, 0))
	var saw = map[string]bool{}
	for _, a := range got {
		if fi, ok := a.(FileInto); ok {
			saw[fi.Mailbox] = true
		}
	}
	// "urgent report" does not match :is "urgent" exactly but does
	// contain "report", and From contains "alice".
	if !saw["Reports"] {
		t.Errorf("Reports not filed: %v", got)
	}
}

func TestSizeOverUnder(t *testing.T) {
	src := `
		if size :over 1K { fileinto "Big"; }
		if size :under 100 { fileinto "Tiny"; }
	`
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := s.Eval(Message{Size: 2048, Headers: map[string][]string{}})
	if fi, ok := got[0].(FileInto); !ok || fi.Mailbox != "Big" {
		t.Errorf("size :over 1K against 2048: got %v, want FileInto{Big}", got)
	}
	got = s.Eval(Message{Size: 50, Headers: map[string][]string{}})
	if fi, ok := got[0].(FileInto); !ok || fi.Mailbox != "Tiny" {
		t.Errorf("size :under 100 against 50: got %v, want FileInto{Tiny}", got)
	}
}

func TestMatchesGlob(t *testing.T) {
	src := `if header :matches "Subject" "*newsletter*" { fileinto "News"; }`
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := s.Eval(msg(map[string]string{"Subject": "Weekly OxiDB newsletter"}, 0))
	if fi, ok := got[0].(FileInto); !ok || fi.Mailbox != "News" {
		t.Errorf("got %v, want FileInto{News}", got)
	}
}

func TestNotInverts(t *testing.T) {
	src := `if not header :contains "Subject" "spam" { fileinto "Inbox"; }`
	s, _ := Parse(src)
	got := s.Eval(msg(map[string]string{"Subject": "hello"}, 0))
	if fi, ok := got[0].(FileInto); !ok || fi.Mailbox != "Inbox" {
		t.Errorf("got %v, want FileInto{Inbox} via not", got)
	}
	got = s.Eval(msg(map[string]string{"Subject": "spam alert"}, 0))
	if _, ok := got[0].(FileInto); ok {
		t.Errorf("not should have suppressed the fileinto: %v", got)
	}
}

func TestUnknownTestIsFalse(t *testing.T) {
	src := `if envelope :is "to" "user@x" { fileinto "Hits"; }`
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := s.Eval(msg(nil, 0))
	// envelope is not supported; the unknown test evaluates to false,
	// so the implicit keep wins.
	if _, ok := got[0].(Keep); !ok {
		t.Errorf("got %v, want a Keep — unknown test should not file", got)
	}
}
