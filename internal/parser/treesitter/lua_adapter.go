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
	pf.Edges = luaLocalFunctionCalls(ctx, root, content)
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
// closures included. A file with any parse error, or whose own code can reach
// the debug library (see luaScan), proves nothing.
//
// The proof assumes no code outside this file mutates its locals or upvalues
// through the debug library or the C API: another module calling
// debug.setupvalue on a function this file exports is not visible here.
func luaLocalFunctionCalls(ctx context.Context, root *sitter.Node, content []byte) []graph.Edge {
	if root.HasError() {
		return nil
	}
	edges, hazard := luaScan(ctx, root, content)
	if hazard {
		return nil
	}
	return edges
}

// luaHazardNames are the globals through which a chunk can reach the debug
// library, whose setlocal/setupvalue/upvaluejoin/sethook rewrite locals at run
// time: the library itself, the global tables that hold it, and the loaders
// that return it or run code that can name it.
var luaHazardNames = map[string]bool{
	"debug": true, "_G": true, "_ENV": true, "package": true, "getfenv": true,
	"require": true, "load": true, "loadstring": true, "dofile": true, "loadfile": true,
}

// luaScan walks a chunk once, resolving bare calls by lexical scope and
// reporting whether the chunk can reach the debug library. A hazard is a free
// (not lexically bound) reference to one of luaHazardNames, except:
//   - debug.traceback and debug.getinfo, which read the stack but cannot
//     rebind a local or upvalue;
//   - _G.x, _ENV.x, _G["x"], _ENV["x"], rawget/rawset(_G, "x", ...) and
//     package.loaded.x / package.loaded["x"], for a literal x outside
//     luaHazardNames;
//   - package.path, package.cpath and package.config;
//   - require, dofile and loadfile of a literal other than "debug" (other
//     files are outside this proof, as above);
//   - load and loadstring of a literal chunk that is itself hazard-free.
//
// Field and method names, table-constructor keys, labels and string data are
// never references. A computed key or module name is a hazard.
func luaScan(ctx context.Context, root *sitter.Node, content []byte) ([]graph.Edge, bool) {
	hazard := false
	var sites []luaCallSite
	var walk func(n *sitter.Node, scope *luaScope)
	walkChildren := func(n *sitter.Node, scope *luaScope) {
		for i := 0; i < int(n.ChildCount()); i++ {
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
	free := func(n *sitter.Node, scope *luaScope) string {
		if n != nil && n.Type() == "identifier" && scope.lookup(text(n)) == nil {
			return text(n)
		}
		return ""
	}
	// safeKey reports whether n is a literal naming none of luaHazardNames.
	safeKey := func(n *sitter.Node) bool {
		s, ok := luaLiteral(n, content)
		return ok && !luaHazardNames[s]
	}
	// globalTable reports whether n is a free _G or _ENV, or package.loaded.
	globalTable := func(n *sitter.Node, scope *luaScope) bool {
		switch free(n, scope) {
		case "_G", "_ENV":
			return true
		}
		return n != nil && n.Type() == "dot_index_expression" &&
			free(childByFieldName(n, "table"), scope) == "package" && text(childByFieldName(n, "field")) == "loaded"
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
			if name != nil && name.Type() != "identifier" {
				walk(name, scope)
			}
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
				walkChildren(body, inner)
			}
			if cond := childByFieldName(n, "condition"); cond != nil {
				walk(cond, inner)
			}
			return
		case "block":
			// Every other block (do, while, if/elseif/else, loop and function
			// bodies) is its own scope.
			walkChildren(n, &luaScope{parent: scope})
			return
		case "identifier":
			if luaHazardNames[free(n, scope)] {
				hazard = true
			}
			return
		case "goto_statement", "label_statement":
			return
		case "field":
			// `{k = v}` names a key; `{[k] = v}` evaluates one.
			if firstChild(n, "[") == nil {
				if v := childByFieldName(n, "value"); v != nil {
					walk(v, scope)
				}
				return
			}
		case "method_index_expression":
			walk(childByFieldName(n, "table"), scope)
			return
		case "dot_index_expression":
			table, field := childByFieldName(n, "table"), text(childByFieldName(n, "field"))
			switch {
			case free(table, scope) == "debug":
				// traceback and getinfo read the stack; neither rebinds anything.
				hazard = hazard || (field != "traceback" && field != "getinfo")
				return
			case free(table, scope) == "package" && (field == "path" || field == "cpath" || field == "config"):
				return
			case globalTable(table, scope):
				hazard = hazard || luaHazardNames[field]
				return
			}
			walk(table, scope)
			return
		case "bracket_index_expression":
			if globalTable(childByFieldName(n, "table"), scope) {
				hazard = hazard || !safeKey(childByFieldName(n, "field"))
				return
			}
		case "function_call":
			name := childByFieldName(n, "name")
			if name != nil && name.Type() == "identifier" {
				sites = append(sites, luaCallSite{call: n, name: text(name), binding: scope.lookup(text(name))})
			}
			args := luaArgs(childByFieldName(n, "arguments"))
			switch free(name, scope) {
			case "require", "dofile", "loadfile":
				// package.loaded preloads "_G" and "package", which hold
				// the debug library, so a literal naming any hazard is one;
				// LuaJIT's "ffi" reaches the C API.
				if len(args) == 1 {
					if s, ok := luaLiteral(args[0], content); ok && !luaHazardNames[s] && s != "ffi" {
						return
					}
				}
				hazard = true
				return
			case "load", "loadstring":
				if len(args) > 0 {
					if s, ok := luaLiteral(args[0], content); ok && !luaChunkHazard(ctx, s) {
						for _, a := range args[1:] {
							walk(a, scope)
						}
						return
					}
				}
				hazard = true
				return
			case "rawget", "rawset":
				if len(args) >= 2 && globalTable(args[0], scope) {
					hazard = hazard || !safeKey(args[1])
					for _, a := range args[2:] {
						walk(a, scope)
					}
					return
				}
			}
		}
		walkChildren(n, scope)
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
	return edges, hazard
}

// luaChunkHazard reports whether a literal chunk handed to load can reach the
// debug library; a chunk that does not parse is treated as one that can.
func luaChunkHazard(ctx context.Context, chunk string) bool {
	root, err := parse(ctx, luagrammar.GetLanguage(), []byte(chunk))
	if err != nil || root.HasError() {
		return true
	}
	_, hazard := luaScan(ctx, root, []byte(chunk))
	return hazard
}

// luaArgs returns a call's argument expressions.
func luaArgs(args *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for i := 0; args != nil && i < int(args.NamedChildCount()); i++ {
		if c := args.NamedChild(i); c.Type() != "comment" {
			out = append(out, c)
		}
	}
	return out
}

// luaLiteral returns the value of a string literal without escapes. A string
// with a backslash is not read: its value need not equal its spelling.
func luaLiteral(n *sitter.Node, content []byte) (string, bool) {
	if n == nil || n.Type() != "string" {
		return "", false
	}
	s := nodeText(childByFieldName(n, "content"), content)
	if strings.ContainsRune(s, '\\') {
		return "", false
	}
	// A long string drops the line break that opens it.
	for _, nl := range []string{"\r\n", "\n\r", "\n", "\r"} {
		if strings.HasPrefix(s, nl) {
			return s[len(nl):], true
		}
	}
	return s, true
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
