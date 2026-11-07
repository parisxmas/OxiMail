// Package sieve is a small interpreter for a strict subset of RFC 5228
// Sieve, used for per-account inbound filtering at delivery time.
//
// Supported, and nothing else:
//
//   - Control: require, if / elsif / else, stop.
//   - Actions: keep, discard, fileinto. fileinto takes one mailbox name
//     and the optional :create tag (RFC 5490). Delivery creates a missing
//     folder anyway, so :create changes nothing.
//   - Tests: header, address, exists, size (:over / :under), allof,
//     anyof, not, true, false. Field and key arguments take a string or
//     a string list.
//   - Match types :is (default), :contains, :matches. Comparators
//     "i;ascii-casemap" (default) and "i;octet". Address parts :all
//     (default), :localpart, :domain.
//   - require accepts "fileinto", "mailbox", "comparator-i;octet" and
//     "comparator-i;ascii-casemap". Unlike RFC 5228, fileinto and
//     :create work without their require, because scripts already
//     stored rely on that.
//
// Parse refuses everything else with an error naming the construct:
// other commands (redirect, reject, vacation, ...), other tests
// (envelope, ...), other tags (fileinto :flags, :copy, :count, ...)
// and any other extension in require (imap4flags, ...). A script is
// never half-run: callers validate with Parse when a script is set,
// and the delivery path treats a Parse error as "no script", so a
// stored script that does not parse gets default delivery.
//
// Usage:
//
//	script, err := sieve.Parse(text)
//	if err != nil { ... }
//	actions := script.Eval(sieve.Message{
//	    Headers: ..., Size: ..., Raw: ...,
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
// syntax error, or anything outside the supported subset (see the
// package doc), returns the location and a short description.
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
	mailbox   string    // fileinto
	test      test      // if
	block     []command // body of if
	elifs     []elseif
	elseBlock []command
}

type elseif struct {
	test  test
	block []command
}

// Comparators (RFC 5228 §2.7.3).
const (
	comparatorCasemap = "i;ascii-casemap"
	comparatorOctet   = "i;octet"
)

