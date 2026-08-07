package nodes

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerChange()
	registerSwitch()
	registerRange()
	registerFilter()
}

// ---------------------------------------------------------------------------
// change
// ---------------------------------------------------------------------------

// changeRule is one operation in a Change node's rule list.
type changeRule struct {
	Op string // set, change, delete, move

	// Target property and its type (msg, flow, global).
	Prop     string
	PropType string

	// To is the value for "set".
	To TypedValue

	// From/Replace drive "change" (search and replace).
	From    TypedValue
	Replace TypedValue
	re      *regexp.Regexp // compiled when From is a regex
}

type changeNode struct {
	rules []changeRule
	svc   node.Services
}

func registerChange() {
	node.MustRegister(node.Descriptor{
		Type:         "change",
		Category:     node.CategoryFunction,
		Color:        colorFunction,
		Icon:         "swap",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "change",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "set, change, delete and move are supported for msg, flow and global " +
				"targets. JSONata-typed values are not evaluated in this build.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "rules", Kind: node.PropList, Label: "Rules", Fields: []node.Prop{
				{Name: "t", Kind: node.PropSelect, Label: "Action", Default: "set", Options: []node.Option{
					{Value: "set", Label: "Set"},
					{Value: "change", Label: "Change"},
					{Value: "delete", Label: "Delete"},
					{Value: "move", Label: "Move"},
				}},
				{Name: "p", Kind: node.PropTypedInput, Label: "Property", TypeProp: "pt",
					TypedInputTypes: []string{node.TypeMsg, node.TypeFlow, node.TypeGlobal}},
				{Name: "to", Kind: node.PropTypedInput, Label: "To", TypeProp: "tot"},
				{Name: "from", Kind: node.PropTypedInput, Label: "Search for", TypeProp: "fromt"},
			}},
		},
		Help: "Sets, changes, moves or deletes properties of a message, or of flow " +
			"and global context.",
	}, newChange)
}

func newChange(def *node.Definition) (node.Node, error) {
	n := &changeNode{svc: def.Services}

	raw, _ := def.Node.Prop("rules")
	arr, ok := raw.([]any)
	if !ok {
		// A flow written before the rules array existed carries a single
		// implicit set rule on the node itself.
		if p := def.Node.PropString("property", ""); p != "" {
			n.rules = append(n.rules, changeRule{
				Op: "set", Prop: p, PropType: node.TypeMsg,
				To: ReadTypedValue(def.Node.Raw, "to", "toType", node.TypeStr),
			})
		}
		return n, nil
	}

	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		r := changeRule{
			Op:       stringOr(m["t"], "set"),
			Prop:     stringOr(m["p"], ""),
			PropType: stringOr(m["pt"], node.TypeMsg),
			To:       ReadTypedValue(m, "to", "tot", node.TypeStr),
			From:     ReadTypedValue(m, "from", "fromt", node.TypeStr),
		}
		// Node-RED reuses the "to" field as the replacement text for a change
		// rule, which is why there is no separate key here.
		r.Replace = r.To

		if r.Prop == "" {
			return nil, fmt.Errorf("rule %d has no target property", i+1)
		}
		if r.Op == "change" && r.From.Type == node.TypeRe {
			re, err := regexp.Compile(r.From.Value)
			if err != nil {
				return nil, fmt.Errorf("rule %d: invalid regular expression %q: %w", i+1, r.From.Value, err)
			}
			r.re = re
		}
		n.rules = append(n.rules, r)
	}
	return n, nil
}

func (n *changeNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc}

	for i, r := range n.rules {
		if err := n.apply(ec, r); err != nil {
			return fmt.Errorf("rule %d (%s %s): %w", i+1, r.Op, r.Prop, err)
		}
	}
	out.Send(0, m)
	return nil
}

