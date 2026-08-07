package nodes

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// A Mustache renderer, because the Template node needs one and Go has no
// standard-library equivalent.
//
// This is deliberately mustache.js's dialect rather than the specification's,
// because the flows we have to run were written against Node-RED, which embeds
// mustache.js. Two places where that matters and where a spec-conformant
// implementation would silently produce different output:
//
//   - The escape set includes "/", "=" and backtick, which the specification
//     does not mention. A template emitting a URL path through {{ }} comes out
//     with &#x2F; in Node-RED, and a flow may well be compensating for that.
//   - Sections standing alone on a line take the whole line with them,
//     including the newline. Without that, every section adds a blank line and
//     a generated YAML or CSV document is malformed rather than merely ugly.
//
// Not implemented, and refused rather than ignored: partials ({{>name}}),
// because there is nowhere to load one from, and custom delimiters, because
// nothing in a flow file can set them.

type mustacheKind int

const (
	mustacheText mustacheKind = iota
	mustacheVar               // {{name}} — escaped
	mustacheRaw               // {{{name}}} or {{&name}} — not escaped
	mustacheSection
	mustacheInverted
)

type mustacheNode struct {
	kind mustacheKind
	// text is the literal for mustacheText, and the tag name otherwise.
	text string
	kids []mustacheNode
}

// parseMustache compiles a template once, at node construction, so a malformed
// template fails the deploy rather than the first message.
func parseMustache(src string) ([]mustacheNode, error) {
	p := &mustacheParser{src: src}
	nodes, err := p.parseUntil("")
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.src) {
		return nil, fmt.Errorf("unexpected {{/%s}}", p.closedWith)
	}
	return nodes, nil
}

type mustacheParser struct {
	src string
	pos int
	// closedWith records the name on the closing tag that ended the last
	// parseUntil, for the error message when it did not match.
	closedWith string
	// lineNonSpace is whether anything other than whitespace has appeared on
	// the source line currently being read. The standalone rule turns on it, and
	// it has to be parser-wide rather than per-section because the scan runs
	// straight through the source while parseUntil recurses.
	lineNonSpace bool
}

// scanLine advances the line state over a run of literal text.
func (p *mustacheParser) scanLine(s string) {
	for i := range len(s) {
		switch s[i] {
		case '\n':
			p.lineNonSpace = false
		case ' ', '\t', '\r':
		default:
			p.lineNonSpace = true
		}
	}
}

