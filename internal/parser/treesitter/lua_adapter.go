//go:build cgo

package treesitter

import (
	"bytes"
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
		raw := strings.TrimSpace(nodeText(name, content))
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
		rng := luaRange(fn, content)
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
		rng := luaRange(call, content)
		pf.References = append(pf.References, graph.Reference{Kind: "call", Name: name, QualifiedName: name, Range: rng})
	}
	pf.Edges = luaLocalFunctionCalls(root, content)
	return pf, nil
}

type luaBinding struct {
	// fn is the declaring `local function` statement, or nil for any other
	// local (a parameter, a loop variable, `local x = ...`): a call through
	// one of those names is not provable and stays unresolved.
	fn         *sitter.Node
	reassigned bool
}

type luaScope struct {
	parent *luaScope
	names  map[string]*luaBinding
}

func (s *luaScope) lookup(name string) *luaBinding {
	for ; s != nil; s = s.parent {
		if b, ok := s.names[name]; ok {
			return b
		}
	}
	return nil
}

func (s *luaScope) declare(name string, b *luaBinding) {
	if s.names == nil {
		s.names = map[string]*luaBinding{}
	}
	s.names[name] = b
}

type luaCallSite struct {
	call    *sitter.Node
	name    string
	binding *luaBinding
}

// luaLocalFunctionCalls returns a call edge for every bare `name(...)` call
// whose innermost visible binding is a `local function` statement that is
// never assigned to. Every other call stays unresolved: globals, fields,
// methods, parameters, other locals, and any binding with a plain assignment
// or a non-local `function name()` statement against it anywhere in its scope,
// closures included. A file with any parse error, or that names `debug` as an
// identifier or string (whose setlocal/setupvalue rewrite locals at run time),
// proves nothing.
func luaLocalFunctionCalls(root *sitter.Node, content []byte) []graph.Edge {
	if root.HasError() || luaNamesDebug(root, content) {
		return nil
	}
	var sites []luaCallSite
	var walk func(n *sitter.Node, scope *luaScope)
	walkChildren := func(n *sitter.Node, scope *luaScope, from int) {
		for i := from; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), scope)
		}
	}
	text := func(n *sitter.Node) string { return strings.TrimSpace(nodeText(n, content)) }
	assign := func(target *sitter.Node, scope *luaScope) {
		if target != nil && target.Type() == "identifier" {
			if b := scope.lookup(text(target)); b != nil {
				b.reassigned = true
			}
		}
	}
	// function walks a function's parameters and body in a fresh scope.
	function := func(n *sitter.Node, scope *luaScope, method bool) {
		inner := &luaScope{parent: scope}
		if method {
			inner.declare("self", &luaBinding{})
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			if c := n.Child(i); c.Type() == "parameter_list" {
				for j := 0; j < int(c.NamedChildCount()); j++ {
					if p := c.NamedChild(j); p.Type() == "identifier" {
						inner.declare(text(p), &luaBinding{})
					}
				}
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			if c := n.Child(i); c.Type() == "function_body" {
				walkChildren(c, &luaScope{parent: inner}, 0)
			}
		}
	}
	walk = func(n *sitter.Node, scope *luaScope) {
		switch n.Type() {
		case "function_statement":
			name := childByFieldName(n, "name")
			if firstChild(n, "local") != nil {
				// `local function f` binds f before its body, so f is visible
				// to its own recursion.
				scope.declare(text(name), &luaBinding{fn: n})
				function(n, scope, false)
				return
			}
			// `function f()` assigns f; `function M.f()` / `M:f()` assign a field.
			if name != nil && name.NamedChildCount() == 1 {
				assign(name.NamedChild(0), scope)
			}
			function(n, scope, name != nil && strings.Contains(text(name), ":"))
			return
		case "function":
			function(n, scope, false)
			return
		case "variable_declaration":
			local := firstChild(n, "local") != nil
			var declared []string
			for i := 0; i < int(n.ChildCount()); i++ {
				c := n.Child(i)
				if c.Type() != "variable_declarator" {
					walk(c, scope)
					continue
				}
				if c.NamedChildCount() == 1 && c.NamedChild(0).Type() == "identifier" {
					if local {
						declared = append(declared, text(c.NamedChild(0)))
					} else {
						assign(c.NamedChild(0), scope)
					}
					continue
				}
				walk(c, scope)
			}
			// A local's initialiser runs before the local is in scope.
			for _, name := range declared {
				scope.declare(name, &luaBinding{})
			}
			return
		case "for_statement":
			inner := &luaScope{parent: scope}
			for i := 0; i < int(n.ChildCount()); i++ {
				c := n.Child(i)
				switch c.Type() {
				case "for_numeric":
					// The bounds are evaluated outside the loop variable's scope.
					v := childByFieldName(c, "var")
					for j := 0; j < int(c.ChildCount()); j++ {
						if v == nil || c.Child(j).StartByte() != v.StartByte() {
							walk(c.Child(j), scope)
						}
					}
					if v != nil {
						inner.declare(text(v), &luaBinding{})
					}
				case "for_generic":
					if e := childByFieldName(c, "expression_list"); e != nil {
						walk(e, scope)
					}
					if ids := childByFieldName(c, "identifier_list"); ids != nil {
						for j := 0; j < int(ids.NamedChildCount()); j++ {
							inner.declare(text(ids.NamedChild(j)), &luaBinding{})
						}
					}
				default:
					walk(c, inner)
				}
			}
			return
		case "do_statement", "while_statement", "repeat_statement":
			// A repeat body's locals are visible in its until condition.
			walkChildren(n, &luaScope{parent: scope}, 0)
			return
		case "if_statement":
			inner := &luaScope{parent: scope}
			for i := 0; i < int(n.ChildCount()); i++ {
				c := n.Child(i)
				if c.Type() == "if_elseif" || c.Type() == "if_else" {
					inner = &luaScope{parent: scope}
				}
				walk(c, inner)
			}
			return
		case "function_call":
			if prefix := childByFieldName(n, "prefix"); prefix != nil && prefix.Type() == "identifier" {
				if name := luaCallName(n, content); name == text(prefix) {
					sites = append(sites, luaCallSite{call: n, name: name, binding: scope.lookup(name)})
				}
			}
		}
		walkChildren(n, scope, 0)
	}
	walk(root, &luaScope{})
	var edges []graph.Edge
	for _, site := range sites {
		if site.binding == nil || site.binding.fn == nil || site.binding.reassigned {
			continue
		}
		decl := luaRange(site.binding.fn, content)
		at := luaRange(site.call, content)
		edges = append(edges, graph.Edge{
			DstName:  site.name,
			Kind:     "calls",
			Evidence: graph.LuaLocalFunctionEvidence + strconv.Itoa(decl.StartLine) + ":" + strconv.Itoa(decl.StartCol),
			Line:     at.StartLine,
			Col:      at.StartCol,
		})
	}
	return edges
}

