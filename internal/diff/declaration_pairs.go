package diff

import (
	"go/build"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/store"
)

// DeclarationPairs annotates declarations of a moved file that are provably
// the same declaration on both sides. Like FileRenames it replaces nothing:
// the paired declarations still appear as removed and added.
//
// Continuity is asserted only when the move cannot change what a declaration
// means. Only Go files are paired. An exact rename pairs inside one directory,
// where the directory is the package's import path and only the file name's
// build role can change membership, and across directories only when the
// indexed module evidence gives both files the same module path, import path,
// package clause, test role and build constraint and the module's go.mod bytes
// are unchanged (a module root that moved with its packages). A file removed
// and another added under the same package identity pair a declaration only
// when its own bytes, the directives before it and the file's imports are
// equal (see editedMoves). Every other language is refused: callers reach Lua,
// C/C++, Ruby and PHP declarations through the file path (require, include)
// and Kotlin top-level functions through a class named after the file, while
// their stable keys may not carry the path.
type DeclarationPairs struct {
	Pairs   []DeclarationPair `json:"pairs"`
	Refused []RefusedPairing  `json:"refused"`
	// Limitation is set when no file rename was examined.
	Limitation string `json:"limitation,omitempty"`
}

type DeclarationPair struct {
	FromPath string                    `json:"from_path"`
	ToPath   string                    `json:"to_path"`
	Before   store.SemanticDeclaration `json:"before"`
	After    store.SemanticDeclaration `json:"after"`
}

// RefusedPairing names a file rename, or one declaration of it, that was not
// paired and why. Declaration is the base-side declaration when the refusal is
// per declaration.
type RefusedPairing struct {
	FromPath    string                     `json:"from_path"`
	ToPath      string                     `json:"to_path"`
	Reason      string                     `json:"reason"`
	Declaration *store.SemanticDeclaration `json:"declaration,omitempty"`
}

const (
	refuseLanguageNotProven     = "language_not_proven"
	refuseSemanticsIncompatible = "semantics_incompatible"
	refuseFileNotParsed         = "file_not_parsed"
	refuseParserSemanticsDiffer = "parser_semantics_differ"
	refuseModuleIdentityChanged = "module_identity_changed"
	refuseBuildRoleChanged      = "build_role_changed"
	refuseDeclarationNotUnique  = "declaration_not_unique"
	refuseDeclarationUnmatched  = "declaration_unmatched"
	refuseModuleRootChanged     = "module_root_changed"
	refuseModuleEvidenceMissing = "module_evidence_unavailable"
	refuseWorkspaceUncertain    = "workspace_uncertain"
	refuseModuleReplaceInRepo   = "module_replace_in_repo"
	refuseModulePathNotUnique   = "module_path_not_unique"
	refusePackageChanged        = "package_identity_changed"
	refusePackageIgnored        = "package_ignored"
	refuseContentChanged        = "content_changed"
	refuseMoveAmbiguous         = "move_ambiguous"
	refuseDeclContentChanged    = "declaration_content_changed"
	refuseDeclContextChanged    = "declaration_context_changed"
	refuseDeclContentMissing    = "declaration_content_unavailable"
)

