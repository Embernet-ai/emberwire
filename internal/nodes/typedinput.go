// Package nodes contains the built-in node types.
package nodes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// TypedValue is a value paired with the type discriminator that says how to
// interpret it.
//
// This is Node-RED's typedInput, and it is the mechanism that makes flows
// composable: a field can hold a literal, a property of the incoming message, a
// context lookup, or an environment variable, and the node does not care which.
// The discriminator is stored in a companion property whose name varies by node
// — "pt" and "tot" on a Change rule, "vt" on a Switch rule — so it is always
// read explicitly rather than guessed.
type TypedValue struct {
	Type  string
	Value string
}

// ReadTypedValue pulls a typed value out of a raw config map, given the key
// holding the value and the key holding its type.
//
// defType is used when the type key is absent, which happens for flows written
// against older Node-RED versions where some fields were untyped strings.
func ReadTypedValue(raw map[string]any, valueKey, typeKey, defType string) TypedValue {
	tv := TypedValue{Type: defType}
	if t, ok := raw[typeKey].(string); ok && t != "" {
		tv.Type = t
	}
	switch v := raw[valueKey].(type) {
	case string:
		tv.Value = v
	case nil:
		tv.Value = ""
	case float64:
		tv.Value = strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		tv.Value = strconv.FormatBool(v)
	default:
		// A structured literal — a json-typed field holding an object. Keep it
		// as its JSON text so Eval can decode it uniformly.
		if b, err := json.Marshal(v); err == nil {
			tv.Value = string(b)
		} else {
			tv.Value = fmt.Sprint(v)
		}
	}
	return tv
}

// EvalContext is what a typed value needs in order to resolve.
type EvalContext struct {
	Msg      *engine.Msg
	Services node.Services
	// Now is the clock, injectable so date-typed values are testable.
	Now func() time.Time
}

// Eval resolves a typed value.
//
// Returning (nil, false, nil) means the reference was valid but resolved to
// nothing — a message property that is absent, a context key that was never
// set. Callers distinguish that from an error, because "not present" is a
// normal control-flow signal in a flow and an error is not.
func (tv TypedValue) Eval(ec EvalContext) (any, bool, error) {
	switch tv.Type {
	case node.TypeMsg:
		if ec.Msg == nil {
			return nil, false, nil
		}
		v, ok, err := ec.Msg.Get(tv.Value)
		return v, ok, err

	case node.TypeFlow, node.TypeGlobal:
		if ec.Services == nil {
			return nil, false, nil
		}
		scope := node.ScopeFlow
		if tv.Type == node.TypeGlobal {
			scope = node.ScopeGlobal
		}
		// Context keys may themselves be property paths — flow.get("a.b") is
		// idiomatic — so the first segment is the stored key and the remainder
		// addresses into the stored value.
		key, rest := splitContextKey(tv.Value)
		root, ok, err := ec.Services.Context(scope).Get(key)
		if err != nil || !ok {
			return nil, false, err
		}
		if rest == "" {
			return root, true, nil
		}
		v, ok, err := engine.GetProperty(root, rest)
		return v, ok, err

	case node.TypeStr:
		return tv.Value, true, nil

	case node.TypeNum:
		f, err := strconv.ParseFloat(strings.TrimSpace(tv.Value), 64)
		if err != nil {
			return nil, false, fmt.Errorf("%q is not a number", tv.Value)
		}
		return f, true, nil

	case node.TypeBool:
		b, err := strconv.ParseBool(strings.TrimSpace(tv.Value))
		if err != nil {
			return nil, false, fmt.Errorf("%q is not a boolean", tv.Value)
		}
		return b, true, nil

	case node.TypeJSON:
		if strings.TrimSpace(tv.Value) == "" {
			return nil, false, nil
		}
		var out any
		if err := json.Unmarshal([]byte(tv.Value), &out); err != nil {
			return nil, false, fmt.Errorf("invalid JSON: %w", err)
		}
		return out, true, nil

	case node.TypeBin:
		// Node-RED stores a Buffer literal as a JSON array of byte values.
		var arr []any
		if err := json.Unmarshal([]byte(tv.Value), &arr); err == nil {
			buf := make([]byte, 0, len(arr))
			for _, e := range arr {
				f, ok := e.(float64)
				if !ok || f < 0 || f > 255 {
					return nil, false, fmt.Errorf("buffer element %v is not a byte", e)
				}
				buf = append(buf, byte(f))
			}
			return buf, true, nil
		}
		// Otherwise treat it as base64, which is how larger literals are held.
		b, err := base64.StdEncoding.DecodeString(tv.Value)
		if err != nil {
			return nil, false, fmt.Errorf("not a byte array or base64: %w", err)
		}
		return b, true, nil

	case node.TypeRe:
		re, err := regexp.Compile(tv.Value)
		if err != nil {
			return nil, false, fmt.Errorf("invalid regular expression: %w", err)
		}
		return re, true, nil

	case node.TypeDate:
		now := time.Now
		if ec.Now != nil {
			now = ec.Now
		}
		// Node-RED's date type yields milliseconds since the epoch.
		return float64(now().UnixMilli()), true, nil

	case node.TypeEnv:
		if ec.Services == nil {
			return nil, false, nil
		}
		v, ok := ec.Services.Env(tv.Value)
		if !ok {
			return nil, false, nil
		}
		return v, true, nil

	case node.TypeJSONata:
		// JSONata is a full expression language and is not implemented in this
		// build. Erroring is deliberate: silently returning the expression text
		// would make a flow appear to work while routing on a literal string.
		return nil, false, fmt.Errorf("JSONata expressions are not supported in this build; see docs/compatibility.md")

	case "":
		// An untyped field in an old flow file is a string literal.
		return tv.Value, true, nil

	default:
		return nil, false, fmt.Errorf("unknown value type %q", tv.Type)
	}
}

