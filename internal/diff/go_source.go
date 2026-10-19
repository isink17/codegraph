package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/store"
	"golang.org/x/mod/modfile"
)

// goSource is what a Go file's bytes say outside and inside its top-level
// declarations. context holds everything a declaration can depend on that is
// not part of its own bytes: the package clause, the imports and their
// comments (a cgo preamble), directives that name their target, and
// directives that precede no declaration.
type goSource struct {
	context string
	decls   []goTopDecl
}

type goTopDecl struct {
	startLine, endLine int
	hash               string
	// initializes is set for a var declaration with initializer values:
	// package initialization runs those in file order.
	initializes bool
}

func initializesVars(d ast.Decl) bool {
	g, ok := d.(*ast.GenDecl)
	if !ok || g.Tok != token.VAR {
		return false
	}
	for _, s := range g.Specs {
		if v, ok := s.(*ast.ValueSpec); ok && len(v.Values) > 0 {
			return true
		}
	}
	return false
}

// parseGoSource reads a file only when the bytes are the ones the index
// parsed (contentHash) and the whole file parses.
func parseGoSource(content []byte, contentHash string) (goSource, bool) {
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != contentHash {
		return goSource{}, false
	}
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, "", content, goparser.ParseComments|goparser.SkipObjectResolution)
	if err != nil || f.Name == nil {
		return goSource{}, false
	}
	tf := fset.File(f.Pos())
	off := func(p token.Pos) int { return tf.Offset(p) }
	ctx := []string{"package " + f.Name.Name}
	var imports []string
	for _, spec := range f.Imports {
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports = append(imports, name+" "+spec.Path.Value)
	}
	sort.Strings(imports)
	ctx = append(ctx, imports...)
	// Every comment of an import declaration is context: the comment on a
	// "C" import, in or outside a group, is the cgo preamble.
	isImport := make([]bool, len(f.Decls))
	var importRanges [][2]token.Pos
	for i, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			isImport[i] = true
			from := g.Pos()
			if g.Doc != nil {
				from = g.Doc.Pos()
			}
			importRanges = append(importRanges, [2]token.Pos{from, g.End()})
		}
	}
	inImport := func(p token.Pos) bool {
		for _, r := range importRanges {
			if p >= r[0] && p < r[1] {
				return true
			}
		}
		return false
	}
	// A directive applies to the declaration after it, so it is hashed with
	// that declaration; one inside a declaration is part of its bytes. A
	// directive that names its target (linkname, cgo) applies anywhere.
	attached := make([][]string, len(f.Decls))
	for _, group := range f.Comments {
		for _, c := range group.List {
			switch {
			case c.Pos() < f.Package:
				ctx = append(ctx, "header "+c.Text)
				continue
			case inImport(c.Pos()):
				ctx = append(ctx, "import "+c.Text)
				continue
			case !goDirective(c.Text):
				continue
			case strings.HasPrefix(c.Text, "//go:linkname") || strings.HasPrefix(c.Text, "//go:cgo_"):
				ctx = append(ctx, "named "+c.Text)
				continue
			}
			i := sort.Search(len(f.Decls), func(i int) bool { return f.Decls[i].End() > c.Pos() })
			switch {
			case i == len(f.Decls) || isImport[i]:
				ctx = append(ctx, "unattached "+c.Text)
			case f.Decls[i].Pos() > c.Pos():
				attached[i] = append(attached[i], c.Text)
			}
		}
	}
	out := goSource{context: strings.Join(ctx, "\n")}
	for i, d := range f.Decls {
		h := sha256.New()
		for _, dir := range attached[i] {
			h.Write([]byte(dir + "\n"))
		}
		h.Write([]byte{0})
		h.Write(content[off(d.Pos()):off(d.End())])
		out.decls = append(out.decls, goTopDecl{startLine: fset.Position(d.Pos()).Line, endLine: fset.Position(d.End()).Line, hash: hex.EncodeToString(h.Sum(nil)), initializes: initializesVars(d)})
	}
	return out, true
}