func declarationPairs(renames FileRenames, base, head store.SemanticGraph, semanticOK bool, incomplete map[string]bool, baseTree, headTree []SourceFile, baseSrc, headSrc func(string) ([]byte, bool)) DeclarationPairs {
	d := DeclarationPairs{Pairs: []DeclarationPair{}, Refused: []RefusedPairing{}}
	if renames.Limitation != "" {
		d.Limitation = "declarations not paired because file renames were not paired"
		return d
	}
	baseSrc, headSrc = memoSource(baseSrc), memoSource(headSrc)
	var baseScope, headScope goModuleScope
	if base.GoModules != nil && head.GoModules != nil {
		baseScope, headScope = newGoModuleScope(base.GoModules, baseTree, baseSrc), newGoModuleScope(head.GoModules, headTree, headSrc)
	}
	baseFiles, headFiles := filesByPath(base.Files), filesByPath(head.Files)
	baseDecls, headDecls := declarationsByPath(base.Declarations), declarationsByPath(head.Declarations)
	for _, r := range renames.Exact {
		refuse := func(reason string) {
			d.Refused = append(d.Refused, RefusedPairing{FromPath: r.From, ToPath: r.To, Reason: reason})
		}
		bf, bok := baseFiles[r.From]
		hf, hok := headFiles[r.To]
		switch {
		case !semanticOK:
			refuse(refuseSemanticsIncompatible)
			continue
		case r.Language != "go":
			refuse(refuseLanguageNotProven)
			continue
		case !bok || !hok || incomplete[r.From] || incomplete[r.To]:
			refuse(refuseFileNotParsed)
			continue
		case bf.Language != hf.Language || bf.ParserProfile != hf.ParserProfile ||
			bf.ParserSemanticEpoch != hf.ParserSemanticEpoch || bf.ParserCallEdges != hf.ParserCallEdges:
			refuse(refuseParserSemanticsDiffer)
			continue
		case path.Dir(r.From) != path.Dir(r.To):
			if reason := crossDirectoryRefusal(base.GoModules, head.GoModules, baseScope, headScope, bf, hf, baseTree, headTree); reason != "" {
				refuse(reason)
				continue
			}
		case goBuildRole(r.From) != goBuildRole(r.To):
			refuse(refuseBuildRoleChanged)
			continue
		case baseTree == nil || headTree == nil:
			refuse(refuseModuleEvidenceMissing)
			continue
		case goModuleEvidence(baseTree, path.Dir(r.From)) != goModuleEvidence(headTree, path.Dir(r.To)):
			refuse(refuseModuleRootChanged)
			continue
		}
		before, after := groupByShape(baseDecls[r.From]), groupByShape(headDecls[r.To])
		for _, key := range sortedKeys(before) {
			b, a := before[key], after[key]
			switch {
			case len(b) != 1 || len(a) > 1:
				for i := range b {
					d.Refused = append(d.Refused, RefusedPairing{FromPath: r.From, ToPath: r.To, Reason: refuseDeclarationNotUnique, Declaration: &b[i]})
				}
			case len(a) == 0:
				d.Refused = append(d.Refused, RefusedPairing{FromPath: r.From, ToPath: r.To, Reason: refuseDeclarationUnmatched, Declaration: &b[0]})
			default:
				d.Pairs = append(d.Pairs, DeclarationPair{FromPath: r.From, ToPath: r.To, Before: b[0], After: a[0]})
			}
		}
	}
	if semanticOK {
		d.Refused = append(d.Refused, changedContentMoves(renames, base, head, incomplete, baseTree, headTree, baseScope, headScope)...)
		pairs, refused := editedMoves(renames, base, head, incomplete, baseTree, headTree, baseScope, headScope, baseSrc, headSrc)
		d.Pairs = append(d.Pairs, pairs...)
		d.Refused = append(d.Refused, refused...)
	}
	return d
}

// crossDirectoryRefusal is why two Go files in different directories cannot
// be proven to belong to one package, or "" when the indexed evidence gives
// them the same module path, import path, package clause, test role and build
// constraint. An import path names a directory, so that holds only when the
// module root moved with the package, still declares the same module path,
// and the bytes of that module's go.mod are unchanged.
func crossDirectoryRefusal(baseModules, headModules *store.SemanticGoModules, baseScope, headScope goModuleScope, bf, hf store.SemanticFile, baseTree, headTree []SourceFile) string {
	switch {
	case goIgnoredDirectory(bf.Path) || goIgnoredDirectory(hf.Path):
		return refusePackageIgnored
	case goBuildRole(bf.Path) != goBuildRole(hf.Path):
		return refuseBuildRoleChanged
	case baseModules == nil || headModules == nil || bf.Go == nil || hf.Go == nil:
		return refuseModuleEvidenceMissing
	}
	b, h := bf.Go, hf.Go
	if b.ModuleState != store.GoModuleComplete || h.ModuleState != store.GoModuleComplete {
		return refuseModuleEvidenceMissing
	}
	if reason := baseScope.refusal(b.ModuleRoot); reason != "" {
		return reason
	}
	if reason := headScope.refusal(h.ModuleRoot); reason != "" {
		return reason
	}
	switch {
	case moduleRoots(baseModules, b.ModulePath) != 1 || moduleRoots(headModules, h.ModulePath) != 1:
		return refuseModulePathNotUnique
	case b.ModulePath != h.ModulePath || b.ImportPath != h.ImportPath:
		return refuseModuleIdentityChanged
	case baseTree == nil || headTree == nil:
		return refuseModuleEvidenceMissing
	}
	baseMod, headMod := treeHash(baseTree, path.Join(b.ModuleRoot, "go.mod")), treeHash(headTree, path.Join(h.ModuleRoot, "go.mod"))
	switch {
	case baseMod == "" || headMod == "":
		return refuseModuleEvidenceMissing
	case baseMod != headMod:
		return refuseModuleRootChanged
	case b.PackageClause != h.PackageClause || b.ExternalTest != h.ExternalTest:
		return refusePackageChanged
	case b.BuildConstraint != h.BuildConstraint:
		return refuseBuildRoleChanged
	}
	return ""
}

