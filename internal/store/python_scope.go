package store

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

const (
	pyScopeVeto       = "tmp_python_scope_veto"
	pyScopeResolution = "tmp_python_scope_resolution"
)

// pythonModuleCandidatePaths turns a Python import specifier written in
// `sourceFile` into the repository-relative files that specifier could name.
// It answers with syntax only: which of the candidates actually exists, and
// whether exactly one of them does, is decided against the file table.
//
// Module identity is a path, never a basename: `foo/utils.py` and
// `bar/utils.py` are different modules and no candidate list conflates them.
//
//	pkg/mod.py          <- pkg.mod
//	pkg/mod/__init__.py <- pkg.mod
//	pkg/__init__.py     <- pkg
//
// A relative import is anchored at the importing file's package and nowhere
// else, so it yields one candidate pair or none. An absolute import is
// anchored at the repository root and nowhere else.
//
// The repository root is the only import root CodeGraph can prove. `sys.path`
// is a runtime fact: the same tree resolves `import helpers` inside `pkg/`
// differently when run as `python pkg/main.py` (where `pkg/` is `sys.path[0]`)
// than when imported as `pkg.main` from the root, and no repository structure
// distinguishes the two. Roots a repository states rather than implies -- a
// `pyproject.toml` `packages` table, a `setup.cfg` `package_dir`, a `src/`
// layout -- are not read, so an import only such a root could satisfy stays
// unresolved. A wrong high-confidence edge is worse than a missing one.
func pythonModuleCandidatePaths(sourceFile, specifier string) []string {
	if specifier == "" {
		return nil
	}
	dots := 0
	for dots < len(specifier) && specifier[dots] == '.' {
		dots++
	}
	rest := specifier[dots:]
	var segments []string
	if rest != "" {
		segments = strings.Split(rest, ".")
		for _, segment := range segments {
			if segment == "" {
				return nil
			}
		}
	}
	if dots > 0 {
		dir := pythonParentDir(canonicalStoredPath(sourceFile))
		// `.x` is the importing file's own package; each further dot climbs one.
		for i := 1; i < dots; i++ {
			if dir == "" {
				return nil
			}
			dir = pythonParentDir(dir)
		}
		return pythonModuleFiles(dir, segments)
	}
	if len(segments) == 0 {
		return nil
	}
	return pythonModuleFiles("", segments)
}

