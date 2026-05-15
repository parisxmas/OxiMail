package arc

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"
)

const sampleRaw = "From: Alice <alice@partners.test>\r\n" +
	"To: bob@oximail.test\r\n" +
	"Subject: hello\r\n" +
	"Date: Mon, 01 Jan 2024 10:00:00 +0000\r\n" +
	"Message-Id: <abc@partners.test>\r\n" +
	"\r\n" +
	"hello world\r\n"

func genKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return k
}

func TestSealAddsThreeHeadersInExpectedOrder(t *testing.T) {
	key := genKey(t)
	sealed, err := Seal([]byte(sampleRaw), SealOptions{
		Domain:      "oximail.test",
		Selector:    "s1",
		Key:         key,
		AuthResults: "oximail.test; spf=pass smtp.mailfrom=alice@partners.test",
		Now:         time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	s := string(sealed)
	// All three headers are present, prepended above the original
	// From line, in the AS / AMS / AAR order RFC 8617 §5.1.3
	// specifies.
	asIdx := strings.Index(s, "ARC-Seal:")
	amsIdx := strings.Index(s, "ARC-Message-Signature:")
	aarIdx := strings.Index(s, "ARC-Authentication-Results:")
	fromIdx := strings.Index(s, "From: Alice")
	if asIdx < 0 || amsIdx < 0 || aarIdx < 0 {
		t.Fatalf("sealed message missing ARC headers:\n%s", s)
	}
	if !(asIdx < amsIdx && amsIdx < aarIdx && aarIdx < fromIdx) {
		t.Errorf("ARC headers in wrong order: AS=%d AMS=%d AAR=%d From=%d", asIdx, amsIdx, aarIdx, fromIdx)
	}
	// The instance tag is i=1; cv=none.
	if !strings.Contains(s, "i=1") || !strings.Contains(s, "cv=none") {
		t.Errorf("expected i=1 and cv=none in the seal:\n%s", s)
	}
}

func TestSealRoundTripsThroughVerify(t *testing.T) {
	key := genKey(t)
	sealed, err := Seal([]byte(sampleRaw), SealOptions{
		Domain:      "oximail.test",
		Selector:    "s1",
		Key:         key,
		AuthResults: "oximail.test; spf=pass",
		Now:         time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// VerifyLastInstance recomputes the body hash, the AMS signature,
	// and the AS signature against our key's public counterpart.
	if err := VerifyLastInstance(sealed, &key.PublicKey); err != nil {
		t.Fatalf("VerifyLastInstance on our own seal: %v", err)
	}
}

func TestSealRefusesAPriorChain(t *testing.T) {
	prior := "ARC-Seal: i=1; a=rsa-sha256; cv=none; d=other.test; s=s; b=AAAA\r\n" + sampleRaw
	key := genKey(t)
	_, err := Seal([]byte(prior), SealOptions{
		Domain: "oximail.test", Selector: "s1", Key: key,
		AuthResults: "oximail.test",
	})
	if !errors.Is(err, ErrPriorChain) {
		t.Fatalf("Seal on a prior-chain message: err = %v, want ErrPriorChain", err)
	}
}

func TestHasChainDetectsAnyARCHeader(t *testing.T) {
	cases := map[string]bool{
		"ARC-Seal: ...\r\n" + sampleRaw:                  true,
		"ARC-Message-Signature: ...\r\n" + sampleRaw:     true,
		"ARC-Authentication-Results: ...\r\n" + sampleRaw: true,
		sampleRaw:                                        false,
	}
	for in, want := range cases {
		if got := HasChain([]byte(in)); got != want {
			t.Errorf("HasChain(%.30q…) = %v, want %v", in, got, want)
		}
	}
}

func TestSealDetectsBodyModification(t *testing.T) {
	key := genKey(t)
	sealed, err := Seal([]byte(sampleRaw), SealOptions{
		Domain: "oximail.test", Selector: "s1", Key: key,
		AuthResults: "oximail.test; spf=pass",
		Now:         time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Tamper with the body — the body hash in AMS must no longer
	// match.
	tampered := strings.Replace(string(sealed), "hello world", "goodbye world", 1)
	if err := VerifyLastInstance([]byte(tampered), &key.PublicKey); err == nil {
		t.Fatal("VerifyLastInstance accepted a tampered body — bh= check is broken")
	}
}

func TestSealDetectsHeaderModification(t *testing.T) {
	key := genKey(t)
	sealed, err := Seal([]byte(sampleRaw), SealOptions{
		Domain: "oximail.test", Selector: "s1", Key: key,
		AuthResults: "oximail.test; spf=pass",
		Now:         time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Tamper with a header that is in the AMS h= list — the AMS
	// signature must no longer verify.
	tampered := strings.Replace(string(sealed), "Subject: hello", "Subject: tampered", 1)
	if err := VerifyLastInstance([]byte(tampered), &key.PublicKey); err == nil {
		t.Fatal("VerifyLastInstance accepted a tampered Subject header")
	}
}

func TestSealRequiresAKey(t *testing.T) {
	if _, err := Seal([]byte(sampleRaw), SealOptions{Domain: "x", Selector: "s"}); err == nil {
		t.Fatal("Seal without a key returned no error")
	}
}
