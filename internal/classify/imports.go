package classify

import "strings"

// Per-language linkage rules (P8).
//
// Each rule answers one question: does the unresolved `dst_name` provably belong
// to one of *this file's* imports, and is that import provably standard-library
// or provably a non-project dependency?
//
// Every rule obeys three constraints:
//
//  1. Co-occurrence is not linkage. An import must be tied to the name by the
//     language's own rules -- Go's parser-rewritten import-path prefix, a Java
//     import whose last segment IS the type named at the call site, and so on.
//     "The file imports something external, therefore this call is external" is
//     the reasoning this package exists to refuse.
//  2. Never claim `project`. An import proven to be project-local (a relative
//     TypeScript specifier, a Python `from .mod import`) yields `unknown`, not
//     `project`: `project` means a bound `dst_symbol_id`, and these edges have
//     none. Saying `project` here would assert a symbol we failed to find.
//  3. Never let row order decide. `os` and `os.path` can both be imported, so
//     prefix rules take the longest match; the last-segment rules gather every
//     matching import and return `unknown` when they disagree, rather than
//     answering from whichever `file_imports` row came first. Two imports
//     binding one name is the P3 situation, and the P3 answer is to refuse.
//
// A rule receives the already-normalized import list (NormalizeImports) and must
// not mutate it.
type importRule func(dstName string, imports []string) string

var importRules = map[string]importRule{
	"go":         classifyGo,
	"python":     classifyPython,
	"java":       classifyJava,
	"rust":       classifyRust,
	"typescript": classifyTypeScript,
	"cpp":        classifyCpp,
}

// classifyCpp trusts only an explicit top-level std namespace in the persisted
// destination. Header delimiters and using-directives are not retained as
// enough evidence to distinguish a project declaration from the library.
func classifyCpp(dstName string, _ []string) string {
	if firstSegment(strings.TrimPrefix(dstName, "::"), "::") == "std" {
		return Stdlib
	}
	return Unknown
}

// classifyGo relies on the strongest linkage evidence in the repository: both Go
// adapters rewrite a call's package qualifier into the *full import path* when
// the qualifier matches an import of that file (internal/parser/golang/adapter.go
// callName, internal/parser/treesitter/go_adapter.go goCallName). So
// `fmt.Println()` is persisted as dst_name `fmt.Println`, and a call into a
// dependency as `github.com/gin-gonic/gin.New`. The import path is inside the
// name; no aliasing guesswork is needed.
//
// Stdlib: the matched import path's first segment contains no dot AND is a known
// standard-library root. Go reserves dotless first segments for the standard
// library, but that alone would also accept a locally-replaced module named
// `myapp`, so membership in goStdlibRoots is required as well.
//
// External is NOT claimed for Go. A dependency path (`github.com/x/y`) and this
// repository's own module path are the same shape, and CodeGraph never reads
// go.mod, so `github.com/isink17/codegraph/internal/store.Get` is indistinguishable
// from a third-party call. Unknown is the honest answer.
func classifyGo(dstName string, imports []string) string {
	matched, ok := longestImportPrefix(dstName, imports, ".")
	if !ok {
		return Unknown
	}
	root, _, _ := strings.Cut(matched, "/")
	if strings.Contains(root, ".") {
		// Domain-form path: a dependency or this very module. Not decidable.
		return Unknown
	}
	if goStdlibRoots[root] {
		return Stdlib
	}
	return Unknown
}

// classifyPython links on the module prefix the tree-sitter adapter preserves:
// `import os` + `os.getcwd()` is persisted as dst_name `os.getcwd`, which the
// import `os` prefixes exactly.
//
// Not provable, therefore unknown:
//   - `from os.path import join` + `join()`. `file_imports` stores only the
//     module (`os.path`); the imported symbol names are dropped at the parser
//     boundary, so nothing ties `join` to it.
//   - Anything under the non-cgo regex adapter, which drops the receiver from
//     dst_name entirely (`os.getcwd()` -> `getcwd`).
//   - Relative imports (`from .mod import x`), which NormalizeImports discards:
//     project-local, and `project` is not ours to claim.
//
// External is not claimed: a third-party distribution and a first-party package
// are both plain dotted module paths, and no project package list is indexed.
func classifyPython(dstName string, imports []string) string {
	matched, ok := longestImportPrefix(dstName, imports, ".")
	if !ok {
		return Unknown
	}
	root, _, _ := strings.Cut(matched, ".")
	if pythonStdlibModules[root] {
		return Stdlib
	}
	return Unknown
}