func pythonParentDir(p string) string {
	dir := path.Dir(p)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

func pythonModuleFiles(dir string, segments []string) []string {
	base := strings.Join(segments, "/")
	if dir != "" {
		if base == "" {
			base = dir
		} else {
			base = dir + "/" + base
		}
	}
	if base == "" {
		return nil
	}
	if len(segments) == 0 {
		// `from . import x` names a package, never a module file.
		return []string{base + "/__init__.py"}
	}
	return []string{base + ".py", base + "/__init__.py"}
}

type pyScopeImport struct {
	source, imported, local, kind, owner string
	wildcard                             bool
}

// pyBinding is one spelling an import puts in scope, flattened out of the
// evidence row so the resolver matches names rather than re-deriving forms.
//
// `import a.b` yields two: `a.b`, which reaches the module the statement named,
// and `a`, which reaches the top package it actually bound. `import a.b as x`
// yields only `x`, reaching `a.b` -- which is why ImportedName records what the
// local name is bound to and not merely whether an alias was written.
type pyBinding struct {
	prefix   string // the local spelling that reaches this module
	owner    string // lexical scope the import was written in
	kind     string
	source   string // module specifier the prefix reaches
	imported string // named imports only: the symbol asked for
}

// pyScopeLocal is negative evidence: a lexical scope binds this name itself, so
// an import of the same name written no nearer does not reach a call below it.
type pyScopeLocal struct {
	owner, name string
	// declaration marks a nested `def`/`class` name. It shadows an import like
	// any other local, but the name it binds is a symbol this graph holds, so
	// it is not a reason to keep every other strategy off the call.
	declaration bool
}

type pyScopeEdge struct {
	id, file, src int64
	name          string
}

// pythonScopeVisible reports whether evidence owned by lexical scope `owner`
// reaches a call site in scope `at`. Scopes are dotted declaration paths, and
// "" is the module.
func pythonScopeVisible(owner, at string) bool {
	return owner == "" || owner == at || strings.HasPrefix(at, owner+".")
}

// pythonCallScope is the lexical scope of the symbol a call was written in: its
// qualified name with the module prefix removed.
//
// The module is derived from the file path exactly as the parsers derive it, so
// a file whose basename holds a dot (`settings.local.py`) yields the same scope
// spelling the parser recorded as an owner. Without the path it falls back to
// dropping one segment, which is right for every ordinary basename.
func pythonCallScope(qualified, filePath string) string {
	if filePath != "" {
		module := strings.TrimSuffix(path.Base(filePath), path.Ext(filePath))
		if module != "" {
			if rest, ok := strings.CutPrefix(qualified, module+"."); ok {
				return rest
			}
			if qualified == module {
				return ""
			}
		}
	}
	if i := strings.IndexByte(qualified, '.'); i >= 0 {
		return qualified[i+1:]
	}
	return ""
}

func pythonLeadingSegment(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// pythonBindingsOf flattens one file's import rows into access spellings.
func pythonBindingsOf(imports []pyScopeImport) []pyBinding {
	out := make([]pyBinding, 0, len(imports))
	for _, imp := range imports {
		if imp.wildcard || imp.kind == graph.ScopeImportLocalBinding {
			continue
		}
		if imp.kind != "namespace" {
			if imp.local == "" {
				continue
			}
			out = append(out, pyBinding{prefix: imp.local, owner: imp.owner, kind: imp.kind, source: imp.source, imported: imp.imported})
			continue
		}
		// Aliased when the bound module is not the bare name the statement
		// would otherwise have put in scope.
		if imp.imported != imp.local {
			out = append(out, pyBinding{prefix: imp.local, owner: imp.owner, kind: "namespace", source: imp.source})
			continue
		}
		out = append(out, pyBinding{prefix: imp.source, owner: imp.owner, kind: "namespace", source: imp.source})
		if imp.local != imp.source {
			// `import a.b` also binds `a`, which reaches the package itself.
			out = append(out, pyBinding{prefix: imp.local, owner: imp.owner, kind: "namespace", source: imp.local})
		}
	}
	return out
}

// pythonScopeDecision is one edge's answer: which module and symbol its
// lexical scope proves it meant, and whether anything claimed the name at all.
type pythonScopeDecision struct {
	// spec is the import specifier naming the module to look in, and want the
	// symbol name to look for. Both empty on a claimed but undecidable edge:
	// it is vetoed and bound to nothing.
	spec, want string
	paths      []string
	// pkgPaths, when set, names the package `from pkg import name` imported
	// from. If that package binds `name` at module level itself, `name` is that
	// binding rather than the `pkg.name` submodule, and the edge is dropped.
	pkgPaths []string
	pkgName  string
}

// pythonScopeAnswers is what one pass over the Python evidence decides.
type pythonScopeAnswers struct {
	// claimed is every edge whose leading name the calling scope already gives
	// a meaning -- an import it can see, or a local binding of its own. A
	// repository-wide same-name match is a false edge for all of them, decided
	// or not.
	claimed map[int64]struct{}
	// imported and sameModule are the edges this pass can bind, by strategy.
	imported, sameModule map[int64]int64
}

func (a pythonScopeAnswers) handled() map[int64]struct{} {
	out := make(map[int64]struct{}, len(a.claimed)+len(a.sameModule))
	for id := range a.claimed {
		out[id] = struct{}{}
	}
	for id := range a.sameModule {
		out[id] = struct{}{}
	}
	return out
}

// resolvePythonScope binds Python call edges that repository-local import
// syntax proves, and blocks the weaker repo-wide strategies from guessing at
// the ones it proves are import-bound but cannot decide.
//
// Two evidence levels, both exact:
//
//   - python_import_scope: the call's leading name is bound by an import
//     visible in the call's own lexical scope, nothing nearer shadows it, that
//     import names exactly one repository module file, and that file declares
//     exactly one top-level symbol under the requested name.
//   - python_module_scope: the call is a bare name the calling file itself
//     declares exactly once at module level, with no import and no local
//     binding of that name in scope.
//
// Anything else stays unresolved on purpose. An import that names a module
// outside the repository, a module the layout cannot single out, or a symbol
// the target module does not declare, is still evidence: it says the call site
// meant *that* module, so binding a same-named symbol somewhere else would be a
// false edge. So is a parameter or an assignment of the same name -- the call
// site gave it a meaning that is not a repository symbol at all. Those edges
// are vetoed rather than handed on.
func resolvePythonScope(ctx context.Context, q execQuerier, repoID int64, only map[int64]struct{}, recordClaims bool) (int, map[int64]struct{}, error) {
	answers, err := pythonScopeDecide(ctx, q, repoID, only, false)
	if err != nil {
		return 0, nil, err
	}
	if recordClaims {
		// Only the transaction-scoped caller needs the table: the repo-wide
		// strategies that run after it read the veto from there. The
		// incremental entrypoint reads the returned set instead, and writing a
		// temp table it never queries would leave rows on the pooled
		// connection for nothing.
		if err := pythonScopeRecordClaims(ctx, q, answers.claimed); err != nil {
			return 0, nil, err
		}
	}
	total := 0
	for _, group := range []struct {
		results  map[int64]int64
		strategy string
	}{
		{answers.imported, ResolutionStrategyPythonImportScope},
		{answers.sameModule, ResolutionStrategyPythonModuleScope},
	} {
		n, err := pythonScopeApply(ctx, q, group.results, group.strategy)
		if err != nil {
			return 0, nil, err
		}
		total += n
	}
	return total, answers.handled(), nil
}

// recordPythonScopeClaims fills the veto table without binding anything, for
// the transactions that run a repo-wide strategy without running the Python
// pass -- resolveDotSuffixIncrementally. It is the same decision code, so there
// is no second implementation of "claimed" to keep in step with the first.
func recordPythonScopeClaims(ctx context.Context, q execQuerier, repoID int64) error {
	answers, err := pythonScopeDecide(ctx, q, repoID, nil, true)
	if err != nil {
		return err
	}
	return pythonScopeRecordClaims(ctx, q, answers.claimed)
}

func pythonScopeRecordClaims(ctx context.Context, q execQuerier, claims map[int64]struct{}) error {
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+pyScopeVeto+`(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return err
	}
	claimed := make([]int64, 0, len(claims))
	for id := range claims {
		claimed = append(claimed, id)
	}
	sort.Slice(claimed, func(i, j int) bool { return claimed[i] < claimed[j] })
	for start := 0; start < len(claimed); start += 400 {
		end := min(start+400, len(claimed))
		args := make([]any, 0, end-start)
		for _, id := range claimed[start:end] {
			args = append(args, id)
		}
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO `+pyScopeVeto+`(edge_id) VALUES `+valuePlaceholders(end-start), args...); err != nil {
			return err
		}
	}
	return nil
}

