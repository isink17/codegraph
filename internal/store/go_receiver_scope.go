package store

import (
	"context"
	"path"
	"slices"
	"strings"
)

// Go local receiver scope (P22.28).
//
// This file owns two related decisions about a Go selector call `x.Method()`,
// and keeping them separate is the whole design.
//
// The first is the veto, and it is a correctness fix rather than a feature.
// Before this slice, the parsers rewrote the qualifier of any selector call
// into an import path whenever the qualifier's spelling matched one of the
// file's import aliases -- without checking that the qualifier was the import
// binding at all. A parameter, receiver, range variable or local named after an
// imported package produced a high-confidence `module_import` edge into that
// package, which no Go compiler would agree with. The parsers now keep a
// locally bound qualifier verbatim, and resolverGoLocalQualifierSQL withholds
// the resulting `x.Method` spelling from every generic strategy. That spelling
// collides with a package function's `dot_tail2` by construction, so without
// the veto the miswire would simply reappear one strategy lower down.
//
// The second is the bind. Where the syntax states the local's type outright,
// `go_receiver_scope` resolves the call to the one method of that name on that
// type in a proven package. Where it does not -- `s := NewStore()`, a range
// variable, a type-switch binding -- the edge stays unresolved. That is the
// intended outcome, not a gap to close later by guessing: the evidence that
// proves `x` is local is exactly the evidence that proves it is not the package
// it is spelled like, and a wrong high-confidence edge costs more than a
// missing one.
//
// Deliberately not inferred here: return types (`s := NewStore()`), field types,
// interface implementations, embedded/promoted methods, and anything else
// needing dataflow. Each is a separate slice.
//
// Pointer/value method sets: no filtering, and that is the correct answer for
// the shapes this evidence proves rather than a gap. Every local this file can
// type -- a receiver, a parameter, a named result, a `var`, a `:=` -- is an
// addressable variable, and Go's method set rules let an addressable value call
// pointer-receiver methods just as a pointer can call value-receiver ones. A
// pointer/value restriction would reject calls the compiler accepts. The
// `is_pointer` column is recorded because the evidence is free to carry and a
// later slice covering non-addressable expressions will need it.

// resolverGoLocalQualifierSQL withholds a Go selector call from the generic
// strategies when the calling file's own lexical scope binds its qualifier.
//
// It is a predicate rather than a temp table for the same reason
// resolverGoBareScopeSQL is: the class of edges is decided by the edge's own
// row plus evidence keyed on it, so there is nothing to precompute and no
// second definition of the rule that could drift from the first.
//
// goLocalQualifierClaims is the Go-side twin used by the binder entrypoints,
// and go_receiver_scope_sql_twin_test.go pins the two together.
var resolverGoLocalQualifierSQL = `(
		f.language <> 'go'
		OR instr(edges.dst_name, '.') = 0
		OR instr(edges.dst_name, '/') > 0
		OR instr(edges.dst_name, ':') > 0
		OR NOT EXISTS (
			SELECT 1 FROM go_local_binding_evidence g
			WHERE g.repo_id = edges.repo_id
			  AND g.file_id = edges.file_id
			  AND g.name = substr(edges.dst_name, 1, instr(edges.dst_name, '.') - 1)
			  AND edges.line BETWEEN g.scope_start_line AND g.scope_end_line
		)
	)`

// goSelectorQualifier splits a `Qualifier.Method` spelling. It returns ok only
// for the single-dot, unqualified shape a local receiver call produces: a
// spelling carrying '/' or ':' is an import path or a foreign identity, and one
// carrying a second dot names something this evidence cannot own.
func goSelectorQualifier(dstName string) (qualifier, method string, ok bool) {
	if strings.ContainsAny(dstName, "/:") {
		return "", "", false
	}
	dot := strings.IndexByte(dstName, '.')
	if dot <= 0 || dot == len(dstName)-1 {
		return "", "", false
	}
	qualifier, method = dstName[:dot], dstName[dot+1:]
	if strings.ContainsRune(method, '.') {
		return "", "", false
	}
	return qualifier, method, true
}

// goLocalBinding is one row of go_local_binding_evidence.
type goLocalBinding struct {
	name       string
	startLine  int
	endLine    int
	typeName   string
	typePkg    string
	importPath string
}

// goReceiverEdge is an unresolved Go selector call whose qualifier some binding
// in its own file claims.
type goReceiverEdge struct {
	id       int64
	fileID   int64
	filePath string
	line     int
	callerPk string
	method   string
	isTest   bool
	binding  goLocalBinding
}

