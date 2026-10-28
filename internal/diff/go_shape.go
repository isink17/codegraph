package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build/constraint"
	goparser "go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/store"
)

// goShape is one top-level Go declaration outside function bodies: its name,
// line span and tokens without comments, trailing commas or the semicolons
// before a closing bracket, so reformatting and comments do not change it.
type goShape struct {
	name               string
	startLine, endLine int
	tokens             string
	body               string
	hasBody            bool
}

// goShapes keys every top-level declaration of a Go file except imports:
// "func Name" or "func (Recv) Name", "type Name", "const Name" or "var Name".
// A value is keyed per name with its own expression, so splitting or joining
// a multi-name spec compares like for like. A const that repeats the previous
// expression implicitly carries it, and one whose expression uses iota also
// carries its index in the group.
func goShapes(content []byte) (map[string][]goShape, bool) {
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, "", content, goparser.SkipObjectResolution)
	if err != nil || f.Name == nil {
		return nil, false
	}
	tf := fset.File(f.Pos())
	src := func(n ast.Node) string {
		if n == nil {
			return ""
		}
		return goTokens(content[tf.Offset(n.Pos()):tf.Offset(n.End())])
	}
	out := map[string][]goShape{}
	add := func(key, name string, from, to token.Pos, tokens, body string, hasBody bool) {
		out[key] = append(out[key], goShape{name: name, startLine: fset.Position(from).Line, endLine: fset.Position(to).Line, tokens: tokens, body: body, hasBody: hasBody})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			key, end := "func "+d.Name.Name, d.End()
			if d.Recv != nil && len(d.Recv.List) > 0 {
				key = "func (" + types.ExprString(d.Recv.List[0].Type) + ") " + d.Name.Name
			}
			if d.Body != nil {
				end = d.Body.Pos()
			}
			body := ""
			hasBody := d.Body != nil
			if hasBody {
				body = goTokens(content[tf.Offset(d.Body.Pos()):tf.Offset(d.Body.End())])
			}
			add(key, d.Name.Name, d.Pos(), end, goTokens(content[tf.Offset(d.Pos()):tf.Offset(end)]), body, hasBody)
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			var typ ast.Expr
			var values []ast.Expr
			for index, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					add("type "+s.Name.Name, s.Name.Name, s.Pos(), s.End(), src(s), "", false)
				case *ast.ValueSpec:
					if d.Tok == token.VAR || s.Type != nil || len(s.Values) > 0 {
						typ, values = s.Type, s.Values
					}
					for i, n := range s.Names {
						value := ""
						switch {
						case len(values) == len(s.Names):
							value = src(values[i])
						case len(values) > 0:
							parts := make([]string, len(values))
							for j, v := range values {
								parts[j] = src(v)
							}
							value = strings.Join(parts, " , ")
						}
						shape := src(typ) + " = " + value
						if d.Tok == token.CONST && strings.Contains(" "+value+" ", " iota ") {
							shape = "iota#" + strconv.Itoa(index) + " " + shape
						}
						add(d.Tok.String()+" "+n.Name, n.Name, s.Pos(), s.End(), shape, "", false)
					}
				}
			}
		}
	}
	for _, v := range out {
		sort.Slice(v, func(i, j int) bool { return v[i].tokens < v[j].tokens })
	}
	return out, true
}

func goTokens(src []byte) string {
	var s scanner.Scanner
	s.Init(token.NewFileSet().AddFile("", -1, len(src)), src, nil, 0)
	var toks []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.RPAREN || tok == token.RBRACE || tok == token.RBRACK {
			for len(toks) > 0 && (toks[len(toks)-1] == "," || toks[len(toks)-1] == ";") {
				toks = toks[:len(toks)-1]
			}
		}
		switch {
		case tok == token.SEMICOLON:
			lit = ";"
		case !tok.IsLiteral():
			lit = tok.String()
		}
		toks = append(toks, lit)
	}
	for len(toks) > 0 && toks[len(toks)-1] == ";" {
		toks = toks[:len(toks)-1]
	}
	return strings.Join(toks, " ")
}