// parseUntil reads nodes until it meets {{/section}}, or the end of the input
// when section is empty.
func (p *mustacheParser) parseUntil(section string) ([]mustacheNode, error) {
	var out []mustacheNode

	for p.pos < len(p.src) {
		open := strings.Index(p.src[p.pos:], "{{")
		if open < 0 {
			out = appendText(out, p.src[p.pos:])
			p.pos = len(p.src)
			break
		}
		open += p.pos

		close := strings.Index(p.src[open:], "}}")
		if close < 0 {
			return nil, fmt.Errorf("unclosed {{ at offset %d", open)
		}
		close += open

		raw := false
		inner := p.src[open+2 : close]
		// A triple mustache is {{{name}}}: the two-brace close we found is the
		// first two of three, so the tag actually ends one byte later.
		if strings.HasPrefix(inner, "{") && close+2 < len(p.src) && p.src[close+2] == '}' {
			raw = true
			inner = inner[1:]
			close++
		}

		tag := strings.TrimSpace(inner)
		if tag == "" {
			return nil, fmt.Errorf("empty tag at offset %d", open)
		}

		sigil := byte(0)
		switch tag[0] {
		case '#', '^', '/', '!', '&', '>', '=':
			sigil = tag[0]
			tag = strings.TrimSpace(tag[1:])
		}

		// Standalone handling has to be decided before the literal preceding the
		// tag is committed, because it trims that literal's trailing indent.
		before := p.src[p.pos:open]
		after := p.src[close+2:]
		p.scanLine(before)

		skip := 0
		if sigil == '#' || sigil == '^' || sigil == '/' || sigil == '!' || sigil == '=' || sigil == '>' {
			if n, ok := restOfLineIsBlank(after); ok && !p.lineNonSpace {
				// The tag has the line to itself: drop the indent in front of it
				// and the newline behind it.
				before = before[:strings.LastIndexByte(before, '\n')+1]
				skip = n
			}
		} else {
			// An interpolation is content, so nothing later on this line can be
			// standalone.
			p.lineNonSpace = true
		}
		out = appendText(out, before)
		p.pos = close + 2 + skip

		switch sigil {
		case '!': // comment
			continue

		case '>':
			return nil, fmt.Errorf("partials are not supported: {{>%s}}", tag)

		case '=':
			return nil, fmt.Errorf("changing the delimiters is not supported: {{=%s=}}", tag)

		case '/':
			p.closedWith = tag
			if section == "" {
				return nil, fmt.Errorf("{{/%s}} closes a section that was never opened", tag)
			}
			if tag != section {
				return nil, fmt.Errorf("{{/%s}} closes {{#%s}}", tag, section)
			}
			return out, nil

		case '#', '^':
			if tag == "" {
				return nil, fmt.Errorf("section at offset %d has no name", open)
			}
			kids, err := p.parseUntil(tag)
			if err != nil {
				return nil, err
			}
			kind := mustacheSection
			if sigil == '^' {
				kind = mustacheInverted
			}
			out = append(out, mustacheNode{kind: kind, text: tag, kids: kids})

		default:
			kind := mustacheVar
			if raw || sigil == '&' {
				kind = mustacheRaw
			}
			out = append(out, mustacheNode{kind: kind, text: tag})
		}
	}

	if section != "" {
		return nil, fmt.Errorf("{{#%s}} is never closed", section)
	}
	return out, nil
}

// restOfLineIsBlank reports whether everything between here and the next newline
// is whitespace, and how many bytes to skip to consume it and the newline. The
// end of the template counts as the end of a line.
//
// This is half of the standalone-line rule; the other half is the parser's
// lineNonSpace flag. Without it every section tag leaves a blank line behind,
// and a generated YAML or CSV document comes out malformed rather than untidy.
func restOfLineIsBlank(after string) (int, bool) {
	skip := 0
	for skip < len(after) && (after[skip] == ' ' || after[skip] == '\t' || after[skip] == '\r') {
		skip++
	}
	switch {
	case skip == len(after):
		return skip, true
	case after[skip] == '\n':
		return skip + 1, true
	default:
		return 0, false
	}
}

func appendText(out []mustacheNode, s string) []mustacheNode {
	if s == "" {
		return out
	}
	// Merge with the previous literal so rendering walks fewer nodes.
	if n := len(out); n > 0 && out[n-1].kind == mustacheText {
		out[n-1].text += s
		return out
	}
	return append(out, mustacheNode{kind: mustacheText, text: s})
}

// mustacheScope resolves names against a stack of pushed values, falling back to
// a lookup function for the roots a Template node injects — flow, global and env
// — which are accessors rather than materialised objects.
type mustacheScope struct {
	stack []any
	root  func(path string) (any, bool, error)
}

func (s *mustacheScope) push(v any) { s.stack = append(s.stack, v) }
func (s *mustacheScope) pop()       { s.stack = s.stack[:len(s.stack)-1] }

func (s *mustacheScope) lookup(path string) (any, bool, error) {
	if path == "." {
		if len(s.stack) == 0 {
			return nil, false, nil
		}
		return s.stack[len(s.stack)-1], true, nil
	}

	head, rest, _ := strings.Cut(path, ".")

	// Innermost frame first, as the specification requires: a section over a
	// list of objects shadows the outer message.
	for i := len(s.stack) - 1; i >= 0; i-- {
		frame, ok := s.stack[i].(map[string]any)
		if !ok {
			continue
		}
		v, present := frame[head]
		if !present {
			continue
		}
		if rest == "" {
			return v, true, nil
		}
		return walkPath(v, rest)
	}

	if s.root != nil {
		return s.root(path)
	}
	return nil, false, nil
}