// splitContextKey separates a context key from a property path into it.
//
// flow.get("a") stores under "a"; flow.get("a.b") reads property b of what is
// stored under "a". Bracket forms bind the same way, so "a[0]" reads element
// zero of "a" rather than looking for a key literally named "a[0]".
func splitContextKey(expr string) (key, rest string) {
	if i := strings.IndexAny(expr, ".["); i > 0 {
		if expr[i] == '.' {
			return expr[:i], expr[i+1:]
		}
		return expr[:i], expr[i:]
	}
	return expr, ""
}

// SetTypedTarget writes a value to a destination described by a type and a
// property path: a message property, or a flow- or global-context key.
func SetTypedTarget(ec EvalContext, targetType, target string, value any) error {
	switch targetType {
	case node.TypeMsg, "":
		if ec.Msg == nil {
			return fmt.Errorf("no message to write %q to", target)
		}
		return ec.Msg.Set(target, value)

	case node.TypeFlow, node.TypeGlobal:
		if ec.Services == nil {
			return fmt.Errorf("no context available to write %q to", target)
		}
		scope := node.ScopeFlow
		if targetType == node.TypeGlobal {
			scope = node.ScopeGlobal
		}
		store := ec.Services.Context(scope)
		key, rest := splitContextKey(target)
		if rest == "" {
			return store.Set(key, value)
		}
		// Writing into a nested path has to be read-modify-write, so do it
		// inside Update rather than as a separate get and set — otherwise two
		// nodes writing different sub-paths of the same key lose one another's
		// changes.
		_, err := store.Update(key, func(cur any) (any, error) {
			root, ok := cur.(map[string]any)
			if !ok {
				root = map[string]any{}
			}
			if err := engine.SetProperty(root, rest, value); err != nil {
				return nil, err
			}
			return engine.Denormalise(root), nil
		})
		return err

	default:
		return fmt.Errorf("cannot write to a value of type %q", targetType)
	}
}

// DeleteTypedTarget removes a message property or context key.
func DeleteTypedTarget(ec EvalContext, targetType, target string) error {
	switch targetType {
	case node.TypeMsg, "":
		if ec.Msg == nil {
			return nil
		}
		return ec.Msg.Delete(target)
	case node.TypeFlow, node.TypeGlobal:
		if ec.Services == nil {
			return nil
		}
		scope := node.ScopeFlow
		if targetType == node.TypeGlobal {
			scope = node.ScopeGlobal
		}
		key, rest := splitContextKey(target)
		store := ec.Services.Context(scope)
		if rest == "" {
			return store.Delete(key)
		}
		_, err := store.Update(key, func(cur any) (any, error) {
			root, ok := cur.(map[string]any)
			if !ok {
				return cur, nil
			}
			if err := engine.DeleteProperty(root, rest); err != nil {
				return nil, err
			}
			return engine.Denormalise(root), nil
		})
		return err
	default:
		return fmt.Errorf("cannot delete a value of type %q", targetType)
	}
}