// classifyJava uses the one language here whose import paths end in the very
// identifier the call site writes: `import java.util.List;` is what makes `List`
// usable, so `List.of()` (persisted dst_name `List.of`) links to `java.util.List`
// by last segment. A fully written-out `java.util.Objects.requireNonNull()` is
// covered by the prefix check without needing an import at all.
//
// Stdlib is claimed for `java.*` and for the `javax.*` subtrees the JDK actually
// ships (isJDKNamespace) -- not for `javax.*` as a whole, most of which is
// third-party or Jakarta.
//
// External is not claimed: `com.google.common` and a first-party `com.acme`
// are the same shape, and no group/artifact metadata is indexed.
func classifyJava(dstName string, imports []string) string {
	if isJDKNamespace(dstName) {
		return Stdlib
	}
	head, _, _ := strings.Cut(dstName, ".")
	if head == "" {
		return Unknown
	}
	return verdictOfMatchingImports(imports, func(imp string) (string, bool) {
		if lastSegment(imp, ".") != head {
			return "", false
		}
		if isJDKNamespace(imp) {
			return Stdlib, true
		}
		return Unknown, true
	})
}

// isJDKNamespace reports whether a Java package path is part of the JDK itself.
//
// `java.*` is the whole story for the primary namespace. `javax.*` is not: the
// prefix is reserved against projects, but most of it ships as third-party or
// Jakarta artifacts rather than with the JDK -- `javax.servlet`, `javax.inject`,
// `javax.validation`, `javax.persistence`, `javax.ws.rs`, and (since JDK 11)
// `javax.annotation` are all dependencies. Claiming `stdlib` for those would be
// the "stdlib rule that quietly catches external packages" mistake, so only the
// subtrees that are genuinely part of the platform are listed.
func isJDKNamespace(path string) bool {
	if strings.HasPrefix(path, "java.") {
		return true
	}
	for _, prefix := range jdkJavaxSubtrees {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// jdkJavaxSubtrees are the `javax.*` package roots shipped by the JDK.
var jdkJavaxSubtrees = []string{
	"javax.accessibility.", "javax.crypto.", "javax.imageio.",
	"javax.management.", "javax.naming.", "javax.net.", "javax.print.",
	"javax.script.", "javax.security.", "javax.sound.", "javax.sql.",
	"javax.swing.", "javax.tools.", "javax.xml.",
}

// classifyRust links either directly (`std::mem::swap()` is persisted with its
// `std::` prefix intact) or through a `use` whose last segment is the identifier
// at the call site: `use std::collections::HashMap;` + `HashMap::new()`.
// NormalizeImports expands `use std::collections::{HashMap, HashSet};` into one
// entry per name and strips `as` clauses, so both forms reach this rule as plain
// paths.
//
// External is NOT claimed. Since Rust 2018 uniform paths, a bare first segment
// (`use serde::Serialize`) may name either an external crate or a top-level
// module of *this* crate, and no crate-root module list is indexed. `crate::`,
// `self::`, and `super::` are proven project-local, hence unknown rather than
// project.
func classifyRust(dstName string, imports []string) string {
	head := firstSegment(dstName, "::")
	if head == "" {
		return Unknown
	}
	if rustStdlibCrates[head] {
		return Stdlib
	}
	return verdictOfMatchingImports(imports, func(imp string) (string, bool) {
		if lastSegment(imp, "::") != head {
			return "", false
		}
		if rustStdlibCrates[firstSegment(imp, "::")] {
			return Stdlib, true
		}
		return Unknown, true
	})
}

// classifyTypeScript is the one language where "not from this project" can be a
// property of the import form itself. Node/TypeScript resolution divides
// specifiers into:
//
//   - Project-local: relative or absolute (`./x`, `../x`, `/x`), Node subpath
//     imports (`#x`), and the two near-universal bundler aliases `@/x`
//     (Next.js, Vue) and `~/x` (Nuxt, Vite, webpack). All resolve to the
//     project's own files, so `unknown` -- `project` without a bound
//     `dst_symbol_id` would be a fabrication.
//   - Runtime: `node:`-prefixed, and bare specifiers that name a Node core
//     module. Core wins over `node_modules` in Node's own resolution, so these
//     are `stdlib`.
//   - Installed dependency: a bare specifier that is a package name --
//     `express`, or exactly `@scope/pkg`. Only these are `external`.
//
// A multi-segment unscoped specifier (`src/utils/helpers`, `components/Button`)
// is deliberately NOT external: with `baseUrl` set it is a project path, and it
// is the same string either way. That costs the `lodash/isEqual` deep-import
// form, which is the right trade -- `unknown` over a guess.
//
// Linkage is the other limit: `file_imports` stores the specifier but not the
// local binding, so `import { map } from "lodash"` + `map()` cannot be linked.
// It links when the binding coincides with the specifier's basename, the
// idiomatic namespace/default form: `import * as express from "express"` +
// `express.Router()`.
func classifyTypeScript(dstName string, imports []string) string {
	head, _, _ := strings.Cut(dstName, ".")
	if head == "" {
		return Unknown
	}
	return verdictOfMatchingImports(imports, func(spec string) (string, bool) {
		if tsLocalBinding(spec) != head {
			return "", false
		}
		switch {
		case strings.HasPrefix(spec, "node:"):
			return Stdlib, true
		case isTSProjectLocalSpecifier(spec):
			return Unknown, true
		case nodeCoreModules[spec]:
			return Stdlib, true
		case isTSPackageSpecifier(spec):
			return External, true
		default:
			return Unknown, true
		}
	})
}

// isTSProjectLocalSpecifier covers the specifier forms that resolve against the
// project rather than against `node_modules`. `@/` is unambiguous: npm forbids an
// empty scope, so it can only be a path alias.
func isTSProjectLocalSpecifier(spec string) bool {
	return strings.HasPrefix(spec, ".") ||
		strings.HasPrefix(spec, "/") ||
		strings.HasPrefix(spec, "#") ||
		strings.HasPrefix(spec, "~") ||
		strings.HasPrefix(spec, "@/")
}

// isTSPackageSpecifier reports whether spec is a bare package name -- the only
// form whose `node_modules` origin is a property of the string itself. An
// unscoped `a/b` is excluded because `baseUrl` makes it a project path; a scoped
// name is accepted only in its exact `@scope/pkg` form.
func isTSPackageSpecifier(spec string) bool {
	if strings.HasPrefix(spec, "@") {
		scope, rest, found := strings.Cut(strings.TrimPrefix(spec, "@"), "/")
		return found && scope != "" && rest != "" && !strings.Contains(rest, "/")
	}
	return spec != "" && !strings.Contains(spec, "/")
}

// verdictOfMatchingImports applies match to every import and returns the single
// verdict they agree on, or `unknown` when two matching imports disagree. This is
// what keeps `file_imports` row order out of the answer: a file with both
// `use std::io::Result` and `use crate::error::Result` gets `unknown`, not
// whichever row was inserted first.
func verdictOfMatchingImports(imports []string, match func(string) (string, bool)) string {
	verdict := ""
	for _, imp := range imports {
		got, ok := match(imp)
		if !ok {
			continue
		}
		if verdict != "" && verdict != got {
			return Unknown
		}
		verdict = got
	}
	if verdict == "" {
		return Unknown
	}
	return verdict
}

// tsLocalBinding is the identifier a namespace or default import of spec
// conventionally binds: the specifier's last path segment, minus a `node:`
// prefix and minus a file extension. Returns "" when the specifier cannot bind a
// usable identifier.
func tsLocalBinding(spec string) string {
	trimmed := strings.TrimPrefix(spec, "node:")
	base := lastSegment(trimmed, "/")
	if idx := strings.LastIndexByte(base, '.'); idx > 0 {
		base = base[:idx]
	}
	if base == "" || base == "." || base == ".." {
		return ""
	}
	return base
}

// longestImportPrefix returns the longest import that is either equal to dstName
// or a `separator`-delimited prefix of it. Requiring the separator keeps `os`
// from matching `ossify.run`.
func longestImportPrefix(dstName string, imports []string, separator string) (string, bool) {
	best := ""
	for _, imp := range imports {
		if imp == "" {
			continue
		}
		if dstName != imp && !strings.HasPrefix(dstName, imp+separator) {
			continue
		}
		if len(imp) > len(best) {
			best = imp
		}
	}
	return best, best != ""
}

func firstSegment(path, separator string) string {
	head, _, _ := strings.Cut(path, separator)
	return head
}

func lastSegment(path, separator string) string {
	if idx := strings.LastIndex(path, separator); idx >= 0 {
		return path[idx+len(separator):]
	}
	return path
}

// NormalizeImports turns the raw `file_imports.import_path` rows of one file
// into plain module paths this package can reason about.
//
// Call it once per file and reuse the result for that file's edges: it allocates,
// and Evidence.Imports is defined to be already-normalized.
//
// It exists because import capture is lossy and inconsistent *at the parser
// boundary*: the cgo and non-cgo adapter families disagree on the same language
// (`internal/parser/python/adapter.go` stores `numpy as np` verbatim where the
// tree-sitter adapter stores `numpy`), and several adapters keep syntax that was
// never a module path (`static java.util.Objects.requireNonNull`,
// `std::{fs, io}`). Normalizing here rather than changing 20 adapter call sites
// keeps P8 out of the parsers, and keeps every consumer on identical rules.
//
// Entries that normalize to nothing meaningful -- relative Python imports,
// empty strings -- are dropped rather than passed through as pseudo-modules.
func NormalizeImports(language string, raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		switch language {
		case "python":
			// `import numpy as np` (non-cgo adapter) -> `numpy`. Relative
			// imports (`.mod`, `..pkg`) are project-local: dropped.
			trimmed = strings.TrimSpace(stripAliasClause(trimmed))
			if trimmed == "" || strings.HasPrefix(trimmed, ".") {
				continue
			}
			out = append(out, trimmed)
		case "java":
			// The heuristic adapter keeps `static ` from a static import.
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "static "))
			if trimmed != "" {
				out = append(out, trimmed)
			}
		case "rust":
			out = append(out, expandRustUse(trimmed)...)
		default:
			out = append(out, trimmed)
		}
	}
	return out
}

