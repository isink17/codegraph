package store

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Bare-name type-target scope (P22.9).
//
// P22.4 recorded two deferred "false positive" class bindings. They were
// recovered from the surviving corpus database and from the patch that wrote
// the corpus, and they turn out NOT to be false positives:
//
//	kotlin/A.kt  `import kotlin.io.println` + `class A { ... }`
//	kotlin/B.kt  `class B { fun cross() { A().first() } }`
//	swift/a.swift `import Foundation` + `class Box { ... }`
//	swift/b.swift `func cross() { Box().first() }`
//
// Neither Kotlin file declares a package, so both are in the root package and
// `B.kt` needs no import to see `A`; the two Swift files are one module, where
// `b.swift` needs no import to see `Box`. Both bindings are what the language
// itself resolves. They scored as false positives only because that phase's
// hand truth listed one target per call site and every target in it was a
// function: the truth edge for `A().first()` was `cross -> A.first`, the
// adapter emits the member call separately (and it stays unresolved, counted
// there as a false negative), and the class edge had nowhere to land.
//
// So the recorded pair is a scoring artifact, and this phase deliberately keeps
// both bindings. What the audit for them DID surface is a real and larger class
// of wrong bindings, in different languages:
//
//	mitmproxy/addons/maplocal.py  `from pathlib import Path` ... `Path(x)`
//	                              -> class types.Path (mitmproxy/types.py)
//	proxy/layers/http/_upstream_proxy.py  `from h11._receivebuffer import
//	                              ReceiveBuffer` -> class utils.ReceiveBuffer
//	googletest-message-test.cc    `Message()` -> a DIFFERENT test file's
//	                              `class Message` in googlemock/
//	docs/src/assets/asciinema-player.js  `Bf()` -> `class Bf` in an unrelated
//	                              minified bundle
//
// Every one of those is a bare spelling bound on nothing but repository-global
// uniqueness of a class name, in a language where a name from another file is
// only visible if the file imports it.
//
// The rule:
//
//	In a language whose cross-file visibility is decided by imports, a call
//	edge whose destination spelling is a BARE name may bind a TYPE symbol only
//	when the edge's own file declares that symbol or imports the file that
//	declares it.
//
// Both halves are evidence CodeGraph actually holds. Same-file declaration is a
// scope every language admits. An import is the explicit statement that this
// file can see that file -- the same evidence P19b requires before it will
// cross a language boundary.
//
// The language restriction is the load-bearing part, and the P22.4 pair is why.
// "Same file or imported" is the visibility rule of Python, JavaScript,
// TypeScript, C and C++. It is NOT the rule of Kotlin, Java, C#, Swift, PHP or
// Ruby, where a package, module or namespace makes declarations visible across
// files with no import at all -- so applying it there would refuse every
// legitimate cross-file type reference, starting with the two the corpus
// records. Those languages keep their existing behaviour until their package
// scope is modelled the way P22.6 modelled Go's; the gap is recorded as a P22
// finding rather than papered over with a scope rule that does not match the
// language. Rust is excluded for a narrower reason: its `use` specifiers are
// `::`-separated paths that importSpecifierPath cannot resolve to a file, so
// gating it would mean refusing everything.
//
// Go needs no entry here at all: a bare Go call never reaches these strategies
// (resolverGoBareScopeSQL), because P22.6 answers it from the calling symbol's
// own package -- which is exactly the modelled package scope the languages
// above still lack.
//
// Measured on mitmproxy: the same-file half alone would have dropped 2303
// cross-file Python class bindings, of which 2214 are backed by an import of
// the declaring module and are kept. The 89 that survive removal are the defect
// in its natural habitat -- 84 of them `pathlib.Path` bound to mitmproxy's own
// `types.Path`, plus `collections.Counter`, an `h11` `ReceiveBuffer`, and a
// class declared locally inside a test method.
//
// Deliberately narrow, in four further directions:
//
//   - Only the bare-name level. `exact_qualified` and the dot-tail strategies
//     matched a spelling that carried its own qualifier, which is real evidence
//     from the call site: googletest's `gtest_test_utils.Subprocess(...)` still
//     binds that class across files, because the source wrote the qualifier.
//   - Only type kinds. Nothing here changes how a call binds a function or a
//     method; that is P22.1/P22.6 territory and the bare-name rule for callables
//     is a separate, much larger question (recorded as a finding, not fixed).
//   - Ambiguity counting is untouched. A cross-file type candidate still makes
//     its name ambiguous for the veto, so removing it can never promote some
//     other candidate into a binding; the edge is refused, not retargeted.
//   - Failure is unresolved, never a weaker fallback -- the same direction
//     P22.1/P22.5/P22.6/P22.8 all fail in.

// typeScopeGatedLanguages are the persisted languages whose cross-file
// visibility this rule may decide: a name declared in another file is reachable
// only through an import (or include) the source itself wrote, and
// importSpecifierPath can resolve that spelling to a file.
//
// Adding a language here is a claim about its visibility model AND about the
// specifier syntax being resolvable, so the set is deliberately explicit rather
// than "everything except Go".
var typeScopeGatedLanguages = map[string]struct{}{
	// `from x import Y`, resolvable to x.py / x/__init__.py.
	"python": {},
	// The typescript adapter's language for .ts/.tsx/.js/.jsx/.mjs alike;
	// `./x`, `../x` and bare package specifiers are all path-shaped.
	"typescript": {},
	// The cpp adapter's language for C and C++: `#include "a/b.h"`, and no
	// cross-translation-unit visibility without one.
	"cpp": {},
}

