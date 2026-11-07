package webmail

import (
	"bytes"
	"html"
	"strings"

	nethtml "golang.org/x/net/html"
)

// droppedTags are removed together with their content. Everything that
// runs code, loads a document, submits data or switches the parser into
// a mode where text is not text belongs here.
var droppedTags = tagSet(
	"script", "style", "link", "meta", "base", "head", "title",
	"iframe", "frame", "frameset", "object", "embed", "applet", "portal",
	"noscript", "noembed", "noframes", "template", "xmp", "plaintext",
	"svg", "math", "canvas", "audio", "video", "source", "track", "map", "area",
	"input", "button", "select", "option", "optgroup", "textarea", "datalist",
)

// allowedTags keep their element. A tag in neither set (including form,
// html and body) is unwrapped: the element goes, its children stay.
var allowedTags = tagSet(
	"a", "abbr", "address", "article", "aside", "b", "bdi", "bdo", "big",
	"blockquote", "br", "caption", "center", "cite", "code", "col", "colgroup",
	"dd", "del", "details", "dfn", "div", "dl", "dt", "em", "figcaption",
	"figure", "font", "footer", "h1", "h2", "h3", "h4", "h5", "h6", "header",
	"hr", "i", "img", "ins", "kbd", "li", "main", "mark", "nav", "ol", "p",
	"pre", "q", "rp", "rt", "ruby", "s", "samp", "section", "small", "span",
	"strike", "strong", "sub", "summary", "sup", "table", "tbody", "td",
	"tfoot", "th", "thead", "time", "tr", "tt", "u", "ul", "var", "wbr",
)

// allowedAttrs is the presentational attribute allowlist. href, src and
// style are handled separately; id and class are left out because the
// style sheets they would hook into are dropped.
var allowedAttrs = tagSet(
	"align", "alt", "bgcolor", "border", "cellpadding", "cellspacing", "color",
	"cols", "colspan", "datetime", "dir", "face", "headers", "height", "lang",
	"nowrap", "open", "reversed", "rowspan", "scope", "size", "span", "start",
	"title", "type", "valign", "value", "width",
)

// unsafeStyleTokens drop a whole style declaration. url() and friends
// are how CSS fetches, a backslash can spell any of them as a CSS
// escape, and a comment can split them.
var unsafeStyleTokens = []string{
	"url(", "image(", "image-set(", "cross-fade(", "element(", "expression(",
	"@import", "javascript:", "behavior", "-moz-binding", "://", "\\", "/*", "<", ">",
}

// safeDataImages are the data: URL prefixes kept on img. They load
// nothing from the network; SVG is excluded because it is a document.
var safeDataImages = []string{
	"data:image/png;base64,", "data:image/jpeg;base64,", "data:image/jpg;base64,",
	"data:image/gif;base64,", "data:image/webp;base64,",
}

func tagSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// sanitizeHTML reduces a sender-controlled HTML body to an allowlist of
// inert markup. The result is safe to place in a document, though the
// SPA still renders it in a sandboxed iframe as a second layer.
//
// Remote images (http, https, protocol-relative) are removed unless
// showImages is set, so opening a message fires no tracking pixel.
// hasRemote reports whether the message has any, shown or not, which is
// what drives the SPA's "show images" control.
func sanitizeHTML(src string, showImages bool) (clean string, hasRemote bool) {
	doc, err := nethtml.Parse(strings.NewReader(src))
	if err != nil {
		// The parser gives up on pathological nesting. Show the text.
		return "<pre>" + html.EscapeString(stripTags(src)) + "</pre>", false
	}
	body := findBody(doc)
	if body == nil {
		return "", false
	}
	s := sanitizer{showImages: showImages}
	s.children(body)

	var b bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := nethtml.Render(&b, c); err != nil {
			return "<pre>" + html.EscapeString(stripTags(src)) + "</pre>", false
		}
	}
	return b.String(), s.hasRemote
}

// findBody returns the document's body element, or nil for a document
// without one (a frameset).
func findBody(n *nethtml.Node) *nethtml.Node {
	if n.Type == nethtml.ElementNode && n.Data == "body" {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if b := findBody(c); b != nil {
			return b
		}
	}
	return nil
}

