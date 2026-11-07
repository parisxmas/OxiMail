package vacation

import (
	"strings"
	"testing"
	"time"
)

const sample = "From: alice@partners.test\r\n" +
	"To: me@oximail.test\r\n" +
	"Subject: lunch?\r\n" +
	"Message-Id: <abc@partners.test>\r\n" +
	"Date: Mon, 01 Jan 2024 10:00:00 +0000\r\n" +
	"\r\n" +
	"want to grab lunch?\r\n"

var me = []string{"me@oximail.test"}

func TestShouldReplyAcceptsAPlainMessage(t *testing.T) {
	if !ShouldReply([]byte(sample), "alice@partners.test", me) {
		t.Error("a plain person-to-person message should get an auto-reply")
	}
}

func TestShouldReplyRefusesABounce(t *testing.T) {
	if ShouldReply([]byte(sample), "", me) {
		t.Error("auto-reply to a null-envelope bounce; RFC 3834 forbids it")
	}
}

func TestShouldReplyRefusesAutoSubmitted(t *testing.T) {
	msg := strings.Replace(sample, "Subject: lunch?\r\n", "Subject: lunch?\r\nAuto-Submitted: auto-generated\r\n", 1)
	if ShouldReply([]byte(msg), "alice@partners.test", me) {
		t.Error("auto-reply to a message that already advertises Auto-Submitted")
	}
}

func TestShouldReplyRefusesMailingList(t *testing.T) {
	msg := strings.Replace(sample, "Subject: lunch?\r\n", "Subject: news\r\nList-Id: <devs.oximail.test>\r\n", 1)
	if ShouldReply([]byte(msg), "list@oximail.test", me) {
		t.Error("auto-reply to a List-Id'd message; this would spam the list")
	}
	msg = strings.Replace(sample, "Subject: lunch?\r\n", "Subject: news\r\nPrecedence: bulk\r\n", 1)
	if ShouldReply([]byte(msg), "list@oximail.test", me) {
		t.Error("auto-reply to a Precedence: bulk message")
	}
}

func TestShouldReplyRefusesBCC(t *testing.T) {
	// Our address is in selfAddresses but does not appear in To / Cc.
	msg := "From: alice@partners.test\r\nTo: someone-else@oximail.test\r\nSubject: hi\r\n\r\n"
	if ShouldReply([]byte(msg), "alice@partners.test", me) {
		t.Error("auto-reply to a message we were BCC'd into — leaks our address")
	}
}

func TestReplyHasRFC3834Markers(t *testing.T) {
	rule := Rule{Subject: "I'm away", Body: "Back next week."}
	out, err := Reply(rule, []byte(sample), "me@oximail.test", "alice@partners.test", "vid@oximail.test", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"From: me@oximail.test",
		"To: alice@partners.test",
		"Subject: I'm away",
		"Auto-Submitted: auto-replied",
		"In-Reply-To: <abc@partners.test>",
		"References: <abc@partners.test>",
		"Back next week.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("reply missing %q\n---\n%s", want, s)
		}
	}
}

func TestSuppressorRateLimitsPerSender(t *testing.T) {
	s := NewSuppressor(7 * 24 * time.Hour)
	now := time.Unix(1700000000, 0)
	if !s.Allow(1, "alice@partners.test", now) {
		t.Fatal("first reply blocked")
	}
	if s.Allow(1, "alice@partners.test", now.Add(time.Hour)) {
		t.Error("second reply within the window allowed")
	}
	if !s.Allow(1, "bob@partners.test", now.Add(time.Hour)) {
		t.Error("different sender blocked")
	}
	if !s.Allow(2, "alice@partners.test", now.Add(time.Hour)) {
		t.Error("same sender but different account blocked")
	}
	// Past the window — allowed again.
	if !s.Allow(1, "alice@partners.test", now.Add(8*24*time.Hour)) {
		t.Error("after window elapsed, reply blocked")
	}
}

func TestSuppressorHonoursPerRuleWindow(t *testing.T) {
	s := NewSuppressor(7 * 24 * time.Hour)
	now := time.Unix(1700000000, 0)
	oneDay := s.WindowOf(1)
	if !s.AllowWithin(1, "alice@partners.test", now, oneDay) {
		t.Fatal("first reply blocked")
	}
	if s.AllowWithin(1, "alice@partners.test", now.Add(23*time.Hour), oneDay) {
		t.Error("reply inside a one-day window allowed")
	}
	if !s.AllowWithin(1, "alice@partners.test", now.Add(25*time.Hour), oneDay) {
		t.Error("reply after a one-day window blocked")
	}

	month := s.WindowOf(30)
	if !s.AllowWithin(2, "alice@partners.test", now, month) {
		t.Fatal("first reply blocked")
	}
	if s.AllowWithin(2, "alice@partners.test", now.Add(20*24*time.Hour), month) {
		t.Error("reply inside a thirty-day window allowed")
	}
}

func TestWindowOf(t *testing.T) {
	s := NewSuppressor(7 * 24 * time.Hour)
	for days, want := range map[int]time.Duration{
		0:       7 * 24 * time.Hour,
		-3:      7 * 24 * time.Hour,
		2:       2 * 24 * time.Hour,
		1 << 40: 365 * 24 * time.Hour,
	} {
		if got := s.WindowOf(days); got != want {
			t.Errorf("WindowOf(%d) = %v, want %v", days, got, want)
		}
	}
}

// Sweep must go by each entry's own window: one longer than the default
// has to survive past the default, or the reply limit silently resets.
func TestSweepKeepsLongWindows(t *testing.T) {
	s := NewSuppressor(7 * 24 * time.Hour)
	now := time.Unix(1700000000, 0)
	s.AllowWithin(1, "long@partners.test", now, s.WindowOf(30))
	s.AllowWithin(1, "short@partners.test", now, s.WindowOf(1))

	s.Sweep(now.Add(10 * 24 * time.Hour))
	if len(s.seen) != 1 {
		t.Fatalf("after sweep: %d entries, want 1", len(s.seen))
	}
	if s.AllowWithin(1, "long@partners.test", now.Add(10*24*time.Hour), s.WindowOf(30)) {
		t.Error("sweep forgot an entry still inside its window")
	}
	s.Sweep(now.Add(31 * 24 * time.Hour))
	if len(s.seen) != 0 {
		t.Errorf("expired entry survived the sweep: %d left", len(s.seen))
	}
}