// pythonScopeDecide walks the Python evidence once. With claimsOnly it stops
// after the claim decision, which needs no module lookup and no symbol load --
// that is all the veto-only caller reads, and the two passes it skips are the
// expensive ones.
func pythonScopeDecide(ctx context.Context, q execQuerier, repoID int64, only map[int64]struct{}, claimsOnly bool) (pythonScopeAnswers, error) {
	answers := pythonScopeAnswers{
		claimed:    map[int64]struct{}{},
		imported:   map[int64]int64{},
		sameModule: map[int64]int64{},
	}
	edges, err := pythonScopeEdges(ctx, q, repoID, only)
	if err != nil || len(edges) == 0 {
		return answers, err
	}

	callerIDs := make(map[int64]struct{}, len(edges))
	srcIDs := make(map[int64]struct{}, len(edges))
	for _, e := range edges {
		callerIDs[e.file] = struct{}{}
		if e.src != 0 {
			srcIDs[e.src] = struct{}{}
		}
	}
	callerPaths, err := pythonScopeFilePaths(ctx, q, repoID, sortedIDs(callerIDs))
	if err != nil {
		return answers, err
	}
	imports, locals, err := pythonScopeImports(ctx, q, repoID, sortedIDs(callerIDs))
	if err != nil {
		return answers, err
	}
	scopes, err := pythonScopeCallScopes(ctx, q, repoID, sortedIDs(srcIDs), callerPaths)
	if err != nil {
		return answers, err
	}
	bindings := make(map[int64][]pyBinding, len(imports))
	for file, list := range imports {
		bindings[file] = pythonBindingsOf(list)
	}

	// Pass 1: decide, per edge, what its own lexical scope says its leading
	// name means, and collect the module candidates every claim needs.
	decisions := make(map[int64]pythonScopeDecision, len(edges))
	wildcardFiles := make(map[int64]bool, len(imports))
	candidates := map[string]struct{}{}
	for file, list := range imports {
		for _, imp := range list {
			if imp.wildcard {
				wildcardFiles[file] = true
			}
		}
	}
	for _, e := range edges {
		at := scopes[e.src]
		binding, member, claimed := pythonBindingFor(bindings[e.file], at, e.name)
		if pythonNameIsShadowed(locals[e.file], at, pythonLeadingSegment(e.name), binding.owner, claimed) {
			// The call site bound the name itself. Whatever it holds, it is not
			// a repository symbol this resolver can name.
			answers.claimed[e.id] = struct{}{}
			continue
		}
		if !claimed {
			continue
		}
		var d pythonScopeDecision
		switch {
		case binding.kind == "namespace":
			// `import pkg.helpers` / `import pkg.helpers as h`: the binding is
			// the module, so a single member spelling names a symbol in it.
			// `h.a.b()` would need `a` resolved as a submodule of a submodule,
			// which this pass does not model.
			if pythonSingleSegment(member) {
				d = pythonScopeDecision{spec: binding.source, want: member}
			}
		case member == "":
			// `from pkg.helpers import load [as alias]`: the local name is the
			// imported symbol itself.
			d = pythonScopeDecision{spec: binding.source, want: binding.imported}
		case pythonSingleSegment(member):
			// `from pkg import helpers` + `helpers.load()`: the imported name is
			// a submodule of the imported package as often as it is an object
			// with attributes, and only the submodule reading names something
			// this graph holds. Look for `pkg.helpers` and take `load` from it,
			// unless `pkg` itself binds `helpers` at module level -- then the
			// name is that binding, not the submodule, and nothing is decidable.
			d = pythonScopeDecision{
				spec:     pythonSubmoduleSpecifier(binding.source, binding.imported),
				want:     member,
				pkgPaths: pythonModuleCandidatePaths(callerPaths[e.file], binding.source),
				pkgName:  binding.imported,
			}
		}
		if d.spec != "" {
			d.paths = pythonModuleCandidatePaths(callerPaths[e.file], d.spec)
			for _, candidate := range d.paths {
				candidates[candidate] = struct{}{}
			}
			for _, candidate := range d.pkgPaths {
				candidates[candidate] = struct{}{}
			}
		}
		decisions[e.id] = d
		answers.claimed[e.id] = struct{}{}
	}

	if claimsOnly {
		return answers, nil
	}

	moduleFiles, err := pythonScopeFileIDsByPath(ctx, q, repoID, candidates)
	if err != nil {
		return answers, err
	}

	// Pass 2: turn each decision into at most one target file, then load the
	// symbols of every file that can still answer something.
	targetFile := make(map[int64]int64, len(decisions))
	packageFile := make(map[int64]int64, len(decisions))
	packageFiles := map[int64]struct{}{}
	symbolFiles := make(map[int64]struct{}, len(decisions)+len(callerIDs))
	for _, e := range edges {
		d, ok := decisions[e.id]
		if !ok {
			// A claimed edge with no decision is already answered; only the
			// unclaimed ones can still reach their own module's declarations.
			if _, claimed := answers.claimed[e.id]; !claimed {
				symbolFiles[e.file] = struct{}{}
			}
			continue
		}
		var found int64
		hits := 0
		for _, candidate := range d.paths {
			if id, exists := moduleFiles[candidate]; exists && id != found {
				found = id
				hits++
			}
		}
		if hits != 1 {
			continue
		}
		targetFile[e.id] = found
		symbolFiles[found] = struct{}{}
		switch id, hits := pythonUniqueCandidateFile(d.pkgPaths, moduleFiles); {
		case hits == 1:
			packageFile[e.id] = id
			packageFiles[id] = struct{}{}
			symbolFiles[id] = struct{}{}
		case hits > 1:
			// `pkg.py` and `pkg/__init__.py` both exist: which one `pkg` names
			// is undecidable, so what it exposes as `helpers` is too.
			delete(targetFile, e.id)
		}
	}
	topLevel, err := pythonScopeTopLevelSymbols(ctx, q, repoID, sortedIDs(symbolFiles))
	if err != nil {
		return answers, err
	}
	// A package's own module-level bindings decide whether `from pkg import x`
	// named the `pkg.x` submodule or something `pkg/__init__.py` bound itself.
	pkgImports, pkgLocals, err := pythonScopeImports(ctx, q, repoID, sortedIDs(packageFiles))
	if err != nil {
		return answers, err
	}
	pkgPaths := make(map[int64]string, len(packageFiles))
	for candidate, id := range moduleFiles {
		if _, ok := packageFiles[id]; ok {
			pkgPaths[id] = candidate
		}
	}
	pkgBindings := make(map[int64][]pyBinding, len(packageFiles))
	for id := range packageFiles {
		pkgBindings[id] = pythonBindingsOf(pkgImports[id])
	}

	// Pass 3: bind.
	for _, e := range edges {
		d, decided := decisions[e.id]
		if !decided {
			if _, claimed := answers.claimed[e.id]; claimed {
				continue
			}
			if strings.Contains(e.name, ".") || wildcardFiles[e.file] {
				continue
			}
			// Only module-level `def`s. A bare spelling that could name a type
			// stays with the repository-wide type-scope rule (P22.9), which
			// refuses a class a call site cannot be shown to have meant.
			if g := topLevel[e.file][e.name]; g != nil && g.declarations == 1 && len(g.functions) == 1 {
				answers.sameModule[e.id] = g.functions[0]
			}
			continue
		}
		file, ok := targetFile[e.id]
		if !ok || d.want == "" {
			continue
		}
		if pkg, checked := packageFile[e.id]; checked &&
			pythonPackageRebindsName(pkgPaths[pkg], pkgImports[pkg], pkgBindings[pkg], pkgLocals[pkg], topLevel[pkg], d.pkgName, d.paths) {
			continue
		}
		if g := topLevel[file][d.want]; g != nil && g.declarations == 1 && len(g.topLevel) == 1 {
			answers.imported[e.id] = g.topLevel[0]
		}
	}
	return answers, nil
}