type sanitizer struct {
	showImages bool
	hasRemote  bool
}

// children cleans n's subtree in place.
func (s *sanitizer) children(n *nethtml.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		switch {
		case c.Type == nethtml.TextNode:
		case c.Type != nethtml.ElementNode, c.Namespace != "", droppedTags[c.Data]:
			n.RemoveChild(c)
		case allowedTags[c.Data]:
			s.attrs(c)
			s.children(c)
		default:
			// Unwrap, then carry on from the first hoisted child so
			// it gets cleaned too.
			if c.FirstChild != nil {
				next = c.FirstChild
			}
			for ch := c.FirstChild; ch != nil; ch = c.FirstChild {
				c.RemoveChild(ch)
				n.InsertBefore(ch, c)
			}
			n.RemoveChild(c)
		}
		c = next
	}
}

// attrs rebuilds an element's attribute list from the allowlist.
func (s *sanitizer) attrs(n *nethtml.Node) {
	var kept []nethtml.Attribute
	hasHref := false
	for _, a := range n.Attr {
		if a.Namespace != "" {
			continue
		}
		key := strings.ToLower(a.Key)
		switch {
		case key == "style":
			if v := sanitizeStyle(a.Val); v != "" {
				kept = append(kept, nethtml.Attribute{Key: key, Val: v})
			}
		case key == "href" && n.Data == "a":
			if v := sanitizeLink(a.Val); v != "" {
				kept = append(kept, nethtml.Attribute{Key: key, Val: v})
				hasHref = true
			}
		case key == "src" && n.Data == "img":
			if v := s.imageSource(a.Val); v != "" {
				kept = append(kept, nethtml.Attribute{Key: key, Val: v})
			}
		case allowedAttrs[key]:
			kept = append(kept, nethtml.Attribute{Key: key, Val: a.Val})
		}
	}
	if hasHref {
		// A link must leave the sandboxed frame, not navigate it.
		kept = append(kept,
			nethtml.Attribute{Key: "target", Val: "_blank"},
			nethtml.Attribute{Key: "rel", Val: "noopener noreferrer"})
	}
	n.Attr = kept
}

// sanitizeLink returns a cleaned href, or "" when its scheme is not one
// a mail link may use. Relative URLs go too: they would resolve against
// the webmail origin.
func sanitizeLink(raw string) string {
	u := cleanURL(raw)
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	switch urlScheme(u) {
	case "http", "https", "mailto":
		return u
	}
	return ""
}

// imageSource returns the src to keep for an img, or "" to remove it.
func (s *sanitizer) imageSource(raw string) string {
	u := cleanURL(raw)
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	switch urlScheme(u) {
	case "http", "https":
		s.hasRemote = true
		if s.showImages {
			return u
		}
	case "data":
		low := strings.ToLower(u)
		for _, p := range safeDataImages {
			if strings.HasPrefix(low, p) {
				return u
			}
		}
	}
	return ""
}

// cleanURL undoes what browsers undo before parsing a URL, so that
// "java\tscript:" and " javascript:" are seen for what they are.
func cleanURL(raw string) string {
	u := strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(raw)
	return strings.TrimFunc(u, func(r rune) bool { return r <= ' ' })
}

// urlScheme returns the lower-cased scheme of u, or "" if it has none.
func urlScheme(u string) string {
	for i, c := range u {
		switch c {
		case ':':
			return strings.ToLower(u[:i])
		case '/', '?', '#', '\\':
			return ""
		}
	}
	return ""
}

// sanitizeStyle keeps the declarations of a style attribute that cannot
// fetch anything or run anything.
func sanitizeStyle(style string) string {
	var kept []string
	for _, decl := range strings.Split(style, ";") {
		decl = strings.TrimSpace(decl)
		if decl == "" || !strings.Contains(decl, ":") {
			continue
		}
		low := strings.ToLower(decl)
		safe := true
		for _, tok := range unsafeStyleTokens {
			if strings.Contains(low, tok) {
				safe = false
				break
			}
		}
		if safe {
			kept = append(kept, decl)
		}
	}
	return strings.Join(kept, "; ")
}