// goReceiverCandidate is one method a receiver-scoped call could name.
type goReceiverCandidate struct {
	id     int64
	pkg    string
	isTest bool
}

// resolveGoReceiverScopeStandalone runs the pass in its own transaction.
//
// The pass creates, fills, reads and drops a temp table. On a pooled *sql.DB
// those five statements can land on different connections -- the Store sets no
// MaxOpenConns -- which loses the table between statements, and an error return
// would leave a populated one behind on whichever connection it did land on. A
// transaction pins them to one connection and discards the table on rollback.
// resolveOwnModuleImportsStandalone exists for exactly this reason.
func (s *Store) resolveGoReceiverScopeStandalone(ctx context.Context, repoID int64, scope *ownModuleScope) (int, map[int64]struct{}, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	n, bound, err := s.resolveGoReceiverScopeWithBound(ctx, tx, repoID, scope)
	if err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return n, bound, nil
}

// resolveGoReceiverScope binds and reports only the count; callers that also
// need to know which edges were bound use resolveGoReceiverScopeWithBound.
func (s *Store) resolveGoReceiverScope(ctx context.Context, q execQuerier, repoID int64, scope *ownModuleScope) (int, error) {
	n, _, err := s.resolveGoReceiverScopeWithBound(ctx, q, repoID, scope)
	return n, err
}

// resolveGoReceiverScope binds Go selector calls whose receiver type the syntax
// proves. scope, when non-nil, restricts the pass to an incremental batch the
// same way the other language passes do.
//
// It does not define which edges are withheld from the generic strategies --
// goLocalQualifierClaims does, and it is deliberately wider than what this pass
// can bind. Deriving the veto from this function's own candidate set instead
// would silently un-veto every spelling it declines to consider, such as the
// field chain `s.db.QueryContext`, whose `s` is every bit as local.
func (s *Store) resolveGoReceiverScopeWithBound(ctx context.Context, q execQuerier, repoID int64, scope *ownModuleScope) (int, map[int64]struct{}, error) {
	bound := map[int64]struct{}{}
	edges, err := goReceiverScopeEdges(ctx, q, repoID)
	if err != nil || len(edges) == 0 {
		return 0, bound, err
	}

	typed := make([]goReceiverEdge, 0, len(edges))
	for _, e := range edges {
		if e.binding.typeName == "" {
			continue
		}
		if scope != nil {
			if len(scope.paths) > 0 {
				if _, ok := scope.paths[e.filePath]; !ok {
					continue
				}
			}
			if len(scope.names) > 0 {
				if _, ok := scope.names[e.method]; !ok {
					continue
				}
			}
		}
		typed = append(typed, e)
	}
	if len(typed) == 0 {
		return 0, bound, nil
	}

	// The module list costs a filesystem walk of the whole repository, and only
	// a qualified receiver type (`var x pkg.Type`) ever consults it. Most
	// batches have none, so the walk is deferred until one turns up rather than
	// paid once per binder call.
	var modules []goModule
	modulesLoaded := false
	loadModules := func() error {
		if modulesLoaded {
			return nil
		}
		var err error
		modules, err = goReceiverScopeModules(ctx, q, repoID)
		modulesLoaded = true
		return err
	}

	// Resolve each typed edge to the directory its receiver type must live in.
	// An unqualified type name means the caller's own package; a qualified one
	// means the package its import path maps to, and binds nothing when that
	// import leaves this repository.
	type target struct {
		dir      string
		pkg      string // caller's package, own-package edges only
		ownPkg   bool
		typeName string
		method   string
	}
	targets := make(map[int64]target, len(typed))
	wanted := make(map[string]struct{}, len(typed))
	for _, e := range typed {
		t := target{typeName: e.binding.typeName, method: e.method}
		switch {
		case e.binding.typePkg == "":
			if e.callerPk == "" {
				continue
			}
			t.dir = path.Dir(e.filePath)
			t.pkg = e.callerPk
			t.ownPkg = true
		default:
			if e.binding.importPath == "" {
				continue
			}
			if err := loadModules(); err != nil {
				return 0, bound, err
			}
			dir, ok := modulePackageDir(modules, e.binding.importPath)
			if !ok {
				continue
			}
			t.dir = dir
		}
		targets[e.id] = t
		wanted[t.typeName+"\x00"+t.method] = struct{}{}
	}
	if len(wanted) == 0 {
		return 0, bound, nil
	}

	candidates, err := goReceiverScopeCandidates(ctx, q, repoID, wanted)
	if err != nil {
		return 0, bound, err
	}

	resolution := make([][]any, 0, len(targets))
	for _, e := range typed {
		t, ok := targets[e.id]
		if !ok {
			continue
		}
		var only goReceiverCandidate
		found := 0
		for _, c := range candidates[t.dir+"\x00"+t.typeName+"\x00"+t.method] {
			if t.ownPkg && c.pkg != t.pkg {
				continue
			}
			// Same caller-kind rule the rest of the resolver uses: a test
			// caller may reach any sole candidate, a production caller only a
			// sole production one, so a test helper never answers a production
			// call. See resolverCandidateAggregatesSQL.
			//
			// Across packages the rule is absolute rather than caller-kind:
			// a method declared in package foo's test files is not visible to
			// anything that imports foo, not even another package's tests, so
			// it may only answer a call from foo itself. module_import excludes
			// test candidates outright for the same reason.
			if c.isTest && (!t.ownPkg || !e.isTest) {
				continue
			}
			found++
			only = c
		}
		// Exactly one surviving method, or nothing. No MIN(id), no LIMIT 1, no
		// insertion order: two methods this evidence cannot tell apart mean the
		// evidence did not identify one.
		if found != 1 {
			continue
		}
		resolution = append(resolution, []any{e.id, only.id})
		bound[e.id] = struct{}{}
	}
	if len(resolution) == 0 {
		return 0, bound, nil
	}

	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_go_receiver_resolution(edge_id INTEGER PRIMARY KEY, dst_symbol_id INTEGER NOT NULL)`); err != nil {
		return 0, bound, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM tmp_go_receiver_resolution`); err != nil {
		return 0, bound, err
	}
	if err := insertGoReceiverRows(ctx, q, resolution); err != nil {
		return 0, bound, err
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE edges
		SET dst_symbol_id = r.dst_symbol_id,
		    resolution_strategy = '`+ResolutionStrategyGoReceiverScope+`',
		    resolution_confidence = '`+ResolutionConfidenceHigh+`'
		FROM tmp_go_receiver_resolution r
		WHERE edges.id = r.edge_id AND edges.dst_symbol_id IS NULL
	`); err != nil {
		return 0, bound, err
	}
	if _, err := q.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_go_receiver_resolution`); err != nil {
		return 0, bound, err
	}
	return len(resolution), bound, nil
}