// pythonNameIsShadowed reports whether the call site, or a scope between it and
// the import, binds the leading name itself.
//
// "Between" includes the import's own scope: `h = factory()` beside
// `import pkg.helpers as h` rebinds it either way round, and which of the two
// runs last is control flow this resolver does not model. When no import
// claimed the name at all, any visible local binding of it still shadows the
// module's own declarations.
//
// A nested declaration is the exception to that last rule: with no import to
// shadow, `def inner()` inside `run()` is the target of `inner()`, not a reason
// to refuse one. It shadows an import as any local does, and answers for
// itself otherwise.
func pythonNameIsShadowed(locals []pyScopeLocal, at, name, importOwner string, claimed bool) bool {
	for _, local := range locals {
		if local.name != name || !pythonScopeVisible(local.owner, at) {
			continue
		}
		if !claimed {
			if !local.declaration {
				return true
			}
			continue
		}
		if len(local.owner) >= len(importOwner) {
			return true
		}
	}
	return false
}

// pythonSingleSegment reports whether a member spelling names one attribute --
// the only depth this pass can decide.
func pythonSingleSegment(member string) bool {
	return member != "" && !strings.Contains(member, ".")
}

// pythonSubmoduleSpecifier spells the submodule an imported name may be. The
// leading dots of a relative import are part of the specifier, so `from . import
// helpers` becomes `.helpers` and not `..helpers`.
func pythonSubmoduleSpecifier(source, imported string) string {
	if strings.HasSuffix(source, ".") {
		return source + imported
	}
	return source + "." + imported
}

