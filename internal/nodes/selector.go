package nodes

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

// A CSS selector engine, sized to what an HTML node is actually used for:
// pulling values out of a scraped page or a device's status endpoint.
//
// Node-RED's HTML node runs cheerio, which implements essentially all of CSS.
// This implements a subset — type, id, class, attribute, descendant, child and
// group — and refuses anything else at deploy time rather than at the first
// message. That refusal is the important half. A selector using :nth-child
// against an engine that ignores pseudo-classes would match the wrong elements
// and keep working, which is how a flow ends up reading the wrong meter for a
// month.

// selectorGroup is a comma-separated list; an element matches if any branch does.
type selectorGroup []selectorChain

// selectorChain is a sequence of compound selectors joined by combinators, held
// innermost-last: "div > p .x" parses to [div, p, .x] with the combinators on
// each step after the first.
type selectorChain []selectorStep

type selectorStep struct {
	compound compoundSelector
	// child is true when this step is joined to the previous one by ">" rather
	// than by a descendant space.
	child bool
}

// compoundSelector is everything that must hold of one element at once.
type compoundSelector struct {
	tag     string // "" means any
	id      string
	classes []string
	attrs   []attrSelector
}

type attrSelector struct {
	name string
	// op is "" (presence), "=", "^=", "$=" or "*=".
	op    string
	value string
}

// parseSelector compiles a selector, refusing what it does not implement.
func parseSelector(s string) (selectorGroup, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("the selector is empty")
	}

	var group selectorGroup
	for _, branch := range splitOutsideBrackets(s, ',') {
		chain, err := parseSelectorChain(strings.TrimSpace(branch))
		if err != nil {
			return nil, err
		}
		group = append(group, chain)
	}
	return group, nil
}