// goReceiverScopeEdges loads every unresolved Go selector call whose qualifier
// its own file binds, in one query. The join is the whole point: the number of
// statements this pass runs does not grow with the number of receiver edges.
func goReceiverScopeEdges(ctx context.Context, q execQuerier, repoID int64) ([]goReceiverEdge, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT e.id, e.file_id, f.path, e.line, e.dst_name,
		       COALESCE(src.qualified_name, ''),
		       g.name, g.scope_start_line, g.scope_end_line,
		       g.type_name, g.type_package, g.type_import_path
		FROM edges e
		JOIN files f ON f.id = e.file_id AND f.repo_id = e.repo_id
		JOIN go_local_binding_evidence g
		  ON g.repo_id = e.repo_id AND g.file_id = e.file_id
		 AND g.name = substr(e.dst_name, 1, instr(e.dst_name, '.') - 1)
		 AND e.line BETWEEN g.scope_start_line AND g.scope_end_line
		LEFT JOIN symbols src ON src.id = e.src_symbol_id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL
		  AND f.language = 'go' AND f.is_deleted = 0
		  AND instr(e.dst_name, '.') > 0
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// A name can be bound by several nested scopes at one line. The innermost
	// wins, which is the narrowest range covering the call.
	best := map[int64]goReceiverEdge{}
	for rows.Next() {
		var e goReceiverEdge
		var dstName string
		var b goLocalBinding
		if err := rows.Scan(&e.id, &e.fileID, &e.filePath, &e.line, &dstName, &e.callerPk,
			&b.name, &b.startLine, &b.endLine, &b.typeName, &b.typePkg, &b.importPath); err != nil {
			return nil, err
		}
		qualifier, method, ok := goSelectorQualifier(dstName)
		if !ok || qualifier != b.name {
			continue
		}
		e.filePath = canonicalStoredPath(e.filePath)
		e.isTest = IsTestFilePath(e.filePath)
		e.callerPk = goPackageNameOf(e.callerPk)
		e.method = method
		e.binding = b
		if prev, seen := best[e.id]; seen && (prev.binding.endLine-prev.binding.startLine) <= (b.endLine-b.startLine) {
			continue
		}
		best[e.id] = e
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]goReceiverEdge, 0, len(best))
	for _, e := range best {
		out = append(out, e)
	}
	return out, nil
}