// typeScopeGatedLanguagesSQL is the SQL twin of typeScopeGatedLanguages.
// TestTypeScopeGatedLanguagesSQLMatchesGoTwin pins that they agree.
const typeScopeGatedLanguagesSQL = `('python', 'typescript', 'cpp')`

// typeScopeGatedLanguage reports whether this rule governs a source language.
func typeScopeGatedLanguage(language string) bool {
	_, ok := typeScopeGatedLanguages[language]
	return ok
}

// typeSymbolKindsSQL is the set of symbol kinds that denote a declared type
// rather than something executable. It is the adapters' actual vocabulary, read
// off their emission sites rather than assumed:
//
//	class      typescript, kotlin, swift, cpp, php, ruby, python
//	type       java, csharp, typescript aliases, golang
//	struct     cpp, swift, csharp, rust
//	interface  typescript, csharp, kotlin
//	enum       cpp, swift, csharp, rust           (persisted on fmt, googletest)
//	object     kotlin
//	protocol   swift
//	actor      swift
//	trait      rust
//
// Missing one is not cosmetic: only the bare-name strategy is kind-restricted
// (resolverBareNameKindsSQL), while `exact_qualified` and the suffix strategies
// have no kind filter at all, so an omitted kind reaches a bare-name binding
// through them. A top-level C++ `enum Color` has `qualified_name = "Color"`, so
// a bare `Color(x)` in a translation unit that includes no declaring header
// would bind it on repository-global uniqueness alone -- the P22.4 shape, in a
// kind this set used to miss.
//
// Java and C# emit constructors as kind `function`, so a constructor
// declaration is a callable everywhere it exists and is never caught here.
const typeSymbolKindsSQL = `('class', 'type', 'struct', 'interface', 'enum', 'object', 'protocol', 'actor', 'trait')`

// typeSymbolKinds is the Go-side twin of typeSymbolKindsSQL, read by the
// incremental binder and by the query surfaces. The two must list the same
// kinds; TestTypeSymbolKindsSQLMatchesGoTwin pins that they do.
var typeSymbolKinds = map[string]struct{}{
	"class":     {},
	"type":      {},
	"struct":    {},
	"interface": {},
	"enum":      {},
	"object":    {},
	"protocol":  {},
	"actor":     {},
	"trait":     {},
}

// isTypeSymbolKind reports whether a persisted symbol kind denotes a type
// declaration.
func isTypeSymbolKind(kind string) bool {
	_, ok := typeSymbolKinds[kind]
	return ok
}

// resolverBareNameTypeScopeSQL is the repo-wide half of the rule. It is a
// predicate on the *chosen* candidate rather than on the edge, for the same
// reason resolverGoBareScopeSQL is: whichever id the caller-kind chooser landed
// on has to satisfy the scope, and a strategy that reaches a distant type must
// bind nothing rather than fall through.
//
// It lives in resolverBindableCandidateSQL, so every repo-wide strategy carries
// it and none can be added that forgets it. That matters concretely: the first
// draft of this rule sat on the bare-name strategy alone, and the two P22.4
// false positives simply reappeared one strategy later under
// `receiver_method` -- which matches the same bare `symbols.name` and only asks
// that the candidate have a container, which every class does.
//
// The sqlNotBareName guard is what keeps it to spellings with no qualifier.
// A dst_name carrying a '.', '/' or ':' matched a qualifier the source itself
// wrote, and that is evidence about scope; googletest's
// `gtest_test_utils.Subprocess(...)` therefore still binds that class across
// files.
//
// It requires what every resolver strategy already provides: the update target
// is `edges` and the candidate relation is joined in as `r`. For the common
// qualified spelling the guard short-circuits; otherwise it is one rowid seek
// on `symbols.id` for the single already-chosen candidate.
var resolverBareNameTypeScopeSQL = `(
		f.language NOT IN ` + typeScopeGatedLanguagesSQL + `
		OR ` + sqlNotBareName("edges.dst_name") + `
		OR NOT EXISTS (
			SELECT 1 FROM symbols ts
			WHERE ts.id = (` + resolverChosenCandidateSQL + `)
			AND ts.kind IN ` + typeSymbolKindsSQL + `
			AND ts.file_id <> edges.file_id
			AND NOT EXISTS (
				SELECT 1 FROM ` + resolverImportScopeTable + ` isc
				WHERE isc.file_id = edges.file_id AND isc.target_file_id = ts.file_id
			)
		)
	)`

// -- import scope -----------------------------------------------------------
//
// resolverImportScopeTable holds (file_id, target_file_id) pairs: the importing
// file said, in its own source, that it can see the target file. It is the SQL
// side's only view of importScopeForRepo, populated once per resolve from Go so
// the specifier rules cannot be restated -- and therefore cannot drift -- in
// SQL, exactly as resolverTestFilesTable does for IsTestFilePath.
//
// An *empty* table means "nothing imports anything", which reduces the rule
// above to its same-file half. That is the conservative reading, and it is what
// a strategy invoked in isolation (a benchmark) gets; in practice such a
// strategy only ever sees qualifier-bearing spellings, which the sqlNotBareName
// guard short-circuits before this table is consulted at all.
const resolverImportScopeTable = `tmp_resolver_import_scope`

