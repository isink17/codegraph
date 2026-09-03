//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	kotlin "github.com/smacker/go-tree-sitter/kotlin"

	"github.com/isink17/codegraph/internal/graph"
)

// KotlinAdapter parses Kotlin source files using tree-sitter.
type KotlinAdapter struct{}

func NewKotlin() *KotlinAdapter { return &KotlinAdapter{} }

func (a *KotlinAdapter) Language() string     { return "kotlin" }
func (a *KotlinAdapter) Extensions() []string { return []string{".kt", ".kts"} }

func (a *KotlinAdapter) Supports(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".kt" || ext == ".kts"
}

func (a *KotlinAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, kotlin.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := packageEvidence(content, "kotlin")
	pf := graph.ParsedFile{
		Language:   "kotlin",
		Scope:      graph.ScopeEvidence{Package: packageEvidence(content, "kotlin")},
		FileTokens: computeFileTokens(content),
	}

	kotlinExtractImports(root, content, &pf)
	kotlinExtractSymbols(root, module, "", "module", content, &pf)
	kotlinExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf, func(target string) string {
		return "func:kotlin:" + testTargetModule(module, "Test", "Tests") + ":" + target
	})
	return pf, nil
}

func kotlinExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, imp := range findDescendants(root, "import_header") {
		ident := childByFieldName(imp, "identifier")
		if ident != nil {
			pf.Imports = append(pf.Imports, nodeText(ident, content))
		}
		addKotlinScope(nodeText(imp, content), &pf.Scope.Imports)
	}
}

func kotlinExtractSymbols(node *sitter.Node, module, container, ownerKind string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration":
			kotlinAddType(child, module, container, "class", content, pf)
		case "object_declaration":
			kotlinAddType(child, module, container, "object", content, pf)
		case "interface_declaration":
			kotlinAddType(child, module, container, "interface", content, pf)
		case "function_declaration":
			if ownerKind == "type" || ownerKind == "module" {
				kotlinAddFunction(child, module, container, content, pf)
			}
		}
	}
}

func kotlinAddType(node *sitter.Node, module, parent, kind string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		nameNode = firstChild(node, "type_identifier")
	}
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	qualified := kotlinQualified(module, name)
	container := module
	if parent != "" {
		qualified = kotlinQualified(module, parent+"."+name)
		container = parent
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "kotlin",
		Kind:          kind,
		Name:          name,
		QualifiedName: qualified,
		ContainerName: container,
		Visibility:    kotlinVisibility(node, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:kotlin:" + qualified,
	})

	body := childByFieldName(node, "body")
	if body == nil {
		body = firstChild(node, "class_body")
	}
	if body != nil {
		nextContainer := name
		if parent != "" {
			nextContainer = parent + "." + name
		}
		kotlinExtractSymbols(body, module, nextContainer, "type", content, pf)
	}
}

func kotlinAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		nameNode = firstChild(node, "simple_identifier")
	}
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	effectiveContainer := module
	if container != "" && container != module {
		effectiveContainer = container
	}
	qualified := kotlinQualified(module, name)
	if container != "" && container != module {
		qualified = kotlinQualified(module, container+"."+name)
	}
	sig := kotlinDeclarationSignature(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "kotlin",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Signature:     sig,
		Visibility:    kotlinVisibility(node, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "func:kotlin:" + qualified,
	})
}

func kotlinQualified(pkg, name string) string {
	return strings.Trim(strings.TrimSpace(pkg)+"."+strings.TrimSpace(name), ".")
}

func kotlinVisibility(node *sitter.Node, content []byte) string {
	text := string(content[node.StartByte():node.EndByte()])
	if brace := strings.IndexByte(text, '{'); brace >= 0 {
		text = text[:brace]
	}
	if eq := strings.IndexByte(text, '='); eq >= 0 {
		text = text[:eq]
	}
	if regexp.MustCompile(`\bprivate\b`).MatchString(text) {
		return "private"
	}
	if regexp.MustCompile(`\bprotected\b`).MatchString(text) {
		return "protected"
	}
	if regexp.MustCompile(`\binternal\b`).MatchString(text) {
		return "internal"
	}
	return "public"
}

func kotlinDeclarationSignature(node *sitter.Node, content []byte) string {
	end := node.EndByte()
	if body := childByFieldName(node, "body"); body != nil {
		end = body.StartByte()
	}
	return strings.TrimSpace(string(content[node.StartByte():end]))
}

func kotlinExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil && call.ChildCount() > 0 {
			fnNode = call.Child(0)
		}
		if fnNode == nil {
			continue
		}
		name := nodeText(fnNode, content)
		if name == "" {
			continue
		}
		line := int(call.StartPoint().Row) + 1
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     name,
			Kind:        "calls",
			Evidence:    name,
			Line:        line,
		})
		pf.References = append(pf.References, graph.Reference{
			Kind:          "call",
			Name:          name,
			QualifiedName: name,
			Range:         nodeRange(call),
		})
	}
}