// goReceiverScopeCandidates loads the Go methods that could answer the wanted
// (type, method) pairs, keyed by the directory that owns them.
func goReceiverScopeCandidates(ctx context.Context, q execQuerier, repoID int64, wanted map[string]struct{}) (map[string][]goReceiverCandidate, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.id, s.name, s.container_name, s.qualified_name, f.path
		FROM symbols s
		JOIN files f ON f.id = s.file_id AND f.repo_id = s.repo_id AND f.is_deleted = 0
		WHERE s.repo_id = ? AND s.language = 'go' AND s.kind = 'method'
		  AND s.container_name != ''
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]goReceiverCandidate{}
	for rows.Next() {
		var id int64
		var name, container, qualified, filePath string
		if err := rows.Scan(&id, &name, &container, &qualified, &filePath); err != nil {
			return nil, err
		}
		if _, ok := wanted[container+"\x00"+name]; !ok {
			continue
		}
		filePath = canonicalStoredPath(filePath)
		key := path.Dir(filePath) + "\x00" + container + "\x00" + name
		out[key] = append(out[key], goReceiverCandidate{
			id:     id,
			pkg:    goPackageNameOf(qualified),
			isTest: IsTestFilePath(filePath),
		})
	}
	return out, rows.Err()
}

func insertGoReceiverRows(ctx context.Context, q execQuerier, rows [][]any) error {
	for start := 0; start < len(rows); start += 400 {
		end := min(start+400, len(rows))
		var b strings.Builder
		b.WriteString(`INSERT OR IGNORE INTO tmp_go_receiver_resolution(edge_id, dst_symbol_id) VALUES `)
		args := make([]any, 0, (end-start)*2)
		for i, row := range rows[start:end] {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?,?)")
			args = append(args, row...)
		}
		if _, err := q.ExecContext(ctx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// goLocalQualifierClaims is the Go-side twin of resolverGoLocalQualifierSQL: the
// set of edges whose qualifier the calling file binds locally, which the binder
// entrypoints must withhold from their generic fallbacks exactly as the SQL
// predicate withholds them from the transactional resolver.
func goLocalQualifierClaims(ctx context.Context, q execQuerier, repoID int64) (map[int64]struct{}, error) {
	claims := map[int64]struct{}{}
	rows, err := q.QueryContext(ctx, `
		SELECT DISTINCT e.id
		FROM edges e
		JOIN files f ON f.id = e.file_id AND f.repo_id = e.repo_id
		JOIN go_local_binding_evidence g
		  ON g.repo_id = e.repo_id AND g.file_id = e.file_id
		 AND g.name = substr(e.dst_name, 1, instr(e.dst_name, '.') - 1)
		 AND e.line BETWEEN g.scope_start_line AND g.scope_end_line
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL
		  AND f.language = 'go' AND f.is_deleted = 0
		  AND instr(e.dst_name, '.') > 0
		  AND instr(e.dst_name, '/') = 0
		  AND instr(e.dst_name, ':') = 0
	`, repoID)
	if err != nil {
		return claims, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return claims, err
		}
		claims[id] = struct{}{}
	}
	return claims, rows.Err()
}

// goReceiverScopeModules reads the repository's module declarations, the same
// evidence module_import uses to map an import path onto a directory of this
// repository.
func goReceiverScopeModules(ctx context.Context, q execQuerier, repoID int64) ([]goModule, error) {
	rows, err := q.QueryContext(ctx, `SELECT root_path FROM repos WHERE id = ?`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var root string
	if rows.Next() {
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if root == "" {
		return nil, nil
	}
	return goModulesUnderRoot(root)
}

// hasGoTargets reports whether any target in a binder batch comes from a Go
// file, so the receiver-scope pass is skipped entirely for batches that cannot
// produce one.
func hasGoTargets(targets []edgeTarget) bool {
	return slices.ContainsFunc(targets, func(t edgeTarget) bool {
		return t.srcLanguage == "go"
	})
}
