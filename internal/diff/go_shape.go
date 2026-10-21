package diff

import (
	"encoding/json"
	"go/ast"
	goparser "go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/store"
)

// goShape is one top-level Go declaration outside function bodies: its line
// span and its tokens without comments, trailing commas or the semicolons
// before a closing bracket, so reformatting and comments do not change it.
type goShape struct {
	startLine, endLine int
	tokens             string
}

// goShapes keys every top-level declaration of a Go file except imports:
// "func Name" or "func (Recv) Name", "type Name", "const A,B" or "var A,B".
// A const spec that repeats the previous expression implicitly carries it.
func goShapes(content []byte) (map[string][]goShape, bool) {
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, "", content, goparser.SkipObjectResolution)
	if err != nil || f.Name == nil {
		return nil, false
	}
	tf := fset.File(f.Pos())
	out := map[string][]goShape{}
	add := func(key string, from, to token.Pos, prefix string) {
		s := goShape{startLine: fset.Position(from).Line, endLine: fset.Position(to).Line}
		s.tokens = prefix + goTokens(content[tf.Offset(from):tf.Offset(to)])
		out[key] = append(out[key], s)
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
			add(key, d.Pos(), end, "")
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			implicit := ""
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					add("type "+s.Name.Name, s.Pos(), s.End(), "")
				case *ast.ValueSpec:
					names := make([]string, len(s.Names))
					for i, n := range s.Names {
						names[i] = n.Name
					}
					prefix := ""
					if d.Tok == token.CONST && s.Type == nil && len(s.Values) == 0 {
						prefix = implicit
					} else if d.Tok == token.CONST {
						implicit = goTokens(content[tf.Offset(s.Names[len(s.Names)-1].End()):tf.Offset(s.End())]) + " | "
					}
					add(d.Tok.String()+" "+strings.Join(names, ","), s.Pos(), s.End(), prefix)
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

// goUnmodeledUncertainty reports Go declarations whose shape changed in a
// modified file while no declaration change of the diff falls inside them:
// struct fields, interface methods, constant values and types are no
// declarations of their own, so such a change would otherwise look like no
// change at all. Function bodies are left to the edge comparison.
func goUnmodeledUncertainty(changes []Change, base, head Revision, incomplete map[string]bool) []RiskUncertainty {
	if base.source == nil || head.source == nil {
		return nil
	}
	type span struct{ start, end int }
	modeled := map[string][2][]span{}
	for _, c := range changes {
		if !strings.HasPrefix(c.Kind, "declaration_") {
			continue
		}
		for side, raw := range []json.RawMessage{c.Before, c.After} {
			var decls []store.SemanticDeclaration
			var d store.SemanticDeclaration
			if json.Unmarshal(raw, &d) == nil && d.Path != "" {
				decls = []store.SemanticDeclaration{d}
			} else {
				_ = json.Unmarshal(raw, &decls)
			}
			for _, d := range decls {
				m := modeled[d.Path]
				m[side] = append(m[side], span{d.StartLine, d.EndLine})
				modeled[d.Path] = m
			}
		}
	}
	covered := func(path string, side int, s goShape) bool {
		for _, m := range modeled[path][side] {
			if m.start <= s.endLine && m.end >= s.startLine {
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
		read := func(r Revision, h string) (map[string][]goShape, bool) {
			b, ok := r.source(c.Identity)
			if !ok || sha256Hex(b) != h {
				return nil, false
			}
			return goShapes(b)
		}
		bs, bok := read(base, before.ContentHash)
		hs, hok := read(head, after.ContentHash)
		if !bok || !hok {
			out = append(out, RiskUncertainty{Reason: "go_declaration_shape_unavailable", Identity: c.Identity})
			continue
		}
		for key, x := range bs {
			y, ok := hs[key]
			if !ok || sameShapes(x, y) {
				continue
			}
			hidden := true
			for _, s := range x {
				hidden = hidden && !covered(c.Identity, 0, s)
			}
			for _, s := range y {
				hidden = hidden && !covered(c.Identity, 1, s)
			}
			if hidden {
				out = append(out, RiskUncertainty{Reason: "go_declaration_change_unmodeled", Identity: c.Identity, Detail: key})
			}
		}
	}
	return out
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
