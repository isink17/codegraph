package golang

import (
	"go/ast"
	"go/token"

	"github.com/isink17/codegraph/internal/classify"
	"github.com/isink17/codegraph/internal/graph"
)

// collectGoLocals records every name a Go file's own lexical blocks bind, with
// the line range of the block that binds it.
//
// The output is deliberately one-sided evidence. It says a name is local; it
// does not say what the name holds unless the syntax spells the type out. A
// `s := NewStore()` binding carries no type, only the function it was
// initialized from; the resolver types it through that function's recorded
// result or not at all.
type goLocalCollector struct {
	fset    *token.FileSet
	imports map[string]string
	out     []graph.GoLocalBinding
	inits   []goInitRef
}

// goInitRef is an initializer call awaiting the shadow check, which needs every
// binding of the file first.
type goInitRef struct {
	index      int
	callee     string
	qualifier  string
	importPath string
	line       int
}

func collectGoLocals(fset *token.FileSet, file *ast.File, imports map[string]string) []graph.GoLocalBinding {
	c := &goLocalCollector{fset: fset, imports: imports}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// Receiver, parameters and named results are bound across the whole
		// function, so their scope is the declaration, not the body.
		start, end := c.span(fn.Pos(), fn.End())
		c.fields(fn.Recv, start, end)
		if fn.Type != nil {
			c.fields(fn.Type.Params, start, end)
			c.fields(fn.Type.Results, start, end)
		}
		c.stmt(fn.Body)
		c.funcLits(fn.Body)
	}

	idx := newGoLocalIndex(c.out)
	for _, ref := range c.inits {
		shadow := ref.callee
		if ref.qualifier != "" {
			shadow = ref.qualifier
		}
		if idx.boundAt(shadow, ref.line) {
			continue
		}
		c.out[ref.index].InitFunction = ref.callee
		c.out[ref.index].InitImportPath = ref.importPath
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if callee, inner, ok := goChainedCall(call.Fun); ok && !idx.boundAt(callee, c.fset.Position(inner.Pos()).Line) {
			start, end := c.span(inner.Pos(), inner.End())
			c.out = append(c.out, graph.GoLocalBinding{Name: callee + "()", ScopeStartLine: start, ScopeEndLine: end, InitFunction: callee})
		}
		return true
	})
	return c.out
}

// goChainedCall matches a selector made directly on a call of a bare function
// name, `NewServer().Start`, and returns that name and the inner call.
func goChainedCall(fun ast.Expr) (string, *ast.CallExpr, bool) {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return "", nil, false
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return "", nil, false
	}
	id, ok := inner.Fun.(*ast.Ident)
	if !ok {
		return "", nil, false
	}
	// A predeclared function or conversion (`new(T)`, `string(b)`) is never a
	// function this graph records a result for.
	if _, builtin := classify.BuiltinCandidate("go", id.Name); builtin {
		return "", nil, false
	}
	return id.Name, inner, true
}