// splitOutsideBrackets splits on a separator that is not inside [ ], so that an
// attribute value containing one does not tear the selector in half.
func splitOutsideBrackets(s string, sep byte) []string {
	var (
		out   []string
		cur   strings.Builder
		depth int
	)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '[':
			depth++
			cur.WriteByte(c)
		case c == ']':
			if depth > 0 {
				depth--
			}
			cur.WriteByte(c)
		case c == sep && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

// parseSelectorChain reads one comma-free branch.
//
// It scans rather than splitting on whitespace, because a combinator inside an
// attribute value is not a combinator: "[href*=a>b]" contains a > that means
// nothing, and treating it as a child combinator would silently change which
// elements match.
func parseSelectorChain(s string) (selectorChain, error) {
	var (
		chain     selectorChain
		nextChild bool
		cur       strings.Builder
		depth     int
	)

	flush := func() error {
		tok := strings.TrimSpace(cur.String())
		cur.Reset()
		if tok == "" {
			return nil
		}
		c, err := parseCompound(tok)
		if err != nil {
			return err
		}
		chain = append(chain, selectorStep{compound: c, child: nextChild})
		nextChild = false
		return nil
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if depth > 0 {
			cur.WriteByte(c)
			if c == ']' {
				depth--
			}
			continue
		}

		switch c {
		case '[':
			depth++
			cur.WriteByte(c)

		case ' ', '\t', '\n', '\r':
			if err := flush(); err != nil {
				return nil, err
			}

		case '>':
			if err := flush(); err != nil {
				return nil, err
			}
			if len(chain) == 0 {
				return nil, fmt.Errorf("a selector may not start with >")
			}
			nextChild = true

		case '+', '~':
			return nil, fmt.Errorf("the %q sibling combinator is not supported in this build; "+
				"see docs/compatibility.md", string(c))

		default:
			cur.WriteByte(c)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if depth > 0 {
		return nil, fmt.Errorf("unclosed [ in selector %q", s)
	}
	if nextChild {
		return nil, fmt.Errorf("a selector may not end with >")
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("a selector branch is empty")
	}
	return chain, nil
}

func parseCompound(s string) (compoundSelector, error) {
	var c compoundSelector

	for i := 0; i < len(s); {
		switch s[i] {
		case '*':
			i++

		case '#':
			name, n, err := readName(s[i+1:])
			if err != nil {
				return c, fmt.Errorf("id selector in %q: %w", s, err)
			}
			c.id = name
			i += 1 + n

		case '.':
			name, n, err := readName(s[i+1:])
			if err != nil {
				return c, fmt.Errorf("class selector in %q: %w", s, err)
			}
			c.classes = append(c.classes, name)
			i += 1 + n

		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return c, fmt.Errorf("unclosed [ in %q", s)
			}
			attr, err := parseAttrSelector(s[i+1 : i+end])
			if err != nil {
				return c, err
			}
			c.attrs = append(c.attrs, attr)
			i += end + 1

		case ':':
			// Refused rather than ignored: a selector whose pseudo-class is
			// dropped matches more elements than it says, silently.
			return c, fmt.Errorf("pseudo-classes and pseudo-elements are not supported "+
				"in this build: %q; see docs/compatibility.md", s)

		case '|':
			return c, fmt.Errorf("namespace selectors are not supported in this build: %q", s)

		default:
			if c.tag != "" || i != 0 {
				return c, fmt.Errorf("unexpected %q in selector %q", string(s[i]), s)
			}
			name, n, err := readName(s)
			if err != nil {
				return c, fmt.Errorf("type selector in %q: %w", s, err)
			}
			// HTML tag names are matched case-insensitively, and x/net/html
			// lower-cases them during parsing.
			c.tag = strings.ToLower(name)
			i += n
		}
	}
	return c, nil
}

func parseAttrSelector(s string) (attrSelector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return attrSelector{}, fmt.Errorf("an attribute selector is empty")
	}

	// The unsupported operators are checked first, because both of them end in
	// "=" and would otherwise be read as a plain equality against a name with a
	// stray character on the end — matching nothing, forever, quietly.
	for _, op := range []string{"~=", "|="} {
		if strings.Contains(s, op) {
			return attrSelector{}, fmt.Errorf("the %q attribute operator is not supported "+
				"in this build; see docs/compatibility.md", op)
		}
	}
	for _, op := range []string{"^=", "$=", "*=", "="} {
		if i := strings.Index(s, op); i > 0 {
			name := strings.TrimSpace(s[:i])
			value := strings.TrimSpace(s[i+len(op):])
			value = strings.Trim(value, `"'`)
			if name == "" {
				return attrSelector{}, fmt.Errorf("attribute selector %q has no name", s)
			}
			return attrSelector{name: strings.ToLower(name), op: op, value: value}, nil
		}
	}
	return attrSelector{name: strings.ToLower(s)}, nil
}

// readName reads a CSS identifier.
func readName(s string) (string, int, error) {
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '-' || c == '_' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c >= 0x80 {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return "", 0, fmt.Errorf("expected a name at %q", s)
	}
	return s[:i], i, nil
}

// ---------------------------------------------------------------------------
// matching
// ---------------------------------------------------------------------------

// Select returns every element matching the group, in document order.
func (g selectorGroup) Select(root *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && g.matches(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

func (g selectorGroup) matches(n *html.Node) bool {
	for _, chain := range g {
		if chain.matches(n) {
			return true
		}
	}
	return false
}

// matches walks the chain backwards from the element, which is how a selector
// engine avoids exploring the whole tree for every candidate.
func (c selectorChain) matches(n *html.Node) bool {
	last := len(c) - 1
	if !c[last].compound.matches(n) {
		return false
	}
	return matchAncestors(c[:last], n, c[last].child)
}

// matchAncestors checks the remaining steps against n's ancestors. childOnly is
// the combinator joining the already-matched step to the one before it.
func matchAncestors(steps selectorChain, n *html.Node, childOnly bool) bool {
	if len(steps) == 0 {
		return true
	}
	step := steps[len(steps)-1]

	if childOnly {
		parent := elementParent(n)
		if parent == nil || !step.compound.matches(parent) {
			return false
		}
		return matchAncestors(steps[:len(steps)-1], parent, step.child)
	}

	// Descendant: try every ancestor. Backtracking is needed because an earlier
	// ancestor may satisfy the rest of the chain where a later one does not.
	for a := elementParent(n); a != nil; a = elementParent(a) {
		if step.compound.matches(a) && matchAncestors(steps[:len(steps)-1], a, step.child) {
			return true
		}
	}
	return false
}

func elementParent(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode {
			return p
		}
	}
	return nil
}

func (c compoundSelector) matches(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if c.tag != "" && n.Data != c.tag {
		return false
	}
	if c.id != "" && attrValue(n, "id") != c.id {
		return false
	}
	for _, want := range c.classes {
		if !hasClass(n, want) {
			return false
		}
	}
	for _, a := range c.attrs {
		got, present := lookupAttr(n, a.name)
		if !present {
			return false
		}
		switch a.op {
		case "":
		case "=":
			if got != a.value {
				return false
			}
		case "^=":
			if !strings.HasPrefix(got, a.value) {
				return false
			}
		case "$=":
			if !strings.HasSuffix(got, a.value) {
				return false
			}
		case "*=":
			if !strings.Contains(got, a.value) {
				return false
			}
		}
	}
	return true
}

func lookupAttr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

func attrValue(n *html.Node, name string) string {
	v, _ := lookupAttr(n, name)
	return v
}

func hasClass(n *html.Node, want string) bool {
	for _, c := range strings.Fields(attrValue(n, "class")) {
		if c == want {
			return true
		}
	}
	return false
}

// nodeText returns the concatenated text of an element and its descendants.
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// nodeInnerHTML renders an element's children back to markup.
//
// It is re-rendered from the parse tree rather than sliced out of the source, so
// what comes out is normalised markup rather than the original bytes: unquoted
// attributes gain quotes, implied tags appear, and self-closing spellings are
// regularised. That is worth saying out loud, because a flow comparing the
// result against a stored string will not match on the first run after a source
// page changes its formatting.
func nodeInnerHTML(n *html.Node) (string, error) {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}