func (n *changeNode) apply(ec EvalContext, r changeRule) error {
	switch r.Op {
	case "set":
		v, ok, err := r.To.Eval(ec)
		if err != nil {
			return err
		}
		if !ok {
			// Node-RED deletes the target when the source does not resolve,
			// rather than writing undefined.
			return DeleteTypedTarget(ec, r.PropType, r.Prop)
		}
		return SetTypedTarget(ec, r.PropType, r.Prop, v)

	case "delete":
		return DeleteTypedTarget(ec, r.PropType, r.Prop)

	case "move":
		// "to" holds the destination for a move.
		dest := r.To.Value
		destType := r.To.Type
		if destType == "" {
			destType = node.TypeMsg
		}
		if dest == "" {
			return fmt.Errorf("move has no destination")
		}
		v, ok, err := readTypedTarget(ec, r.PropType, r.Prop)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := SetTypedTarget(ec, destType, dest, v); err != nil {
			return err
		}
		return DeleteTypedTarget(ec, r.PropType, r.Prop)

	case "change":
		cur, ok, err := readTypedTarget(ec, r.PropType, r.Prop)
		if err != nil || !ok {
			return err
		}
		replaced, changed, err := n.substitute(ec, r, cur)
		if err != nil || !changed {
			return err
		}
		return SetTypedTarget(ec, r.PropType, r.Prop, replaced)

	default:
		return fmt.Errorf("unknown action %q", r.Op)
	}
}

// substitute implements the "change" action: replace occurrences of From with
// Replace inside the current value.
func (n *changeNode) substitute(ec EvalContext, r changeRule, cur any) (any, bool, error) {
	repl, replOK, err := r.Replace.Eval(ec)
	if err != nil {
		return nil, false, err
	}
	if !replOK {
		repl = ""
	}

	if r.re != nil {
		s, ok := cur.(string)
		if !ok {
			// A regex replace against a non-string is a no-op, as it is in
			// Node-RED, rather than a stringify-and-replace surprise.
			return cur, false, nil
		}
		return r.re.ReplaceAllString(s, fmt.Sprint(repl)), true, nil
	}

	from, fromOK, err := r.From.Eval(ec)
	if err != nil {
		return nil, false, err
	}
	if !fromOK {
		return cur, false, nil
	}

	// A whole-value match replaces with the typed replacement, preserving its
	// type. A substring match only makes sense on strings.
	if deepEqual(cur, from) {
		return repl, true, nil
	}
	s, ok := cur.(string)
	if !ok {
		return cur, false, nil
	}
	fromStr, ok := from.(string)
	if !ok {
		return cur, false, nil
	}
	if fromStr == "" || !strings.Contains(s, fromStr) {
		return cur, false, nil
	}
	return strings.ReplaceAll(s, fromStr, fmt.Sprint(repl)), true, nil
}

func readTypedTarget(ec EvalContext, typ, prop string) (any, bool, error) {
	return TypedValue{Type: orDefault(typ, node.TypeMsg), Value: prop}.Eval(ec)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func stringOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// ---------------------------------------------------------------------------
// switch
// ---------------------------------------------------------------------------

// switchRule is one test in a Switch node.
type switchRule struct {
	Op  string
	V   TypedValue
	V2  TypedValue // second operand, for "btwn"
	re  *regexp.Regexp
	Cas bool // case-sensitive, for regex
}

type switchNode struct {
	prop     TypedValue
	rules    []switchRule
	checkAll bool // evaluate every rule, not just up to the first match
	repair   bool // rebuild msg.parts on the way out
	svc      node.Services
}

func registerSwitch() {
	node.MustRegister(node.Descriptor{
		Type:         "switch",
		Category:     node.CategoryFunction,
		Color:        colorFunction,
		Icon:         "switch",
		Inputs:       1,
		Outputs:      1,
		OutputsProp:  "outputs",
		PaletteLabel: "switch",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "All comparison operators are supported except jsonata_exp, which " +
				"needs an expression engine this build does not ship.",
			UnsupportedProps: []string{"jsonata_exp"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "property", Kind: node.PropTypedInput, Label: "Property", TypeProp: "propertyType",
				Default: "payload"},
			{Name: "outputs", Kind: node.PropNumber, Label: "Outputs", Default: 1},
			{Name: "checkall", Kind: node.PropSelect, Label: "Matching", Default: "true",
				Options: []node.Option{
					{Value: "true", Label: "Check all rules"},
					{Value: "false", Label: "Stop after first match"},
				}},
			{Name: "rules", Kind: node.PropList, Label: "Rules", Fields: []node.Prop{
				{Name: "t", Kind: node.PropSelect, Label: "Test", Default: "eq", Options: switchOps()},
				{Name: "v", Kind: node.PropTypedInput, Label: "Value", TypeProp: "vt"},
				{Name: "v2", Kind: node.PropTypedInput, Label: "and", TypeProp: "v2t"},
			}},
		},
		Help: "Routes a message to one or more outputs by testing a property. Each " +
			"rule corresponds to one output, in order.",
	}, newSwitch)
}