// collectGoFunctionResults records the result type of every non-generic
// package-level function with exactly one result whose type is a named type.
func collectGoFunctionResults(file *ast.File, imports map[string]string) []graph.GoFunctionResult {
	var out []graph.GoFunctionResult
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Type == nil || fn.Type.TypeParams != nil || fn.Type.Results == nil {
			continue
		}
		results := fn.Type.Results.List
		if len(results) != 1 || len(results[0].Names) > 1 {
			continue
		}
		name, pkg, _ := goTypeSpelling(results[0].Type)
		if name == "" {
			continue
		}
		r := graph.GoFunctionResult{Function: fn.Name.Name, TypeName: name}
		if pkg != "" {
			if r.TypeImportPath = imports[pkg]; r.TypeImportPath == "" {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// funcLits records the bindings of every function literal in a subtree.
//
// It is a separate full-subtree walk rather than a hook in the statement walk
// because a literal can appear in any expression position -- an `if` condition,
// a switch tag, a range expression, a call argument -- and a whitelist of the
// positions worth visiting is exactly the kind of thing that silently disagrees
// with the tree-sitter adapter, which visits every child. stmt never descends
// into expressions, so each literal is reached here exactly once however deeply
// it is nested.
func (c *goLocalCollector) funcLits(body *ast.BlockStmt) {
	if body == nil {
		return
	}
	ast.Inspect(body, func(node ast.Node) bool {
		lit, ok := node.(*ast.FuncLit)
		if !ok {
			return true
		}
		start, end := c.span(lit.Pos(), lit.End())
		if lit.Type != nil {
			c.fields(lit.Type.Params, start, end)
			c.fields(lit.Type.Results, start, end)
		}
		c.stmt(lit.Body)
		return true
	})
}

func (c *goLocalCollector) span(pos, end token.Pos) (int, int) {
	return c.fset.Position(pos).Line, c.fset.Position(end).Line
}

func (c *goLocalCollector) emit(name string, start, end int, typ ast.Expr) bool {
	if name == "" || name == "_" {
		return false
	}
	b := graph.GoLocalBinding{Name: name, ScopeStartLine: start, ScopeEndLine: end}
	if typ != nil {
		b.TypeName, b.TypePackage, b.Pointer = goTypeSpelling(typ)
		if b.TypePackage != "" {
			b.TypeImportPath = c.imports[b.TypePackage]
		}
	}
	c.out = append(c.out, b)
	return true
}

// initFrom records that the binding just emitted is initialized by a call of a
// bare or import-qualified function name. Any other initializer proves nothing.
func (c *goLocalCollector) initFrom(value ast.Expr) {
	call, ok := value.(*ast.CallExpr)
	if !ok {
		return
	}
	ref := goInitRef{index: len(c.out) - 1, line: c.fset.Position(call.Pos()).Line}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		ref.callee = fun.Name
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		if !ok || c.imports[pkg.Name] == "" {
			return
		}
		ref.callee, ref.qualifier, ref.importPath = fun.Sel.Name, pkg.Name, c.imports[pkg.Name]
	default:
		return
	}
	c.inits = append(c.inits, ref)
}

func (c *goLocalCollector) fields(list *ast.FieldList, start, end int) {
	if list == nil {
		return
	}
	for _, f := range list.List {
		for _, name := range f.Names {
			c.emit(name.Name, start, end, f.Type)
		}
	}
}

// stmt walks a statement, binding whatever it declares into the scope the
// statement itself owns.
func (c *goLocalCollector) stmt(n ast.Stmt) {
	switch s := n.(type) {
	case nil:
		return
	case *ast.BlockStmt:
		start, end := c.span(s.Pos(), s.End())
		for _, inner := range s.List {
			c.declare(inner, start, end)
			c.stmt(inner)
		}
	case *ast.IfStmt:
		// The init statement and both branches share the `if` as their scope,
		// which is what Go does: `if v, ok := m[k]; ok` reaches the else.
		start, end := c.span(s.Pos(), s.End())
		c.declare(s.Init, start, end)
		c.stmt(s.Init)
		c.stmt(s.Body)
		c.stmt(s.Else)
	case *ast.ForStmt:
		start, end := c.span(s.Pos(), s.End())
		c.declare(s.Init, start, end)
		c.stmt(s.Init)
		c.stmt(s.Post)
		c.stmt(s.Body)
	case *ast.RangeStmt:
		// Range variables have no syntax-proven type: the element type comes
		// from the ranged expression, which is inference this evidence does not
		// do. They are recorded as untyped locals, which vetoes and binds
		// nothing.
		start, end := c.span(s.Pos(), s.End())
		if s.Tok == token.DEFINE {
			c.ident(s.Key, start, end)
			c.ident(s.Value, start, end)
		}
		c.stmt(s.Body)
	case *ast.SwitchStmt:
		start, end := c.span(s.Pos(), s.End())
		c.declare(s.Init, start, end)
		c.stmt(s.Init)
		c.stmt(s.Body)
	case *ast.TypeSwitchStmt:
		// `switch v := x.(type)` rebinds v to a different type per clause, so
		// the binding is recorded untyped even though a single-type clause
		// would in principle prove one.
		start, end := c.span(s.Pos(), s.End())
		c.declare(s.Init, start, end)
		c.stmt(s.Init)
		if assign, ok := s.Assign.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			for _, lhs := range assign.Lhs {
				c.ident(lhs, start, end)
			}
		}
		c.stmt(s.Body)
	case *ast.SelectStmt:
		c.stmt(s.Body)
	case *ast.CaseClause:
		start, end := c.span(s.Pos(), s.End())
		for _, inner := range s.Body {
			c.declare(inner, start, end)
			c.stmt(inner)
		}
	case *ast.CommClause:
		start, end := c.span(s.Pos(), s.End())
		c.declare(s.Comm, start, end)
		c.stmt(s.Comm)
		for _, inner := range s.Body {
			c.declare(inner, start, end)
			c.stmt(inner)
		}
	case *ast.LabeledStmt:
		c.stmt(s.Stmt)
	}
}

// declare records what a single statement binds into the scope that holds it,
// from the line the declaration starts on: a use above it names an outer
// binding.
func (c *goLocalCollector) declare(n ast.Stmt, start, end int) {
	switch s := n.(type) {
	case *ast.DeclStmt:
		decl, ok := s.Decl.(*ast.GenDecl)
		if !ok {
			return
		}
		for _, spec := range decl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			specStart := max(start, c.fset.Position(value.Pos()).Line)
			for i, name := range value.Names {
				// `var x T` states the type. `var x = expr` does not, unless
				// the expression is a composite literal naming its own type.
				typ := value.Type
				if typ == nil && i < len(value.Values) {
					typ = compositeLiteralType(value.Values[i])
				}
				if c.emit(name.Name, specStart, end, typ) && typ == nil && decl.Tok == token.VAR && len(value.Names) == 1 && len(value.Values) == 1 {
					c.initFrom(value.Values[0])
				}
			}
		}
	case *ast.AssignStmt:
		if s.Tok != token.DEFINE {
			return
		}
		start = max(start, c.fset.Position(s.Pos()).Line)
		// Only a 1:1 assignment can carry a type across. `a, b := f()` binds
		// both names untyped. An initializer call is read only for a single
		// name: with several, `:=` may redeclare one that keeps its old type.
		paired := len(s.Lhs) == len(s.Rhs)
		for i, lhs := range s.Lhs {
			var typ ast.Expr
			if paired {
				typ = compositeLiteralType(s.Rhs[i])
			}
			if c.identTyped(lhs, start, end, typ) && typ == nil && len(s.Lhs) == 1 && len(s.Rhs) == 1 {
				c.initFrom(s.Rhs[0])
			}
		}
	}
}