type test struct {
	name        string
	matchType   string   // header / address: "is", "contains", "matches"
	comparator  string   // header / address
	addressPart string   // address: "all", "localpart", "domain"
	fields      []string // header / address / exists
	keys        []string // header / address
	over        bool     // size: :over rather than :under
	limit       int64    // size
	tests       []test   // for allof / anyof / not
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

func (k tokKind) String() string {
	switch k {
	case tkIdent:
		return "an identifier"
	case tkString:
		return "a string"
	case tkNumber:
		return "a number"
	case tkTag:
		return "a tag"
	case tkLBrace:
		return "{"
	case tkRBrace:
		return "}"
	case tkLParen:
		return "("
	case tkRParen:
		return ")"
	case tkLBracket:
		return "["
	case tkRBracket:
		return "]"
	case tkComma:
		return ","
	case tkSemi:
		return ";"
	}
	return "end of script"
}

type token struct {
	kind tokKind
	val  string
	num  int64
	line int
}

// String names the token for error messages.
func (t token) String() string {
	if t.val != "" {
		return fmt.Sprintf("%q", t.val)
	}
	return t.kind.String()
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

// supportedExtensions is what `require` accepts. Everything in it is
// implemented; anything else makes Parse fail.
var supportedExtensions = map[string]bool{
	"fileinto":                   true,
	"mailbox":                    true,
	"comparator-i;octet":         true,
	"comparator-i;ascii-casemap": true,
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token { return p.tokens[p.pos] }
func (p *parser) take() token { t := p.tokens[p.pos]; p.pos++; return t }
func (p *parser) expect(k tokKind) (token, error) {
	t := p.peek()
	if t.kind != k {
		return t, fmt.Errorf("sieve: line %d: expected %v, got %v", t.line, k, t)
	}
	p.pos++
	return t, nil
}

// peekKeyword reports whether the next token is the given identifier.
func (p *parser) peekKeyword(name string) bool {
	t := p.peek()
	return t.kind == tkIdent && strings.EqualFold(t.val, name)
}

// parseBlock reads commands up to the end of the script (top) or the
// closing brace, which it leaves for the caller. require is legal only
// before the first other command of the script (RFC 5228 §3.2).
func (p *parser) parseBlock(top bool) ([]command, error) {
	var cmds []command
	requireAllowed := top
	for {
		t := p.peek()
		if t.kind == tkEOF {
			return cmds, nil
		}
		if t.kind == tkRBrace {
			if top {
				return nil, fmt.Errorf("sieve: line %d: unexpected }", t.line)
			}
			return cmds, nil
		}
		c, err := p.parseCommand()
		if err != nil {
			return nil, err
		}
		if c.name == "require" {
			if !requireAllowed {
				return nil, fmt.Errorf("sieve: line %d: require must come before any other command", t.line)
			}
			continue
		}
		requireAllowed = false
		cmds = append(cmds, c)
	}
}

// parseBraced reads a { ... } block.
func (p *parser) parseBraced() ([]command, error) {
	if _, err := p.expect(tkLBrace); err != nil {
		return nil, err
	}
	block, err := p.parseBlock(false)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tkRBrace); err != nil {
		return nil, err
	}
	return block, nil
}

func (p *parser) parseCommand() (command, error) {
	idTok, err := p.expect(tkIdent)
	if err != nil {
		return command{}, err
	}
	cmd := command{name: strings.ToLower(idTok.val)}
	switch cmd.name {
	case "if":
		return p.parseIf(cmd)
	case "elsif", "else":
		return command{}, fmt.Errorf("sieve: line %d: %s without a matching if", idTok.line, cmd.name)
	}

	args, err := p.parseArgs()
	if err != nil {
		return command{}, err
	}
	switch cmd.name {
	case "require":
		err = checkRequire(idTok.line, args)
	case "keep", "discard", "stop":
		if len(args) > 0 {
			err = fmt.Errorf("sieve: line %d: %s takes no arguments", idTok.line, cmd.name)
		}
	case "fileinto":
		cmd.mailbox, err = fileintoMailbox(idTok.line, args)
	default:
		err = fmt.Errorf("sieve: line %d: unsupported command %q", idTok.line, idTok.val)
	}
	if err != nil {
		return command{}, err
	}
	if _, err := p.expect(tkSemi); err != nil {
		return command{}, err
	}
	return cmd, nil
}

// parseIf reads the rest of an if statement, gluing any elsif / else
// that follow onto it. cmd is the already-named "if" command.
func (p *parser) parseIf(cmd command) (command, error) {
	var err error
	if cmd.test, err = p.parseTest(); err != nil {
		return command{}, err
	}
	if cmd.block, err = p.parseBraced(); err != nil {
		return command{}, err
	}
	for p.peekKeyword("elsif") {
		p.pos++
		var e elseif
		if e.test, err = p.parseTest(); err != nil {
			return command{}, err
		}
		if e.block, err = p.parseBraced(); err != nil {
			return command{}, err
		}
		cmd.elifs = append(cmd.elifs, e)
	}
	if p.peekKeyword("else") {
		p.pos++
		if cmd.elseBlock, err = p.parseBraced(); err != nil {
			return command{}, err
		}
	}
	return cmd, nil
}

// arg is one parsed command or test argument: a :tag (kind tkTag), a
// string or string list (tkString, list set for the bracketed form),
// or a number (tkNumber).
type arg struct {
	kind tokKind
	tag  string // lower-cased, without the colon
	strs []string
	list bool
	num  int64
	line int
}

// parseArgs reads arguments until a token that cannot start one.
func (p *parser) parseArgs() ([]arg, error) {
	var args []arg
	for {
		t := p.peek()
		switch t.kind {
		case tkTag:
			p.pos++
			args = append(args, arg{kind: tkTag, tag: strings.ToLower(t.val), line: t.line})
		case tkString:
			p.pos++
			args = append(args, arg{kind: tkString, strs: []string{t.val}, line: t.line})
		case tkNumber:
			p.pos++
			args = append(args, arg{kind: tkNumber, num: t.num, line: t.line})
		case tkLBracket:
			ss, err := p.parseStringList()
			if err != nil {
				return nil, err
			}
			args = append(args, arg{kind: tkString, strs: ss, list: true, line: t.line})
		default:
			return args, nil
		}
	}
}

// checkRequire validates a require command's arguments: one string or
// string list naming only supported extensions.
func checkRequire(line int, args []arg) error {
	if len(args) != 1 || args[0].kind != tkString {
		return fmt.Errorf("sieve: line %d: require takes one string or string list", line)
	}
	for _, ext := range args[0].strs {
		if !supportedExtensions[ext] {
			return fmt.Errorf("sieve: line %d: unsupported extension %q in require", line, ext)
		}
	}
	return nil
}

// fileintoMailbox validates fileinto's arguments and returns the
// mailbox name. :flags is refused because the delivery path has no way
// to set flags on the filed message.
func fileintoMailbox(line int, args []arg) (string, error) {
	mailbox, seen := "", false
	for _, a := range args {
		switch {
		case a.kind == tkTag && a.tag == "create":
		case a.kind == tkTag && a.tag == "flags":
			return "", fmt.Errorf("sieve: line %d: fileinto :flags is not supported, flags cannot be applied at delivery", a.line)
		case a.kind == tkTag:
			return "", fmt.Errorf("sieve: line %d: fileinto: unsupported tag :%s", a.line, a.tag)
		case a.kind == tkString && !a.list && !seen:
			mailbox, seen = a.strs[0], true
		default:
			return "", fmt.Errorf("sieve: line %d: fileinto takes exactly one mailbox name", a.line)
		}
	}
	if mailbox == "" {
		return "", fmt.Errorf("sieve: line %d: fileinto needs a mailbox name", line)
	}
	return mailbox, nil
}

func (p *parser) parseTest() (test, error) {
	idTok, err := p.expect(tkIdent)
	if err != nil {
		return test{}, err
	}
	tst := test{name: strings.ToLower(idTok.val)}
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
	case "header", "address", "exists", "size":
	default:
		return test{}, fmt.Errorf("sieve: line %d: unsupported test %q", idTok.line, idTok.val)
	}

	args, err := p.parseArgs()
	if err != nil {
		return test{}, err
	}
	switch tst.name {
	case "header", "address":
		err = tst.parseMatchArgs(idTok.line, args)
	case "exists":
		if len(args) != 1 || args[0].kind != tkString {
			err = fmt.Errorf("sieve: line %d: exists takes one string or string list", idTok.line)
		} else {
			tst.fields = args[0].strs
		}
	case "size":
		err = tst.parseSizeArgs(idTok.line, args)
	}
	if err != nil {
		return test{}, err
	}
	return tst, nil
}

// parseMatchArgs fills in a header or address test: optional match
// type, comparator and (address only) address-part tags, then the
// field list and the key list, each a string or string list.
func (t *test) parseMatchArgs(line int, args []arg) error {
	t.matchType, t.comparator, t.addressPart = "is", comparatorCasemap, "all"
	var matchSet, cmpSet, partSet bool
	var positional [][]string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a.kind == tkNumber {
			return fmt.Errorf("sieve: line %d: %s does not take a number", a.line, t.name)
		}
		if a.kind == tkString {
			positional = append(positional, a.strs)
			continue
		}
		switch {
		case a.tag == "is" || a.tag == "contains" || a.tag == "matches":
			if matchSet {
				return fmt.Errorf("sieve: line %d: %s has more than one match type", a.line, t.name)
			}
			t.matchType, matchSet = a.tag, true
		case a.tag == "comparator":
			if cmpSet {
				return fmt.Errorf("sieve: line %d: %s has more than one :comparator", a.line, t.name)
			}
			i++
			if i >= len(args) || args[i].kind != tkString || args[i].list {
				return fmt.Errorf("sieve: line %d: :comparator needs a comparator name", a.line)
			}
			t.comparator, cmpSet = args[i].strs[0], true
			if t.comparator != comparatorCasemap && t.comparator != comparatorOctet {
				return fmt.Errorf("sieve: line %d: unsupported comparator %q", a.line, t.comparator)
			}
		case t.name == "address" && (a.tag == "all" || a.tag == "localpart" || a.tag == "domain"):
			if partSet {
				return fmt.Errorf("sieve: line %d: address has more than one address part", a.line)
			}
			t.addressPart, partSet = a.tag, true
		default:
			return fmt.Errorf("sieve: line %d: %s: unsupported tag :%s", a.line, t.name, a.tag)
		}
	}
	if len(positional) != 2 {
		return fmt.Errorf("sieve: line %d: %s takes a field list and a key list", line, t.name)
	}
	t.fields, t.keys = positional[0], positional[1]
	return nil
}

