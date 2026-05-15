package arc

import (
	"bytes"
	"strings"
)

// Relaxed canonicalization, per RFC 6376 §3.4.2 (header) and §3.4.4
// (body). ARC reuses the same algorithm so the AMS body-hash and the
// AS / AMS header-hash inputs are identical to what DKIM would
// compute.

// canonicalizeHeader returns one header in relaxed form: lowercased
// name, leading whitespace after the colon dropped, internal runs of
// whitespace collapsed to a single SP, trailing whitespace stripped,
// terminated by CRLF.
//
// raw must already have CRLF / continuation lines stripped of folding
// — the helper accepts both folded and unfolded input by unfolding
// any CRLF + WSP it finds.
func canonicalizeHeader(name, value string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	// Unfold: CRLF followed by WSP becomes a single SP.
	value = unfold(value)
	// Collapse whitespace runs to a single SP and trim trailing WSP.
	value = collapseWS(value)
	value = strings.TrimRight(value, " \t")
	value = strings.TrimLeft(value, " \t")
	return name + ":" + value + "\r\n"
}

// unfold removes CRLF + WSP folding (RFC 5322 §2.2.3): a CRLF
// immediately followed by space or tab is treated as a single space.
func unfold(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' && i+2 < len(s) && s[i+1] == '\n' && (s[i+2] == ' ' || s[i+2] == '\t') {
			b.WriteByte(' ')
			i += 2 // skip CRLF; the WSP is replaced by the SP we just wrote
			continue
		}
		if s[i] == '\n' && i+1 < len(s) && (s[i+1] == ' ' || s[i+1] == '\t') {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// collapseWS reduces every run of spaces / tabs to a single space.
func collapseWS(s string) string {
	var b strings.Builder
	prevWS := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			if !prevWS {
				b.WriteByte(' ')
				prevWS = true
			}
			continue
		}
		prevWS = false
		b.WriteByte(c)
	}
	return b.String()
}

// canonicalizeBody returns the message body in relaxed form: WSP runs
// inside each line collapsed to a single SP, trailing WSP stripped,
// trailing empty lines reduced to one CRLF. The input must use CRLF
// line endings — it is expected to come straight from an RFC 5322
// message.
func canonicalizeBody(body []byte) []byte {
	if len(body) == 0 {
		return []byte("\r\n")
	}
	lines := bytes.Split(body, []byte("\r\n"))
	for i, line := range lines {
		l := collapseWS(string(line))
		l = strings.TrimRight(l, " \t")
		lines[i] = []byte(l)
	}
	// Trim trailing empty lines.
	for len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return []byte("\r\n")
	}
	return append(bytes.Join(lines, []byte("\r\n")), '\r', '\n')
}

// splitHeadersBody splits a raw message into its header block and
// body. The blank line that terminates the headers is consumed.
func splitHeadersBody(raw []byte) (headers, body []byte) {
	sep := []byte("\r\n\r\n")
	i := bytes.Index(raw, sep)
	if i < 0 {
		return raw, nil
	}
	return raw[:i+2], raw[i+4:]
}

// extractHeader returns the (name, value) of the first header whose
// name matches `want` (case-insensitive). The value is the unfolded
// content after the colon — leading whitespace trimmed, but otherwise
// raw (no canonicalization).
func extractHeader(headers []byte, want string) (string, string, bool) {
	want = strings.ToLower(want)
	for _, h := range splitHeaders(headers) {
		if i := strings.IndexByte(h, ':'); i > 0 {
			if strings.EqualFold(strings.TrimSpace(h[:i]), want) {
				return h[:i], strings.TrimLeft(h[i+1:], " \t"), true
			}
		}
	}
	return "", "", false
}

// splitHeaders breaks the header block into individual unfolded
// headers (one entry per logical header field). Continuation lines
// are joined onto their owner.
func splitHeaders(headers []byte) []string {
	var out []string
	var cur strings.Builder
	for _, rawLine := range bytes.Split(headers, []byte("\r\n")) {
		line := string(rawLine)
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			cur.WriteByte('\n') // preserve the fold so unfold handles it
			cur.WriteString(line)
			continue
		}
		if cur.Len() > 0 {
			out = append(out, unfold(cur.String()))
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		out = append(out, unfold(cur.String()))
	}
	return out
}
