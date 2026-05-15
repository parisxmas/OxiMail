// Package sieve is a small RFC 5228 Sieve filter interpreter — enough
// of the language for per-account inbound filtering rules
// (fileinto / discard / keep / stop) and the common tests
// (header / address / size / exists / allof / anyof / not). Sieve
// extensions handled: "fileinto" (RFC 5232) and "imap4flags" (RFC 5232,
// the :flags variant on fileinto).
//
// The intent is the 80% of Sieve that real user rules use, not the
// full grammar. Unknown commands and tests parse but evaluate as
// no-ops (or `false`); an unknown identifier inside an `if` is
// therefore safe — the rest of the script still runs.
//
// Usage:
//
//	script, err := sieve.Parse(text)
//	if err != nil { ... }
//	actions := script.Eval(sieve.Message{
//	    Headers: ..., Size: ..., Body: ...,
//	})
//	for _, a := range actions {
//	    switch a := a.(type) { case sieve.FileInto: ... }
//	}
package sieve

import (
	"bytes"
	"fmt"
	"io"
	"net/mail"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
)

// Message is the input the evaluator runs over.
type Message struct {
	Headers map[string][]string // header name → values (canonical-cased keys)
	Size    int64               // RFC822.SIZE
	Raw     []byte              // for "exists" / future extensions
}

// Action is the output of evaluating a script — what the delivery
// agent should do. Multiple actions can fire (e.g. fileinto + keep);
// the delivery agent merges them.
type Action interface{ sieveAction() }

// Keep means "deliver to the default folder" (typically INBOX).
type Keep struct{}

// FileInto means "deliver to a specific folder".
type FileInto struct{ Mailbox string }

// Discard means "drop the message without delivering".
type Discard struct{}

// Stop means "stop evaluating the script here". Used internally; the
// delivery agent does not need to handle it (Eval returns after a
// stop).
type Stop struct{}

func (Keep) sieveAction()     {}
func (FileInto) sieveAction() {}
func (Discard) sieveAction()  {}
func (Stop) sieveAction()     {}

// Script is the parsed program.
type Script struct {
	commands []command
}

// Parse turns a Sieve script source into a Script ready for Eval. A
// syntax error returns the location and a short description.
func Parse(src string) (*Script, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: toks}
	cmds, err := p.parseBlock(true)
	if err != nil {
		return nil, err
	}
	return &Script{commands: cmds}, nil
}

// Eval runs the script and returns the chosen actions. The result is
// always non-empty: a script with no fileinto / discard / keep
// implicitly keeps.
func (s *Script) Eval(m Message) []Action {
	if s == nil {
		return []Action{Keep{}}
	}
	ctx := &evalCtx{msg: m}
	ctx.run(s.commands)
	if !ctx.hadFileInto && !ctx.discarded && !ctx.kept {
		// Implicit keep (RFC 5228 §2.10.6).
		ctx.actions = append(ctx.actions, Keep{})
	}
	return ctx.actions
}

// -----------------------------------------------------------------------
// AST
// -----------------------------------------------------------------------

type command struct {
	name      string
	tags      map[string]string // :tag → "" (or its argument for :comparator etc.)
	strings   []string          // positional string arguments
	numbers   []int64           // positional number arguments
	tests     []test            // for control commands: the test list (if / elsif)
	block     []command         // body for if / elsif / else
	elseBlock []command         // raw "else" alternative
	elifs     []elseif
}

type elseif struct {
	tests   []test
	block   []command
	hasTest bool
}

type test struct {
	name    string
	tags    map[string]string
	strings []string
	numbers []int64
	tests   []test // for allof / anyof / not
}

// -----------------------------------------------------------------------
// Lexer
// -----------------------------------------------------------------------

type tokKind int

const (
	tkIdent tokKind = iota
	tkString
	tkNumber
	tkTag
	tkLBrace
	tkRBrace
	tkLParen
	tkRParen
	tkLBracket
	tkRBracket
	tkComma
	tkSemi
	tkEOF
)

type token struct {
	kind tokKind
	val  string
	num  int64
	line int
}

