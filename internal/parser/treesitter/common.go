package treesitter

import (
	"context"
	"strings"
	"unicode"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/texttoken"
)

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
	parent := node.Parent()
	if parent == nil {
		return ""
	}
	// Find our index in parent.
	idx := -1
	for i := range int(parent.ChildCount()) {
		if parent.Child(i) == node {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return ""
	}
	var lines []string
	for i := idx - 1; i >= 0; i-- {
		prev := parent.Child(i)
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
	for _, sym := range pf.Symbols {
		if sym.Kind != "function" || !strings.HasPrefix(sym.Name, "Test") {
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
		})
	}
}

// linkTestsPython creates TestLinks for Python test functions.
func linkTestsPython(module string, pf *graph.ParsedFile) {
	for _, sym := range pf.Symbols {
		if sym.Kind != "function" || !strings.HasPrefix(sym.Name, "test_") {
			continue
		}
		target := strings.TrimPrefix(sym.Name, "test_")
		if target == "" {
			continue
		}
		pf.TestLinks = append(pf.TestLinks, graph.TestLink{
			TestName:        sym.QualifiedName,
			TargetName:      module + "." + target,
			Reason:          "test_name_match",
			Score:           0.7,
			TestSymbolKey:   sym.StableKey,
			TargetStableKey: "func:" + module + "::" + target,
		})
	}
}

// linkTestsGeneric creates TestLinks for languages that follow common test
// naming patterns (test/Test prefix).
func linkTestsGeneric(module string, pf *graph.ParsedFile) {
	for _, sym := range pf.Symbols {
		if sym.Kind != "function" {
			continue
		}
		var target string
		switch {
		case strings.HasPrefix(sym.Name, "test_"):
			target = strings.TrimPrefix(sym.Name, "test_")
		case strings.HasPrefix(sym.Name, "Test"):
			target = strings.TrimPrefix(sym.Name, "Test")
		case strings.HasPrefix(sym.Name, "test"):
			target = strings.TrimPrefix(sym.Name, "test")
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
			TargetStableKey: "func:" + module + "::" + target,
		})
	}
}