// goDirective reports a comment the toolchain reads as an instruction rather
// than prose; build constraints are part of the file header.
func goDirective(text string) bool {
	for _, p := range []string{"//go:", "//line ", "/*line ", "//export ", "//extern "} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// declarationHash is the hash of the one top-level declaration whose lines
// hold d, when no other top-level declaration shares a line with it.
func (s goSource) declarationHash(d store.SemanticDeclaration) (goTopDecl, bool) {
	match := -1
	for i, t := range s.decls {
		if t.endLine < d.StartLine || t.startLine > d.EndLine {
			continue
		}
		if match >= 0 || t.startLine > d.StartLine || t.endLine < d.EndLine {
			return goTopDecl{}, false
		}
		match = i
	}
	if match < 0 {
		return goTopDecl{}, false
	}
	t := s.decls[match]
	if match > 0 && s.decls[match-1].endLine >= t.startLine || match+1 < len(s.decls) && s.decls[match+1].startLine <= t.endLine {
		return goTopDecl{}, false
	}
	return t, true
}

// goModuleScope says which modules of one revision a go.work or a replace by
// a directory can make mean something else. A go.work joins only the modules
// it uses; a directory replace changes the build of the module declaring it
// and makes the target directory's module stand in for another path.
type goModuleScope struct {
	members, replaced map[string]bool
	workspaceErr      bool
	replaceErr        bool
}

func newGoModuleScope(m *store.SemanticGoModules, tree []SourceFile, source func(string) ([]byte, bool)) goModuleScope {
	s := goModuleScope{members: map[string]bool{}, replaced: map[string]bool{}}
	read := func(p string) ([]byte, bool) {
		h := treeHash(tree, p)
		if source == nil || h == "" {
			return nil, false
		}
		b, ok := source(p)
		if !ok {
			return nil, false
		}
		sum := sha256.Sum256(b)
		return b, hex.EncodeToString(sum[:]) == h
	}
	target := func(root, dir string) (string, bool) {
		if filepath.IsAbs(dir) || path.IsAbs(dir) || strings.Contains(dir, `\`) {
			return "", false
		}
		return path.Clean(path.Join(root, dir)), true
	}
	for _, w := range m.Workspaces {
		data, ok := read(path.Join(w, "go.work"))
		var wf *modfile.WorkFile
		var err error
		if ok {
			wf, err = modfile.ParseWork("go.work", data, nil)
		}
		if !ok || err != nil {
			s.workspaceErr = true
			continue
		}
		for _, u := range wf.Use {
			t, ok := target(w, u.Path)
			if !ok {
				s.workspaceErr = true
				continue
			}
			s.members[t] = true
		}
		for _, r := range wf.Replace {
			if r.New.Version == "" {
				if t, ok := target(w, r.New.Path); ok {
					s.replaced[t] = true
				} else {
					s.workspaceErr = true
				}
			}
		}
	}
	for _, mod := range m.Modules {
		if !mod.LocalReplace {
			continue
		}
		data, ok := read(path.Join(mod.Root, "go.mod"))
		var mf *modfile.File
		var err error
		if ok {
			mf, err = modfile.Parse("go.mod", data, nil)
		}
		if !ok || err != nil {
			s.replaceErr = true
			continue
		}
		s.replaced[mod.Root] = true
		for _, r := range mf.Replace {
			if r.New.Version == "" {
				if t, ok := target(mod.Root, r.New.Path); ok {
					s.replaced[t] = true
				} else {
					s.replaceErr = true
				}
			}
		}
	}
	return s
}

// refusal is why the module rooted at root may not keep its import identity,
// or "".
func (s goModuleScope) refusal(root string) string {
	switch {
	case s.workspaceErr || s.members[root]:
		return refuseWorkspaceUncertain
	case s.replaceErr || s.replaced[root]:
		return refuseModuleReplaceInRepo
	}
	return ""
}

// memoSource caches a revision's file reads for one comparison.
func memoSource(source func(string) ([]byte, bool)) func(string) ([]byte, bool) {
	if source == nil {
		return nil
	}
	type entry struct {
		b  []byte
		ok bool
	}
	cache := map[string]entry{}
	return func(p string) ([]byte, bool) {
		if e, hit := cache[p]; hit {
			return e.b, e.ok
		}
		b, ok := source(p)
		cache[p] = entry{b, ok}
		return b, ok
	}
}
