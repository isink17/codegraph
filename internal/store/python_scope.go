package store

import (
	"context"
	"path"
	"sort"
	"strings"
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
// else, so it yields one candidate pair or none. An absolute import has no
// single anchor -- a repository may or may not use a src-layout root -- so it
// yields the same pair under the importing file's directory and under every
// ancestor up to the repository root. Two of those existing at once is a
// layout the resolver cannot decide, and it binds nothing rather than pick.
//
// ponytail: an absolute import costs 2*(depth+1) candidates whether or not the
// module is repository-local, and those are persisted per import at index time
// (scope_module_candidate_evidence). Third-party imports pay it for nothing. If
// a deep tree's candidate table measures large, the lever is to stop the
// ancestor walk at the first root that actually holds the first segment as a
// directory -- which needs a repository file index this pure function does not
// have, so it belongs in the caller, not here.
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
	dir := path.Dir(canonicalStoredPath(sourceFile))
	if dir == "." || dir == "/" {
		dir = ""
	}
	if dots > 0 {
		// `.x` is the importing file's own package; each further dot climbs one.
		for i := 1; i < dots; i++ {
			if dir == "" {
				return nil
			}
			dir = path.Dir(dir)
			if dir == "." || dir == "/" {
				dir = ""
			}
		}
		return pythonModuleFiles(dir, segments)
	}
	if len(segments) == 0 {
		return nil
	}
	var out []string
	for {
		out = append(out, pythonModuleFiles(dir, segments)...)
		if dir == "" {
			return out
		}
		dir = path.Dir(dir)
		if dir == "." || dir == "/" {
			dir = ""
		}
	}
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
	source, imported, local, kind string
	wildcard                      bool
}

type pyScopeEdge struct {
	id, file int64
	name     string
}

