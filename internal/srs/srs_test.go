package srs

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("a-stable-secret-for-tests-32bytes!")

func TestRoundTrip(t *testing.T) {
	const sender = "alice@partners.example"
	const fwd = "oximail.test"

	enc, err := Encode(testSecret, sender, fwd)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasPrefix(enc, "SRS0=") {
		t.Errorf("encoded address %q is missing the SRS0 prefix", enc)
	}
	if !strings.HasSuffix(enc, "@"+fwd) {
		t.Errorf("encoded address %q is not hosted on the forwarder domain", enc)
	}

	got, err := Decode(testSecret, enc, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != sender {
		t.Errorf("round-trip got %q, want %q", got, sender)
	}
}

func TestIsRecognizesSRSAddresses(t *testing.T) {
	enc, err := Encode(testSecret, "alice@partners.example", "oximail.test")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !Is(enc) {
		t.Errorf("Is(%q) = false, want true", enc)
	}
	if Is("alice@partners.example") {
		t.Error("Is on a plain address returned true")
	}
	if Is("not-an-email") {
		t.Error("Is on a malformed address returned true")
	}
}

func TestDecodeRejectsAPlainAddress(t *testing.T) {
	_, err := Decode(testSecret, "alice@partners.example", time.Hour)
	if !errors.Is(err, ErrNotSRS) {
		t.Fatalf("Decode plain address: err = %v, want ErrNotSRS", err)
	}
}

func TestDecodeRejectsATamperedHash(t *testing.T) {
	enc, _ := Encode(testSecret, "alice@partners.example", "oximail.test")
	// Flip one character of the hash (right after "SRS0=").
	idx := len("SRS0=")
	tampered := []byte(enc)
	if tampered[idx] == 'A' {
		tampered[idx] = 'B'
	} else {
		tampered[idx] = 'A'
	}
	if _, err := Decode(testSecret, string(tampered), time.Hour); err == nil {
		t.Fatal("Decode accepted a tampered hash; want a mismatch error")
	}
}

func TestDecodeRejectsAWrongSecret(t *testing.T) {
	enc, _ := Encode(testSecret, "alice@partners.example", "oximail.test")
	other := []byte("a-DIFFERENT-secret-for-tests-yep!")
	if _, err := Decode(other, enc, time.Hour); err == nil {
		t.Fatal("Decode accepted an address signed with a different secret")
	}
}

func TestEncodeRequiresASecret(t *testing.T) {
	if _, err := Encode([]byte("too short"), "alice@partners.example", "oximail.test"); err == nil {
		t.Fatal("Encode accepted a < 16-byte secret")
	}
}
