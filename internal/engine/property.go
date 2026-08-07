// Package engine implements the Emberwire flow runtime: messages, the flow graph,
// the scheduler and message dispatch.
//
// Property expressions are Node-RED compatible. The reference implementation is
// RED.util.normalisePropertyExpression in @node-red/util/lib/util.js; the grammar
// accepted here is deliberately the same one, because every flow written against
// Node-RED encodes property paths in it and we import those flows verbatim.
package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Token is one step of a property path.
//
// Exactly one of the three forms is active:
//
//	Key    != ""     — an object key, e.g. the "b" in a.b
//	IsIndex          — an array index, e.g. the 0 in a[0]
//	Nested != nil    — a sub-expression, e.g. the msg.i in a[msg.i]
//
// Nested is what makes a[msg.i] work. Node-RED resolves the inner expression
// against the message at evaluation time, so the same path can address a
// different element on every message. That means a Path is not fully static and
// cannot be pre-flattened to a string.
type Token struct {
	Key     string
	Index   int
	IsIndex bool
	Nested  Path
}

// String renders a token back into expression syntax. Round-tripping a parsed
// path through String and ParsePath yields an equivalent path, which is what the
// editor relies on when it echoes a property back into an edit dialog.
func (t Token) String() string {
	switch {
	case t.Nested != nil:
		return "[" + t.Nested.String() + "]"
	case t.IsIndex:
		return "[" + strconv.Itoa(t.Index) + "]"
	case isBareIdentifier(t.Key):
		return t.Key
	default:
		return "[" + strconv.Quote(t.Key) + "]"
	}
}

// Path is a parsed property expression.
type Path []Token

// String renders the whole path. Leading dots are elided so a.b[0] does not come
// back as a.b.[0].
func (p Path) String() string {
	var b strings.Builder
	for i, t := range p {
		s := t.String()
		if i > 0 && !strings.HasPrefix(s, "[") {
			b.WriteByte('.')
		}
		b.WriteString(s)
	}
	return b.String()
}

// Static reports whether the path contains no nested sub-expressions. A static
// path resolves identically for every message, so callers that cache lookups can
// only do so when this is true.
func (p Path) Static() bool {
	for _, t := range p {
		if t.Nested != nil {
			return false
		}
	}
	return true
}

// ErrInvalidPath is the sentinel wrapping every parse failure, so callers can
// distinguish a malformed expression from a missing property.
var ErrInvalidPath = errors.New("invalid property expression")

func pathErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidPath, fmt.Sprintf(format, args...))
}