// walkPath follows a dotted path into a decoded JSON value. Mustache has no
// bracket syntax, so numeric segments index into arrays — which is how
// {{payload.0.name}} reaches the first element.
func walkPath(v any, path string) (any, bool, error) {
	for _, seg := range strings.Split(path, ".") {
		switch t := v.(type) {
		case map[string]any:
			nv, ok := t[seg]
			if !ok {
				return nil, false, nil
			}
			v = nv
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false, nil
			}
			v = t[i]
		default:
			return nil, false, nil
		}
	}
	return v, true, nil
}

// renderMustache renders a compiled template.
func renderMustache(nodes []mustacheNode, scope *mustacheScope) (string, error) {
	var b strings.Builder
	if err := renderInto(&b, nodes, scope); err != nil {
		return "", err
	}
	return b.String(), nil
}

func renderInto(b *strings.Builder, nodes []mustacheNode, scope *mustacheScope) error {
	for _, n := range nodes {
		switch n.kind {
		case mustacheText:
			b.WriteString(n.text)

		case mustacheVar, mustacheRaw:
			v, ok, err := scope.lookup(n.text)
			if err != nil {
				return err
			}
			if !ok {
				// Mustache renders a missing name as nothing. That is the one
				// place this engine stays quiet about an absent value, and it
				// is because a template is presentation: a half-filled string
				// is the intended behaviour of an optional field.
				continue
			}
			s := mustacheString(v)
			if n.kind == mustacheVar {
				s = escapeMustache(s)
			}
			b.WriteString(s)

		case mustacheSection:
			v, ok, err := scope.lookup(n.text)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := renderSection(b, n, v, scope); err != nil {
				return err
			}

		case mustacheInverted:
			v, ok, err := scope.lookup(n.text)
			if err != nil {
				return err
			}
			if ok && mustacheTruthy(v) {
				continue
			}
			if err := renderInto(b, n.kids, scope); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderSection(b *strings.Builder, n mustacheNode, v any, scope *mustacheScope) error {
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			scope.push(item)
			err := renderInto(b, n.kids, scope)
			scope.pop()
			if err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		scope.push(t)
		defer scope.pop()
		return renderInto(b, n.kids, scope)
	default:
		if !mustacheTruthy(v) {
			return nil
		}
		// A truthy scalar renders the block once with itself as {{.}}.
		scope.push(v)
		defer scope.pop()
		return renderInto(b, n.kids, scope)
	}
}

// mustacheTruthy is JavaScript truthiness, because that is what the flow author
// was writing against. Notably: 0 and "" are false, and an empty array is false
// even though an empty JavaScript array is truthy — that second one is
// mustache's own rule for sections, not JavaScript's.
func mustacheTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	default:
		if f, ok := asFloat(v); ok {
			// NaN is falsy, and NaN != NaN is how it is detected.
			return f != 0 && f == f
		}
		return true
	}
}

// mustacheString renders a value the way JavaScript's String() would, which is
// what mustache.js interpolates. The number case is the one that matters: Go's
// default float formatting would turn 5 into 5.000000 and a rendered SQL
// statement or MQTT topic would be wrong in a way nobody notices until it is in
// a database.
func mustacheString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case []byte:
		return string(t)
	default:
		if f, ok := asFloatStrict(v); ok {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

// asFloatStrict is asFloat without the string case: a string that happens to
// parse as a number must render as itself, not as a normalised number. "007"
// stays "007".
func asFloatStrict(v any) (float64, bool) {
	if _, isStr := v.(string); isStr {
		return 0, false
	}
	return asFloat(v)
}

// mustacheEscaper matches mustache.js's entityMap exactly. The "/" entry is not
// in the Mustache specification and is the reason a rendered path comes out as
// a&#x2F;b — a flow moved from Node-RED must see the same thing.
var mustacheEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
	"/", "&#x2F;",
	"`", "&#x60;",
	"=", "&#x3D;",
)

func escapeMustache(s string) string { return mustacheEscaper.Replace(s) }