func switchOps() []node.Option {
	return []node.Option{
		{Value: "eq", Label: "=="},
		{Value: "neq", Label: "!="},
		{Value: "lt", Label: "<"},
		{Value: "lte", Label: "<="},
		{Value: "gt", Label: ">"},
		{Value: "gte", Label: ">="},
		{Value: "btwn", Label: "is between"},
		{Value: "cont", Label: "contains"},
		{Value: "regex", Label: "matches regex"},
		{Value: "true", Label: "is true"},
		{Value: "false", Label: "is false"},
		{Value: "null", Label: "is null"},
		{Value: "nnull", Label: "is not null"},
		{Value: "istype", Label: "is of type"},
		{Value: "empty", Label: "is empty"},
		{Value: "nempty", Label: "is not empty"},
		{Value: "hask", Label: "has key"},
		{Value: "else", Label: "otherwise"},
	}
}

func newSwitch(def *node.Definition) (node.Node, error) {
	n := &switchNode{
		svc:      def.Services,
		prop:     ReadTypedValue(def.Node.Raw, "property", "propertyType", node.TypeMsg),
		checkAll: def.Node.PropString("checkall", "true") != "false",
		repair:   def.Node.PropBool("repair", false),
	}
	if n.prop.Value == "" {
		n.prop.Value = engine.PropPayload
	}

	raw, _ := def.Node.Prop("rules")
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("switch node has no rules")
	}
	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		r := switchRule{
			Op:  stringOr(m["t"], "eq"),
			V:   ReadTypedValue(m, "v", "vt", node.TypeStr),
			V2:  ReadTypedValue(m, "v2", "v2t", node.TypeStr),
			Cas: boolOr(m["case"], false),
		}
		if r.Op == "regex" {
			expr := r.V.Value
			if !r.Cas {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("rule %d: invalid regular expression %q: %w", i+1, r.V.Value, err)
			}
			r.re = re
		}
		if r.Op == "jsonata_exp" {
			return nil, fmt.Errorf("rule %d uses a JSONata expression, which is not supported in this build", i+1)
		}
		n.rules = append(n.rules, r)
	}
	if len(n.rules) == 0 {
		return nil, fmt.Errorf("switch node has no rules")
	}
	return n, nil
}

func boolOr(v any, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(t)
		if err == nil {
			return b
		}
	}
	return def
}

func (n *switchNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc}

	value, present, err := n.prop.Eval(ec)
	if err != nil {
		return fmt.Errorf("reading %s: %w", n.prop.Value, err)
	}

	byPort := make([][]*engine.Msg, len(n.rules))
	matched := false

	for i, r := range n.rules {
		if r.Op == "else" {
			// "otherwise" fires only when nothing before it did.
			if !matched {
				byPort[i] = []*engine.Msg{m}
				matched = true
			}
			continue
		}

		ok, err := n.test(ec, r, value, present)
		if err != nil {
			return fmt.Errorf("rule %d (%s): %w", i+1, r.Op, err)
		}
		if !ok {
			continue
		}
		byPort[i] = []*engine.Msg{m}
		matched = true
		if !n.checkAll {
			break
		}
	}

	out.SendAll(byPort)
	return nil
}