// isIdentStart matches the characters Node-RED permits to open an identifier
// after a dot: letters, digits, $ and _.
func isIdentStart(c byte) bool {
	return c == '$' || c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isBareIdentifier reports whether a key can be written without bracket-quoting.
func isBareIdentifier(s string) bool {
	if s == "" || isAllDigits(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentStart(s[i]) {
			return false
		}
	}
	return true
}

// ParsePath parses a Node-RED property expression.
//
// Accepted forms, all of which appear in real flows:
//
//	payload                 a bare key
//	payload.value           dotted keys
//	payload[0]              numeric index
//	payload["a key"]        quoted key, single or double quotes
//	payload[msg.index]      nested expression, resolved per message
//	a.b[2]["c d"].e         any combination of the above
//
// A bare numeric first segment (e.g. "0.foo") is rejected: Node-RED treats a
// leading digit-run as a key only inside brackets, and silently accepting it here
// would change how an imported flow behaves.
func ParsePath(expr string) (Path, error) {
	if expr == "" {
		return nil, pathErr("zero-length")
	}

	var parts Path
	var (
		start     int
		inString  bool
		inBox     bool
		quoteChar byte
	)

	// flushBare emits the run of characters [start,i) as a key or index token.
	// Inside brackets a digit-run is an index; outside, it is always a key.
	flushBare := func(i int) error {
		if start == i {
			return nil
		}
		v := expr[start:i]
		if inBox {
			if !isAllDigits(v) {
				return pathErr("unquoted key %q at position %d", v, start)
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return pathErr("index %q at position %d: %v", v, start, err)
			}
			parts = append(parts, Token{Index: n, IsIndex: true})
			return nil
		}
		parts = append(parts, Token{Key: v})
		return nil
	}

	for i := 0; i < len(expr); i++ {
		c := expr[i]

		if inString {
			if c == quoteChar {
				if i == start {
					return nil, pathErr("empty quoted key at position %d", start)
				}
				parts = append(parts, Token{Key: expr[start:i]})
				inString = false
				// A quoted key is only legal inside brackets, and the bracket
				// must close immediately after it.
				if inBox && (i+1 >= len(expr) || expr[i+1] != ']') {
					return nil, pathErr("expected ] after quoted key at position %d", i+1)
				}
				start = i + 1
			}
			continue
		}

		switch c {
		case '\'', '"':
			if i != start {
				return nil, pathErr("unexpected quote at position %d", i)
			}
			if !inBox {
				return nil, pathErr("quoted key outside brackets at position %d", i)
			}
			inString = true
			quoteChar = c
			start = i + 1

		case '.':
			if i == 0 {
				return nil, pathErr("leading .")
			}
			if inBox {
				return nil, pathErr("unexpected . inside brackets at position %d", i)
			}
			if err := flushBare(i); err != nil {
				return nil, err
			}
			if i == len(expr)-1 {
				return nil, pathErr("trailing .")
			}
			if !isIdentStart(expr[i+1]) {
				return nil, pathErr("invalid character %q after . at position %d", expr[i+1], i+1)
			}
			start = i + 1

		case '[':
			if i == 0 {
				return nil, pathErr("leading [")
			}
			if inBox {
				return nil, pathErr("nested [ at position %d", i)
			}
			if err := flushBare(i); err != nil {
				return nil, err
			}
			if i == len(expr)-1 {
				return nil, pathErr("unclosed [")
			}
			next := expr[i+1]
			switch {
			case next == '"' || next == '\'' || (next >= '0' && next <= '9'):
				// literal key or index
			case isIdentStart(next):
				// A sub-expression such as [msg.index]. Find its matching ] and
				// recurse; sub-expressions do not nest further, which matches
				// Node-RED — it only ever resolves one level here.
				end := strings.IndexByte(expr[i+1:], ']')
				if end < 0 {
					return nil, pathErr("unclosed [ at position %d", i)
				}
				end += i + 1
				sub, err := ParsePath(expr[i+1 : end])
				if err != nil {
					return nil, fmt.Errorf("sub-expression at position %d: %w", i+1, err)
				}
				// The leading "msg." is kept here rather than stripped, so that
				// String round-trips the expression the user typed. It is
				// stripped at resolve time instead — see resolveToken.
				parts = append(parts, Token{Nested: sub})
				i = end
				start = i + 1
				continue
			default:
				return nil, pathErr("invalid character %q after [ at position %d", next, i+1)
			}
			start = i + 1
			inBox = true

		case ']':
			if !inBox {
				return nil, pathErr("unmatched ] at position %d", i)
			}
			if err := flushBare(i); err != nil {
				return nil, err
			}
			// A closing bracket may only be followed by another accessor. This
			// rejects run-ons such as a[0]x, which Node-RED also refuses.
			if i+1 < len(expr) && expr[i+1] != '.' && expr[i+1] != '[' {
				return nil, pathErr("unexpected %q after ] at position %d", expr[i+1], i+1)
			}
			start = i + 1
			inBox = false

		case ' ', '\t', '\n', '\r':
			return nil, pathErr("unexpected whitespace at position %d", i)
		}
	}

	if inBox {
		return nil, pathErr("unclosed [")
	}
	if inString {
		return nil, pathErr("unclosed quote")
	}

	if start < len(expr) {
		if err := flushBare(len(expr)); err != nil {
			return nil, err
		}
	}
	if len(parts) == 0 {
		return nil, pathErr("resolved to no path segments")
	}
	// A leading numeric segment is ambiguous and Node-RED does not produce one.
	if parts[0].Key != "" && isAllDigits(parts[0].Key) {
		return nil, pathErr("leading numeric segment %q", parts[0].Key)
	}
	return parts, nil
}

// resolveToken reduces a token to a concrete key or index. Nested tokens are
// evaluated against root, which is the message the path is being applied to.
//
// A nested expression resolving to a number addresses an array element; anything
// else is stringified and used as an object key. That mirrors JavaScript, where
// obj[1] and obj["1"] are the same lookup.
func resolveToken(t Token, root any) (key string, index int, isIndex bool, err error) {
	switch {
	case t.Nested != nil:
		// Node-RED only admits the msg.<path> form inside brackets, and
		// RED.util.getMessageProperty strips that leading "msg." before
		// resolving. Do the same, but only here — the parsed path keeps the
		// prefix so it renders back exactly as the user wrote it.
		sub := t.Nested
		if len(sub) > 1 && sub[0].Key == "msg" {
			sub = sub[1:]
		}
		v, ok := getPath(root, sub, root)
		if !ok {
			return "", 0, false, fmt.Errorf("sub-expression %q did not resolve", t.Nested)
		}
		switch n := v.(type) {
		case int:
			return "", n, true, nil
		case int64:
			return "", int(n), true, nil
		case float64:
			if n == float64(int(n)) && n >= 0 {
				return "", int(n), true, nil
			}
			return strconv.FormatFloat(n, 'f', -1, 64), 0, false, nil
		case string:
			return n, 0, false, nil
		default:
			return fmt.Sprint(v), 0, false, nil
		}
	case t.IsIndex:
		return "", t.Index, true, nil
	default:
		return t.Key, 0, false, nil
	}
}

// getPath walks a path over cur. root is carried separately because nested
// sub-expressions always resolve against the whole message, not the current
// sub-object.
func getPath(cur any, p Path, root any) (any, bool) {
	for _, t := range p {
		key, idx, isIdx, err := resolveToken(t, root)
		if err != nil {
			return nil, false
		}
		next, ok := step(cur, key, idx, isIdx)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// step performs a single lookup against a container.
func step(cur any, key string, idx int, isIdx bool) (any, bool) {
	if isIdx {
		switch c := cur.(type) {
		case []any:
			if idx < 0 || idx >= len(c) {
				return nil, false
			}
			return c[idx], true
		case []byte:
			if idx < 0 || idx >= len(c) {
				return nil, false
			}
			// Indexing a buffer yields the byte value, as it does in Node-RED
			// where msg.payload is a Buffer.
			return float64(c[idx]), true
		case string:
			if idx < 0 || idx >= len(c) {
				return nil, false
			}
			return string(c[idx]), true
		case map[string]any:
			// JavaScript object with numeric-string keys.
			v, ok := c[strconv.Itoa(idx)]
			return v, ok
		default:
			return nil, false
		}
	}

	switch c := cur.(type) {
	case map[string]any:
		v, ok := c[key]
		return v, ok
	case []any:
		// Node-RED exposes .length on arrays; flows use it in switch nodes.
		if key == "length" {
			return float64(len(c)), true
		}
		if isAllDigits(key) {
			n, _ := strconv.Atoi(key)
			if n >= 0 && n < len(c) {
				return c[n], true
			}
		}
		return nil, false
	case []byte:
		if key == "length" {
			return float64(len(c)), true
		}
		return nil, false
	case string:
		if key == "length" {
			return float64(len(c)), true
		}
		return nil, false
	default:
		return nil, false
	}
}

// GetProperty reads expr from root. The boolean reports whether the property
// exists; a property present but set to nil returns (nil, true), which is a
// distinction switch nodes depend on.
func GetProperty(root any, expr string) (any, bool, error) {
	p, err := ParsePath(expr)
	if err != nil {
		return nil, false, err
	}
	v, ok := getPath(root, p, root)
	return v, ok, nil
}

// SetProperty writes value at expr, creating intermediate containers as it goes.
// A missing container is created as a map, unless the next token is an index, in
// which case it is created as a slice — the same auto-vivification Node-RED
// performs, so that msg.payload.a[0].b = 1 works on an empty message.
//
// Setting a slice element beyond the current length grows the slice and pads with
// nil, matching JavaScript's sparse-array assignment.
func SetProperty(root any, expr string, value any) error {
	p, err := ParsePath(expr)
	if err != nil {
		return err
	}
	return setPath(root, p, value, root)
}

// DeleteProperty removes expr from root. Deleting a missing property is not an
// error, matching JavaScript's delete operator.
func DeleteProperty(root any, expr string) error {
	p, err := ParsePath(expr)
	if err != nil {
		return err
	}
	if len(p) == 0 {
		return pathErr("cannot delete empty path")
	}
	parent := root
	if len(p) > 1 {
		v, ok := getPath(root, p[:len(p)-1], root)
		if !ok {
			return nil
		}
		parent = v
	}
	last := p[len(p)-1]
	key, idx, isIdx, err := resolveToken(last, root)
	if err != nil {
		return err
	}
	switch c := parent.(type) {
	case map[string]any:
		if isIdx {
			delete(c, strconv.Itoa(idx))
		} else {
			delete(c, key)
		}
	case *[]any:
		if isIdx && idx >= 0 && idx < len(*c) {
			*c = append((*c)[:idx], (*c)[idx+1:]...)
		}
	}
	return nil
}

func setPath(root any, p Path, value any, msgRoot any) error {
	cur := root
	for i := 0; i < len(p)-1; i++ {
		key, idx, isIdx, err := resolveToken(p[i], msgRoot)
		if err != nil {
			return err
		}
		// Decide what an absent container should be created as, based on how the
		// *next* token addresses it.
		nextIsIndex := false
		if nk, ni, nIdx, nerr := resolveToken(p[i+1], msgRoot); nerr == nil {
			nextIsIndex = nIdx
			_, _ = nk, ni
		}

		next, err := descendOrCreate(cur, key, idx, isIdx, nextIsIndex)
		if err != nil {
			return err
		}
		cur = next
	}

	key, idx, isIdx, err := resolveToken(p[len(p)-1], msgRoot)
	if err != nil {
		return err
	}
	return assign(cur, key, idx, isIdx, value)
}

// descendOrCreate returns the child container at the given key, creating it when
// absent or when the existing value is not a container.
func descendOrCreate(cur any, key string, idx int, isIdx, nextIsIndex bool) (any, error) {
	makeChild := func() any {
		if nextIsIndex {
			return &[]any{}
		}
		return map[string]any{}
	}

	switch c := cur.(type) {
	case map[string]any:
		k := key
		if isIdx {
			k = strconv.Itoa(idx)
		}
		existing, ok := c[k]
		if ok {
			if norm, changed := normaliseContainer(existing); changed {
				c[k] = norm
				return norm, nil
			}
			if isContainer(existing) {
				return existing, nil
			}
		}
		child := makeChild()
		c[k] = child
		return child, nil

	case *[]any:
		if !isIdx {
			return nil, pathErr("cannot address array with key %q", key)
		}
		grow(c, idx)
		existing := (*c)[idx]
		if existing != nil {
			if norm, changed := normaliseContainer(existing); changed {
				(*c)[idx] = norm
				return norm, nil
			}
			if isContainer(existing) {
				return existing, nil
			}
		}
		child := makeChild()
		(*c)[idx] = child
		return child, nil

	default:
		return nil, pathErr("cannot descend into %T", cur)
	}
}

// assign writes a leaf value into a container.
func assign(cur any, key string, idx int, isIdx bool, value any) error {
	switch c := cur.(type) {
	case map[string]any:
		k := key
		if isIdx {
			k = strconv.Itoa(idx)
		}
		c[k] = value
		return nil
	case *[]any:
		if !isIdx {
			return pathErr("cannot address array with key %q", key)
		}
		grow(c, idx)
		(*c)[idx] = value
		return nil
	default:
		return pathErr("cannot assign into %T", cur)
	}
}

// grow extends a slice so that index idx is addressable, padding with nil.
func grow(s *[]any, idx int) {
	for len(*s) <= idx {
		*s = append(*s, nil)
	}
}

func isContainer(v any) bool {
	switch v.(type) {
	case map[string]any, *[]any:
		return true
	}
	return false
}

// normaliseContainer converts a plain []any into the *[]any form used during
// mutation, so that appends performed deeper in the walk are visible to the
// caller. It reports whether a conversion happened.
func normaliseContainer(v any) (any, bool) {
	if s, ok := v.([]any); ok {
		cp := s
		return &cp, true
	}
	return v, false
}

// Denormalise recursively converts *[]any back into []any. Mutation uses pointer
// slices so that growth propagates; everything outside the engine — JSON
// encoding, the JS bridge, node implementations — expects plain slices.
func Denormalise(v any) any {
	switch t := v.(type) {
	case *[]any:
		s := *t
		out := make([]any, len(s))
		for i := range s {
			out[i] = Denormalise(s[i])
		}
		return out
	case []any:
		for i := range t {
			t[i] = Denormalise(t[i])
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = Denormalise(val)
		}
		return t
	default:
		return v
	}
}