func treeHash(tree []SourceFile, p string) string {
	for _, f := range tree {
		if f.Path == p {
			return f.ContentHash
		}
	}
	return ""
}

func moduleRoots(m *store.SemanticGoModules, modulePath string) int {
	n := 0
	for _, mod := range m.Modules {
		if mod.Path == modulePath {
			n++
		}
	}
	return n
}

// goIgnoredDirectory reports a path the go command never builds as an
// ordinary package: under testdata, vendor, or a directory starting with _
// or a dot.
func goIgnoredDirectory(p string) bool {
	for _, el := range strings.Split(path.Dir(p), "/") {
		if el == "testdata" || el == "vendor" || strings.HasPrefix(el, "_") || (strings.HasPrefix(el, ".") && el != ".") {
			return true
		}
	}
	return false
}

// changedContentMoves lists Go files removed from one directory and added
// under another with the same file name and package identity but different
// bytes. The file is not a rename; editedMoves judges its declarations one by
// one.
func changedContentMoves(renames FileRenames, base, head store.SemanticGraph, incomplete map[string]bool, baseTree, headTree []SourceFile, baseScope, headScope goModuleScope) []RefusedPairing {
	if base.GoModules == nil || head.GoModules == nil || baseTree == nil || headTree == nil {
		return nil
	}
	renamed := map[string]bool{}
	for _, r := range renames.Exact {
		renamed[r.From], renamed[r.To] = true, true
	}
	key := func(f store.SemanticFile, modules *store.SemanticGoModules, scope goModuleScope) string {
		g := f.Go
		if f.Language != "go" || g == nil || g.ModuleState != store.GoModuleComplete || moduleRoots(modules, g.ModulePath) != 1 ||
			incomplete[f.Path] || renamed[f.Path] || goIgnoredDirectory(f.Path) || scope.refusal(g.ModuleRoot) != "" {
			return ""
		}
		return canonical([]any{g.ModulePath, g.ImportPath, g.PackageClause, g.ExternalTest, g.BuildConstraint, goBuildRole(f.Path), path.Base(f.Path)})
	}
	inTree := func(tree []SourceFile) map[string]bool {
		out := make(map[string]bool, len(tree))
		for _, f := range tree {
			out[f.Path] = true
		}
		return out
	}
	baseIn, headIn := inTree(baseTree), inTree(headTree)
	removed, added := map[string][]store.SemanticFile{}, map[string][]store.SemanticFile{}
	for _, f := range base.Files {
		if k := key(f, base.GoModules, baseScope); k != "" && !headIn[f.Path] {
			removed[k] = append(removed[k], f)
		}
	}
	for _, f := range head.Files {
		if k := key(f, head.GoModules, headScope); k != "" && !baseIn[f.Path] {
			added[k] = append(added[k], f)
		}
	}
	var out []RefusedPairing
	for k, from := range removed {
		to := added[k]
		// Equal bytes that are not an exact rename are shared with other
		// moved files (file_renames lists them as ambiguous).
		reason := refuseContentChanged
		if len(from) != 1 || len(to) != 1 || from[0].ContentHash == to[0].ContentHash {
			reason = refuseMoveAmbiguous
		}
		// ponytail: every removed/added combination is listed; a key is an
		// import path plus file name, so groups stay small.
		for _, f := range from {
			for _, t := range to {
				out = append(out, RefusedPairing{FromPath: f.Path, ToPath: t.Path, Reason: reason})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromPath != out[j].FromPath {
			return out[i].FromPath < out[j].FromPath
		}
		return out[i].ToPath < out[j].ToPath
	})
	return out
}

// declarationShape is a declaration's identity without its path. Equal shapes
// in byte-identical files of one directory under one parser are the same
// declaration; the stable key is compared as stored, never normalized.
func declarationShape(d store.SemanticDeclaration) string {
	c := d
	c.Path = ""
	return canonical(c)
}

func groupByShape(decls []store.SemanticDeclaration) map[string][]store.SemanticDeclaration {
	out := map[string][]store.SemanticDeclaration{}
	for _, d := range decls {
		k := declarationShape(d)
		out[k] = append(out[k], d)
	}
	return out
}

func sortedKeys(m map[string][]store.SemanticDeclaration) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func filesByPath(files []store.SemanticFile) map[string]store.SemanticFile {
	out := make(map[string]store.SemanticFile, len(files))
	for _, f := range files {
		out[f.Path] = f
	}
	return out
}

func declarationsByPath(decls []store.SemanticDeclaration) map[string][]store.SemanticDeclaration {
	out := map[string][]store.SemanticDeclaration{}
	for _, d := range decls {
		out[d.Path] = append(out[d.Path], d)
	}
	return out
}

// goModuleEvidence lists the path and content hash of every go.mod and
// go.work in dir and each of its parents. A directory is its package's import
// path only while the modules above it are unchanged.
// ponytail: any go.mod or go.work byte change refuses, even a require edit;
// reading the module directive would keep those pairs.
func goModuleEvidence(tree []SourceFile, dir string) string {
	ancestors := map[string]bool{}
	for d := dir; ; d = path.Dir(d) {
		ancestors[d] = true
		if d == "." || d == "/" {
			break
		}
	}
	var out []string
	for _, f := range tree {
		if b := path.Base(f.Path); (b == "go.mod" || b == "go.work") && ancestors[path.Dir(f.Path)] {
			out = append(out, f.Path+"="+f.ContentHash)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// goBuildRole is the part of a Go file name the toolchain reads: the _test
// suffix, any GOOS/GOARCH suffix, and a leading _ or . that makes the
// toolchain ignore the file. Two names with different roles can belong
// to different compilations of the same directory. go/build decides which
// elements are constraints: under a GOOS and GOARCH nothing names, a name
// ending in a known one does not match.
func goBuildRole(p string) string {
	base := path.Base(p)
	if strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
		return "ignored"
	}
	name := strings.TrimSuffix(base, ".go")
	stem := strings.TrimSuffix(name, "_test")
	role := ""
	if stem != name {
		role = "test"
	}
	ctxt := build.Context{GOOS: "codegraph", GOARCH: "codegraph", Compiler: "gc",
		OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("package p\n")), nil }}
	parts := strings.Split(stem, "_")[1:]
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	for _, part := range parts {
		if ok, err := ctxt.MatchFile(".", "x_"+part+".go"); err != nil || !ok {
			role += "|" + part
		}
	}
	return role
}

// editedMoves pairs declarations that left removed Go files for added files
// of the same package identity when nothing a declaration means can differ:
// the module keeps its import identity, the declaration's shape is unique in
// that package on both sides, and its own bytes, the directives before it and
// its file's package clause and imports are equal. A shape found on both
// sides that fails any of these is refused with the reason.
func editedMoves(renames FileRenames, base, head store.SemanticGraph, incomplete map[string]bool, baseTree, headTree []SourceFile, baseScope, headScope goModuleScope, baseSrc, headSrc func(string) ([]byte, bool)) ([]DeclarationPair, []RefusedPairing) {
	if base.GoModules == nil || head.GoModules == nil || baseTree == nil || headTree == nil {
		return nil, nil
	}
	renamed := map[string]bool{}
	for _, r := range renames.Exact {
		renamed[r.From], renamed[r.To] = true, true
	}
	type side struct {
		other     map[string]string
		decls     map[string][]store.SemanticDeclaration
		files     map[string]store.SemanticFile
		src       func(string) ([]byte, bool)
		parsed    map[string]*goSource
		keys      map[string]string
		movedKeys map[string]bool
		// shapes counts every declaration of a package identity; moved holds
		// those of removed (base) or added (head) files.
		shapes map[string]map[string]int
		moved  map[string]map[string][]store.SemanticDeclaration
		roots  map[string]string
	}
	treeMap := func(tree []SourceFile) map[string]string {
		out := make(map[string]string, len(tree))
		for _, f := range tree {
			out[f.Path] = f.ContentHash
		}
		return out
	}
	baseMap, headMap := treeMap(baseTree), treeMap(headTree)
	build := func(g store.SemanticGraph, other map[string]string, src func(string) ([]byte, bool)) *side {
		s := &side{other: other, decls: declarationsByPath(g.Declarations), files: filesByPath(g.Files), src: src,
			parsed: map[string]*goSource{}, keys: map[string]string{}, movedKeys: map[string]bool{}, shapes: map[string]map[string]int{}, moved: map[string]map[string][]store.SemanticDeclaration{}, roots: map[string]string{}}
		for _, f := range g.Files {
			gf := f.Go
			if f.Language != "go" || gf == nil || gf.ModuleState != store.GoModuleComplete || moduleRoots(g.GoModules, gf.ModulePath) != 1 ||
				incomplete[f.Path] || goIgnoredDirectory(f.Path) {
				continue
			}
			k := canonical([]any{gf.ModulePath, gf.ImportPath, gf.PackageClause, gf.ExternalTest, gf.BuildConstraint, goBuildRole(f.Path), f.ParserProfile, f.ParserSemanticEpoch, f.ParserCallEdges})
			s.keys[f.Path], s.roots[k] = k, gf.ModuleRoot
			if _, kept := other[f.Path]; !kept && !renamed[f.Path] {
				s.movedKeys[k] = true
			}
		}
		return s
	}
	b, h := build(base, headMap, baseSrc), build(head, baseMap, headSrc)
	// Only packages with a removed file in base and an added one in head can
	// hold a move; shapes are counted for those alone.
	for _, s := range []*side{b, h} {
		for p, k := range s.keys {
			if !b.movedKeys[k] || !h.movedKeys[k] {
				continue
			}
			if s.shapes[k] == nil {
				s.shapes[k], s.moved[k] = map[string]int{}, map[string][]store.SemanticDeclaration{}
			}
			_, kept := s.other[p]
			for _, d := range s.decls[p] {
				sh := movedShape(d)
				s.shapes[k][sh]++
				if !kept && !renamed[p] {
					s.moved[k][sh] = append(s.moved[k][sh], d)
				}
			}
		}
	}
	source := func(s *side, p string) *goSource {
		if gs, ok := s.parsed[p]; ok {
			return gs
		}
		var out *goSource
		if s.src != nil {
			if data, ok := s.src(p); ok {
				if gs, ok := parseGoSource(data, s.files[p].ContentHash); ok {
					out = &gs
				}
			}
		}
		s.parsed[p] = out
		return out
	}
	hash := func(s *side, d store.SemanticDeclaration) (string, string, bool) {
		gs := source(s, d.Path)
		if gs == nil {
			return "", "", false
		}
		dh, ok := gs.declarationHash(d)
		return gs.context, dh, ok
	}
	var pairs []DeclarationPair
	var refused []RefusedPairing
	for k, from := range b.moved {
		to := h.moved[k]
		if len(from) == 0 || len(to) == 0 {
			continue
		}
		bRoot, hRoot := b.roots[k], h.roots[k]
		groupReason := baseScope.refusal(bRoot)
		if groupReason == "" {
			groupReason = headScope.refusal(hRoot)
		}
		if groupReason == "" {
			switch bm, hm := baseMap[path.Join(bRoot, "go.mod")], headMap[path.Join(hRoot, "go.mod")]; {
			case bm == "" || hm == "":
				groupReason = refuseModuleEvidenceMissing
			case bm != hm:
				groupReason = refuseModuleRootChanged
			}
		}
		for sh, bs := range from {
			as := to[sh]
			if len(as) == 0 {
				continue
			}
			reason := groupReason
			if reason == "" && (b.shapes[k][sh] != 1 || h.shapes[k][sh] != 1) {
				reason = refuseDeclarationNotUnique
			}
			if reason == "" {
				bc, bh, bok := hash(b, bs[0])
				hc, hh, hok := hash(h, as[0])
				switch {
				case !bok || !hok:
					reason = refuseDeclContentMissing
				case bc != hc:
					reason = refuseDeclContextChanged
				case bh != hh:
					reason = refuseDeclContentChanged
				default:
					pairs = append(pairs, DeclarationPair{FromPath: bs[0].Path, ToPath: as[0].Path, Before: bs[0], After: as[0]})
					continue
				}
			}
			for i := range bs {
				for _, a := range as {
					refused = append(refused, RefusedPairing{FromPath: bs[i].Path, ToPath: a.Path, Reason: reason, Declaration: &bs[i]})
				}
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		return canonical([]any{pairs[i].FromPath, pairs[i].ToPath, pairs[i].Before}) < canonical([]any{pairs[j].FromPath, pairs[j].ToPath, pairs[j].Before})
	})
	sort.Slice(refused, func(i, j int) bool { return canonical(refused[i]) < canonical(refused[j]) })
	return pairs, refused
}

// movedShape is a declaration's identity without its path or position.
func movedShape(d store.SemanticDeclaration) string {
	c := d
	c.Path, c.StartLine, c.StartCol, c.EndLine, c.EndCol = "", 0, 0, 0, 0
	return canonical(c)
}