func (n *switchNode) test(ec EvalContext, r switchRule, value any, present bool) (bool, error) {
	switch r.Op {
	case "null":
		return !present || value == nil, nil
	case "nnull":
		return present && value != nil, nil
	case "true":
		b, ok := value.(bool)
		return ok && b, nil
	case "false":
		b, ok := value.(bool)
		return ok && !b, nil
	case "empty":
		return isEmpty(value, present), nil
	case "nempty":
		return !isEmpty(value, present), nil
	case "istype":
		return typeNameOf(value, present) == r.V.Value, nil
	case "regex":
		s, ok := value.(string)
		if !ok {
			return false, nil
		}
		return r.re.MatchString(s), nil
	}

	operand, opOK, err := r.V.Eval(ec)
	if err != nil {
		return false, err
	}

	switch r.Op {
	case "hask":
		key, ok := operand.(string)
		if !ok || !present {
			return false, nil
		}
		obj, ok := value.(map[string]any)
		if !ok {
			return false, nil
		}
		_, has := obj[key]
		return has, nil

	case "eq":
		return deepEqual(value, operand), nil
	case "neq":
		return !deepEqual(value, operand), nil

	case "lt", "lte", "gt", "gte":
		if !opOK || !present {
			return false, nil
		}
		cmp, ok := compare(value, operand)
		if !ok {
			return false, nil
		}
		switch r.Op {
		case "lt":
			return cmp < 0, nil
		case "lte":
			return cmp <= 0, nil
		case "gt":
			return cmp > 0, nil
		default:
			return cmp >= 0, nil
		}

	case "btwn":
		lo, loOK, err := r.V.Eval(ec)
		if err != nil {
			return false, err
		}
		hi, hiOK, err := r.V2.Eval(ec)
		if err != nil {
			return false, err
		}
		if !loOK || !hiOK || !present {
			return false, nil
		}
		cl, ok1 := compare(value, lo)
		ch, ok2 := compare(value, hi)
		if !ok1 || !ok2 {
			return false, nil
		}
		// Node-RED accepts the bounds in either order, so a rule written
		// "between 10 and 1" still behaves sensibly.
		if cmpLoHi, ok := compare(lo, hi); ok && cmpLoHi > 0 {
			return ch >= 0 && cl <= 0, nil
		}
		return cl >= 0 && ch <= 0, nil

	case "cont":
		s, ok := value.(string)
		if !ok {
			return false, nil
		}
		sub, ok := operand.(string)
		if !ok {
			sub = fmt.Sprint(operand)
		}
		return strings.Contains(s, sub), nil

	default:
		return false, fmt.Errorf("unknown operator %q", r.Op)
	}
}

func isEmpty(v any, present bool) bool {
	if !present || v == nil {
		return true
	}
	switch t := v.(type) {
	case string:
		return len(t) == 0
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case []byte:
		return len(t) == 0
	case engine.ImmutableBytes:
		return len(t) == 0
	default:
		// Numbers and booleans are never "empty" in Node-RED's sense.
		return false
	}
}

// typeNameOf reports the JavaScript-flavoured type name a flow author expects,
// since "is of type" is compared against strings the editor offers.
func typeNameOf(v any, present bool) string {
	if !present {
		return "undefined"
	}
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return "number"
	case []any:
		return "array"
	case []byte, engine.ImmutableBytes:
		return "buffer"
	case map[string]any:
		return "object"
	default:
		return "object"
	}
}

// compare orders two values, reporting false when they are not comparable.
// Numbers compare numerically and strings lexically; a number against a numeric
// string coerces, because edit dialogs persist numbers as strings.
func compare(a, b any) (int, bool) {
	af, aOK := asFloat(a)
	bf, bOK := asFloat(b)
	if aOK && bOK {
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		default:
			return 0, true
		}
	}
	as, aStr := a.(string)
	bs, bStr := b.(string)
	if aStr && bStr {
		return strings.Compare(as, bs), true
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case int32:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// deepEqual compares two values the way a flow author expects: numbers by
// value across representations, everything else structurally.
func deepEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	// Only coerce when at least one side is genuinely numeric. Comparing two
	// numeric strings must stay a string comparison, so that "01" != "1".
	_, aIsStr := a.(string)
	_, bIsStr := b.(string)
	if !aIsStr || !bIsStr {
		if af, ok := asFloat(a); ok {
			if bf, ok := asFloat(b); ok {
				return af == bf
			}
		}
	}
	return reflect.DeepEqual(a, b)
}

