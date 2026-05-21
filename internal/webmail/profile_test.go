package webmail

import (
	"strings"
	"testing"
)

func TestFormatFromHeader(t *testing.T) {
	cases := []struct {
		name        string
		displayName string
		address     string
		want        string
	}{
		{
			// Empty display name preserves the historical bare-address
			// shape; recipients render the address either way and we
			// don't want to silently change the on-wire shape for
			// accounts that never set a name.
			name:    "empty name → bare address",
			address: "alice@example.com",
			want:    "alice@example.com",
		},
		{
			// Whitespace-only names normalise to empty (handled by the
			// caller's TrimSpace), keeping the bare-address path.
			name:        "whitespace name → bare address",
			displayName: "   ",
			address:     "alice@example.com",
			want:        "alice@example.com",
		},
		{
			// Pure-ASCII simple name: net/mail unconditionally quotes
			// the phrase. Either bare-token or quoted is RFC-valid;
			// receivers render both identically.
			name:        "simple ASCII name",
			displayName: "Alice",
			address:     "alice@example.com",
			want:        `"Alice" <alice@example.com>`,
		},
		{
			// Specials force quoting + backslash-escape for the
			// embedded DQUOTE. We let net/mail handle that — the test
			// pins the contract we ship.
			name:        "ASCII with comma needs quoting",
			displayName: `Alice, Inc.`,
			address:     "alice@example.com",
			want:        `"Alice, Inc." <alice@example.com>`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatFromHeader(c.displayName, c.address)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	// Non-ASCII path: we don't lock the exact RFC 2047 output (Go's
	// encoder can choose Q or B and the chosen quote-set isn't
	// stable across versions), but we DO insist that the result is
	// pure ASCII and includes the address — anything else would
	// land literal UTF-8 bytes in the on-wire header, which some
	// receivers reject.
	t.Run("non-ASCII name is RFC 2047 encoded", func(t *testing.T) {
		got := formatFromHeader("Alîce", "alice@example.com")
		for _, r := range got {
			if r > 127 {
				t.Errorf("From header has non-ASCII byte %q in %q — RFC 2047 encoding broken", r, got)
			}
		}
		if !strings.Contains(got, "<alice@example.com>") {
			t.Errorf("expected address inside angle brackets, got %q", got)
		}
		if !strings.Contains(got, "=?") || !strings.Contains(got, "?=") {
			t.Errorf("expected RFC 2047 encoded-word, got %q", got)
		}
	})
}

func TestFormatFromAddr(t *testing.T) {
	// formatFromAddr produces the inbox-list metadata. It MUST stay
	// plaintext UTF-8 even for non-ASCII names — re-encoding to RFC
	// 2047 here would surface `=?utf-8?q?...?=` to the SPA, which
	// renders the encoded-word verbatim because it's not in the
	// business of decoding transport-layer encodings.
	cases := []struct {
		name        string
		displayName string
		address     string
		want        string
	}{
		{name: "empty → bare", address: "x@y.test", want: "x@y.test"},
		{name: "ASCII", displayName: "Alice", address: "alice@y.test", want: "Alice <alice@y.test>"},
		{name: "non-ASCII stays decoded", displayName: "Barış Akın", address: "b@y.test", want: "Barış Akın <b@y.test>"},
		// Whitespace is trimmed (handled by formatFromAddr, not the
		// caller) so a profile with a single accidental space at the
		// end doesn't surface as `Name  <addr>` in the list.
		{name: "trim whitespace", displayName: "  Alice  ", address: "x@y.test", want: "Alice <x@y.test>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatFromAddr(c.displayName, c.address)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestNormaliseDisplayName(t *testing.T) {
	t.Run("trims whitespace", func(t *testing.T) {
		got, err := normaliseDisplayName("  Alice  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "Alice" {
			t.Errorf("got %q, want %q", got, "Alice")
		}
	})

	t.Run("empty is allowed (clears the display name)", func(t *testing.T) {
		got, err := normaliseDisplayName("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	// CR / LF / NUL would let a caller inject extra header lines into
	// the outbound From — classic header-injection vector. Reject
	// outright at the API boundary, don't try to "fix" by stripping.
	for _, ch := range []string{"\r", "\n", "\x00", "x\ny", "x\rx"} {
		t.Run("rejects control char "+strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(ch, "\r", "\\r"), "\n", "\\n"), "\x00", "\\0"), func(t *testing.T) {
			if _, err := normaliseDisplayName(ch); err == nil {
				t.Errorf("normaliseDisplayName(%q) accepted a control char; must reject", ch)
			}
		})
	}

	t.Run("enforces length cap", func(t *testing.T) {
		long := strings.Repeat("a", MaxDisplayNameLength+1)
		if _, err := normaliseDisplayName(long); err == nil {
			t.Errorf("normaliseDisplayName accepted a %d-char name; cap is %d", len(long), MaxDisplayNameLength)
		}
	})

	t.Run("accepts at the cap", func(t *testing.T) {
		atCap := strings.Repeat("a", MaxDisplayNameLength)
		got, err := normaliseDisplayName(atCap)
		if err != nil {
			t.Fatalf("unexpected error at cap: %v", err)
		}
		if got != atCap {
			t.Errorf("got %q, want unchanged at cap", got)
		}
	})
}
