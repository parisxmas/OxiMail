package queue

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log"
	"net/mail"
	"strings"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/parisxmas/OxiMail/internal/store"
)

// signMessage prepends a DKIM-Signature header to raw, signing with the
// From-header domain's key when one is configured (via oximailctl).
//
// It fails open: a missing key, an unparseable key, or any signing error
// leaves the message unsigned and is logged. A signing hiccup must never
// block delivery — unsigned mail still goes out.
func signMessage(st *store.Store, raw []byte) []byte {
	domain := fromHeaderDomain(raw)
	if domain == "" {
		return raw
	}
	d, err := st.GetDomain(domain)
	if err != nil || d.DKIMPrivateKey == "" {
		return raw // no DKIM key for this domain — send unsigned
	}
	key, err := parsePrivateKey(d.DKIMPrivateKey)
	if err != nil {
		log.Printf("queue: DKIM key for %s is unparseable (%v) — sending unsigned", domain, err)
		return raw
	}

	var signed bytes.Buffer
	err = dkim.Sign(&signed, bytes.NewReader(raw), &dkim.SignOptions{
		Domain:                 domain,
		Selector:               d.DKIMSelector,
		Signer:                 key,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
	})
	if err != nil {
		log.Printf("queue: DKIM signing for %s failed (%v) — sending unsigned", domain, err)
		return raw
	}
	return signed.Bytes()
}

// parsePrivateKey decodes a PEM-encoded PKCS#1 RSA private key.
func parsePrivateKey(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New("not a PEM block")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// fromHeaderDomain extracts the lower-cased domain of the message's From
// header, or "" if it cannot be parsed.
func fromHeaderDomain(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	addr, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		return ""
	}
	at := strings.LastIndexByte(addr.Address, '@')
	if at < 0 || at == len(addr.Address)-1 {
		return ""
	}
	return strings.ToLower(addr.Address[at+1:])
}
