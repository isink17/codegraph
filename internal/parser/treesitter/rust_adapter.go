//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	rust "github.com/smacker/go-tree-sitter/rust"

	"github.com/isink17/codegraph/internal/graph"
)

// RustAdapter parses Rust source files using tree-sitter.
type RustAdapter struct{}

func NewRust() *RustAdapter { return &RustAdapter{} }

func (a *RustAdapter) Language() string     { return "rust" }
func (a *RustAdapter) Extensions() []string { return []string{".rs"} }

func (a *RustAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".rs")
}

func (a *RustAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, rust.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := rustModulePath(path)
	pf := graph.ParsedFile{
		Language:   "rust",
		Scope:      graph.ScopeEvidence{ModulePath: module},
		FileTokens: computeFileTokens(content),
	}

	rustExtractImports(root, module, content, &pf)
	rustExtractSymbols(root, module, "", path, content, &pf)
	rustExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf, func(target string) string {
		return "func:rust:" + testTargetModule(module, "_test") + ":" + target
	})
	return pf, nil
}

func rustExtractImports(root *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	for i := range int(root.ChildCount()) {
		child := root.Child(i)
		if child.Type() == "use_declaration" {
			if arg := childByFieldName(child, "argument"); arg != nil {
				pf.Imports = append(pf.Imports, nodeText(arg, content))
				addRustScope(nodeText(child, content), rustVisibility(child, content) == "public", module, &pf.Scope.Imports)
			}
		}
		if child.Type() == "mod_item" {
			if name := childByFieldName(child, "name"); name != nil {
				if body := childByFieldName(child, "body"); body != nil {
					rustExtractImports(body, module+"::"+nodeText(name, content), content, pf)
				}
			}
		}
	}
}

func rustModulePath(path string) string {
	p := filepath.ToSlash(path)
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	base := strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1]))
	if (base == "lib" || base == "main") && !strings.Contains(p, "/src/") {
		return "crate"
	}
	if filepath.IsAbs(path) && !strings.Contains(p, "/src/") && len(parts) > 1 {
		parts = parts[len(parts)-1:]
	}
	if base == "lib" || base == "main" || base == "mod" {
		parts = parts[:len(parts)-1]
	} else {
		parts[len(parts)-1] = base
	}
	for i, part := range parts {
		if part == "src" {
			parts = parts[i+1:]
			break
		}
	}
	if len(parts) > 0 && parts[0] == "bin" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return "crate"
	}
	return "crate::" + strings.Join(parts, "::")
}

func rustExtractSymbols(node *sitter.Node, module, container, path string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "function_item":
			rustAddFunction(child, module, container, content, pf)
		case "struct_item":
			rustAddType(child, module, "struct", content, pf)
		case "enum_item":
			rustAddType(child, module, "enum", content, pf)
		case "trait_item":
			rustAddType(child, module, "trait", content, pf)
		case "impl_item":
			rustExtractImpl(child, module, path, content, pf)
		case "mod_item":
			nameNode := childByFieldName(child, "name")
			body := childByFieldName(child, "body")
			if nameNode != nil {
				name := nodeText(nameNode, content)
				m := graph.RustModule{Name: name, OwnerModule: module, Inline: body != nil, Visibility: rustVisibility(child, content)}
				if body == nil {
					m.ExternalPath = filepath.ToSlash(filepath.Join(rustModuleSourceBase(path), name))
				}
				pf.Scope.Modules = append(pf.Scope.Modules, m)
				if body != nil {
					rustExtractSymbols(body, module+"::"+name, "", path, content, pf)
				}
			}
		}
	}
}

func rustAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	effectiveContainer := module
	if container != "" && container != module {
		effectiveContainer = container
	}
	qualified := module + "::" + name
	if container != "" && container != module {
		qualified = module + "::" + container + "::" + name
	}
	stableKey := "func:rust:" + module + ":" + name

	vis := rustVisibility(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "rust",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     stableKey,
	})
}

func rustAddType(node *sitter.Node, module, kind string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	vis := rustVisibility(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "rust",
		Kind:          kind,
		Name:          name,
		QualifiedName: module + "::" + name,
		ContainerName: module,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:rust:" + module + ":" + name,
	})
}

func rustExtractImpl(node *sitter.Node, module, path string, content []byte, pf *graph.ParsedFile) {
	typeNode := childByFieldName(node, "type")
	if typeNode == nil {
		return
	}
	typeName := nodeText(typeNode, content)
	body := childByFieldName(node, "body")
	if body != nil {
		rustExtractSymbols(body, module, typeName, path, content, pf)
	}
}

func rustModuleSourceBase(path string) string {
	dir := filepath.ToSlash(filepath.Dir(path))
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if base != "lib" && base != "main" && base != "mod" {
		dir = filepath.ToSlash(filepath.Join(dir, base))
	}
	return dir
}
func rustVisibility(node *sitter.Node, content []byte) string {
	vis := firstChild(node, "visibility_modifier")
	if vis != nil {
		text := strings.TrimSpace(nodeText(vis, content))
		if text == "pub" {
			return "public"
		}
		return "restricted:" + strings.TrimSuffix(strings.TrimPrefix(text, "pub("), ")")
	}
	return "private"
}

func rustExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
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