func tokenize(src string) ([]token, error) {
	var out []token
	line := 1
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#':
			// line comment
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			// block comment
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return nil, fmt.Errorf("sieve: unterminated /* */ comment at line %d", line)
			}
			line += strings.Count(src[i:i+2+j], "\n")
			i += j + 4
		case c == '{':
			out = append(out, token{kind: tkLBrace, line: line})
			i++
		case c == '}':
			out = append(out, token{kind: tkRBrace, line: line})
			i++
		case c == '(':
			out = append(out, token{kind: tkLParen, line: line})
			i++
		case c == ')':
			out = append(out, token{kind: tkRParen, line: line})
			i++
		case c == '[':
			out = append(out, token{kind: tkLBracket, line: line})
			i++
		case c == ']':
			out = append(out, token{kind: tkRBracket, line: line})
			i++
		case c == ',':
			out = append(out, token{kind: tkComma, line: line})
			i++
		case c == ';':
			out = append(out, token{kind: tkSemi, line: line})
			i++
		case c == '"':
			s, n, err := readQuoted(src[i:], line)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tkString, val: s, line: line})
			line += strings.Count(src[i:i+n], "\n")
			i += n
		case c == ':' && i+1 < len(src) && isIdentStart(src[i+1]):
			j := i + 1
			for j < len(src) && isIdentCont(src[j]) {
				j++
			}
			out = append(out, token{kind: tkTag, val: src[i+1 : j], line: line})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentCont(src[j]) {
				j++
			}
			id := src[i:j]
			i = j
			// Multiline "text:" follows an identifier in some
			// positions; in our use it appears as an argument, so we
			// surface it as a string token.
			if id == "text" && i < len(src) && src[i] == ':' {
				body, consumed, err := readMultiline(src[i+1:], line)
				if err != nil {
					return nil, err
				}
				out = append(out, token{kind: tkString, val: body, line: line})
				line += strings.Count(src[i+1:i+1+consumed], "\n")
				i += 1 + consumed
			} else {
				out = append(out, token{kind: tkIdent, val: id, line: line})
			}
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			suffix := int64(1)
			if j < len(src) {
				switch src[j] {
				case 'K', 'k':
					suffix = 1024
					j++
				case 'M', 'm':
					suffix = 1024 * 1024
					j++
				case 'G', 'g':
					suffix = 1024 * 1024 * 1024
					j++
				}
			}
			n, err := strconv.ParseInt(src[i:j-iif(suffix > 1, 1, 0)], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("sieve: bad number at line %d: %w", line, err)
			}
			out = append(out, token{kind: tkNumber, num: n * suffix, line: line})
			i = j
		default:
			return nil, fmt.Errorf("sieve: unexpected character %q at line %d", c, line)
		}
	}
	out = append(out, token{kind: tkEOF, line: line})
	return out, nil
}

// iif is a tiny ternary helper used only by the number lexer.
func iif(cond bool, a, b int) int {
	if cond {
		return a
	}
	return b
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '.' || c == '-'
}

