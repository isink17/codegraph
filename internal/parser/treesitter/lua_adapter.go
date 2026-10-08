//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	luagrammar "github.com/smacker/go-tree-sitter/lua"

	"github.com/isink17/codegraph/internal/graph"
)

type LuaAdapter struct{}

func NewLua() *LuaAdapter                       { return &LuaAdapter{} }
func (a *LuaAdapter) Language() string          { return "lua" }
func (a *LuaAdapter) Extensions() []string      { return []string{".lua"} }
func (a *LuaAdapter) Supports(path string) bool { return strings.EqualFold(filepath.Ext(path), ".lua") }

func (a *LuaAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, luagrammar.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	pf := graph.ParsedFile{Language: "lua", FileTokens: computeFileTokens(content)}
	for _, fn := range findDescendants(root, "function_statement") {
		name := childByFieldName(fn, "name")
		if name == nil {
			continue
		}
		raw := nodeText(name, content)
		qname := strings.ReplaceAll(raw, ":", ".")
		last := qname
		if i := strings.LastIndexByte(qname, '.'); i >= 0 {
			last = qname[i+1:]
		}
		container := ""
		if i := strings.LastIndexByte(qname, '.'); i >= 0 {
			container = qname[:i]
		}
		prefix := "global"
		if firstChild(fn, "local") != nil {
			prefix = "local"
		}
		kind := "function"
		if strings.Contains(raw, ":") {
			kind = "method"
		}
		rng := nodeRange(fn)
		// Repeated local names in nested lexical scopes are distinct declarations.
		stableKey := "func:lua:" + prefix + ":" + qname + ":" + strconv.Itoa(rng.StartLine) + ":" + strconv.Itoa(rng.StartCol)
		pf.Symbols = append(pf.Symbols, graph.Symbol{Language: "lua", Kind: kind, Name: last, QualifiedName: qname, ContainerName: container, Range: rng, StableKey: stableKey})
	}
	for _, call := range findDescendants(root, "function_call") {
		prefix := childByFieldName(call, "prefix")
		if prefix == nil || prefix.Type() != "identifier" || strings.TrimSpace(nodeText(prefix, content)) != "require" {
			continue
		}
		args := firstChild(call, "function_arguments")
		if args == nil || args.NamedChildCount() != 1 {
			continue
		}
		arg := args.NamedChild(0)
		if arg.Type() != "string" {
			continue
		}
		specifier := strings.Trim(nodeText(arg, content), "\"'")
		if specifier != "" {
			pf.Imports = append(pf.Imports, specifier)
		}
	}
	for _, call := range findDescendants(root, "function_call") {
		name := luaCallName(call, content)
		if name == "" {
			continue
		}
		pf.References = append(pf.References, graph.Reference{Kind: "call", Name: name, QualifiedName: name, Range: nodeRange(call)})
	}
	return pf, nil
}

func luaCallName(node *sitter.Node, content []byte) string {
	if node == nil || node.Type() != "function_call" {
		return ""
	}
	var parts []string
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "table_dot" || child.Type() == "." {
			parts = append(parts, ".")
			continue
		}
		if !child.IsNamed() {
			continue
		}
		switch child.Type() {
		case "identifier":
			if i > 0 && node.Child(i-1).Type() == "table_dot" {
				parts = append(parts, ".")
			}
			parts = append(parts, nodeText(child, content))
		case "self_call_colon":
			parts = append(parts, ":")
		case "function_call_paren":
		case "function_arguments":
			return ""
		default:
			return ""
		}
	}
	return strings.Join(parts, "")
}