// goUnmodeledUncertainty reports Go source changes the graph does not model.
// Declaration shape changes cover struct fields, interface methods and
// constants; function bodies need their own uncertainty because edges do not
// describe type checking or execution inside a body.
func goUnmodeledUncertainty(changes []Change, base, head Revision, incomplete map[string]bool) []RiskUncertainty {
	var modeled []store.SemanticDeclaration
	for _, c := range changes {
		if !strings.HasPrefix(c.Kind, "declaration_") || c.Before == nil {
			continue
		}
		var d store.SemanticDeclaration
		if json.Unmarshal(c.Before, &d) == nil && d.Path != "" {
			modeled = append(modeled, d)
		} else {
			var ds []store.SemanticDeclaration
			_ = json.Unmarshal(c.Before, &ds)
			modeled = append(modeled, ds...)
		}
	}
	covered := func(path string, s goShape) bool {
		for _, m := range modeled {
			if m.Path == path && m.Name == s.name && m.StartLine <= s.endLine && m.EndLine >= s.startLine {
				return true
			}
		}
		return false
	}
	var out []RiskUncertainty
	for _, c := range changes {
		var before, after SourceFile
		if c.Kind != "file_modified" || json.Unmarshal(c.Before, &before) != nil || json.Unmarshal(c.After, &after) != nil ||
			before.Language != "go" || after.Language != "go" || incomplete[c.Identity] {
			continue
		}
		read := func(r Revision, h string) ([]byte, map[string][]goShape, bool) {
			if r.source == nil {
				return nil, nil, false
			}
			b, ok := r.source(c.Identity)
			if !ok || sha256Hex(b) != h {
				return nil, nil, false
			}
			shapes, ok := goShapes(b)
			return b, shapes, ok
		}
		bb, bs, bok := read(base, before.ContentHash)
		hb, hs, hok := read(head, after.ContentHash)
		if !bok || !hok {
			out = append(out, RiskUncertainty{Reason: "go_declaration_shape_unavailable", Identity: c.Identity})
			continue
		}
		if x, y := goBuildConstraint(bb), goBuildConstraint(hb); x != y {
			out = append(out, RiskUncertainty{Reason: "go_build_constraint_change_unmodeled", Identity: c.Identity, Detail: x + " -> " + y})
		}
		if !sameGoImports(bb, hb) {
			out = append(out, RiskUncertainty{Reason: "go_import_change_unmodeled", Identity: c.Identity})
		}
		for key, x := range bs {
			if !strings.HasPrefix(key, "func ") || len(x) != 1 {
				continue
			}
			y := hs[key]
			if len(y) == 1 && (x[0].hasBody != y[0].hasBody || x[0].body != y[0].body) {
				out = append(out, RiskUncertainty{Reason: "go_function_body_change_unmodeled", Identity: c.Identity, Detail: key})
			}
		}
		for key, x := range bs {
			y, ok := hs[key]
			if !ok || sameShapes(x, y) {
				continue
			}
			hidden := true
			for _, s := range x {
				hidden = hidden && !covered(c.Identity, s)
			}
			if hidden {
				out = append(out, RiskUncertainty{Reason: "go_declaration_change_unmodeled", Identity: c.Identity, Detail: key})
			}
		}
	}
	return out
}

func goFilenameBuildRoleUncertainty(renames FileRenames) []RiskUncertainty {
	var out []RiskUncertainty
	for _, r := range renames.Exact {
		if r.Language != "go" {
			continue
		}
		before, after := goBuildRole(r.From), goBuildRole(r.To)
		if before != after {
			out = append(out, RiskUncertainty{
				Reason:   "go_filename_build_role_change_unmodeled",
				Identity: r.From,
				Detail:   fmt.Sprintf("%s -> %s: %q -> %q", r.From, r.To, before, after),
			})
		}
	}
	return out
}

func sameGoImports(a, b []byte) bool {
	imports := func(src []byte) []string {
		f, _ := goparser.ParseFile(token.NewFileSet(), "", src, goparser.ImportsOnly)
		if f == nil {
			return nil
		}
		out := make([]string, 0, len(f.Imports))
		for _, spec := range f.Imports {
			name := ""
			if spec.Name != nil {
				name = spec.Name.Name
			}
			path, _ := strconv.Unquote(spec.Path.Value)
			out = append(out, name+" "+path)
		}
		sort.Strings(out)
		return out
	}
	return strings.Join(imports(a), "\n") == strings.Join(imports(b), "\n")
}

func sameShapes(a, b []goShape) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].tokens != b[i].tokens {
			return false
		}
	}
	return true
}

// goBuildConstraint is the file's build constraint as the go command reads
// it, in canonical form; "(none)" without one, which no constraint expression
// can spell. The graph indexes a file whatever its constraint says, so a
// changed constraint can add or drop the file from a build while every
// declaration and edge stays the same. The header rules follow go/build
// (parseFileHeader, shouldBuild): a //go:build line anywhere in the leading
// comments controls, two of them make the file invalid, and without one only
// the // +build lines followed by a blank line before the first non-comment
// line count, ANDed, skipping lines that do not parse.
func goBuildConstraint(src []byte) string {
	end, inSlashStar, ended := 0, false, false
	var goBuild string
	seen := 0
	p := src
lines:
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, p = line[:i], p[i+1:]
		} else {
			p = p[len(p):]
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 && !ended {
			end = len(src) - len(p)
			continue
		}
		if !bytes.HasPrefix(line, []byte("//")) {
			ended = true
		}
		if !inSlashStar && constraint.IsGoBuild(string(line)) {
			seen++
			goBuild = string(line)
		}
		for len(line) > 0 {
			if inSlashStar {
				i := bytes.Index(line, []byte("*/"))
				if i < 0 {
					continue lines
				}
				inSlashStar = false
				line = bytes.TrimSpace(line[i+2:])
				continue
			}
			if bytes.HasPrefix(line, []byte("//")) {
				continue lines
			}
			if bytes.HasPrefix(line, []byte("/*")) {
				inSlashStar = true
				line = bytes.TrimSpace(line[2:])
				continue
			}
			break lines
		}
	}
	switch {
	case seen > 1:
		return "invalid: multiple //go:build lines"
	case seen == 1:
		x, err := constraint.Parse(goBuild)
		if err != nil {
			return "invalid: " + goBuild
		}
		return x.String()
	}
	var x constraint.Expr
	for _, line := range bytes.Split(src[:end], []byte("\n")) {
		text := string(bytes.TrimSpace(line))
		if !constraint.IsPlusBuild(text) {
			continue
		}
		y, err := constraint.Parse(text)
		if err != nil {
			continue
		}
		if x == nil {
			x = y
		} else {
			x = &constraint.AndExpr{X: x, Y: y}
		}
	}
	if x == nil {
		return "(none)"
	}
	return x.String()
}