// pythonBindingFor picks the import that claims a call's leading name, out of
// the ones the call's own lexical scope can see.
//
// Nearest scope wins, because that is how Python resolves the leading name: a
// function-local import shadows a module-level one, and a module-level import
// is invisible to a call in a file that never runs it. Within one scope the
// longest matching spelling wins -- `import pkg.helpers` binds both `pkg` and
// `pkg.helpers`, and the latter is the stronger claim on `pkg.helpers.load`.
//
// Two different imports claiming the same spelling at the same distance is not
// a tie to break: it reports a claim with no decidable target, so the edge is
// vetoed and left unresolved.
func pythonBindingFor(bindings []pyBinding, at, name string) (pyBinding, string, bool) {
	var best pyBinding
	bestOwner, bestPrefix, matches := -1, 0, 0
	for _, b := range bindings {
		if b.prefix == "" || !pythonScopeVisible(b.owner, at) {
			continue
		}
		if name != b.prefix && !strings.HasPrefix(name, b.prefix+".") {
			continue
		}
		switch {
		case len(b.owner) > bestOwner || (len(b.owner) == bestOwner && len(b.prefix) > bestPrefix):
			best, bestOwner, bestPrefix, matches = b, len(b.owner), len(b.prefix), 1
		case len(b.owner) == bestOwner && len(b.prefix) == bestPrefix &&
			(b.source != best.source || b.imported != best.imported || b.kind != best.kind):
			matches++
		}
	}
	if bestOwner < 0 {
		return pyBinding{}, "", false
	}
	if matches != 1 {
		// Claimed, undecidable: report the claim with an unusable member so the
		// caller vetoes the edge and binds nothing.
		return pyBinding{owner: best.owner, kind: "ambiguous"}, "", true
	}
	if best.owner != "" {
		// A function-local import binds the name for the whole function, but
		// nothing in this evidence says whether the import statement runs
		// before the call: `load(); from pkg.helpers import load` is an
		// UnboundLocalError, not an edge, and the two spellings are
		// indistinguishable without a persisted source position.
		//
		// So a function-local import is claim and veto evidence only. It stops
		// a module-level import and every weaker repository-wide strategy from
		// binding a name the function rebound, and it never binds one itself.
		// Module-level imports, whose statements always run before any call in
		// the module's functions, still resolve.
		return pyBinding{owner: best.owner, kind: "local-import"}, "", true
	}
	return best, strings.TrimPrefix(name[bestPrefix:], "."), true
}

// pythonUniqueCandidateFile reports the file a candidate list names and how
// many of the candidates exist. More than one is an ambiguous layout, which
// every caller must refuse rather than pick from.
func pythonUniqueCandidateFile(paths []string, files map[string]int64) (int64, int) {
	var found int64
	hits := 0
	for _, candidate := range paths {
		if id, exists := files[candidate]; exists && id != found {
			found = id
			hits++
		}
	}
	return found, hits
}