// ensureResolverImportScopeTable makes the import-scope relation exist for the
// current transaction without populating it.
func ensureResolverImportScopeTable(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+resolverImportScopeTable+
		`(file_id INTEGER NOT NULL, target_file_id INTEGER NOT NULL,
			PRIMARY KEY(file_id, target_file_id)) WITHOUT ROWID`)
	return err
}

// recordResolverImportScope fills the import-scope relation for one repo.
//
// Two scans -- `files` once to index paths, `file_imports` once to resolve
// specifiers -- and one batched insert. Never one query per edge, per import or
// per candidate.
func (s *Store) recordResolverImportScope(ctx context.Context, tx *sql.Tx, repoID int64) error {
	if err := ensureResolverImportScopeTable(ctx, tx); err != nil {
		return err
	}
	scope, err := importScopeForRepo(ctx, tx, repoID)
	if err != nil {
		return err
	}
	return insertResolverImportScope(ctx, tx, scope)
}

// importFileIndex answers "which files could this repository-relative path
// name", by exact path and by extension-stripped path. The suffix buckets are
// what let an absolute module specifier match a file under a source root:
// `app.Ledger` names `src/main/java/app/Ledger.java` in Java layouts as surely
// as `mitmproxy.http` names `mitmproxy/http.py`.
type importFileIndex struct {
	byPath   map[string][]int64
	byBase   map[string][]int64
	bySuffix map[string][]int64
}

