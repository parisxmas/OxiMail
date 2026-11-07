package spam

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/mail"
	"strings"
)

// AuthResult is what the envelope stage learned about a message. It
// feeds the Authentication-Results header on delivered mail and the
// ARC-Authentication-Results header on forwarded mail. The zero value
// means no authentication ran (a Permissive pipeline).
type AuthResult struct {
	Checked    bool
	SPF        string // RFC 8601 result: pass, fail, softfail, neutral, none, temperror, permerror
	MailFrom   string // the identity SPF was checked against, "" for a null sender
	DKIM       []DKIMResult
	DMARC      string // pass, fail, none, or permerror for an unusable From
	FromDomain string // the From domain the DMARC result is about, "" if there is none
}

// DKIMResult is the outcome of verifying one DKIM signature.
type DKIMResult struct {
	Result string // pass, fail, temperror or permerror
	Domain string // the signature's d= tag
}

// Value renders the result as an RFC 8601 header value, without the
// field name, so it serves both Authentication-Results and
// ARC-Authentication-Results. authservID names this server. Every
// message-derived value is sender-controlled, so it is quoted when it
// is not a plain token.
func (a AuthResult) Value(authservID string) string {
	if !a.Checked {
		return authservID + "; none"
	}
	parts := []string{authservID}

	spfPart := "spf=" + a.SPF
	if a.MailFrom != "" {
		spfPart += " smtp.mailfrom=" + pvalue(a.MailFrom)
	}
	parts = append(parts, spfPart)

	if len(a.DKIM) == 0 {
		parts = append(parts, "dkim=none")
	}
	for _, d := range a.DKIM {
		parts = append(parts, "dkim="+d.Result+" header.d="+pvalue(d.Domain))
	}

	dmarcPart := "dmarc=" + a.DMARC
	if a.FromDomain != "" {
		dmarcPart += " header.from=" + pvalue(a.FromDomain)
	}
	parts = append(parts, dmarcPart)
	return strings.Join(parts, "; ")
}

// pvalue renders s as an RFC 8601 property value: bare when it is made
// of atom characters, otherwise a quoted-string with anything outside
// printable ASCII replaced, so a hostile address cannot add a fake
// result or break out of the header.
func pvalue(s string) string {
	plain := s != ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("!#$%&'*+-/=?^_`{|}~.@", c) >= 0) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c > 0x7e:
			b.WriteByte('?')
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// fromParser parses address headers without caring what charset a
// display name is in. Only the addr-spec matters here, and net/mail's
// default decoder fails the whole address on any charset other than
// UTF-8, ISO-8859-1 and US-ASCII.
var fromParser = mail.AddressParser{WordDecoder: &mime.WordDecoder{
	CharsetReader: func(_ string, input io.Reader) (io.Reader, error) { return input, nil },
}}

// FromAddresses returns every address in the message's From header
// fields, in order. A second From field is included rather than
// ignored: anything that signs or judges a message by its first From
// could otherwise be shown a different address from the one a mail
// client displays. It fails if a field does not parse or there are no
// addresses at all.
func FromAddresses(raw []byte) ([]string, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, field := range msg.Header["From"] {
		list, err := fromParser.ParseList(field)
		if err != nil {
			return nil, err
		}
		for _, addr := range list {
			out = append(out, addr.Address)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("spam: message has no From address")
	}
	return out, nil
}