// ---------------------------------------------------------------------------
// range
// ---------------------------------------------------------------------------

type rangeNode struct {
	prop                  string
	minIn, maxIn          float64
	minOut, maxOut        float64
	action                string // scale, clamp, roll
	round                 bool
	svc                   node.Services
	inputSpan, outputSpan float64
}

func registerRange() {
	node.MustRegister(node.Descriptor{
		Type:          "range",
		Category:      node.CategoryFunction,
		Color:         colorFunction,
		Icon:          "range",
		Inputs:        1,
		Outputs:       1,
		PaletteLabel:  "range",
		LabelProp:     "name",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "property", Kind: node.PropString, Label: "Property", Default: "payload"},
			{Name: "minin", Kind: node.PropNumber, Label: "Input from", Default: 0},
			{Name: "maxin", Kind: node.PropNumber, Label: "Input to", Default: 99},
			{Name: "minout", Kind: node.PropNumber, Label: "Output from", Default: 0},
			{Name: "maxout", Kind: node.PropNumber, Label: "Output to", Default: 255},
			{Name: "action", Kind: node.PropSelect, Label: "Action", Default: "scale",
				Options: []node.Option{
					{Value: "scale", Label: "Scale the message"},
					{Value: "clamp", Label: "Scale and limit to the target range"},
					{Value: "roll", Label: "Scale and wrap within the target range"},
				}},
			{Name: "round", Kind: node.PropBool, Label: "Round to the nearest integer"},
		},
		Help: "Maps a numeric property from one range onto another.",
	}, newRange)
}

func newRange(def *node.Definition) (node.Node, error) {
	n := &rangeNode{
		prop:   orDefault(def.Node.PropString("property", ""), engine.PropPayload),
		minIn:  def.Node.PropFloat("minin", 0),
		maxIn:  def.Node.PropFloat("maxin", 99),
		minOut: def.Node.PropFloat("minout", 0),
		maxOut: def.Node.PropFloat("maxout", 255),
		action: def.Node.PropString("action", "scale"),
		round:  def.Node.PropBool("round", false),
		svc:    def.Services,
	}
	n.inputSpan = n.maxIn - n.minIn
	n.outputSpan = n.maxOut - n.minOut
	if n.inputSpan == 0 {
		// Dividing by this later would yield ±Inf and quietly poison every
		// downstream calculation.
		return nil, fmt.Errorf("input range is empty: from and to are both %v", n.minIn)
	}
	return n, nil
}

func (n *rangeNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	raw, ok, err := m.Get(n.prop)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("message has no property %q", n.prop)
	}
	in, ok := asFloat(raw)
	if !ok {
		return fmt.Errorf("%s is %T, which is not a number", n.prop, raw)
	}

	// Multiply before dividing. The obvious spelling — (in-minIn)/inputSpan
	// scaled by outputSpan — computes a fraction first, and a value like 1.1
	// is not representable, so mapping 11 from 0..10 onto 0..100 lands on
	// 110.00000000000001 and a subsequent wrap yields 10.000000000000014.
	// Nobody wants that on a dashboard. This ordering is exact whenever the
	// inputs are.
	scaled := (in-n.minIn)*n.outputSpan/n.inputSpan + n.minOut

	switch n.action {
	case "clamp":
		lo, hi := n.minOut, n.maxOut
		if lo > hi {
			lo, hi = hi, lo
		}
		scaled = math.Min(math.Max(scaled, lo), hi)
	case "roll":
		span := n.outputSpan
		scaled = math.Mod(scaled-n.minOut, span)
		if scaled < 0 {
			scaled += span
		}
		scaled += n.minOut
	}

	if n.round {
		scaled = math.Round(scaled)
	}
	if err := m.Set(n.prop, scaled); err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

// ---------------------------------------------------------------------------
// filter (rbe — report by exception)
// ---------------------------------------------------------------------------