// stripAliasClause removes a trailing ` as <alias>` clause.
func stripAliasClause(path string) string {
	if idx := strings.Index(path, " as "); idx >= 0 {
		return path[:idx]
	}
	return path
}

// expandRustUse turns one raw `use` argument into one path per imported name:
// `std::collections::{HashMap, HashSet}` -> `std::collections::HashMap`,
// `std::collections::HashSet`. Only the outermost brace group is expanded --
// nested groups (`std::{io::Read, fmt}`) keep their inner braces and simply fail
// to match any call name, which yields `unknown` rather than a wrong module.
func expandRustUse(raw string) []string {
	path := strings.TrimSpace(stripAliasClause(raw))
	if path == "" {
		return nil
	}
	openIdx := strings.Index(path, "{")
	if openIdx < 0 {
		return []string{path}
	}
	closeIdx := strings.LastIndex(path, "}")
	if closeIdx < openIdx {
		return nil
	}
	prefix := path[:openIdx]
	out := []string{}
	for _, name := range strings.Split(path[openIdx+1:closeIdx], ",") {
		name = strings.TrimSpace(stripAliasClause(strings.TrimSpace(name)))
		if name == "" || strings.ContainsAny(name, "{}") {
			continue
		}
		if name == "self" {
			// `use std::io::{self, Read}` -- `self` binds the parent module.
			out = append(out, strings.TrimSuffix(prefix, "::"))
			continue
		}
		out = append(out, prefix+name)
	}
	return out
}
