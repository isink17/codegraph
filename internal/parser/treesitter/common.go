//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/texttoken"
)

func intRef(i int) *int { return &i }

// nodeText returns the source text spanned by a tree-sitter node.
func nodeText(node *sitter.Node, content []byte) string {
	if node == nil {
		return ""
	}
	start := node.StartByte()
	end := node.EndByte()
	if int(start) >= len(content) || int(end) > len(content) || start >= end {
		return ""
	}
	return string(content[start:end])
}

// findChildren returns direct children whose Type() matches nodeType.
func findChildren(node *sitter.Node, nodeType string) []*sitter.Node {
	if node == nil {
		return nil
	}
	var out []*sitter.Node
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		if child.Type() == nodeType {
			out = append(out, child)
		}
	}
	return out
}

// findDescendants walks the subtree and collects all nodes whose Type()
// matches nodeType.
func findDescendants(node *sitter.Node, nodeType string) []*sitter.Node {
	if node == nil {
		return nil
	}
	var out []*sitter.Node
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == nodeType {
			out = append(out, n)
		}
		for i := range int(n.ChildCount()) {
			walk(n.Child(i))
		}
	}
	walk(node)
	return out
}

// firstChild returns the first direct child with the given type, or nil.
func firstChild(node *sitter.Node, nodeType string) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		if child.Type() == nodeType {
			return child
		}
	}
	return nil
}

// childByFieldName wraps node.ChildByFieldName for convenience.
func childByFieldName(node *sitter.Node, name string) *sitter.Node {
	if node == nil {
		return nil
	}
	return node.ChildByFieldName(name)
}

// nodeRange converts a tree-sitter node to a graph.Position.
func nodeRange(node *sitter.Node) graph.Position {
	sp := node.StartPoint()
	ep := node.EndPoint()
	return graph.Position{
		StartLine: int(sp.Row) + 1,
		StartCol:  int(sp.Column) + 1,
		EndLine:   int(ep.Row) + 1,
		EndCol:    int(ep.Column) + 1,
	}
}

// computeFileTokens delegates to texttoken.Weights.
func computeFileTokens(content []byte) map[string]float64 {
	return texttoken.Weights(content)
}

// goVisibility determines visibility for Go identifiers.
func goVisibility(name string) string {
	if name == "" {
		return ""
	}
	if strings.ToUpper(name[:1]) == name[:1] {
		return "exported"
	}
	return "package"
}

// pyVisibility determines visibility for Python identifiers.
func pyVisibility(name string) string {
	if name == "" {
		return ""
	}
	if strings.HasPrefix(name, "_") {
		return "module"
	}
	return "public"
}

// heuristicVisibility determines visibility using the heuristic rule:
// uppercase first letter -> public, underscore prefix -> private, else module.
func heuristicVisibility(name string) string {
	if name == "" {
		return ""
	}
	if unicode.IsUpper(rune(name[0])) {
		return "public"
	}
	if strings.HasPrefix(name, "_") {
		return "private"
	}
	return "module"
}

// parse is the common parsing skeleton shared by all tree-sitter adapters.
func parse(ctx context.Context, lang *sitter.Language, content []byte) (*sitter.Node, error) {
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(ctx, nil, content)
	if err != nil {
		return nil, err
	}
	return tree.RootNode(), nil
}

// prevCommentText extracts doc-comment text from comment/block_comment nodes
// immediately preceding the given node (same or previous lines, no gap).
func prevCommentText(node *sitter.Node, content []byte) string {
	if node == nil {
		return ""
	}
	// Walk the sibling chain backwards rather than scanning the parent's
	// children for our own index. Both see the same siblings -- PrevSibling
	// returns anonymous nodes too -- but the scan is O(parent fanout) per
	// symbol, and every Child(i) is a cgo call. P22.15 made that quadratic
	// visible: it let the extractor enter tree-sitter `ERROR` recovery nodes,
	// which are the widest-fanout parents the grammar produces (the single
	// ERROR over googletest's `gtest_unittest.cc` has 914 direct children), so
	// every one of that file's 729 symbols paid a 914-step cgo scan.
	var lines []string
	for prev := node.PrevSibling(); prev != nil; prev = prev.PrevSibling() {
		t := prev.Type()
		if t != "comment" && t != "block_comment" && t != "line_comment" {
			break
		}
		// Check adjacency: the comment must end on the line before the node starts
		// (or the previous comment in the chain).
		text := strings.TrimSpace(nodeText(prev, content))
		text = strings.TrimPrefix(text, "//")
		text = strings.TrimPrefix(text, "#")
		text = strings.TrimPrefix(text, "///")
		text = strings.TrimLeft(text, " ")
		text = strings.TrimPrefix(text, "/*")
		text = strings.TrimSuffix(text, "*/")
		text = strings.TrimSpace(text)
		if text != "" {
			lines = append([]string{text}, lines...)
		}
	}
	return strings.Join(lines, " ")
}