// filterNode passes a message on only when the watched property has changed.
//
// Node-RED registers this type as "rbe" and labels it "filter" in the palette.
// Keeping the type name is what lets an imported flow keep working.
type filterNode struct {
	mode     string // rbe, rbei, deadband, deadbandEq, narrowband, narrowbandEq
	prop     string
	sep      string // topic property, for per-topic state
	gap      float64
	gapPct   bool
	inout    string
	previous map[string]any
	started  map[string]bool
	svc      node.Services
}

func registerFilter() {
	node.MustRegister(node.Descriptor{
		Type:         "rbe",
		Category:     node.CategoryFunction,
		Color:        colorFunction,
		Icon:         "filter",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "filter",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Block-unless-changed and deadband modes are supported. " +
				"Narrowband modes are not implemented in this build.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "func", Kind: node.PropSelect, Label: "Mode", Default: "rbe", Options: []node.Option{
				{Value: "rbe", Label: "Block unless the value changes"},
				{Value: "rbei", Label: "Block unless it changes, ignoring the first value"},
				{Value: "deadband", Label: "Block unless it changes by more than"},
				{Value: "deadbandEq", Label: "Block unless it changes by at least"},
			}},
			{Name: "gap", Kind: node.PropString, Label: "Amount",
				Help: "A number, or a percentage such as 10%."},
			{Name: "property", Kind: node.PropString, Label: "Property", Default: "payload"},
			{Name: "septopics", Kind: node.PropBool, Label: "Track each topic separately", Default: true},
		},
		Help: "Passes a message on only when the watched property has changed, or " +
			"has changed by more than a given amount.",
	}, newFilter)
}

func newFilter(def *node.Definition) (node.Node, error) {
	n := &filterNode{
		mode:     def.Node.PropString("func", "rbe"),
		prop:     orDefault(def.Node.PropString("property", ""), engine.PropPayload),
		previous: map[string]any{},
		started:  map[string]bool{},
		svc:      def.Services,
	}
	switch n.mode {
	case "rbe", "rbei", "deadband", "deadbandEq":
	case "narrowband", "narrowbandEq":
		return nil, fmt.Errorf("filter mode %q is not implemented in this build", n.mode)
	default:
		return nil, fmt.Errorf("unknown filter mode %q", n.mode)
	}

	if def.Node.PropBool("septopics", true) {
		n.sep = engine.PropTopic
	}

	if gap := strings.TrimSpace(def.Node.PropString("gap", "")); gap != "" {
		if strings.HasSuffix(gap, "%") {
			n.gapPct = true
			gap = strings.TrimSuffix(gap, "%")
		}
		f, err := strconv.ParseFloat(gap, 64)
		if err != nil {
			return nil, fmt.Errorf("amount %q is not a number", def.Node.PropString("gap", ""))
		}
		n.gap = f
	}
	return n, nil
}

func (n *filterNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	value, ok, err := m.Get(n.prop)
	if err != nil {
		return err
	}
	if !ok {
		// Nothing to compare. Pass it through rather than swallowing it.
		out.Send(0, m)
		return nil
	}

	key := ""
	if n.sep != "" {
		key = m.Topic()
	}

	prev, hadPrev := n.previous[key]
	first := !n.started[key]
	n.started[key] = true

	switch n.mode {
	case "rbe", "rbei":
		if hadPrev && deepEqual(prev, value) {
			return nil
		}
		n.previous[key] = value
		if first && n.mode == "rbei" {
			// "ignoring the first value" means record it and stay quiet.
			return nil
		}

	case "deadband", "deadbandEq":
		cur, ok := asFloat(value)
		if !ok {
			return fmt.Errorf("%s is %T, which is not a number", n.prop, value)
		}
		if !hadPrev {
			n.previous[key] = cur
			break
		}
		prevF, _ := asFloat(prev)
		delta := math.Abs(cur - prevF)
		threshold := n.gap
		if n.gapPct {
			threshold = math.Abs(prevF) * n.gap / 100
		}
		pass := delta > threshold
		if n.mode == "deadbandEq" {
			pass = delta >= threshold
		}
		if !pass {
			return nil
		}
		n.previous[key] = cur
	}

	out.Send(0, m)
	return nil
}