func luaNamesDebug(root *sitter.Node, content []byte) bool {
	// The string form catches require("debug") and _G["debug"].
	for _, kind := range []string{"identifier", "string_content"} {
		for _, n := range findDescendants(root, kind) {
			if strings.TrimSpace(nodeText(n, content)) == "debug" {
				return true
			}
		}
	}
	return false
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
			parts = append(parts, strings.TrimSpace(nodeText(child, content)))
		case "self_call_colon":
			parts = append(parts, ":")
		case "function_call_paren":
		case "function_arguments", "table_argument", "string_argument":
			// Arguments follow the callee; they are not part of its name.
			return strings.Join(parts, "")
		default:
			return ""
		}
	}
	return strings.Join(parts, "")
}

// luaRange is nodeRange starting at the node's first non-space byte. The pinned
// grammar folds the newline before a statement that starts at column zero into
// that statement's first token, which would otherwise place the statement,
// and a call that opens it, at the end of the previous line.
func luaRange(n *sitter.Node, content []byte) graph.Position {
	rng := nodeRange(n)
	start := int(n.StartByte())
	end := int(n.EndByte())
	for start < end && strings.IndexByte(" \t\r\n\f\v", content[start]) >= 0 {
		start++
	}
	line := 1 + bytes.Count(content[:start], []byte{'\n'})
	if line != rng.StartLine || start != int(n.StartByte()) {
		rng.StartLine = line
		rng.StartCol = start - (bytes.LastIndexByte(content[:start], '\n') + 1) + 1
	}
	return rng
}