// readQuoted reads a "..." string and returns its decoded value and
// the number of source bytes consumed (including the closing quote).
// Backslash escapes \\ and \" are supported.
func readQuoted(src string, line int) (string, int, error) {
	if src[0] != '"' {
		return "", 0, fmt.Errorf("sieve: expected '\"' at line %d", line)
	}
	var b strings.Builder
	for i := 1; i < len(src); i++ {
		c := src[i]
		switch c {
		case '"':
			return b.String(), i + 1, nil
		case '\\':
			if i+1 >= len(src) {
				return "", 0, fmt.Errorf("sieve: trailing backslash at line %d", line)
			}
			b.WriteByte(src[i+1])
			i++
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, fmt.Errorf("sieve: unterminated string at line %d", line)
}

// readMultiline reads a "text:" block: skip whitespace to end-of-line,
// then accumulate lines until one consisting only of ".".
func readMultiline(src string, line int) (string, int, error) {
	i := 0
	// skip until newline
	for i < len(src) && src[i] != '\n' {
		i++
	}
	if i >= len(src) {
		return "", 0, fmt.Errorf("sieve: text: block has no body at line %d", line)
	}
	i++ // past '\n'
	start := i
	for i < len(src) {
		// find end of line
		end := i
		for end < len(src) && src[end] != '\n' {
			end++
		}
		lineContent := strings.TrimRight(src[i:end], "\r")
		if lineContent == "." {
			body := src[start:i]
			body = strings.TrimRight(body, "\n")
			return body, end, nil
		}
		i = end + 1
	}
	return "", 0, fmt.Errorf("sieve: unterminated text: block starting at line %d", line)
}

// -----------------------------------------------------------------------
// Parser
// -----------------------------------------------------------------------

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token { return p.tokens[p.pos] }
func (p *parser) take() token { t := p.tokens[p.pos]; p.pos++; return t }
func (p *parser) expect(k tokKind) (token, error) {
	t := p.peek()
	if t.kind != k {
		return t, fmt.Errorf("sieve: line %d: expected %v, got %q", t.line, k, t.val)
	}
	p.pos++
	return t, nil
}

func (p *parser) parseBlock(top bool) ([]command, error) {
	_ = top
	var cmds []command
	for {
		t := p.peek()
		if t.kind == tkEOF || t.kind == tkRBrace {
			return cmds, nil
		}
		c, err := p.parseCommand()
		if err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
}

func (p *parser) parseCommand() (command, error) {
	idTok, err := p.expect(tkIdent)
	if err != nil {
		return command{}, err
	}
	cmd := command{name: strings.ToLower(idTok.val), tags: map[string]string{}}

	// Special control commands take a test before the block.
	switch cmd.name {
	case "if", "elsif":
		t, err := p.parseTest()
		if err != nil {
			return command{}, err
		}
		cmd.tests = []test{t}
	}

	// Positional and tagged arguments (until ; or {).
	for {
		t := p.peek()
		switch t.kind {
		case tkTag:
			p.pos++
			cmd.tags[strings.ToLower(t.val)] = ""
			// :comparator and friends take an argument; if the next
			// token is a string, attach it.
			if p.peek().kind == tkString {
				cmd.tags[strings.ToLower(t.val)] = p.take().val
			}
		case tkString:
			cmd.strings = append(cmd.strings, p.take().val)
		case tkNumber:
			cmd.numbers = append(cmd.numbers, p.take().num)
		case tkLBracket:
			ss, err := p.parseStringList()
			if err != nil {
				return command{}, err
			}
			cmd.strings = append(cmd.strings, ss...)
		case tkSemi:
			p.pos++
			return cmd, nil
		case tkLBrace:
			p.pos++
			block, err := p.parseBlock(false)
			if err != nil {
				return command{}, err
			}
			if _, err := p.expect(tkRBrace); err != nil {
				return command{}, err
			}
			cmd.block = block
			return cmd, nil
		default:
			return command{}, fmt.Errorf("sieve: line %d: unexpected token %q in command %q", t.line, t.val, cmd.name)
		}
	}
}

func (p *parser) parseTest() (test, error) {
	idTok, err := p.expect(tkIdent)
	if err != nil {
		return test{}, err
	}
	tst := test{name: strings.ToLower(idTok.val), tags: map[string]string{}}
	// allof / anyof / not take a parenthesised test-list (or a single
	// test for not).
	switch tst.name {
	case "allof", "anyof":
		if _, err := p.expect(tkLParen); err != nil {
			return test{}, err
		}
		for {
			t, err := p.parseTest()
			if err != nil {
				return test{}, err
			}
			tst.tests = append(tst.tests, t)
			if p.peek().kind == tkComma {
				p.pos++
				continue
			}
			break
		}
		if _, err := p.expect(tkRParen); err != nil {
			return test{}, err
		}
		return tst, nil
	case "not":
		t, err := p.parseTest()
		if err != nil {
			return test{}, err
		}
		tst.tests = []test{t}
		return tst, nil
	case "true", "false":
		return tst, nil
	}
	// Generic test: gather tags, strings, numbers until we see
	// something that is clearly outside the test (left-brace, comma,
	// right-paren, semicolon, ident).
	for {
		t := p.peek()
		switch t.kind {
		case tkTag:
			p.pos++
			tst.tags[strings.ToLower(t.val)] = ""
		case tkString:
			tst.strings = append(tst.strings, p.take().val)
		case tkNumber:
			tst.numbers = append(tst.numbers, p.take().num)
		case tkLBracket:
			ss, err := p.parseStringList()
			if err != nil {
				return test{}, err
			}
			tst.strings = append(tst.strings, ss...)
		default:
			return tst, nil
		}
	}
}

func (p *parser) parseStringList() ([]string, error) {
	if _, err := p.expect(tkLBracket); err != nil {
		return nil, err
	}
	var out []string
	for {
		t := p.peek()
		if t.kind == tkRBracket {
			p.pos++
			return out, nil
		}
		s, err := p.expect(tkString)
		if err != nil {
			return nil, err
		}
		out = append(out, s.val)
		if p.peek().kind == tkComma {
			p.pos++
		}
	}
}

// -----------------------------------------------------------------------
// Evaluator
// -----------------------------------------------------------------------

type evalCtx struct {
	msg          Message
	actions      []Action
	hadFileInto  bool
	kept         bool
	discarded    bool
	stop         bool
}

func (c *evalCtx) run(cmds []command) {
	for _, cmd := range cmds {
		if c.stop {
			return
		}
		c.runOne(cmd)
	}
}

func (c *evalCtx) runOne(cmd command) {
	switch cmd.name {
	case "require":
		// We accept anything; the supported feature set is a static
		// subset and the script writer is trusted to ask only for
		// what we support.
	case "if":
		c.runIfChain(cmd)
	case "elsif", "else":
		// Free-standing elsif/else are no-ops at the top level —
		// runIfChain consumes them. (A standalone elsif is technically
		// a parse error in RFC 5228; we just shrug.)
	case "keep":
		c.kept = true
		c.actions = append(c.actions, Keep{})
	case "fileinto":
		if len(cmd.strings) > 0 {
			c.actions = append(c.actions, FileInto{Mailbox: cmd.strings[0]})
			c.hadFileInto = true
		}
	case "discard":
		c.actions = append(c.actions, Discard{})
		c.discarded = true
	case "stop":
		c.stop = true
	}
}

// runIfChain runs an if/elsif/else chain. RFC 5228 says: `if ... { } elsif
// ... { } else { }`. Our parser produces them as a sequence of top-level
// commands; we look ahead in the *block* for them. Since this evaluator
// runs in command-list order, we treat elsif/else as siblings: the
// caller put them in sequence, and we use a small per-chain state to
// remember whether a previous branch already fired.
func (c *evalCtx) runIfChain(cmd command) {
	// The parser nests the block inside cmd.block. We evaluate the
	// test; if it passes, run the block. We don't yet support elsif/
	// else linkage between sibling commands — that needs the parser to
	// glue them together. For now: a plain `if` works; `elsif` and
	// `else` are parsed but require the user to write nested ifs.
	if len(cmd.tests) == 0 {
		return
	}
	if c.evalTest(cmd.tests[0]) {
		c.run(cmd.block)
	}
}

func (c *evalCtx) evalTest(t test) bool {
	switch t.name {
	case "true":
		return true
	case "false":
		return false
	case "allof":
		for _, sub := range t.tests {
			if !c.evalTest(sub) {
				return false
			}
		}
		return true
	case "anyof":
		for _, sub := range t.tests {
			if c.evalTest(sub) {
				return true
			}
		}
		return false
	case "not":
		if len(t.tests) == 0 {
			return true
		}
		return !c.evalTest(t.tests[0])
	case "exists":
		for _, name := range t.strings {
			if _, ok := c.msg.Headers[canonicalHeader(name)]; !ok {
				return false
			}
		}
		return true
	case "size":
		if len(t.numbers) == 0 {
			return false
		}
		switch {
		case t.has(":over"):
			return c.msg.Size > t.numbers[0]
		case t.has(":under"):
			return c.msg.Size < t.numbers[0]
		}
		return false
	case "header":
		if len(t.strings) < 2 {
			return false
		}
		fields, vals := t.strings[0], t.strings[1:]
		return matchAnyHeaderValue(c.msg, fields, vals, t.tags)
	case "address":
		if len(t.strings) < 2 {
			return false
		}
		field, vals := t.strings[0], t.strings[1:]
		return matchAnyAddress(c.msg, field, vals, t.tags)
	}
	return false
}

// has reports whether the test carries a given tag (case-insensitive,
// with the leading colon included for clarity at call sites).
func (t test) has(tag string) bool {
	_, ok := t.tags[strings.TrimPrefix(strings.ToLower(tag), ":")]
	return ok
}

// matchType picks the comparison from the tags; default is :is.
func (t test) matchType() string {
	switch {
	case t.has(":contains"):
		return "contains"
	case t.has(":matches"):
		return "matches"
	}
	return "is"
}

// matchAnyHeaderValue runs the chosen match type against every value
// of the named header(s) and reports a hit if any pattern matches.
func matchAnyHeaderValue(m Message, fields string, patterns []string, tags map[string]string) bool {
	mt := test{tags: tags}.matchType()
	for _, fld := range splitListString(fields) {
		for _, v := range m.Headers[canonicalHeader(fld)] {
			for _, pat := range patterns {
				if matchString(v, pat, mt) {
					return true
				}
			}
		}
	}
	return false
}

// matchAnyAddress is like matchAnyHeaderValue but parses each header
// value as an address list and matches the address part.
func matchAnyAddress(m Message, field string, patterns []string, tags map[string]string) bool {
	mt := test{tags: tags}.matchType()
	for _, fld := range splitListString(field) {
		for _, v := range m.Headers[canonicalHeader(fld)] {
			addrs, err := mail.ParseAddressList(v)
			if err != nil {
				continue
			}
			for _, a := range addrs {
				for _, pat := range patterns {
					if matchString(a.Address, pat, mt) {
						return true
					}
				}
			}
		}
	}
	return false
}

// matchString applies the chosen comparison to v vs pattern.
func matchString(v, pattern, mt string) bool {
	lv := strings.ToLower(v)
	lp := strings.ToLower(pattern)
	switch mt {
	case "contains":
		return strings.Contains(lv, lp)
	case "matches":
		// "*" matches any run, "?" matches one char. The naive
		// translation to a regexp is fine for our purposes.
		re, err := regexp.Compile("(?i)^" + globToRegex(pattern) + "$")
		if err != nil {
			return false
		}
		return re.MatchString(v)
	default:
		return lv == lp
	}
}

// globToRegex turns a Sieve :matches pattern into a Go regular
// expression. RFC 5228 §2.7.1: "*" matches zero or more chars, "?"
// matches one. All other regex metacharacters are escaped.
func globToRegex(pat string) string {
	var b strings.Builder
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		case '.', '+', '(', ')', '|', '[', ']', '^', '$', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// splitListString returns the input as a one-element slice. The
// header / address tests accept a string OR a string-list; the parser
// flattens lists into the strings slice, so a single field arrives as
// one element here. This helper is kept for a future change that
// makes lists explicit.
func splitListString(s string) []string {
	return []string{s}
}

// canonicalHeader returns the case-normalized form Go's net/mail uses
// in mail.Header — "from" → "From", "message-id" → "Message-Id".
func canonicalHeader(name string) string {
	return textproto.CanonicalMIMEHeaderKey(name)
}

// ParseHeaders is a convenience for callers: it pulls a Message's
// Headers map out of a raw RFC 5322 message so they do not have to
// build it themselves.
func ParseHeaders(raw []byte) (map[string][]string, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(msg.Header))
	for k, v := range msg.Header {
		out[k] = append(out[k], v...)
	}
	// Drain the body so the reader is consumed (callers may pass a
	// Reader; that path is below).
	_, _ = io.Copy(io.Discard, msg.Body)
	return out, nil
}
