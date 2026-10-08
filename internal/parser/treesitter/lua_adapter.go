//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/parser/treesitter/luagrammar"
	sitter "github.com/smacker/go-tree-sitter"

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
	for _, fn := range findDescendants(root, "function_declaration") {
		name := childByFieldName(fn, "name")
		raw := luaDottedName(name, content)
		if raw == "" {
			continue
		}
		qname := strings.ReplaceAll(raw, ":", ".")
		last, container := qname, ""
		if i := strings.LastIndexByte(qname, '.'); i >= 0 {
			last, container = qname[i+1:], qname[:i]
		}
		prefix := "global"
		if firstChild(fn, "local") != nil {
			prefix = "local"
		}
		kind := "function"
		if name.Type() == "method_index_expression" {
			kind = "method"
		}
		rng := nodeRange(fn)
		// Repeated local names in nested lexical scopes are distinct declarations.
		stableKey := "func:lua:" + prefix + ":" + qname + ":" + strconv.Itoa(rng.StartLine) + ":" + strconv.Itoa(rng.StartCol)
		pf.Symbols = append(pf.Symbols, graph.Symbol{Language: "lua", Kind: kind, Name: last, QualifiedName: qname, ContainerName: container, Range: rng, StableKey: stableKey})
	}
	for _, call := range findDescendants(root, "function_call") {
		name := luaCallName(call, content)
		if name == "" {
			continue
		}
		pf.References = append(pf.References, graph.Reference{Kind: "call", Name: name, QualifiedName: name, Range: nodeRange(call)})
		if name != "require" {
			continue
		}
		// Only a single literal string argument names a module: require("m"),
		// require "m" or require [[m]].
		args := childByFieldName(call, "arguments")
		if args == nil || args.NamedChildCount() != 1 || args.NamedChild(0).Type() != "string" {
			continue
		}
		if specifier := nodeText(childByFieldName(args.NamedChild(0), "content"), content); specifier != "" {
			pf.Imports = append(pf.Imports, specifier)
		}
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
	text := func(n *sitter.Node) string { return nodeText(n, content) }
	assign := func(target *sitter.Node, scope *luaScope) {
		if target != nil && target.Type() == "identifier" {
			if b := scope.lookup(text(target)); b != nil {
				b.reassigned = true
			}
		}
	}
	// names returns the identifiers a variable_list declares or assigns, and
	// walks every other target (`t[f()]`, `g().x`) as an expression.
	names := func(list *sitter.Node, scope *luaScope) []*sitter.Node {
		var out []*sitter.Node
		for i := 0; i < int(list.ChildCount()); i++ {
			if list.FieldNameForChild(i) != "name" {
				continue
			}
			if c := list.Child(i); c.Type() == "identifier" {
				out = append(out, c)
			} else {
				walk(c, scope)
			}
		}
		return out
	}
	// function walks a function's parameters and body in a fresh scope.
	function := func(n *sitter.Node, scope *luaScope, method bool) {
		inner := &luaScope{parent: scope}
		if method {
			inner.declare("self", &luaBinding{})
		}
		if params := childByFieldName(n, "parameters"); params != nil {
			for i := 0; i < int(params.NamedChildCount()); i++ {
				if p := params.NamedChild(i); p.Type() == "identifier" {
					inner.declare(text(p), &luaBinding{})
				}
			}
		}
		if body := childByFieldName(n, "body"); body != nil {
			walk(body, inner)
		}
	}
	walk = func(n *sitter.Node, scope *luaScope) {
		switch n.Type() {
		case "function_declaration":
			name := childByFieldName(n, "name")
			if firstChild(n, "local") != nil {
				// `local function f` binds f before its body, so f is visible
				// to its own recursion.
				scope.declare(text(name), &luaBinding{fn: n})
				function(n, scope, false)
				return
			}
			// `function f()` assigns f; `function M.f()` / `M:f()` assign a field.
			assign(name, scope)
			function(n, scope, name != nil && name.Type() == "method_index_expression")
			return
		case "function_definition":
			function(n, scope, false)
			return
		case "variable_declaration":
			// `local x` or `local x, y <const> = ...`: the initialiser runs
			// before the locals are in scope.
			list, values := firstChild(n, "variable_list"), (*sitter.Node)(nil)
			if a := firstChild(n, "assignment_statement"); a != nil {
				list, values = firstChild(a, "variable_list"), firstChild(a, "expression_list")
			}
			if values != nil {
				walk(values, scope)
			}
			if list != nil {
				for _, id := range names(list, scope) {
					scope.declare(text(id), &luaBinding{})
				}
			}
			return
		case "assignment_statement":
			if list := firstChild(n, "variable_list"); list != nil {
				for _, id := range names(list, scope) {
					assign(id, scope)
				}
			}
			if values := firstChild(n, "expression_list"); values != nil {
				walk(values, scope)
			}
			return
		case "for_statement":
			// The loop header is evaluated outside the loop variables' scope.
			inner := &luaScope{parent: scope}
			if clause := childByFieldName(n, "clause"); clause != nil {
				switch clause.Type() {
				case "for_numeric_clause":
					for _, field := range []string{"start", "end", "step"} {
						if e := childByFieldName(clause, field); e != nil {
							walk(e, scope)
						}
					}
					if v := childByFieldName(clause, "name"); v != nil {
						inner.declare(text(v), &luaBinding{})
					}
				case "for_generic_clause":
					if e := firstChild(clause, "expression_list"); e != nil {
						walk(e, scope)
					}
					if list := firstChild(clause, "variable_list"); list != nil {
						for _, id := range names(list, scope) {
							inner.declare(text(id), &luaBinding{})
						}
					}
				}
			}
			if body := childByFieldName(n, "body"); body != nil {
				walk(body, inner)
			}
			return
		case "repeat_statement":
			// A repeat body's locals are visible in its until condition.
			inner := &luaScope{parent: scope}
			if body := childByFieldName(n, "body"); body != nil {
				walkChildren(body, inner, 0)
			}
			if cond := childByFieldName(n, "condition"); cond != nil {
				walk(cond, inner)
			}
			return
		case "block":
			// Every other block (do, while, if/elseif/else, loop and function
			// bodies) is its own scope.
			walkChildren(n, &luaScope{parent: scope}, 0)
			return
		case "function_call":
			if name := childByFieldName(n, "name"); name != nil && name.Type() == "identifier" {
				sites = append(sites, luaCallSite{call: n, name: text(name), binding: scope.lookup(text(name))})
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
		decl := nodeRange(site.binding.fn)
		at := nodeRange(site.call)
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

// luaCallName spells a call's callee as a dotted path: `f`, `a.b.c`, `a.b:m`.
// Any other callee (`(g)()`, `h()()`, `t[1]()`) has no name.
func luaCallName(call *sitter.Node, content []byte) string {
	return luaDottedName(childByFieldName(call, "name"), content)
}

func luaDottedName(n *sitter.Node, content []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "identifier":
		return nodeText(n, content)
	case "dot_index_expression", "method_index_expression":
		sep, member := ".", childByFieldName(n, "field")
		if n.Type() == "method_index_expression" {
			sep, member = ":", childByFieldName(n, "method")
		}
		table := luaDottedName(childByFieldName(n, "table"), content)
		if table == "" || member == nil || member.Type() != "identifier" {
			return ""
		}
		return table + sep + nodeText(member, content)
	}
	return ""
}
