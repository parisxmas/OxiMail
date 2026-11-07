package sieve

import (
	"reflect"
	"strings"
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

// evalOne parses src and returns the actions for m, failing on a parse
// error.
func evalOne(t *testing.T, src string, m Message) []Action {
	t.Helper()
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return s.Eval(m)
}

func TestElsifElseBranches(t *testing.T) {
	src := `
		if header :is "Subject" "a" { fileinto "A"; }
		elsif header :is "Subject" "b" { fileinto "B"; }
		elsif header :is "Subject" "c" { fileinto "C"; }
		else { fileinto "Other"; }
	`
	for subject, want := range map[string]string{"a": "A", "b": "B", "c": "C", "z": "Other"} {
		got := evalOne(t, src, msg(map[string]string{"Subject": subject}, 0))
		if !reflect.DeepEqual(got, []Action{FileInto{Mailbox: want}}) {
			t.Errorf("Subject %q: got %v, want only FileInto{%s}", subject, got, want)
		}
	}
}

func TestElseNotRunAfterMatchingIf(t *testing.T) {
	got := evalOne(t, `if true { keep; } else { discard; } fileinto "After";`, msg(nil, 0))
	want := []Action{Keep{}, FileInto{Mailbox: "After"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStopInsideElse(t *testing.T) {
	got := evalOne(t, `if false { keep; } else { discard; stop; } fileinto "Never";`, msg(nil, 0))
	if !reflect.DeepEqual(got, []Action{Discard{}}) {
		t.Errorf("got %v, want only Discard", got)
	}
}

// Everything outside the subset must fail Parse: that one error is what
// refuses the script at set time (webmail, oximailctl) and gives default
// delivery at run time (smtp treats a Parse error as "no script").
func TestParseRefuses(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"unknown extension", `require ["fileinto", "vacation"]; keep;`, `"vacation"`},
		{"unknown extension, single string", `require "foo";`, `"foo"`},
		{"imap4flags cannot be honoured", `require "imap4flags";`, `"imap4flags"`},
		{"require after a command", `keep; require "fileinto";`, "require must come before"},
		{"require in a block", `if true { require "fileinto"; }`, "require must come before"},
		{"redirect", `redirect "a@b.test";`, `"redirect"`},
		{"reject", `reject "no";`, `"reject"`},
		{"vacation", `vacation "away";`, `"vacation"`},
		{"unknown command in a branch", `if true { keep; } else { notify "x"; }`, `"notify"`},
		{"unknown test", `if envelope :is "to" "user@x" { keep; }`, `"envelope"`},
		{"nested unknown test", `if not anyof(true, hasflag "x") { keep; }`, `"hasflag"`},
		{"fileinto :flags", `fileinto :flags "\\Seen" "X";`, ":flags"},
		{"fileinto :copy", `fileinto :copy "X";`, ":copy"},
		{"fileinto no mailbox", `fileinto :create;`, "mailbox name"},
		{"fileinto two mailboxes", `fileinto "X" "Y";`, "one mailbox"},
		{"fileinto list", `fileinto ["X"];`, "one mailbox"},
		{"unknown match type", `if header :regex "Subject" "x" { keep; }`, ":regex"},
		{"unknown comparator", `if header :comparator "i;unicode" "Subject" "x" { keep; }`, `"i;unicode"`},
		{"address part on header", `if header :domain "From" "x" { keep; }`, ":domain"},
		{"header missing keys", `if header "Subject" { keep; }`, "key list"},
		{"header extra argument", `if header "Subject" "a" "b" { keep; }`, "key list"},
		{"size without tag", `if size 10 { keep; }`, "size takes"},
		{"keep with argument", `keep "x";`, "no arguments"},
		{"elsif without if", `keep; elsif true { keep; }`, "without a matching if"},
		{"else without if", `else { keep; }`, "without a matching if"},
		{"stray closing brace", `} discard;`, "unexpected }"},
		{"missing semicolon", `keep discard;`, "expected ;"},
		{"unterminated block", `if true { keep;`, "expected }"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.src)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestSupportedRequires(t *testing.T) {
	src := `require ["fileinto", "mailbox", "comparator-i;octet", "comparator-i;ascii-casemap"];
		require "fileinto";
		keep;`
	if _, err := Parse(src); err != nil {
		t.Fatalf("parse: %v", err)
	}
}

func TestFileintoCreateKeepsMailboxName(t *testing.T) {
	for _, src := range []string{
		`require ["fileinto", "mailbox"]; fileinto :create "X";`,
		`fileinto :CREATE "X";`,
	} {
		got := evalOne(t, src, msg(nil, 0))
		if !reflect.DeepEqual(got, []Action{FileInto{Mailbox: "X"}}) {
			t.Errorf("%s: got %v, want FileInto{X}", src, got)
		}
	}
}

func TestHeaderFieldList(t *testing.T) {
	src := `if header :contains ["To", "Cc"] ["alice", "bob"] { fileinto "Hit"; }`
	hits := []map[string]string{
		{"To": "alice@x.test"},
		{"Cc": "alice@x.test"},
		{"Cc": "bob@x.test"},
	}
	for _, h := range hits {
		if got := evalOne(t, src, msg(h, 0)); !reflect.DeepEqual(got, []Action{FileInto{Mailbox: "Hit"}}) {
			t.Errorf("%v: got %v, want FileInto{Hit}", h, got)
		}
	}
	// The second field name must not be treated as a key.
	miss := msg(map[string]string{"To": "cc@x.test"}, 0)
	if got := evalOne(t, src, miss); !reflect.DeepEqual(got, []Action{Keep{}}) {
		t.Errorf("got %v, want the implicit keep", got)
	}
}

func TestAddressFieldList(t *testing.T) {
	src := `if address :is ["To", "Cc"] "boss@x.test" { fileinto "Hit"; }`
	got := evalOne(t, src, msg(map[string]string{"Cc": "The Boss <boss@x.test>"}, 0))
	if !reflect.DeepEqual(got, []Action{FileInto{Mailbox: "Hit"}}) {
		t.Errorf("got %v, want FileInto{Hit}", got)
	}
}

func TestAddressParts(t *testing.T) {
	m := msg(map[string]string{"From": "Boss <boss@corp.test>"}, 0)
	for src, want := range map[string]bool{
		`if address :localpart "From" "boss" { discard; }`:          true,
		`if address :domain "From" "corp.test" { discard; }`:        true,
		`if address :domain :is "From" "boss" { discard; }`:         false,
		`if address :all "From" "boss@corp.test" { discard; }`:      true,
		`if address :localpart :is "From" "corp.test" { discard; }`: false,
	} {
		_, hit := evalOne(t, src, m)[0].(Discard)
		if hit != want {
			t.Errorf("%s: matched = %v, want %v", src, hit, want)
		}
	}
}

func TestComparatorArgumentIsNotAField(t *testing.T) {
	m := msg(map[string]string{"Subject": "Lunch"}, 0)
	octet := `if header :comparator "i;octet" :is "Subject" "lunch" { discard; }`
	if _, hit := evalOne(t, octet, m)[0].(Discard); hit {
		t.Error("i;octet must be case-sensitive")
	}
	casemap := `if header :comparator "i;ascii-casemap" :is "Subject" "lunch" { discard; }`
	if _, hit := evalOne(t, casemap, m)[0].(Discard); !hit {
		t.Error("i;ascii-casemap must ignore case")
	}
}

func TestMatchesEscapesRegexSyntax(t *testing.T) {
	m := msg(map[string]string{"Subject": "a{2} 5*"}, 0)
	for _, src := range []string{
		`if header :matches "Subject" "a{2}*" { discard; }`,
		`if header :matches "Subject" "a{2} 5\\*" { discard; }`,
	} {
		if _, hit := evalOne(t, src, m)[0].(Discard); !hit {
			t.Errorf("%s did not match", src)
		}
	}
	lit := `if header :matches "Subject" "a{2} \\*" { discard; }`
	if _, hit := evalOne(t, lit, m)[0].(Discard); hit {
		t.Error(`\\* must match only a literal star`)
	}
}