// resolvePythonScope binds Python call edges that repository-local import
// syntax proves, and blocks the weaker repo-wide strategies from guessing at
// the ones it proves are import-bound but cannot decide.
//
// Two evidence levels, both exact:
//
//   - python_import_scope: the call's leading name is bound by an import in the
//     calling file, that import names exactly one repository module file, and
//     that file declares exactly one top-level symbol under the requested name.
//   - python_module_scope: the call is a bare name the calling file itself
//     declares exactly once at module level, and no import in the file binds
//     that name.
//
// Anything else stays unresolved on purpose. An import that names a module
// outside the repository, a module the layout cannot single out, or a symbol
// the target module does not declare, is still evidence: it says the call site
// meant *that* module, so binding a same-named symbol somewhere else would be a
// false edge. Those edges are vetoed rather than handed on.
func resolvePythonScope(ctx context.Context, q execQuerier, repoID int64, only map[int64]struct{}) (int, map[int64]struct{}, error) {
	edges, err := pythonScopeEdges(ctx, q, repoID, only)
	if err != nil || len(edges) == 0 {
		return 0, nil, err
	}

	callerIDs := make(map[int64]struct{}, len(edges))
	for _, e := range edges {
		callerIDs[e.file] = struct{}{}
	}
	callerPaths, err := pythonScopeFilePaths(ctx, q, repoID, sortedIDs(callerIDs))
	if err != nil {
		return 0, nil, err
	}
	imports, err := pythonScopeImports(ctx, q, repoID, sortedIDs(callerIDs))
	if err != nil {
		return 0, nil, err
	}

	// Pass 1: decide, per edge, which import binding (if any) claims its leading
	// name, and what module file and symbol name that binding asks for. Both are
	// decidable from syntax alone, so the module candidates every claim needs
	// are collected in this single walk.
	type decision struct {
		// spec is the import specifier naming the module to look in, and want
		// the symbol name to look for. Both empty means the edge is claimed but
		// undecidable: it is vetoed and bound to nothing.
		spec, want string
		paths      []string
	}
	decisions := make(map[int64]decision, len(edges))
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
		binding, member, ok := pythonBindingFor(imports[e.file], e.name)
		if !ok {
			continue
		}
		var d decision
		switch {
		case binding.kind == "namespace":
			// `import pkg.helpers` / `import pkg.helpers as h`: the binding is
			// the module, so a single member spelling names a symbol in it.
			// `h.a.b()` would need `a` resolved as a submodule of a submodule,
			// which this pass does not model.
			if pythonSingleSegment(member) {
				d = decision{spec: binding.source, want: member}
			}
		case member == "":
			// `from pkg.helpers import load [as alias]`: the local name is the
			// imported symbol itself.
			d = decision{spec: binding.source, want: binding.imported}
		case pythonSingleSegment(member):
			// `from pkg import helpers` + `helpers.load()`: the imported name is
			// a submodule of the imported package as often as it is an object
			// with attributes, and only the submodule reading names something
			// this graph holds. Look for `pkg.helpers` and take `load` from it.
			d = decision{spec: pythonSubmoduleSpecifier(binding.source, binding.imported), want: member}
		}
		if d.spec != "" {
			d.paths = pythonModuleCandidatePaths(callerPaths[e.file], d.spec)
			for _, candidate := range d.paths {
				candidates[candidate] = struct{}{}
			}
		}
		decisions[e.id] = d
	}

	moduleFiles, err := pythonScopeFileIDsByPath(ctx, q, repoID, candidates)
	if err != nil {
		return 0, nil, err
	}

	// Pass 2: turn each decision into at most one target file, then load the
	// symbols of every file that can still answer something.
	targetFile := make(map[int64]int64, len(decisions))
	symbolFiles := make(map[int64]struct{}, len(decisions)+len(callerIDs))
	for _, e := range edges {
		d, ok := decisions[e.id]
		if !ok {
			symbolFiles[e.file] = struct{}{}
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
	}
	topLevel, err := pythonScopeTopLevelSymbols(ctx, q, repoID, sortedIDs(symbolFiles))
	if err != nil {
		return 0, nil, err
	}

	// Pass 3: bind.
	imported := map[int64]int64{}
	sameModule := map[int64]int64{}
	for _, e := range edges {
		d, claimed := decisions[e.id]
		if !claimed {
			if strings.Contains(e.name, ".") || wildcardFiles[e.file] {
				continue
			}
			// Only module-level `def`s. A bare spelling that could name a type
			// stays with the repository-wide type-scope rule (P22.9), which
			// refuses a class a call site cannot be shown to have meant.
			if g := topLevel[e.file][e.name]; g != nil && g.declarations == 1 && len(g.functions) == 1 {
				sameModule[e.id] = g.functions[0]
			}
			continue
		}
		file, ok := targetFile[e.id]
		if !ok || d.want == "" {
			continue
		}
		if g := topLevel[file][d.want]; g != nil && g.declarations == 1 && len(g.topLevel) == 1 {
			imported[e.id] = g.topLevel[0]
		}
	}

	// handled is what the generic strategies must not touch afterwards: every
	// edge an import claimed (decided or not) plus every edge this pass bound.
	handled := make(map[int64]struct{}, len(decisions)+len(sameModule))
	for id := range decisions {
		handled[id] = struct{}{}
	}
	for id := range sameModule {
		handled[id] = struct{}{}
	}
	total := 0
	for _, group := range []struct {
		results  map[int64]int64
		strategy string
	}{
		{imported, ResolutionStrategyPythonImportScope},
		{sameModule, ResolutionStrategyPythonModuleScope},
	} {
		n, err := pythonScopeApply(ctx, q, group.results, group.strategy)
		if err != nil {
			return 0, nil, err
		}
		total += n
	}
	return total, handled, nil
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

// pythonBindingFor picks the import that claims a call's leading name. The
// longest matching local binding wins -- `import pkg.helpers` binds the whole
// dotted spelling, and that is a stronger claim on `pkg.helpers.load` than a
// bare `pkg` binding would be. Two different bindings claiming the same name is
// not a tie to break: it reports a claim with no decidable target, so the edge
// is vetoed and left unresolved.
func pythonBindingFor(imports []pyScopeImport, name string) (pyScopeImport, string, bool) {
	var best pyScopeImport
	bestLen, matches := 0, 0
	for _, imp := range imports {
		if imp.wildcard || imp.local == "" {
			continue
		}
		if name != imp.local && !strings.HasPrefix(name, imp.local+".") {
			continue
		}
		if len(imp.local) > bestLen {
			best, bestLen, matches = imp, len(imp.local), 1
			continue
		}
		if len(imp.local) == bestLen && (imp.source != best.source || imp.imported != best.imported || imp.kind != best.kind) {
			matches++
		}
	}
	if bestLen == 0 {
		return pyScopeImport{}, "", false
	}
	if matches != 1 {
		// Claimed, undecidable: report the claim with an unusable member so the
		// caller vetoes the edge and binds nothing.
		return pyScopeImport{kind: "ambiguous"}, "", true
	}
	return best, strings.TrimPrefix(name[bestLen:], "."), true
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
	rows, err := q.QueryContext(ctx, `SELECT e.id,e.file_id,e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='python' AND f.is_deleted=0 AND e.dst_symbol_id IS NULL AND e.dst_name != '' AND e.edge_kind <> 'cross_language_ref'`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []pyScopeEdge
	for rows.Next() {
		var e pyScopeEdge
		if err := rows.Scan(&e.id, &e.file, &e.name); err != nil {
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

func pythonScopeImports(ctx context.Context, q execQuerier, repoID int64, ids []int64) (map[int64][]pyScopeImport, error) {
	out := make(map[int64][]pyScopeImport, len(ids))
	err := chunkedInt64Query(ctx, q, ids, `SELECT file_id,source_specifier,imported_name,local_name,import_kind,wildcard FROM scope_import_evidence WHERE repo_id=? AND language='python' AND file_id IN (`, repoID, func(scan func(...any) error) error {
		var file int64
		var imp pyScopeImport
		var wildcard int
		if err := scan(&file, &imp.source, &imp.imported, &imp.local, &imp.kind, &wildcard); err != nil {
			return err
		}
		imp.wildcard = wildcard != 0
		out[file] = append(out[file], imp)
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

// recordPythonScopeVeto marks every Python call edge whose leading name an
// import statement in the calling file binds. Claiming is what matters, not
// resolving: once the file's syntax says a name came from a specific module, a
// repo-wide same-name match elsewhere is a false edge whether or not
// resolvePythonScope could decide the real target.
//
// It is the SQL twin of pythonBindingFor's claim test and must stay one: the
// Go pass decides destinations, this decides who else may not. Set-based, one
// statement per repository -- never per edge. `substr` rather than `LIKE`
// because a local binding may legitimately contain `_`, which LIKE would treat
// as a wildcard.
func recordPythonScopeVeto(ctx context.Context, q execQuerier, repoID int64) error {
	_, err := q.ExecContext(ctx, `
		INSERT OR IGNORE INTO `+pyScopeVeto+`(edge_id)
		SELECT e.id
		FROM edges e
		JOIN files f ON f.id = e.file_id AND f.language = 'python' AND f.is_deleted = 0
		JOIN scope_import_evidence i
		  ON i.repo_id = e.repo_id AND i.file_id = e.file_id AND i.language = 'python'
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND e.dst_name != ''
		  AND e.edge_kind <> 'cross_language_ref'
		  AND i.wildcard = 0 AND i.local_name != ''
		  AND (e.dst_name = i.local_name
		    OR substr(e.dst_name, 1, length(i.local_name) + 1) = i.local_name || '.')
	`, repoID)
	return err
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
