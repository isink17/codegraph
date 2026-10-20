package golang

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	cgparser "github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/texttoken"
)

func intRef(i int) *int { return &i }

type Adapter struct{}

func New() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Language() string {
	return "go"
}

func (a *Adapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

func (a *Adapter) Extensions() []string {
	return []string{".go"}
}

func (a *Adapter) Parse(_ context.Context, path string, content []byte) (graph.ParsedFile, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, content, parser.ParseComments)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	pf := graph.ParsedFile{
		Language:   "go",
		FileTokens: texttoken.Weights(content),
	}

	pkgName := file.Name.Name
	importAliases := map[string]string{}
	for _, imp := range file.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		pf.Imports = append(pf.Imports, importPath)
		base := filepath.Base(importPath)
		if imp.Name != nil && imp.Name.Name != "_" && imp.Name.Name != "." {
			base = imp.Name.Name
		}
		importAliases[base] = importPath
	}

	commentMap := map[ast.Node]string{}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			commentMap[d] = commentText(d.Doc)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				commentMap[spec] = commentText(d.Doc)
			}
		}
	}

	locals := newGoLocalIndex(collectGoLocals(fset, file, importAliases))
	pf.Scope.GoLocals = locals.bindings
	pf.Scope.GoFunctionResults = collectGoFunctionResults(file, importAliases)
	pf.Scope.GoFile = cgparser.GoFileIdentity(content)

	contextStack := []*graph.Symbol{}
	enterFuncStack := []bool{}
	symbolByKey := map[string]*graph.Symbol{}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := buildFuncSymbol(fset, content, pkgName, d, commentMap[d])
			pf.Symbols = append(pf.Symbols, sym)
			symbolByKey[sym.StableKey] = &pf.Symbols[len(pf.Symbols)-1]
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					sym := buildTypeSymbol(fset, content, pkgName, s, commentMap[s])
					pf.Symbols = append(pf.Symbols, sym)
					symbolByKey[sym.StableKey] = &pf.Symbols[len(pf.Symbols)-1]
				case *ast.ValueSpec:
					for _, name := range s.Names {
						sym := buildValueSymbol(fset, pkgName, name, commentMap[s])
						pf.Symbols = append(pf.Symbols, sym)
						symbolByKey[sym.StableKey] = &pf.Symbols[len(pf.Symbols)-1]
					}
				}
			}
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			if len(enterFuncStack) == 0 {
				return true
			}
			enteredFunc := enterFuncStack[len(enterFuncStack)-1]
			enterFuncStack = enterFuncStack[:len(enterFuncStack)-1]
			if enteredFunc && len(contextStack) > 0 {
				contextStack = contextStack[:len(contextStack)-1]
			}
			return true
		}
		enteredFunc := false
		defer func() {
			enterFuncStack = append(enterFuncStack, enteredFunc)
		}()
		switch node := n.(type) {
		case *ast.FuncDecl:
			key := stableFuncKey(pkgName, node)
			if sym := symbolByKey[key]; sym != nil {
				contextStack = append(contextStack, sym)
				enteredFunc = true
			}
			return true
		case *ast.CallExpr:
			if len(contextStack) == 0 {
				return true
			}
			name := callName(fset, node.Fun, importAliases, locals)
			if name == "" {
				return true
			}
			pos := fset.Position(node.Fun.Pos())
			pf.Edges = append(pf.Edges, graph.Edge{
				SrcSymbolID: 0,
				DstName:     name,
				Kind:        "calls",
				Evidence:    renderNode(content, fset, node.Fun),
				Line:        pos.Line,
				Col:         pos.Column,
			})
			rng := toRange(fset, node.Pos(), node.End())
			rng.StartCol = fset.Position(node.Fun.Pos()).Column
			pf.References = append(pf.References, graph.Reference{
				Kind:          "call",
				Name:          name,
				QualifiedName: name,
				Range:         rng,
			})
		}
		return true
	})

	linkTests(pkgName, &pf)
	return pf, nil
}