// parseSizeArgs fills in a size test: exactly one of :over / :under,
// then a number.
func (t *test) parseSizeArgs(line int, args []arg) error {
	if len(args) != 2 || args[0].kind != tkTag || args[1].kind != tkNumber ||
		(args[0].tag != "over" && args[0].tag != "under") {
		return fmt.Errorf("sieve: line %d: size takes :over or :under and a number", line)
	}
	t.over, t.limit = args[0].tag == "over", args[1].num
	return nil
}

func (p *parser) parseStringList() ([]string, error) {
	if _, err := p.expect(tkLBracket); err != nil {
		return nil, err
	}
	var out []string
	for {
		s, err := p.expect(tkString)
		if err != nil {
			return nil, err
		}
		out = append(out, s.val)
		if p.peek().kind != tkComma {
			break
		}
		p.pos++
	}
	if _, err := p.expect(tkRBracket); err != nil {
		return nil, err
	}
	return out, nil
}

// -----------------------------------------------------------------------
// Evaluator
// -----------------------------------------------------------------------

type evalCtx struct {
	msg         Message
	actions     []Action
	hadFileInto bool
	kept        bool
	discarded   bool
	stop        bool
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
	case "if":
		c.runIfChain(cmd)
	case "keep":
		c.kept = true
		c.actions = append(c.actions, Keep{})
	case "fileinto":
		c.actions = append(c.actions, FileInto{Mailbox: cmd.mailbox})
		c.hadFileInto = true
	case "discard":
		c.actions = append(c.actions, Discard{})
		c.discarded = true
	case "stop":
		c.stop = true
	}
}