// pythonPackageRebindsName reports whether `pkg/__init__.py` binds `name` at
// module level as something other than the submodule at `subPaths`.
//
// `from pkg import name` binds whatever `pkg` exposes as `name`. Python looks
// at the package's own namespace first, so a `name = Obj()`, a `def name`, a
// `class name` or an import that binds `name` to anything else all win over the
// `pkg/name.py` submodule -- and none of them is a target this resolver can
// name. The one module-level binding that proves the submodule is an import of
// the submodule itself (`from . import name`), which is exactly the re-export
// that makes the submodule reading right.
//
// No object or attribute inference happens here: every check is syntax already
// recorded as evidence.
func pythonPackageRebindsName(pkgPath string, imports []pyScopeImport, bindings []pyBinding, locals []pyScopeLocal, symbols map[string]*pySymbolGroup, name string, subPaths []string) bool {
	if name == "" {
		return false
	}
	if g := symbols[name]; g != nil && len(g.topLevel) > 0 {
		return true
	}
	for _, imp := range imports {
		// `from .base import *` at module level can bind any name, including
		// this one, and nothing in the syntax says whether it did.
		if imp.wildcard && imp.owner == "" {
			return true
		}
	}
	for _, local := range locals {
		if local.owner == "" && local.name == name {
			return true
		}
	}
	wanted := make(map[string]struct{}, len(subPaths))
	for _, p := range subPaths {
		wanted[p] = struct{}{}
	}
	for _, b := range bindings {
		if b.owner != "" || b.prefix != name {
			continue
		}
		spec := b.source
		if b.kind != "namespace" {
			spec = pythonSubmoduleSpecifier(b.source, b.imported)
		}
		proven := false
		for _, candidate := range pythonModuleCandidatePaths(pkgPath, spec) {
			if _, ok := wanted[candidate]; ok {
				proven = true
				break
			}
		}
		if !proven {
			return true
		}
	}
	return false
}