func buildFuncSymbol(fset *token.FileSet, content []byte, pkg string, fn *ast.FuncDecl, doc string) graph.Symbol {
	name := fn.Name.Name
	qualified := pkg + "." + name
	container := pkg
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv := receiverName(fn.Recv.List[0].Type)
		container = recv
		qualified = pkg + "." + recv + "." + name
	}
	kind := "function"
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		kind = "method"
	}
	return graph.Symbol{
		Language:      "go",
		Kind:          kind,
		Name:          name,
		QualifiedName: qualified,
		ContainerName: container,
		Signature:     renderFuncSignature(content, fset.File(fn.Pos()), fn),
		Visibility:    visibility(name),
		Range:         toRange(fset, fn.Pos(), fn.End()),
		DocSummary:    doc,
		StableKey:     stableFuncKey(pkg, fn),
	}
}

func buildTypeSymbol(fset *token.FileSet, content []byte, pkg string, ts *ast.TypeSpec, doc string) graph.Symbol {
	name := ts.Name.Name
	return graph.Symbol{
		Language:      "go",
		Kind:          "type",
		Name:          name,
		QualifiedName: pkg + "." + name,
		ContainerName: pkg,
		Signature:     renderTypeSignature(content, fset.File(ts.Pos()), ts),
		Visibility:    visibility(name),
		Range:         toRange(fset, ts.Pos(), ts.End()),
		DocSummary:    doc,
		StableKey:     "type:" + pkg + ":" + name,
	}
}

func buildValueSymbol(fset *token.FileSet, pkg string, ident *ast.Ident, doc string) graph.Symbol {
	name := ident.Name
	return graph.Symbol{
		Language:      "go",
		Kind:          "value",
		Name:          name,
		QualifiedName: pkg + "." + name,
		ContainerName: pkg,
		Visibility:    visibility(name),
		Range:         toRange(fset, ident.Pos(), ident.End()),
		DocSummary:    doc,
		StableKey:     "value:" + pkg + ":" + name,
	}
}

func stableFuncKey(pkg string, fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return "func:" + pkg + ":" + receiverName(fn.Recv.List[0].Type) + ":" + fn.Name.Name
	}
	return "func:" + pkg + "::" + fn.Name.Name
}

func visibility(name string) string {
	if name == "" {
		return ""
	}
	if strings.ToUpper(name[:1]) == name[:1] {
		return "exported"
	}
	return "package"
}

// callName renders a call's destination spelling. A selector whose qualifier is
// an import alias is rewritten to the import path, which is what makes
// `fmt.Println` reach the standard library rather than a local `fmt`.
//
// A qualifier the file's own lexical scope binds is never that import, however
// many import aliases happen to share its spelling, so the rewrite is skipped
// and the qualifier is kept verbatim. The resulting `x.Method` spelling is what
// the Go receiver-scope resolver reads, and what the local-qualifier veto stops
// every generic strategy from binding by coincidence.
//
// A selector made directly on a call of a bare function name is spelled
// `F().Method`, the qualifier collectGoLocals binds for it. Deeper chains
// produce no edge.
func callName(fset *token.FileSet, expr ast.Expr, imports map[string]string, locals goLocalIndex) string {
	if callee, inner, ok := goChainedCall(expr); ok {
		if locals.boundAt(callee, fset.Position(inner.Pos()).Line) {
			return ""
		}
		return callee + "()." + expr.(*ast.SelectorExpr).Sel.Name
	}
	return qualifiedCallName(fset, expr, imports, locals)
}

func qualifiedCallName(fset *token.FileSet, expr ast.Expr, imports map[string]string, locals goLocalIndex) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok && locals.boundAt(id.Name, fset.Position(id.Pos()).Line) {
			return id.Name + "." + e.Sel.Name
		}
		left := qualifiedCallName(fset, e.X, imports, locals)
		if left == "" {
			return ""
		}
		if importPath, ok := imports[left]; ok {
			return importPath + "." + e.Sel.Name
		}
		return left + "." + e.Sel.Name
	case *ast.ParenExpr:
		return qualifiedCallName(fset, e.X, imports, locals)
	case *ast.IndexListExpr:
		return qualifiedCallName(fset, e.X, imports, locals)
	default:
		return ""
	}
}