// linkTestsGo creates TestLinks for Go test functions.
func linkTestsGo(module string, pf *graph.ParsedFile) {
	for i, sym := range pf.Symbols {
		if sym.Kind != "function" || !strings.HasPrefix(sym.Name, "Test") {
			continue
		}
		// TestMain is the harness hook (func TestMain(m *testing.M)); it tests
		// nothing, and "Main" is a common exported production name.
		if sym.Name == "TestMain" {
			continue
		}
		target := strings.TrimPrefix(sym.Name, "Test")
		if target == "" {
			continue
		}
		pf.TestLinks = append(pf.TestLinks, graph.TestLink{
			TestName:        sym.QualifiedName,
			TargetName:      module + "." + target,
			Reason:          "test_name_match",
			Score:           0.8,
			TestSymbolKey:   sym.StableKey,
			TargetStableKey: "func:" + module + "::" + target,
			TestSymbolIndex: intRef(i),
		})
	}
}

// linkTestsPython creates TestLinks for Python test functions.
//
// Python symbol stable keys scope to the defining file's module (its
// basename), so a key minted with the *test* module (`func:test_utils::parse`)
// can never match the production symbol (`func:utils::parse`). The target key
// therefore strips the pytest/unittest filename affix from the module, which is
// the same convention IsTestFilePath recognises. A test module with no affix
// keeps its own name (an intra-file guess, the pre-P22.2 behaviour).
func linkTestsPython(module string, pf *graph.ParsedFile) {
	targetModule := strings.TrimPrefix(module, "test_")
	targetModule = strings.TrimSuffix(targetModule, "_test")
	if targetModule == "" {
		targetModule = module
	}
	for i, sym := range pf.Symbols {
		if sym.Kind != "function" || !strings.HasPrefix(sym.Name, "test_") {
			continue
		}
		target := strings.TrimPrefix(sym.Name, "test_")
		if target == "" {
			continue
		}
		pf.TestLinks = append(pf.TestLinks, graph.TestLink{
			TestName:        sym.QualifiedName,
			TargetName:      targetModule + "." + target,
			Reason:          "test_name_match",
			Score:           0.7,
			TestSymbolKey:   sym.StableKey,
			TargetStableKey: "func:" + targetModule + "::" + target,
			TestSymbolIndex: intRef(i),
		})
	}
}

// linkTestsGeneric creates TestLinks for languages that follow common test
// naming patterns. The adapter supplies the production function-key grammar;
// generic code must not invent a key namespace of its own.
func linkTestsGeneric(module string, pf *graph.ParsedFile, targetKey func(string) string) {
	for i, sym := range pf.Symbols {
		if sym.Kind != "function" {
			continue
		}
		var target string
		switch {
		case strings.HasPrefix(sym.Name, "test_"):
			target = strings.TrimPrefix(sym.Name, "test_")
		case strings.HasPrefix(sym.Name, "Test") && testNameUpperBoundary(sym.Name[4:]):
			target = sym.Name[4:]
		case strings.HasPrefix(sym.Name, "test") && testNameLowerBoundary(sym.Name[4:]):
			target = sym.Name[4:]
		default:
			continue
		}
		if target == "" {
			continue
		}
		pf.TestLinks = append(pf.TestLinks, graph.TestLink{
			TestName:        sym.QualifiedName,
			TargetName:      module + "." + target,
			Reason:          "test_name_match",
			Score:           0.7,
			TestSymbolKey:   sym.StableKey,
			TargetStableKey: targetKey(target),
			TestSymbolIndex: intRef(i),
		})
	}
}

func testNameLowerBoundary(suffix string) bool {
	if suffix == "" {
		return false
	}
	if suffix[0] == '_' {
		return len(suffix) > 1
	}
	r, _ := utf8.DecodeRuneInString(suffix)
	return unicode.IsUpper(r)
}

func testNameUpperBoundary(suffix string) bool {
	if suffix == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(suffix)
	return unicode.IsUpper(r)
}

func testTargetModule(module string, suffixes ...string) string {
	for _, suffix := range suffixes {
		if strings.HasSuffix(module, suffix) && len(module) > len(suffix) {
			return strings.TrimSuffix(module, suffix)
		}
	}
	if strings.HasPrefix(module, "test_") && len(module) > len("test_") {
		return strings.TrimPrefix(module, "test_")
	}
	return module
}
