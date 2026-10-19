package diff

import (
	"go/build"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/store"
)

// DeclarationPairs annotates declarations of an exact file rename that are
// provably the same declaration on both sides. Like FileRenames it replaces
// nothing: the paired declarations still appear as removed and added, and
// risk still cites their dependents.
//
// Continuity is asserted only when the move cannot change what a declaration
// means. Only Go files are paired: inside one directory, where the directory
// is the package's import path and only the file name's build role can change
// membership, and across directories only when the indexed module evidence
// gives both files the same import path, package clause, test role and build
// constraint (a module root that moved with its packages). A cross-directory
// move whose content changed is listed as refused, never paired. Every other
// language is refused: callers reach Lua,
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
)

func declarationPairs(renames FileRenames, base, head store.SemanticGraph, semanticOK bool, incomplete map[string]bool, baseTree, headTree []SourceFile) DeclarationPairs {
	d := DeclarationPairs{Pairs: []DeclarationPair{}, Refused: []RefusedPairing{}}
	if renames.Limitation != "" {
		d.Limitation = "declarations not paired because file renames were not paired"
		return d
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
			if reason := crossDirectoryRefusal(base.GoModules, head.GoModules, bf, hf, baseTree, headTree); reason != "" {
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
		d.Refused = append(d.Refused, changedContentMoves(renames, base, head, incomplete, baseTree, headTree)...)
	}
	return d
}

// crossDirectoryRefusal is why two Go files in different directories cannot
// be proven to belong to one package, or "" when the indexed evidence gives
// them the same import path, package clause, test role and build constraint.
// An import path names a directory, so that holds only when the module root
// moved with the package, still declares the same module path, and its go.mod
// bytes are unchanged (as a same-directory rename requires).
func crossDirectoryRefusal(baseModules, headModules *store.SemanticGoModules, bf, hf store.SemanticFile, baseTree, headTree []SourceFile) string {
	switch {
	case goIgnoredDirectory(bf.Path) || goIgnoredDirectory(hf.Path):
		return refusePackageIgnored
	case goBuildRole(bf.Path) != goBuildRole(hf.Path):
		return refuseBuildRoleChanged
	case baseModules == nil || headModules == nil || bf.Go == nil || hf.Go == nil:
		return refuseModuleEvidenceMissing
	}
	if reason := repoModuleRefusal(baseModules, headModules); reason != "" {
		return reason
	}
	b, h := bf.Go, hf.Go
	switch {
	case b.ModuleState != store.GoModuleComplete || h.ModuleState != store.GoModuleComplete:
		return refuseModuleEvidenceMissing
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

// repoModuleRefusal refuses repositories whose import paths may not map to
// their directories: a go.work can join modules, and a replace by a
// directory can substitute one module for another.
func repoModuleRefusal(base, head *store.SemanticGoModules) string {
	for _, m := range []*store.SemanticGoModules{base, head} {
		if len(m.Workspaces) > 0 {
			return refuseWorkspaceUncertain
		}
	}
	for _, m := range []*store.SemanticGoModules{base, head} {
		for _, mod := range m.Modules {
			if mod.LocalReplace {
				return refuseModuleReplaceInRepo
			}
		}
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
// bytes. Their declarations are not paired: nothing proves a declaration's
// content survived the edit.
func changedContentMoves(renames FileRenames, base, head store.SemanticGraph, incomplete map[string]bool, baseTree, headTree []SourceFile) []RefusedPairing {
	if base.GoModules == nil || head.GoModules == nil || repoModuleRefusal(base.GoModules, head.GoModules) != "" || baseTree == nil || headTree == nil {
		return nil
	}
	renamed := map[string]bool{}
	for _, r := range renames.Exact {
		renamed[r.From], renamed[r.To] = true, true
	}
	key := func(f store.SemanticFile, modules *store.SemanticGoModules) string {
		g := f.Go
		if f.Language != "go" || g == nil || g.ModuleState != store.GoModuleComplete || moduleRoots(modules, g.ModulePath) != 1 ||
			incomplete[f.Path] || renamed[f.Path] || goIgnoredDirectory(f.Path) {
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
		if k := key(f, base.GoModules); k != "" && !headIn[f.Path] {
			removed[k] = append(removed[k], f)
		}
	}
	for _, f := range head.Files {
		if k := key(f, head.GoModules); k != "" && !baseIn[f.Path] {
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