func (c *goLocalCollector) ident(n ast.Expr, start, end int) {
	c.identTyped(n, start, end, nil)
}

func (c *goLocalCollector) identTyped(n ast.Expr, start, end int, typ ast.Expr) bool {
	if id, ok := n.(*ast.Ident); ok {
		return c.emit(id.Name, start, end, typ)
	}
	return false
}

// compositeLiteralType returns the type a composite literal names directly, and
// nil for every other expression. `Store{}` and `&Store{}` prove a type; a call,
// a field selection, a conversion or anything else does not, and this slice
// deliberately does not infer through them.
func compositeLiteralType(n ast.Expr) ast.Expr {
	switch e := n.(type) {
	case *ast.CompositeLit:
		return e.Type
	case *ast.UnaryExpr:
		if e.Op != token.AND {
			return nil
		}
		lit, ok := e.X.(*ast.CompositeLit)
		if !ok || lit.Type == nil {
			return nil
		}
		return &ast.StarExpr{X: lit.Type}
	default:
		return nil
	}
}

// goTypeSpelling reduces a type expression to the bare name, its written
// package qualifier, and whether it was a pointer. It returns an empty name for
// any shape whose identity this evidence cannot state: slices, maps, channels,
// function types, interfaces, anonymous structs and array types are all types a
// method could be called on, but none of them is a named type this graph holds.
func goTypeSpelling(n ast.Expr) (name, pkg string, pointer bool) {
	switch e := n.(type) {
	case *ast.StarExpr:
		name, pkg, _ = goTypeSpelling(e.X)
		return name, pkg, true
	case *ast.ParenExpr:
		return goTypeSpelling(e.X)
	case *ast.Ident:
		return e.Name, "", false
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		if !ok {
			return "", "", false
		}
		return e.Sel.Name, id.Name, false
	case *ast.IndexExpr:
		return goTypeSpelling(e.X)
	case *ast.IndexListExpr:
		return goTypeSpelling(e.X)
	default:
		return "", "", false
	}
}

// goLocalIndex answers "is this name locally bound at this line" without a scan
// per call site.
type goLocalIndex struct {
	bindings []graph.GoLocalBinding
	byName   map[string][]graph.GoLocalBinding
}

func newGoLocalIndex(bindings []graph.GoLocalBinding) goLocalIndex {
	idx := goLocalIndex{bindings: bindings, byName: make(map[string][]graph.GoLocalBinding, len(bindings))}
	for _, b := range bindings {
		idx.byName[b.Name] = append(idx.byName[b.Name], b)
	}
	return idx
}

func (i goLocalIndex) boundAt(name string, line int) bool {
	for _, b := range i.byName[name] {
		if line >= b.ScopeStartLine && line <= b.ScopeEndLine {
			return true
		}
	}
	return false
}
