package nodes

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/embernet-ai/emberwire/internal/node"
)

// The compatibility matrix is generated from the registry rather than written by
// hand, and this test fails when the file on disk no longer matches.
//
// A hand-maintained matrix drifts the moment somebody adds a node in a hurry,
// and a stale compatibility claim is worse than none: it is the document a
// customer reads before deciding whether their flow will work. Generating it
// means the only way to change what it says is to change what the code declares.
//
// Regenerate with:
//
//	EMBERWIRE_UPDATE_DOCS=1 go test ./internal/nodes/ -run TestCompatibilityMatrix
const compatDocPath = "../../docs/compatibility.md"

func TestCompatibilityMatrixIsCurrent(t *testing.T) {
	want := renderCompatibilityDoc()

	if os.Getenv("EMBERWIRE_UPDATE_DOCS") == "1" {
		if err := os.MkdirAll(filepath.Dir(compatDocPath), 0o755); err != nil {
			t.Fatalf("creating docs directory: %v", err)
		}
		if err := os.WriteFile(compatDocPath, []byte(want), 0o644); err != nil {
			t.Fatalf("writing %s: %v", compatDocPath, err)
		}
		t.Logf("regenerated %s", compatDocPath)
		return
	}

	got, err := os.ReadFile(compatDocPath)
	if err != nil {
		t.Fatalf("reading %s: %v\nRegenerate with EMBERWIRE_UPDATE_DOCS=1 go test ./internal/nodes/",
			compatDocPath, err)
	}
	if normaliseNewlines(string(got)) != normaliseNewlines(want) {
		t.Errorf("%s is out of date with the node registry.\n"+
			"Regenerate with: EMBERWIRE_UPDATE_DOCS=1 go test ./internal/nodes/", compatDocPath)
	}
}

func normaliseNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func renderCompatibilityDoc() string {
	var b strings.Builder

	b.WriteString("# Node compatibility\n\n")
	b.WriteString("Generated from the node registry. Do not edit by hand — change the\n")
	b.WriteString("`Compatibility` field on the node's descriptor and regenerate:\n\n")
	b.WriteString("```\nEMBERWIRE_UPDATE_DOCS=1 go test ./internal/nodes/\n```\n\n")

	b.WriteString("A node that is partially compatible and silent about how is worse than one\n")
	b.WriteString("that is obviously absent: the flow appears to work and quietly does the wrong\n")
	b.WriteString("thing. Every entry below has to say what is missing, and a test fails the\n")
	b.WriteString("build if one does not.\n\n")

	b.WriteString("## What the levels mean\n\n")
	b.WriteString("| Level | Meaning |\n|---|---|\n")
	b.WriteString("| **full** | Behaves as the Node-RED node of the same type does. |\n")
	b.WriteString("| **partial** | A subset. The notes say exactly which parts are missing. |\n")
	b.WriteString("| **divergent** | Deliberately behaves differently. The notes say why. |\n")
	b.WriteString("| **emberwire-only** | No Node-RED counterpart. |\n\n")

	b.WriteString("## Not supported at all\n\n")
	b.WriteString("**Node-RED community nodes.** They are npm packages that need Node.js.\n")
	b.WriteString("There is no version of this where they work.\n\n")
	b.WriteString("**JSONata expressions.** Any property typed `jsonata` is refused with an\n")
	b.WriteString("error rather than ignored. Returning the expression text would make a flow\n")
	b.WriteString("appear to work while routing on a literal string.\n\n")

	descs := node.Default.Descriptors()

	// Summary counts first, so the state of the port is visible at a glance
	// without reading the whole table.
	counts := map[string]int{}
	for _, d := range descs {
		counts[d.Compatibility.Level]++
	}
	b.WriteString("## Summary\n\n")
	b.WriteString(fmt.Sprintf("%d node types registered.\n\n", len(descs)))
	b.WriteString("| Level | Count |\n|---|---|\n")
	for _, lvl := range []string{node.CompatFull, node.CompatPartial, node.CompatDivergent, node.CompatOnly} {
		b.WriteString(fmt.Sprintf("| %s | %d |\n", lvl, counts[lvl]))
	}
	b.WriteString("\n")

	byCategory := map[node.Category][]node.Descriptor{}
	for _, d := range descs {
		byCategory[d.Category] = append(byCategory[d.Category], d)
	}
	cats := make([]string, 0, len(byCategory))
	for c := range byCategory {
		cats = append(cats, string(c))
	}
	sort.Strings(cats)

	for _, c := range cats {
		b.WriteString(fmt.Sprintf("## %s\n\n", capitalise(c)))
		b.WriteString("| Type | Level | Notes |\n|---|---|---|\n")
		for _, d := range byCategory[node.Category(c)] {
			notes := d.Compatibility.Notes
			if len(d.Compatibility.UnsupportedProps) > 0 {
				if notes != "" {
					notes += " "
				}
				notes += "Ignored properties: `" +
					strings.Join(d.Compatibility.UnsupportedProps, "`, `") + "`."
			}
			if notes == "" {
				notes = "—"
			}
			b.WriteString(fmt.Sprintf("| `%s` | %s | %s |\n",
				d.Type, d.Compatibility.Level, escapePipes(notes)))
		}
		b.WriteString("\n")
	}

	return b.String()
}

func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

// capitalise upper-cases the first letter of a category name. Category names
// are ASCII by construction, so this needs none of the Unicode machinery that
// strings.Title was deprecated for getting wrong.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