// runIfChain runs an if / elsif / else chain: the first branch whose
// test passes, or the else block when none does.
func (c *evalCtx) runIfChain(cmd command) {
	if c.evalTest(cmd.test) {
		c.run(cmd.block)
		return
	}
	for _, e := range cmd.elifs {
		if c.evalTest(e.test) {
			c.run(e.block)
			return
		}
	}
	c.run(cmd.elseBlock)
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
		return !c.evalTest(t.tests[0])
	case "exists":
		for _, name := range t.fields {
			if _, ok := c.msg.Headers[canonicalHeader(name)]; !ok {
				return false
			}
		}
		return true
	case "size":
		if t.over {
			return c.msg.Size > t.limit
		}
		return c.msg.Size < t.limit
	case "header":
		return matchAnyHeaderValue(c.msg, t)
	case "address":
		return matchAnyAddress(c.msg, t)
	}
	return false
}

// matchAnyHeaderValue runs the chosen match type against every value
// of the named header(s) and reports a hit if any key matches.
func matchAnyHeaderValue(m Message, t test) bool {
	for _, fld := range t.fields {
		for _, v := range m.Headers[canonicalHeader(fld)] {
			for _, key := range t.keys {
				if matchString(v, key, t.matchType, t.comparator) {
					return true
				}
			}
		}
	}
	return false
}

// matchAnyAddress is like matchAnyHeaderValue but parses each header
// value as an address list and matches the chosen part of each address.
func matchAnyAddress(m Message, t test) bool {
	for _, fld := range t.fields {
		for _, v := range m.Headers[canonicalHeader(fld)] {
			addrs, err := mail.ParseAddressList(v)
			if err != nil {
				continue
			}
			for _, a := range addrs {
				part := addressPart(a.Address, t.addressPart)
				for _, key := range t.keys {
					if matchString(part, key, t.matchType, t.comparator) {
						return true
					}
				}
			}
		}
	}
	return false
}

// addressPart picks the :all, :localpart or :domain part of addr.
func addressPart(addr, part string) string {
	at := strings.LastIndexByte(addr, '@')
	switch {
	case part == "localpart" && at >= 0:
		return addr[:at]
	case part == "domain":
		return addr[at+1:]
	}
	return addr
}

// matchString applies the chosen comparison to v vs pattern.
func matchString(v, pattern, mt, comparator string) bool {
	if comparator != comparatorOctet {
		v, pattern = strings.ToLower(v), strings.ToLower(pattern)
	}
	switch mt {
	case "contains":
		return strings.Contains(v, pattern)
	case "matches":
		// "*" matches any run, "?" matches one char. The naive
		// translation to a regexp is fine for our purposes.
		re, err := regexp.Compile("(?s)^" + globToRegex(pattern) + "$")
		if err != nil {
			return false
		}
		return re.MatchString(v)
	default:
		return v == pattern
	}
}

// globToRegex turns a Sieve :matches pattern into a Go regular
// expression. RFC 5228 §2.7.1: "*" matches zero or more chars, "?"
// matches one, a backslash makes the next char literal. All other
// regex metacharacters are escaped.
func globToRegex(pat string) string {
	var b strings.Builder
	rs := []rune(pat)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		case '\\':
			if i+1 < len(rs) {
				i++
			}
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		}
	}
	return b.String()
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