func pythonScopeEdges(ctx context.Context, q execQuerier, repoID int64, only map[int64]struct{}) ([]pyScopeEdge, error) {
	where, args := "", []any{repoID}
	if len(only) > 0 {
		ids := sortedIDs(only)
		where = ` AND e.id IN (` + tsPlaceholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT e.id,e.file_id,e.src_symbol_id,e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='python' AND f.is_deleted=0 AND e.dst_symbol_id IS NULL AND e.dst_name != '' AND e.edge_kind <> 'cross_language_ref'`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []pyScopeEdge
	for rows.Next() {
		var e pyScopeEdge
		if err := rows.Scan(&e.id, &e.file, &e.src, &e.name); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

func pythonScopeFilePaths(ctx context.Context, q execQuerier, repoID int64, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	err := chunkedInt64Query(ctx, q, ids, `SELECT id,path FROM files WHERE repo_id=? AND id IN (`, repoID, func(scan func(...any) error) error {
		var id int64
		var p string
		if err := scan(&id, &p); err != nil {
			return err
		}
		out[id] = canonicalStoredPath(p)
		return nil
	})
	return out, err
}

// pythonScopeImports loads one file's scope evidence, separating the import
// bindings from the negative evidence about what each lexical scope binds
// itself. Both live in scope_import_evidence: the table records syntax-proven
// scope facts, and `import_kind` says which kind of fact a row is.
//
// `owner_module` carries the lexical scope for Python. The column was added for
// Rust module ownership (migration 030) and every Rust reader filters on
// `language='rust'`, so the two meanings never meet.
func pythonScopeImports(ctx context.Context, q execQuerier, repoID int64, ids []int64) (map[int64][]pyScopeImport, map[int64][]pyScopeLocal, error) {
	imports := make(map[int64][]pyScopeImport, len(ids))
	locals := make(map[int64][]pyScopeLocal, len(ids))
	err := chunkedInt64Query(ctx, q, ids, `SELECT file_id,source_specifier,imported_name,local_name,import_kind,wildcard,owner_module FROM scope_import_evidence WHERE repo_id=? AND language='python' AND file_id IN (`, repoID, func(scan func(...any) error) error {
		var file int64
		var imp pyScopeImport
		var wildcard int
		if err := scan(&file, &imp.source, &imp.imported, &imp.local, &imp.kind, &wildcard, &imp.owner); err != nil {
			return err
		}
		if imp.kind == graph.ScopeImportLocalBinding || imp.kind == graph.ScopeImportNestedDeclaration {
			if imp.local != "" {
				locals[file] = append(locals[file], pyScopeLocal{
					owner:       imp.owner,
					name:        imp.local,
					declaration: imp.kind == graph.ScopeImportNestedDeclaration,
				})
			}
			return nil
		}
		imp.wildcard = wildcard != 0
		imports[file] = append(imports[file], imp)
		return nil
	})
	return imports, locals, err
}

// pythonScopeCallScopes maps each call's source symbol to the lexical scope it
// was written in, so evidence can be filtered to what that scope can see.
func pythonScopeCallScopes(ctx context.Context, q execQuerier, repoID int64, ids []int64, filePaths map[int64]string) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	err := chunkedInt64Query(ctx, q, ids, `SELECT id,file_id,qualified_name FROM symbols WHERE repo_id=? AND language='python' AND id IN (`, repoID, func(scan func(...any) error) error {
		var id, file int64
		var qualified string
		if err := scan(&id, &file, &qualified); err != nil {
			return err
		}
		out[id] = pythonCallScope(qualified, filePaths[file])
		return nil
	})
	return out, err
}

// pySymbolGroup is what one file declares under one name.
//
// declarations counts every declaration of the name anywhere in the file, not
// just the module-level ones: a module that declares `Handler` twice -- once as
// a class, once as a function -- has no single answer for `Handler`, and the
// module-level filter alone would hide the collision instead of reporting it.
type pySymbolGroup struct {
	topLevel     []int64
	functions    []int64
	declarations int
}

// pythonScopeTopLevelSymbols indexes, per file, what that file declares. A
// nested function or a method is not what a bare name or an imported name
// reaches, and the parser records that distinction in the qualified name: a
// module-level declaration is exactly `<module>.<name>`.
func pythonScopeTopLevelSymbols(ctx context.Context, q execQuerier, repoID int64, ids []int64) (map[int64]map[string]*pySymbolGroup, error) {
	out := make(map[int64]map[string]*pySymbolGroup, len(ids))
	err := chunkedInt64Query(ctx, q, ids, `SELECT s.id,s.file_id,s.name,s.qualified_name,s.kind FROM symbols s WHERE s.repo_id=? AND s.language='python' AND s.file_id IN (`, repoID, func(scan func(...any) error) error {
		var id, file int64
		var name, qualified, kind string
		if err := scan(&id, &file, &name, &qualified, &kind); err != nil {
			return err
		}
		if name == "" {
			return nil
		}
		if out[file] == nil {
			out[file] = map[string]*pySymbolGroup{}
		}
		group := out[file][name]
		if group == nil {
			group = &pySymbolGroup{}
			out[file][name] = group
		}
		group.declarations++
		if strings.Count(qualified, ".") != 1 || !strings.HasSuffix(qualified, "."+name) {
			return nil
		}
		group.topLevel = append(group.topLevel, id)
		if kind == "function" {
			group.functions = append(group.functions, id)
		}
		return nil
	})
	return out, err
}

func pythonScopeFileIDsByPath(ctx context.Context, q execQuerier, repoID int64, canonical map[string]struct{}) (map[string]int64, error) {
	if len(canonical) == 0 {
		return nil, nil
	}
	variants := make(map[string]string, len(canonical)*2)
	for p := range canonical {
		for _, variant := range storedPathVariants(p) {
			variants[variant] = p
		}
	}
	lookup := make([]string, 0, len(variants))
	for variant := range variants {
		lookup = append(lookup, variant)
	}
	sort.Strings(lookup)
	out := make(map[string]int64, len(canonical))
	for start := 0; start < len(lookup); start += 400 {
		end := min(start+400, len(lookup))
		args := make([]any, 0, end-start+1)
		args = append(args, repoID)
		for _, variant := range lookup[start:end] {
			args = append(args, variant)
		}
		rows, err := q.QueryContext(ctx, `SELECT path,id FROM files WHERE repo_id=? AND is_deleted=0 AND language='python' AND path IN (`+tsPlaceholders(end-start)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			var id int64
			if err := rows.Scan(&p, &id); err != nil {
				rows.Close()
				return nil, err
			}
			out[variants[p]] = id
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// chunkedInt64Query runs one query per bounded slice of ids, so evidence is
// loaded per repository or per scoped batch rather than per edge.
func chunkedInt64Query(ctx context.Context, q execQuerier, ids []int64, prefix string, repoID int64, scanRow func(func(...any) error) error) error {
	for start := 0; start < len(ids); start += 400 {
		end := min(start+400, len(ids))
		args := make([]any, 0, end-start+1)
		args = append(args, repoID)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := q.QueryContext(ctx, prefix+tsPlaceholders(end-start)+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := scanRow(rows.Scan); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func pythonScopeApply(ctx context.Context, q execQuerier, results map[int64]int64, strategy string) (int, error) {
	if len(results) == 0 {
		return 0, nil
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+pyScopeResolution+`(edge_id INTEGER PRIMARY KEY,dst_symbol_id INTEGER NOT NULL) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM `+pyScopeResolution); err != nil {
		return 0, err
	}
	ids := make([]int64, 0, len(results))
	for id := range results {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for start := 0; start < len(ids); start += 400 {
		end := min(start+400, len(ids))
		vals := make([]any, 0, (end-start)*2)
		for _, id := range ids[start:end] {
			vals = append(vals, id, results[id])
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO `+pyScopeResolution+`(edge_id,dst_symbol_id) VALUES `+pairPlaceholders(end-start), vals...); err != nil {
			return 0, err
		}
	}
	if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT dst_symbol_id FROM `+pyScopeResolution+` r WHERE r.edge_id=edges.id),resolution_strategy='`+strategy+`',resolution_confidence='`+resolutionConfidenceFor(strategy)+`' WHERE id IN (SELECT edge_id FROM `+pyScopeResolution+`)`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM `+pyScopeResolution); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// invalidatePythonScopeBindings clears the Python call edges whose scope answer
// a batch of changed files could have altered: the changed files' own edges,
// and the edges of every Python file whose import candidates name one of them.
//
// The reverse direction is the one that needs persisted evidence. A module that
// is renamed away, deleted, or that loses the symbol an importer asked for, no
// longer appears in any candidate path computed from the importer -- so the
// importer has to be found from the module side, which is what
// scope_module_candidate_evidence records at index time.
//
// Unlike TypeScript re-exports, a Python import binding never forwards through
// another module here, so one reverse step is the whole dependent set.
func (s *Store) invalidatePythonScopeBindings(ctx context.Context, repoID int64, paths []string) ([]string, error) {
	changed, err := fileIDsByPaths(ctx, s.db, repoID, paths)
	if err != nil {
		return nil, err
	}
	affected := make(map[int64]struct{}, len(changed))
	for _, id := range changed {
		affected[id] = struct{}{}
	}
	canonical := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		p = CanonicalRelPath(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		canonical = append(canonical, p)
	}
	sort.Strings(canonical)
	for start := 0; start < len(canonical); start += 400 {
		end := min(start+400, len(canonical))
		args := make([]any, 0, end-start+1)
		args = append(args, repoID)
		for _, p := range canonical[start:end] {
			args = append(args, p)
		}
		rows, qerr := s.db.QueryContext(ctx, `
			SELECT c.source_file_id
			FROM scope_module_candidate_evidence c
			JOIN files f ON f.id = c.source_file_id AND f.repo_id = c.repo_id
			WHERE c.repo_id = ? AND f.language = 'python' AND c.candidate_path IN (`+tsPlaceholders(end-start)+`)
		`, args...)
		if qerr != nil {
			return nil, qerr
		}
		for rows.Next() {
			var id int64
			if qerr = rows.Scan(&id); qerr != nil {
				rows.Close()
				return nil, qerr
			}
			affected[id] = struct{}{}
		}
		if qerr = rows.Close(); qerr != nil {
			return nil, qerr
		}
	}
	// An `__init__.py` appearing, disappearing or changing decides whether its
	// directory is an importable package at all and what that package binds at
	// module level. The candidate reverse-lookup above already covers callers
	// that named it; the subtree is swept as well because a package's own files
	// are the ones whose relative imports it answers.
	for _, p := range canonical {
		if path.Base(p) != "__init__.py" {
			continue
		}
		dir := pythonParentDir(p)
		prefix := dir + "/"
		if dir == "" {
			prefix = ""
		}
		rows, qerr := s.db.QueryContext(ctx, `
			SELECT id FROM files
			WHERE repo_id = ? AND is_deleted = 0 AND language = 'python' AND path LIKE ? ESCAPE '\'
		`, repoID, escapeSQLLikePrefix(prefix)+`%`)
		if qerr != nil {
			return nil, qerr
		}
		for rows.Next() {
			var id int64
			if qerr = rows.Scan(&id); qerr != nil {
				rows.Close()
				return nil, qerr
			}
			affected[id] = struct{}{}
		}
		if qerr = rows.Close(); qerr != nil {
			return nil, qerr
		}
	}
	if len(affected) == 0 {
		return nil, nil
	}
	var edgeIDs []int64
	names := map[string]struct{}{}
	if err := chunkedInt64Query(ctx, s.db, sortedIDs(affected),
		`SELECT e.id,e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='python' AND e.edge_kind <> 'cross_language_ref' AND e.file_id IN (`,
		repoID, func(scan func(...any) error) error {
			var id int64
			var name string
			if err := scan(&id, &name); err != nil {
				return err
			}
			edgeIDs = append(edgeIDs, id)
			if name != "" {
				names[name] = struct{}{}
			}
			return nil
		}); err != nil {
		return nil, err
	}
	if _, err := clearEdgeResolutions(ctx, s.db, edgeIDs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// escapeSQLLikePrefix makes a stored path safe as a LIKE prefix: `_` and `%`
// are legal in a filename and wildcards in a pattern.
func escapeSQLLikePrefix(prefix string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`)
	return replacer.Replace(prefix)
}