// importScopeForRepo returns, for each file, the set of file ids it can see.
//
// Two sources, and only two:
//
//  1. the files its own specifiers name; and
//  2. the files those files reach through a *relative* specifier of their own.
//
// (2) is one hop, and only through a relative specifier. It exists because a
// barrel file is how packages publish their members:
// `mitmproxy/proxy/layers/__init__.py` is nothing but `from .tcp import
// TCPLayer` and eleven siblings, so a caller writing `from mitmproxy.proxy.layers
// import TCPLayer` has import evidence for `tcp.py` that stops one file short of
// naming it.
//
// Two things the hop is deliberately NOT. It is not transitive reachability:
// only relative specifiers are followed, so importing a package whose
// `__init__.py` merely depends on some distant module (`import app.types`) buys
// no scope at all -- which is why `pathlib.Path` in
// `mitmproxy/addons/maplocal.py` stays unable to reach mitmproxy's own
// `types.Path`. And it is not restricted to barrel FILE NAMES: any file's
// relative imports are followed, not just `__init__`/`index`/`mod`. That is
// correct for Python, where a module has no export control and
// `from a import X` reaches anything `a.py` imported; it is wider than
// TypeScript's rule, where only `export ... from` re-exports and the adapter
// does not distinguish the two. The wider reading is the conservative direction
// for THIS rule -- it keeps bindings rather than refusing them -- so the cost is
// recall kept, not precision lost, and it is recorded as a P22 finding rather
// than guessed at.
//
// It is deliberately a *scope* answer, not a target choice: a specifier that
// several files answer contributes all of them, because the question here is
// only "could this file see that declaration", and the destination itself is
// still chosen by the resolver's ordinary uniqueness rule. Refusing on
// ambiguity here would silently narrow scope rather than narrow evidence.
func importScopeForRepo(ctx context.Context, q queryContexter, repoID int64) (map[int64]map[int64]struct{}, error) {
	index, pathByID, err := loadImportFileIndex(ctx, q, repoID)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT fi.file_id, fi.import_path FROM file_imports fi
		JOIN files f ON f.id = fi.file_id
		WHERE f.repo_id = ?
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	direct := map[int64]map[int64]struct{}{}
	reExports := map[int64]map[int64]struct{}{}
	for rows.Next() {
		var fileID int64
		var specifier string
		if err := rows.Scan(&fileID, &specifier); err != nil {
			return nil, err
		}
		relative := isRelativeImportSpecifier(specifier)
		for _, targetID := range index.lookup(pathByID[fileID], specifier) {
			if targetID == fileID {
				continue
			}
			addToScope(direct, fileID, targetID)
			if relative {
				addToScope(reExports, fileID, targetID)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	scope := make(map[int64]map[int64]struct{}, len(direct))
	for fileID, targets := range direct {
		for targetID := range targets {
			addToScope(scope, fileID, targetID)
			for reExported := range reExports[targetID] {
				if reExported != fileID {
					addToScope(scope, fileID, reExported)
				}
			}
		}
	}
	return scope, nil
}

// addToScope records one (file, visible file) pair.
func addToScope(scope map[int64]map[int64]struct{}, fileID, targetID int64) {
	if scope[fileID] == nil {
		scope[fileID] = map[int64]struct{}{}
	}
	scope[fileID][targetID] = struct{}{}
}

// isRelativeImportSpecifier reports whether a specifier names something inside
// the importing file's own package: `./x`, `../x` (JS/TS), `.x`, `..x` (Python).
// A bare or absolute-dotted specifier is not relative even though it contains
// dots, so `mitmproxy.types` is excluded and `.types` is not.
func isRelativeImportSpecifier(specifier string) bool {
	return strings.HasPrefix(strings.TrimSpace(specifier), ".")
}

// loadImportFileIndex builds the path indexes and the id->path map one scan of
// `files` produces.
func loadImportFileIndex(ctx context.Context, q queryContexter, repoID int64) (importFileIndex, map[int64]string, error) {
	index := importFileIndex{
		byPath:   map[string][]int64{},
		byBase:   map[string][]int64{},
		bySuffix: map[string][]int64{},
	}
	pathByID := map[int64]string{}
	rows, err := q.QueryContext(ctx, `SELECT id, path FROM files WHERE repo_id = ? AND is_deleted = 0`, repoID)
	if err != nil {
		return importFileIndex{}, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var stored string
		if err := rows.Scan(&id, &stored); err != nil {
			return importFileIndex{}, nil, err
		}
		// canonicalStoredPath, not CanonicalRelPath: these bytes come out of
		// `files.path`, which a Windows-written database stores with backslashes.
		// CanonicalRelPath only calls filepath.ToSlash, a no-op off Windows, so the
		// '/'-anchored suffix walk below would produce no buckets at all and every
		// cross-file type binding would be refused on a database written elsewhere.
		filePath := canonicalStoredPath(stored)
		if filePath == "" {
			continue
		}
		pathByID[id] = filePath
		index.byPath[filePath] = append(index.byPath[filePath], id)
		base := strings.TrimSuffix(filePath, path.Ext(filePath))
		if base == "" || base == filePath {
			continue
		}
		index.byBase[base] = append(index.byBase[base], id)
		// Every `/`-anchored tail of the extension-stripped path, so a specifier
		// that names a module relative to a source root still finds its file.
		// Bounded by the path's own depth, which is small.
		for i := 0; i < len(base); i++ {
			if base[i] == '/' {
				index.bySuffix[base[i+1:]] = append(index.bySuffix[base[i+1:]], id)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return importFileIndex{}, nil, err
	}
	return index, pathByID, nil
}

// lookup returns every file id the specifier may name, seen from importerPath.
func (i importFileIndex) lookup(importerPath, specifier string) []int64 {
	resolved, ok := importSpecifierPath(importerPath, specifier)
	if !ok {
		return nil
	}
	var out []int64
	// `pkg/mod` names pkg/mod.py, pkg/mod.ts, ... ; `pkg/mod/__init__` is
	// Python's package form; and the same tail under a source root is the Java
	// and Kotlin form. Exact path last, for specifiers that carry the extension.
	out = append(out, i.byBase[resolved]...)
	out = append(out, i.byPath[resolved]...)
	out = append(out, i.byBase[resolved+"/__init__"]...)
	out = append(out, i.bySuffix[resolved]...)
	if ext := path.Ext(resolved); ext != "" {
		// A specifier that carries an extension is still a tail: C and C++
		// `#include "gtest/gtest.h"` names `test/gtest/gtest/gtest.h` under a
		// vendored include root exactly the way a module specifier names a file
		// under a source root.
		stripped := strings.TrimSuffix(resolved, ext)
		out = append(out, i.byBase[stripped]...)
		out = append(out, i.bySuffix[stripped]...)
	}
	return out
}

// importSpecifierPath turns an import specifier into a repository-relative
// slash path.
//
// It is a deliberate superset of resolveImportSpecifierPath (cross_language_links.go),
// which refuses dotted module names because reading their dots as an extension
// once wired `import app.models` to a root-level `app.ts`. That risk does not
// exist here: this function never reads a dot as an extension, it reads the
// whole dotted specifier as a path, and the result is only ever used to ask
// whether one specific declaring file is in scope -- never to pick a target.
// Turning them away instead would drop Python, Java and Kotlin out of the rule
// entirely, which is most of the population it exists to protect.
func importSpecifierPath(importerPath, specifier string) (string, bool) {
	spec := filepath.ToSlash(strings.TrimSpace(specifier))
	if spec == "" || strings.ContainsAny(spec, " \t\"'()[]{}<>*:;,") {
		return "", false
	}
	switch {
	case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
		spec = path.Join(path.Dir(importerPath), spec)
	case strings.HasPrefix(spec, "."):
		// Python's dotted-relative form: one leading dot is the importer's own
		// package, each further dot climbs one level, and the remainder is a
		// dotted module path below it. `from . import x` (bare ".") names the
		// package directory itself, which no file answers, so it resolves to the
		// directory and finds nothing -- correctly, since the members it
		// publishes are reached through the re-export hop instead.
		dots := 0
		for dots < len(spec) && spec[dots] == '.' {
			dots++
		}
		base := path.Dir(importerPath)
		for i := 1; i < dots; i++ {
			base = path.Dir(base)
		}
		spec = path.Join(base, strings.ReplaceAll(spec[dots:], ".", "/"))
	case strings.Contains(spec, "/"):
		spec = path.Clean(spec)
	default:
		// An absolute dotted or bare module name: `mitmproxy.http`, `app.Ledger`,
		// `os`.
		spec = path.Clean(strings.ReplaceAll(spec, ".", "/"))
	}
	if spec == "" || spec == "." || spec == ".." || strings.HasPrefix(spec, "../") {
		// Escapes the repository: no file of ours answers it.
		return "", false
	}
	if strings.HasPrefix(spec, "/") {
		// An absolute specifier (`#include "/usr/include/gtest/gtest.h"`) names
		// a file outside the repository. Stripping the leading slash would turn
		// it into a repo-relative path and let it tail-match a vendored file of
		// the same name, which is the opposite of what it says.
		return "", false
	}
	return spec, true
}

// insertResolverImportScope fills resolverImportScopeTable. Insert order is
// irrelevant: it is a set keyed by both columns and every reader joins on it.
func insertResolverImportScope(ctx context.Context, tx *sql.Tx, scope map[int64]map[int64]struct{}) error {
	const maxPairsPerInsert = 400
	var b strings.Builder
	args := make([]any, 0, maxPairsPerInsert*2)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, b.String(), args...)
		b.Reset()
		args = args[:0]
		return err
	}
	for fileID, targets := range scope {
		for targetID := range targets {
			if len(args) == 0 {
				b.WriteString(`INSERT OR IGNORE INTO ` + resolverImportScopeTable +
					`(file_id, target_file_id) VALUES (?,?)`)
			} else {
				b.WriteString(",(?,?)")
			}
			args = append(args, fileID, targetID)
			if len(args) >= maxPairsPerInsert*2 {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

// -- query-side twin --------------------------------------------------------
//
// P22.1, P22.6 and P22.7 each showed that a safe persisted resolver is not
// enough: the public surfaces reconstruct relationships at query time from
// `edges.dst_name` on UNRESOLVED edges, so a rule enforced only in the resolver
// is answered anyway by `find_callers`, `find_callees` and `context_for_task`.
// Measured before this gate existed, the two P22.4 cases were still fully
// answerable -- `find_callers A` returned `B.cross`, `find_callees B.cross`
// returned `class A.A` -- from a graph whose edge was honestly unresolved.
//
// The query-side rule needs no import-scope machinery, and that is not a
// shortcut but a consequence of where the two halves sit:
//
//	the name legs match only `dst_symbol_id IS NULL` edges, and any
//	bare-spelling-to-type relationship the evidence supports has just been
//	BOUND by the resolver -- so it reaches the answer through the id leg.
//
// What is left on a bare-name leg pointing at a type is therefore exactly the
// population the resolver refused: no scope evidence, or ambiguity (P3), or a
// test definition a production caller may not bind (P7), or a foreign language
// (P22.7). None of those become evidence by being asked a second time.
//
//	A bare spelling on an unresolved-edge leg may not claim a TYPE symbol.
//
// Qualifier-bearing spellings are untouched here exactly as they are in the
// resolver: they carry evidence the call site itself wrote.

// blocksBareNameTypeClaim reports whether `spelling`, matched against this
// symbol through unresolved-edge name evidence, would claim a type the caller
// never named. It is the query-side twin of resolverBareNameTypeScopeSQL.
func (s goSymbolScope) blocksBareNameTypeClaim(spelling string) bool {
	return typeScopeGatedLanguage(s.language) && goBareCallName(spelling) && isTypeSymbolKind(s.kind)
}

// typeOnlyGatedLanguages returns the languages in which EVERY symbol a query's
// input matched is a type this rule governs.
//
// Per language, not over the whole match set, because a bare-name leg serves
// all the matched symbols at once and cannot say which one an edge meant. If an
// input matches a Python class and a Kotlin class, the Kotlin half is a
// legitimate answer (Kotlin resolves a bare class name across files with no
// import) while the Python half is the refused population -- "every target is a
// type" would have dropped the leg for both, and "any" would have kept it for
// both. The caller keeps the leg and subtracts only the languages that are
// entirely type-and-gated from it.
//
// A language whose matched targets include a function keeps its leg: the
// callers that leg finds are real callers of that function. A target with no
// persisted language contributes nothing, since there is no language to decide.
func typeOnlyGatedLanguages(scopes map[int64]goSymbolScope) map[string]struct{} {
	typeOnly := map[string]bool{}
	for _, scope := range scopes {
		if scope.language == "" {
			continue
		}
		eligible := isTypeSymbolKind(scope.kind) && typeScopeGatedLanguage(scope.language)
		if seen, ok := typeOnly[scope.language]; ok {
			typeOnly[scope.language] = seen && eligible
			continue
		}
		typeOnly[scope.language] = eligible
	}
	out := map[string]struct{}{}
	for language, only := range typeOnly {
		if only {
			out[language] = struct{}{}
		}
	}
	return out
}

// languagesExcept returns `languages` without the excluded set, preserving the
// input's order so statement text stays a function of the input.
func languagesExcept(languages []string, excluded map[string]struct{}) []string {
	out := make([]string, 0, len(languages))
	for _, language := range languages {
		if _, drop := excluded[language]; drop {
			continue
		}
		out = append(out, language)
	}
	return out
}

// -- incremental invalidation ----------------------------------------------
//
// P22.9 makes import evidence a resolution input, and that opens the direction
// P22.8 closed for declarations: a file's bindings can now be invalidated by an
// edit to a DIFFERENT file that changes nothing but its import list.
//
// Concretely, before this pass existed: index `app/pkg/tcp.py` (`class Layer`),
// `app/pkg/__init__.py` (`from .tcp import Layer`) and `app/caller.py`
// (`import app.pkg`, calls `Layer()`). The caller's edge binds through the
// re-export hop. Now delete the relative import from `__init__.py` and update.
// The indexer collects `changedSymbolNameSet` from the new parse's SYMBOLS, and
// `__init__.py` declares none, so the batch carries no names at all; the
// path-scoped pass only touches edges inside `__init__.py`; and the caller's
// binding survives an edit that removed the only evidence for it. A fresh index
// refuses it. That is indexing history acting as semantic evidence -- exactly
// what e20b1e1 removed for the declaration direction.
//
// typeScopeNamesForChangedPaths closes it by feeding the batch the names whose
// answer could have changed. The affected files are the changed ones plus the
// files that can see them (either direction of the import relation, computed
// from the same index the rule itself uses), and the affected names are the
// bare type-denoting spellings those files' edges actually carry -- both the
// ones currently bound to a type and the ones still unresolved, so the pass
// covers an import being removed and an import being added. Everything after
// that is the existing machinery: invalidateNameEvidenceBindings clears the
// bindings for those names, and the name pass re-decides them.
//
// The scope is the neighbourhood, never the repository: a save that touches a
// file nothing imports contributes nothing.
func (s *Store) typeScopeNamesForChangedPaths(ctx context.Context, repoID int64, paths []string, scopes *importScopeCache) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	// The exit condition is "this repository holds no file this rule governs",
	// NOT "no changed file is governed". The import index is language-blind and
	// the re-export hop follows any file's relative specifiers, so a file in an
	// ungated language can sit ON the hop: a Ruby `pkg.rb` doing `./tcp` is what
	// makes `tcp.py` visible to a Python caller that imports `pkg`, and testing
	// the changed file's own language would skip the pass for an edit to it. It
	// still keeps the whole pass, import-scope build included, off every update
	// to a Go-only or Java-only repository.
	var hasGatedFile int
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM files WHERE repo_id = ? AND language IN `+
			typeScopeGatedLanguagesSQL+`)`, repoID).Scan(&hasGatedFile); err != nil {
		return nil, err
	}
	if hasGatedFile == 0 {
		return nil, nil
	}
	scope, err := scopes.get(ctx, s, repoID)
	if err != nil {
		return nil, err
	}
	changed, err := fileIDsByPaths(ctx, s.db, repoID, paths)
	if err != nil {
		return nil, err
	}
	if len(changed) == 0 {
		return nil, nil
	}
	// Only the changed files and the files that can SEE them can have flipped: a
	// changed file's own targets keep whatever scope their own imports give
	// them, so pulling those in would only widen the batch.
	affected := make(map[int64]struct{}, len(changed))
	for _, id := range changed {
		affected[id] = struct{}{}
	}
	for viewer, visible := range scope {
		for _, id := range changed {
			if _, ok := visible[id]; ok {
				affected[viewer] = struct{}{}
				break
			}
		}
	}
	if _, err := s.clearOutOfScopeTypeBindings(ctx, s.db, repoID, affected, scope); err != nil {
		return nil, err
	}
	return typeScopeEdgeNames(ctx, s.db, repoID, affected)
}

// typeScopeEdgeNames returns the distinct bare `dst_name` spellings carried by
// the given files' edges that could denote a type: the ones already bound to a
// type symbol, and the ones still unresolved. Ordered, so the batch the caller
// builds is a function of the graph rather than of map iteration.
func typeScopeEdgeNames(ctx context.Context, q queryContexter, repoID int64, fileIDs map[int64]struct{}) ([]string, error) {
	if len(fileIDs) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(fileIDs))
	for id := range fileIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	seen := map[string]struct{}{}
	var out []string
	for _, chunk := range chunkInt64s(ids, sqliteInClauseBatchSize) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		args = append(args, int64SliceToAny(chunk)...)
		rows, err := q.QueryContext(ctx, `
			SELECT DISTINCT e.dst_name
			FROM edges e
			LEFT JOIN symbols t ON t.id = e.dst_symbol_id
			WHERE e.repo_id = ?
			  AND e.file_id IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
			  AND e.dst_name != ''
			  AND instr(e.dst_name, '.') = 0
			  AND instr(e.dst_name, '/') = 0
			  AND instr(e.dst_name, ':') = 0
			  AND (e.dst_symbol_id IS NULL OR t.kind IN `+typeSymbolKindsSQL+`)
			ORDER BY e.dst_name
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// fileIDsByPaths resolves repository-relative paths to file ids, accepting the
// stored separator spellings a pre-P23 database may hold.
func fileIDsByPaths(ctx context.Context, q queryContexter, repoID int64, paths []string) ([]int64, error) {
	stored := make([]string, 0, len(paths)*3)
	for _, p := range paths {
		canonical := CanonicalRelPath(p)
		if canonical == "" {
			continue
		}
		stored = append(stored, storedPathVariants(canonical)...)
	}
	if len(stored) == 0 {
		return nil, nil
	}
	seen := map[int64]struct{}{}
	var out []int64
	for start := 0; start < len(stored); start += sqliteInClauseBatchSize {
		end := min(start+sqliteInClauseBatchSize, len(stored))
		chunk := stored[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := q.QueryContext(ctx,
			`SELECT id FROM files WHERE repo_id = ? AND path IN (`+
				strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// mergeResolverNames unions two name batches, preserving the first batch's
// order and appending the second's new members in their own order, so the
// resulting batch is a function of its inputs.
func mergeResolverNames(primary, extra []string) []string {
	if len(extra) == 0 {
		return primary
	}
	seen := make(map[string]struct{}, len(primary)+len(extra))
	out := make([]string, 0, len(primary)+len(extra))
	for _, batch := range [][]string{primary, extra} {
		for _, name := range batch {
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// clearOutOfScopeTypeBindings unbinds edges in `affected` whose destination the
// current import evidence no longer makes visible.
//
// invalidateNameEvidenceBindings cannot do this job: it clears a binding whose
// name this batch may have made AMBIGUOUS, and a barrel that stops re-exporting
// changes no declaration count at all -- `Layer` is still the only `Layer` in
// the repository, it is merely no longer reachable from the caller. So the
// scope direction needs its own pass, and it is the exact negation of the bind
// rule rather than a restatement of it: the candidate set comes from the same
// `scope` map resolverBareNameTypeScopeSQL reads, so the two cannot disagree.
//
// A nil `affected` means every file in the repository. That is the form the
// once-per-repo repair and the post-deletion revalidation run, and it is what
// brings a database written before this rule existed into line: no migration
// can decide this population, because deciding it needs the import scope
// importScopeForRepo computes in Go from per-language specifier rules, and
// restating those in SQL is exactly the drift resolver_testfile.go forbids.
// Running the rule's own negation instead means the repair cannot disagree with
// the rule, and it is idempotent.
//
// The strategy restriction applies only to the incremental form. P22.8's reason
// for it is that an incremental pass must not clear a binding it could not
// rebuild; the repo-wide forms have the opposite premise -- the rule refuses
// these bindings, so nothing should ever rebuild one -- and leaving a
// `receiver_method` binding standing (which a nested class satisfies, having
// both a name and a container) would mark a repository repaired while it still
// asserts the relation.
//
// `cross_language_ref` is exempt in both forms: those edges are inserted already
// bound by their own pass from import-bridge evidence, no resolver strategy
// considers them, and clearing one would delete a destination nothing rebuilds.
// Same exemption invalidateNameEvidenceBindings and migration 026 make.
func (s *Store) clearOutOfScopeTypeBindings(
	ctx context.Context,
	q execQuerier,
	repoID int64,
	affected map[int64]struct{},
	scope map[int64]map[int64]struct{},
) (int, error) {
	var ids []int64
	if affected != nil {
		if len(affected) == 0 {
			return 0, nil
		}
		ids = make([]int64, 0, len(affected))
		for id := range affected {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}

	selectStale := `
		SELECT e.id, e.file_id, t.file_id
		FROM edges e
		JOIN files f ON f.id = e.file_id
		JOIN symbols t ON t.id = e.dst_symbol_id
		WHERE e.repo_id = ?
		  AND e.dst_name != ''
		  AND instr(e.dst_name, '.') = 0
		  AND instr(e.dst_name, '/') = 0
		  AND instr(e.dst_name, ':') = 0
		  AND f.language IN ` + typeScopeGatedLanguagesSQL + `
		  AND t.kind IN ` + typeSymbolKindsSQL + `
		  AND t.file_id <> e.file_id
		  AND e.edge_kind <> '` + EdgeKindCrossLanguageRef + `'`
	if ids != nil {
		selectStale += `
		  AND e.resolution_strategy IN ` + sqlStrategyInList(incrementallyRedecidableStrategies)
	}

	var stale []int64
	collect := func(query string, args ...any) error {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var edgeID, srcFileID, dstFileID int64
			if err := rows.Scan(&edgeID, &srcFileID, &dstFileID); err != nil {
				return err
			}
			if _, visible := scope[srcFileID][dstFileID]; !visible {
				stale = append(stale, edgeID)
			}
		}
		return rows.Err()
	}

	if ids == nil {
		if err := collect(selectStale, repoID); err != nil {
			return 0, err
		}
	} else {
		for _, chunk := range chunkInt64s(ids, sqliteInClauseBatchSize) {
			args := make([]any, 0, len(chunk)+1)
			args = append(args, repoID)
			args = append(args, int64SliceToAny(chunk)...)
			query := selectStale + ` AND e.file_id IN (` +
				strings.TrimRight(strings.Repeat("?,", len(chunk)), ",") + `)`
			if err := collect(query, args...); err != nil {
				return 0, err
			}
		}
	}
	if len(stale) == 0 {
		return 0, nil
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i] < stale[j] })
	return clearEdgeResolutions(ctx, q, stale)
}

// execQuerier is the read/write surface the repair needs, satisfied by both
// *sql.DB and *sql.Tx.
type execQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// clearEdgeResolutions drops the destination of every listed edge, in one
// transaction when it is handed a *sql.DB so a failed chunk cannot leave the
// graph half-cleared -- the same guarantee invalidateNameEvidenceBindings gives.
// The assignment list is resolverClearResolutionSQL, never restated, so
// destination, strategy and confidence always change together.
func clearEdgeResolutions(ctx context.Context, q execQuerier, edgeIDs []int64) (int, error) {
	if len(edgeIDs) == 0 {
		return 0, nil
	}
	exec := q
	var tx *sql.Tx
	if db, ok := q.(*sql.DB); ok {
		started, err := db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		tx, exec = started, started
		defer func() {
			if tx != nil {
				_ = tx.Rollback()
			}
		}()
	}
	cleared := 0
	for _, chunk := range chunkInt64s(edgeIDs, sqliteInClauseBatchSize) {
		res, err := exec.ExecContext(ctx, `
			UPDATE edges SET `+resolverClearResolutionSQL+`
			WHERE id IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)`,
			int64SliceToAny(chunk)...)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			cleared += int(n)
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		tx = nil
	}
	return cleared, nil
}

// importScopeCache builds the repository's import scope at most once for a
// resolve, however many passes ask for it.
//
// One incremental update runs three things that need it: the invalidation pass
// above, the path-scoped binder and the name-scoped binder. Each build is a
// scan of `files` plus a scan of `file_imports`, which is cheap on a small repo
// and emphatically not on a large monorepo where a one-file save would pay it
// three times. The cache is per-resolve, never on the Store: import evidence
// changes with the graph, and a value that outlived the call would be exactly
// the stale-evidence bug this rule exists to prevent.
//
// A nil cache is valid and means "build your own", which keeps the standalone
// entrypoints (ResolveEdgesForPaths, ResolveEdgesForNames) working unchanged.
type importScopeCache struct {
	store  *Store
	repoID int64
	scope  map[int64]map[int64]struct{}
	loaded bool
}

// newImportScopeCache returns a cache for one repo's resolve.
func newImportScopeCache(s *Store, repoID int64) *importScopeCache {
	return &importScopeCache{store: s, repoID: repoID}
}

// get returns the import scope, building it on first use.
func (c *importScopeCache) get(ctx context.Context, s *Store, repoID int64) (map[int64]map[int64]struct{}, error) {
	if c == nil {
		return importScopeForRepo(ctx, s.db, repoID)
	}
	if c.loaded {
		return c.scope, nil
	}
	scope, err := importScopeForRepo(ctx, c.store.db, c.repoID)
	if err != nil {
		return nil, err
	}
	c.scope, c.loaded = scope, true
	return c.scope, nil
}

// typeScopeRepairSettingKey records that a repository's pre-P22.9 bindings have
// been repaired.
//
// The repair has to run once against a database written by an older release,
// and it cannot be a schema migration: deciding the population needs the import
// scope importScopeForRepo computes in Go from per-language specifier rules, and
// restating those in SQL is the drift resolver_testfile.go forbids. A settings
// key gives it the one property a migration has and a resolver pass does not --
// it runs once and then costs nothing -- while keeping the decision in the same
// code the rule itself uses, so the two cannot disagree.
//
// Keyed per repository, because one database holds several. The key is written
// only after the clear succeeds, so a failure re-runs rather than marking a
// repository repaired that is not.
const typeScopeRepairSettingKey = "resolver.type_scope_repaired.v1"

// repairTypeScopeBindingsOnce clears this repository's pre-P22.9 bindings the
// first time any resolve runs against it, and does nothing afterwards.
//
// Why it is needed at all: no strategy reconsiders an already-bound edge (every
// one matches `dst_symbol_id IS NULL`), and a plain `index` over an unchanged
// tree parses nothing, so without this an upgraded database would keep
// asserting relationships its own resolver now refuses -- while the query
// surfaces already refuse the name-evidence form of the same claim, leaving the
// graph inconsistent with itself.
func (s *Store) repairTypeScopeBindingsOnce(ctx context.Context, repoID int64) error {
	key := typeScopeRepairSettingKey + "." + strconv.FormatInt(repoID, 10)
	var done string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&done)
	switch {
	case err == nil && done == "1":
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if err := s.RevalidateTypeScopeAfterDeletion(ctx, repoID); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES(?, '1')
		 ON CONFLICT(key) DO UPDATE SET value = '1'`, key)
	return err
}

// RepairResolverBindingsOnce brings a repository's persisted bindings up to the
// current resolver's rules, once. It is the public entrypoint the indexer calls
// before deciding which resolve pass to run, so an upgrade converges even on a
// run that parses nothing.
func (s *Store) RepairResolverBindingsOnce(ctx context.Context, repoID int64) error {
	return s.repairTypeScopeBindingsOnce(ctx, repoID)
}

// RevalidateTypeScopeAfterDeletion re-decides every bare-name type binding in a
// repository against the current import evidence.
//
// The indexer calls it after a scan deleted files, because deleting one is the
// strongest import-list edit there is and the one the changed-path pass cannot
// see: a deleted file leaves the import index, so no specifier resolves to it
// any more and "the files that can see the changed file" finds nobody. Deleting
// the barrel that re-exported a class would otherwise strand every binding it
// made visible. Deletions are rare next to saves, so the answer is the
// repo-wide re-decision rather than a reverse index that has to remember what
// the graph used to look like.
//
// It only clears; the passes that follow re-bind whatever is still provable.
func (s *Store) RevalidateTypeScopeAfterDeletion(ctx context.Context, repoID int64) error {
	scope, err := importScopeForRepo(ctx, s.db, repoID)
	if err != nil {
		return err
	}
	_, err = s.clearOutOfScopeTypeBindings(ctx, s.db, repoID, nil, scope)
	return err
}
