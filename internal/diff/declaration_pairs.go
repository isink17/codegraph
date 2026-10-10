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
// means. Version 1 therefore pairs only renames inside one directory: for Go
// the directory is the package's import path, and for the JVM languages the
// compilation scope of a directory is not recorded, while languages that
// derive a module from the path already show it as a different stable key.
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
	refuseSemanticsIncompatible = "semantics_incompatible"
	refuseFileNotParsed         = "file_not_parsed"
	refuseParserSemanticsDiffer = "parser_semantics_differ"
	refuseModuleIdentityChanged = "module_identity_changed"
	refuseBuildRoleChanged      = "build_role_changed"
	refuseDeclarationNotUnique  = "declaration_not_unique"
	refuseDeclarationUnmatched  = "declaration_unmatched"
)

func declarationPairs(renames FileRenames, base, head store.SemanticGraph, semanticOK bool, incomplete map[string]bool) DeclarationPairs {
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
		case !bok || !hok || incomplete[r.From] || incomplete[r.To]:
			refuse(refuseFileNotParsed)
			continue
		case bf.Language != hf.Language || bf.ParserProfile != hf.ParserProfile ||
			bf.ParserSemanticEpoch != hf.ParserSemanticEpoch || bf.ParserCallEdges != hf.ParserCallEdges:
			refuse(refuseParserSemanticsDiffer)
			continue
		case path.Dir(r.From) != path.Dir(r.To):
			refuse(refuseModuleIdentityChanged)
			continue
		case bf.Language == "go" && goBuildRole(r.From) != goBuildRole(r.To):
			refuse(refuseBuildRoleChanged)
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
	return d
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

// goBuildRole is the part of a Go file name the toolchain reads: the _test
// suffix and any GOOS/GOARCH suffix. Two names with different roles can belong
// to different compilations of the same directory. go/build decides which
// elements are constraints: under a GOOS and GOARCH nothing names, a name
// ending in a known one does not match.
func goBuildRole(p string) string {
	name := strings.TrimSuffix(path.Base(p), ".go")
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