func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		left := receiverName(e.X)
		if left == "" {
			return ""
		}
		return left + "." + e.Sel.Name
	case *ast.ParenExpr:
		return receiverName(e.X)
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.IndexExpr:
		return receiverName(e.X)
	case *ast.IndexListExpr:
		return receiverName(e.X)
	default:
		return ""
	}
}

// declText is a node's source text. Signatures are spelled verbatim, as the
// tree-sitter Go adapter spells them, so both builds store the same signature
// for the same declaration. The positions come from parsing content itself, so
// there is no rendering step that could fail or lose a part of the type.
func declText(content []byte, file *token.File, node ast.Node) string {
	return string(content[file.Offset(node.Pos()):file.Offset(node.End())])
}

// renderFuncSignature spells `func (recv) Name[tparams](params) results`.
func renderFuncSignature(content []byte, file *token.File, fn *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")
	if fn.Recv != nil {
		b.WriteString(declText(content, file, fn.Recv))
		b.WriteByte(' ')
	}
	b.WriteString(fn.Name.Name)
	if fn.Type.TypeParams != nil {
		b.WriteString(declText(content, file, fn.Type.TypeParams))
	}
	b.WriteString(declText(content, file, fn.Type.Params))
	if fn.Type.Results != nil {
		b.WriteByte(' ')
		b.WriteString(declText(content, file, fn.Type.Results))
	}
	return b.String()
}

// renderTypeSignature spells a generic type's type parameter list, then a
// space, before `struct`, `interface` or the type's source text.
func renderTypeSignature(content []byte, file *token.File, ts *ast.TypeSpec) string {
	sig := declText(content, file, ts.Type)
	switch ts.Type.(type) {
	case *ast.StructType:
		sig = "struct"
	case *ast.InterfaceType:
		sig = "interface"
	}
	if ts.TypeParams != nil {
		return declText(content, file, ts.TypeParams) + " " + sig
	}
	return sig
}

func renderNode(content []byte, fset *token.FileSet, node ast.Node) string {
	start := fset.Position(node.Pos()).Offset
	end := fset.Position(node.End()).Offset
	if start < 0 || end < 0 || start >= len(content) || end > len(content) || start >= end {
		return ""
	}
	return string(content[start:end])
}

func commentText(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	return strings.TrimSpace(group.Text())
}

func toRange(fset *token.FileSet, start, end token.Pos) graph.Position {
	s := fset.Position(start)
	e := fset.Position(end)
	return graph.Position{
		StartLine: s.Line,
		StartCol:  s.Column,
		EndLine:   e.Line,
		EndCol:    e.Column,
	}
}

func linkTests(pkg string, pf *graph.ParsedFile) {
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
			TargetName:      pkg + "." + target,
			Reason:          "test_name_match",
			Score:           0.8,
			TestSymbolKey:   sym.StableKey,
			TargetStableKey: "func:" + pkg + "::" + target,
			TestSymbolIndex: intRef(i),
		})
	}
}

// Profile identifies the non-cgo Go adapter, which parses with go/ast and does
// build a call graph. See cgparser.Profile for the versioning rule.
// v2 records initializer calls on local bindings, single function result
// types, and edges for selector calls made directly on a call result. v3
// records the package clause and build constraint. v4 stores signatures as the
// declaration's source text, as the tree-sitter adapter does, instead of a
// lossy rendering that dropped parameters and results. v5 adds the type
// parameter list to function and type signatures.
func (a *Adapter) Profile() cgparser.Profile {
	return cgparser.Profile{ID: "go-ast:go:v5", EmitsCallEdges: true}
}
