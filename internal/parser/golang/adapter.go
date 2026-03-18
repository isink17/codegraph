package golang

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/texttoken"
)

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

	contextStack := []*graph.Symbol{}
	enterFuncStack := []bool{}
	symbolByKey := map[string]*graph.Symbol{}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := buildFuncSymbol(fset, pkgName, d, commentMap[d])
			pf.Symbols = append(pf.Symbols, sym)
			symbolByKey[sym.StableKey] = &pf.Symbols[len(pf.Symbols)-1]
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					sym := buildTypeSymbol(fset, pkgName, s, commentMap[s])
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
			src := contextStack[len(contextStack)-1]
			name := callName(node.Fun, importAliases)
			if name == "" {
				return true
			}
			pos := fset.Position(node.Lparen)
			pf.Edges = append(pf.Edges, graph.Edge{
				SrcSymbolID: 0,
				DstName:     name,
				Kind:        "calls",
				Evidence:    renderNode(content, fset, node.Fun),
				Line:        pos.Line,
			})
			pf.References = append(pf.References, graph.Reference{
				Kind:          "call",
				Name:          name,
				QualifiedName: name,
				Range:         toRange(fset, node.Pos(), node.End()),
			})
			_ = src
		}
		return true
	})

	linkTests(pkgName, &pf)
	return pf, nil
}

func buildFuncSymbol(fset *token.FileSet, pkg string, fn *ast.FuncDecl, doc string) graph.Symbol {
	name := fn.Name.Name
	qualified := pkg + "." + name
	container := pkg
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv := renderExpr(fn.Recv.List[0].Type)
		container = recv
		qualified = pkg + "." + recv + "." + name
	}
	return graph.Symbol{
		Language:      "go",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: container,
		Signature:     renderFuncSignature(fn),
		Visibility:    visibility(name),
		Range:         toRange(fset, fn.Pos(), fn.End()),
		DocSummary:    doc,
		StableKey:     stableFuncKey(pkg, fn),
	}
}

func buildTypeSymbol(fset *token.FileSet, pkg string, ts *ast.TypeSpec, doc string) graph.Symbol {
	name := ts.Name.Name
	return graph.Symbol{
		Language:      "go",
		Kind:          "type",
		Name:          name,
		QualifiedName: pkg + "." + name,
		ContainerName: pkg,
		Signature:     renderExpr(ts.Type),
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
		return "func:" + pkg + ":" + renderExpr(fn.Recv.List[0].Type) + ":" + fn.Name.Name
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

func callName(expr ast.Expr, imports map[string]string) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		left := renderExpr(e.X)
		if importPath, ok := imports[left]; ok {
			return importPath + "." + e.Sel.Name
		}
		return left + "." + e.Sel.Name
	default:
		return renderExpr(expr)
	}
}

func renderExpr(expr ast.Expr) string {
	var buf bytes.Buffer
	_ = formatNode(&buf, expr)
	return buf.String()
}

func renderFuncSignature(fn *ast.FuncDecl) string {
	var buf bytes.Buffer
	buf.WriteString("func ")
	if fn.Recv != nil {
		buf.WriteString("(")
		_ = formatNode(&buf, fn.Recv)
		buf.WriteString(") ")
	}
	buf.WriteString(fn.Name.Name)
	_ = formatNode(&buf, fn.Type)
	return buf.String()
}

func renderNode(content []byte, fset *token.FileSet, node ast.Node) string {
	start := fset.Position(node.Pos()).Offset
	end := fset.Position(node.End()).Offset
	if start < 0 || end < 0 || start >= len(content) || end > len(content) || start >= end {
		return ""
	}
	return string(content[start:end])
}

func formatNode(buf *bytes.Buffer, node any) error {
	switch n := node.(type) {
	case ast.Expr:
		return printer(buf, n)
	case *ast.FieldList:
		if n == nil {
			return nil
		}
		buf.WriteByte('(')
		for i, field := range n.List {
			if i > 0 {
				buf.WriteString(", ")
			}
			for j, name := range field.Names {
				if j > 0 {
					buf.WriteString(", ")
				}
				buf.WriteString(name.Name)
			}
			if len(field.Names) > 0 {
				buf.WriteByte(' ')
			}
			if field.Type != nil {
				_ = printer(buf, field.Type)
			}
		}
		buf.WriteByte(')')
		return nil
	case *ast.FuncType:
		if n == nil {
			return nil
		}
		_ = formatNode(buf, n.Params)
		if n.Results != nil && len(n.Results.List) > 0 {
			buf.WriteByte(' ')
			if len(n.Results.List) == 1 && len(n.Results.List[0].Names) == 0 {
				_ = printer(buf, n.Results.List[0].Type)
			} else {
				_ = formatNode(buf, n.Results)
			}
		}
		return nil
	default:
		return nil
	}
}

func printer(buf *bytes.Buffer, expr ast.Expr) error {
	switch e := expr.(type) {
	case *ast.Ident:
		buf.WriteString(e.Name)
	case *ast.SelectorExpr:
		_ = printer(buf, e.X)
		buf.WriteByte('.')
		buf.WriteString(e.Sel.Name)
	case *ast.StarExpr:
		buf.WriteByte('*')
		_ = printer(buf, e.X)
	case *ast.ArrayType:
		buf.WriteString("[]")
		_ = printer(buf, e.Elt)
	case *ast.MapType:
		buf.WriteString("map[")
		_ = printer(buf, e.Key)
		buf.WriteByte(']')
		_ = printer(buf, e.Value)
	case *ast.InterfaceType:
		buf.WriteString("interface{}")
	case *ast.StructType:
		buf.WriteString("struct")
	case *ast.IndexExpr:
		_ = printer(buf, e.X)
		buf.WriteByte('[')
		_ = printer(buf, e.Index)
		buf.WriteByte(']')
	default:
		buf.WriteString(fmt.Sprintf("%T", expr))
	}
	return nil
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
			TargetName:      pkg + "." + target,
			Reason:          "test_name_match",
			Score:           0.8,
			TestSymbolKey:   sym.StableKey,
			TargetStableKey: "func:" + pkg + "::" + target,
		})
	}
}
