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
	"strconv"
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
	// fixed is context without the import specs; imports lists them. named
	// is set when the file has a //go:linkname or //go:cgo_ directive.
	fixed   string
	imports []goImport
	named   bool
	decls   []goTopDecl
}

// goImport is one import spec: name is the explicit name ("", "_", "." or an
// identifier) and path the unquoted import path.
type goImport struct{ name, path string }

type goTopDecl struct {
	startLine, endLine int
	hash               string
	// initializes is set for a var declaration with initializer values:
	// package initialization runs those in file order.
	initializes bool
	// embeds holds the //go:embed directives before or inside it.
	embeds []string
	// idents holds every identifier spelled inside it.
	idents map[string]bool
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
	if sha256Hex(content) != contentHash {
		return goSource{}, false
	}
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, "", content, goparser.ParseComments|goparser.SkipObjectResolution)
	if err != nil || f.Name == nil {
		return goSource{}, false
	}
	tf := fset.File(f.Pos())
	off := func(p token.Pos) int { return tf.Offset(p) }
	var ctx []string
	var imports []string
	var specs []goImport
	for _, spec := range f.Imports {
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports = append(imports, name+" "+spec.Path.Value)
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return goSource{}, false
		}
		specs = append(specs, goImport{name: name, path: p})
	}
	sort.Strings(imports)
	named := false
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
	embeds := make([][]string, len(f.Decls))
	for _, group := range f.Comments {
		for _, c := range group.List {
			if goEmbed(c.Text) {
				if i := sort.Search(len(f.Decls), func(i int) bool { return f.Decls[i].End() > c.Pos() }); i < len(f.Decls) && !isImport[i] {
					embeds[i] = append(embeds[i], c.Text)
				}
			}
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
				named = true
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
	pkg := "package " + f.Name.Name
	out := goSource{
		context: strings.Join(append(append([]string{pkg}, imports...), ctx...), "\n"),
		fixed:   strings.Join(append([]string{pkg}, ctx...), "\n"),
		imports: specs,
		named:   named,
	}
	for i, d := range f.Decls {
		h := sha256.New()
		for _, dir := range attached[i] {
			h.Write([]byte(dir + "\n"))
		}
		h.Write([]byte{0})
		h.Write(content[off(d.Pos()):off(d.End())])
		idents := map[string]bool{}
		ast.Inspect(d, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				idents[id.Name] = true
			}
			return true
		})
		out.decls = append(out.decls, goTopDecl{startLine: fset.Position(d.Pos()).Line, endLine: fset.Position(d.End()).Line, hash: hex.EncodeToString(h.Sum(nil)), initializes: initializesVars(d), embeds: embeds[i], idents: idents})
	}
	return out, true
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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

func goEmbed(text string) bool {
	rest, ok := strings.CutPrefix(text, "//go:embed")
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// embedsKept reports that every //go:embed of a declaration that moved from
// fromDir to toDir names one regular file whose bytes are equal in both
// revisions. A pattern resolves against the source file's directory, so equal
// declaration bytes prove nothing about what is embedded.
// ponytail: globs, directories, quoted names and all: refuse; resolving them
// as the go command does would keep those pairs.
func embedsKept(directives []string, fromDir, toDir string, ev moveEvidence) bool {
	var patterns []string
	for _, d := range directives {
		fields := strings.Fields(strings.TrimPrefix(d, "//go:embed"))
		if len(fields) == 0 {
			return false
		}
		for _, p := range fields {
			if strings.ContainsAny(p, "\"`*?[\\") || strings.HasPrefix(p, "all:") || path.Clean(p) != p || path.IsAbs(p) || p == "." || p == ".." || strings.HasPrefix(p, "../") {
				return false
			}
			patterns = append(patterns, p)
		}
	}
	// regular is the content hash of the file p names under dir when it is a
	// readable regular blob outside any nested module, or "".
	regular := func(tree []SourceFile, src func(string) ([]byte, bool), dir, p string) string {
		file, hash := path.Join(dir, p), ""
		nested := map[string]bool{}
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			nested[path.Join(dir, d, "go.mod")] = true
		}
		for _, f := range tree {
			switch {
			case f.Path == file:
				hash = f.ContentHash
			case strings.HasPrefix(f.Path, file+"/") || nested[f.Path]:
				return ""
			}
		}
		if hash == "" || src == nil {
			return ""
		}
		if _, ok := src(file); !ok {
			return ""
		}
		return hash
	}
	for _, p := range patterns {
		b := regular(ev.baseTree, ev.baseSrc, fromDir, p)
		if b == "" || b != regular(ev.headTree, ev.headSrc, toDir, p) {
			return false
		}
	}
	return true
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
		return b, sha256Hex(b) == h
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
