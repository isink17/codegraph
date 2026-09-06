package store

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"strings"
)

type rustScopeFile struct {
	id           int64
	path, module string
	root         string
}
type rustScopeSymbol struct {
	id, file                                     int64
	name, qualified, container, visibility, kind string
}
type rustScopeImport struct {
	file                 int64
	owner, source, local string
	glob, reexport       bool
}
type rustScopeModule struct {
	owner, name, external, visibility string
	file                              int64
	inline                            bool
}

// RustResolutionStats is bounded-work evidence for one Rust resolver batch.
// It is intentionally not a runtime counter or a public product setting.
type RustResolutionStats struct {
	AffectedCrates        int
	AffectedModules       int
	AffectedEdges         int
	CandidateRows         int
	ReExportNodesVisited  int
	ReExportEvidenceLoads int
	BatchInvalidationOps  int
	BatchApplyOps         int
}

func filepathSlash(path string) string { return strings.ReplaceAll(path, "\\", "/") }

func conventionalRustRoot(path string) string {
	path = filepathSlash(path)
	base := path[strings.LastIndex(path, "/")+1:]
	if base == "lib.rs" || base == "main.rs" {
		return path
	}
	return ""
}

// rustCrateRootPredicateParams is how many parameters one crate root binds in
// rustCrateRootPredicate: the persisted root, the root path in both stored
// spellings, and the directory prefix in both. Every batched Rust statement
// budgets from this.
const rustCrateRootPredicateParams = 5

// rustCrateRootPredicate renders the crate-membership predicate for one batch
// of crate roots: membership already persisted on the evidence row, or a file
// at or under the root's directory whose membership has not been persisted
// yet. It is a *discovery* predicate -- it decides which rows a statement
// loads, never which memberships are true.
//
// crate_root is always written slashed (conventionalRustRoot normalises it),
// but files.path still holds whichever separator the indexing host used, so the
// path half of the predicate has to spell both. Dropping the native spelling
// would silently lose the blank-root branch on a Windows-written database --
// which is exactly the branch that rediscovers a file whose `mod` declaration
// has just been restored.
func rustCrateRootPredicate(fileAlias, evidenceAlias string, roots []string) (string, []any) {
	args := make([]any, 0, len(roots)*rustCrateRootPredicateParams)
	for _, root := range roots {
		args = append(args, root)
	}
	paths := make([]any, 0, len(roots)*2)
	likes := make([]string, 0, len(roots)*2)
	likeArgs := make([]any, 0, len(roots)*2)
	for _, root := range roots {
		dir := root[:strings.LastIndex(root, "/")+1]
		paths = append(paths, root, strings.ReplaceAll(root, "/", `\`))
		likes = append(likes, fileAlias+".path LIKE ?", fileAlias+".path LIKE ?")
		likeArgs = append(likeArgs, dir+"%", strings.ReplaceAll(dir, "/", `\`)+"%")
	}
	args = append(args, paths...)
	args = append(args, likeArgs...)
	// The directory tests are a chain rather than a list because they are
	// prefix matches. Everything that can be an IN list is one, which keeps the
	// expression-tree depth linear in the batch instead of multiplying it --
	// SQLite caps that depth independently of the parameter count.
	return "(" + evidenceAlias + ".crate_root IN (" + sqlitePlaceholders(len(roots)) + ") OR (" +
		evidenceAlias + ".crate_root='' AND (" + fileAlias + ".path IN (" + sqlitePlaceholders(len(paths)) + ") OR " +
		strings.Join(likes, " OR ") + ")))", args
}

// sortedRustRoots is the deterministic root order every batched Rust statement
// uses. Batching must be transport only, so the order cannot depend on map
// iteration.
func sortedRustRoots(roots map[string]struct{}) []string {
	out := make([]string, 0, len(roots))
	for root := range roots {
		out = append(out, root)
	}
	sort.Strings(out)
	return out
}

// rustRootPredicateMaxRoots bounds a batch by SQLite's expression-tree depth,
// which is capped independently of the parameter count (SQLITE_MAX_EXPR_DEPTH,
// 1000 by default). One root contributes two terms to the left-deep OR chain of
// directory prefix tests, so the parameter budget alone would stop protecting
// the statement if sqliteInClauseBatchSize were ever raised.
const rustRootPredicateMaxRoots = 150

// rustRootBatches slices crate roots into groups whose predicate fits the
// shared SQLite parameter budget alongside fixedArgs fixed parameters. An
// arbitrary number of affected crates is supported; the caller is responsible
// for aggregating every batch before deciding anything, because a batch
// boundary is not a crate boundary.
// rustRootBatchSize is how many crate roots one statement may carry alongside
// fixedArgs fixed parameters, under both the parameter budget and the
// expression-depth ceiling.
func rustRootBatchSize(fixedArgs int) int {
	return min(sqliteBatchSize(fixedArgs, rustCrateRootPredicateParams), rustRootPredicateMaxRoots)
}

func rustRootBatches(fixedArgs int, roots []string) ([][]string, error) {
	size := rustRootBatchSize(fixedArgs)
	if size == 0 {
		return nil, errSQLiteBatchImpossible
	}
	batches := make([][]string, 0, (len(roots)+size-1)/size)
	for start := 0; start < len(roots); start += size {
		batches = append(batches, roots[start:min(start+size, len(roots))])
	}
	return batches, nil
}

// rustScopedFileIDs answers which Rust files a set of crate roots covers. It
// resolves the root fan-out once, so every consumer can then bound its own
// statements by file id instead of re-binding three parameters per root. A nil
// root set means "no Rust scoping"; an empty result means no Rust file
// qualifies.
func (s *Store) rustScopedFileIDs(ctx context.Context, repoID int64, roots map[string]struct{}) (map[int64]struct{}, error) {
	if roots == nil {
		return nil, nil
	}
	return rustScopedFileIDs(ctx, s.db, repoID, sortedRustRoots(roots))
}

func rustScopedFileIDs(ctx context.Context, q queryContexter, repoID int64, roots []string) (map[int64]struct{}, error) {
	scoped := map[int64]struct{}{}
	if len(roots) == 0 {
		return scoped, nil
	}
	batches, err := rustRootBatches(1, roots)
	if err != nil {
		return nil, err
	}
	for _, batch := range batches {
		predicate, predicateArgs := rustCrateRootPredicate("f", "e", batch)
		query := `SELECT f.id FROM files f JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE f.repo_id=? AND f.language='rust' AND ` + predicate
		if err := sqliteScanRows(ctx, q, query, append([]any{repoID}, predicateArgs...), func(rows *sql.Rows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			scoped[id] = struct{}{}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return scoped, nil
}

// updateRustCrateRoots persists recomputed crate membership.
//
// crate_root is derived evidence of *currently proven* membership, not an
// append-only historical claim, so a file whose membership can no longer be
// proven is written back empty. Writing only non-empty roots would let a
// removed `mod` declaration keep its old crate and make an incremental update
// resolve edges a fresh index leaves unresolved.
//
// One row binds three parameters -- the CASE pair plus its IN entry -- on top
// of the single fixed repo id, so the batch size comes from that arithmetic
// rather than a hardcoded constant.
func updateRustCrateRoots(ctx context.Context, q execContexter, repoID int64, ids []int64, roots map[int64]string) error {
	if len(ids) == 0 {
		return nil
	}
	size := sqliteBatchSize(1, 3)
	if size == 0 {
		return errSQLiteBatchImpossible
	}
	for start := 0; start < len(ids); start += size {
		batch := ids[start:min(start+size, len(ids))]
		cases := make([]string, 0, len(batch))
		args := make([]any, 0, 1+len(batch)*3)
		for _, id := range batch {
			cases = append(cases, "WHEN ? THEN ?")
			args = append(args, id, roots[id])
		}
		args = append(args, repoID)
		args = append(args, int64SliceToAny(batch)...)
		query := `UPDATE file_scope_evidence SET crate_root=CASE file_id ` + strings.Join(cases, " ") +
			` ELSE crate_root END WHERE repo_id=? AND file_id IN (` + sqlitePlaceholders(len(batch)) + `)`
		if _, err := q.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) rustRootsForPaths(ctx context.Context, repoID int64, paths []string) (map[string]struct{}, error) {
	wanted := make([]string, 0, len(paths)*3)
	seen := map[string]struct{}{}
	for _, path := range paths {
		for _, variant := range storedPathVariants(CanonicalRelPath(path)) {
			if _, ok := seen[variant]; !ok {
				seen[variant] = struct{}{}
				wanted = append(wanted, variant)
			}
		}
	}
	roots := map[string]struct{}{}
	for start := 0; start < len(wanted); start += sqliteInClauseBatchSize {
		end := min(start+sqliteInClauseBatchSize, len(wanted))
		q := `SELECT f.path,COALESCE(e.crate_root,'') FROM files f LEFT JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE f.repo_id=? AND f.language='rust' AND f.path IN (` + sqlitePlaceholders(end-start) + `)`
		args := make([]any, 0, end-start+1)
		args = append(args, repoID)
		for _, path := range wanted[start:end] {
			args = append(args, path)
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var path, root string
			if err := rows.Scan(&path, &root); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if root == "" {
				root = conventionalRustRoot(path)
			}
			if root != "" {
				roots[root] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	// Replacing a non-root Rust file rewrites its scope row before resolution,
	// so that row cannot seed its own crate. Recover a unique root in the same
	// directory from the surviving root evidence; ambiguous sibling roots stay
	// unresolved rather than widening the affected set.
	if len(roots) == 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT crate_root FROM file_scope_evidence WHERE repo_id=? AND language='rust' AND crate_root!=''`, repoID)
		if err != nil {
			return nil, err
		}
		var candidates []string
		for rows.Next() {
			var root string
			if err := rows.Scan(&root); err != nil {
				_ = rows.Close()
				return nil, err
			}
			candidates = append(candidates, root)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			rows, err := s.db.QueryContext(ctx, `SELECT path FROM files WHERE repo_id=? AND language='rust' AND is_deleted=0 AND (path LIKE '%/lib.rs' OR path LIKE '%/main.rs' OR path LIKE '%\lib.rs' OR path LIKE '%\main.rs' OR path IN ('lib.rs','main.rs'))`, repoID)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var path string
				if err := rows.Scan(&path); err != nil {
					_ = rows.Close()
					return nil, err
				}
				// A crate root is a slashed identity everywhere else (it is what
				// crate_root persists), so normalise the stored spelling here
				// rather than leaking a backslashed root into the predicates.
				candidates = append(candidates, filepathSlash(path))
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
		}
		for _, path := range wanted {
			matched := ""
			for _, root := range candidates {
				dir := root[:strings.LastIndex(root, "/")+1]
				if strings.HasPrefix(filepathSlash(path), dir) {
					if matched != "" {
						matched = ""
						break
					}
					matched = root
				}
			}
			if matched != "" {
				roots[matched] = struct{}{}
			}
		}
	}
	if len(roots) == 1 {
		var root string
		for candidate := range roots {
			root = candidate
		}
		for _, path := range wanted {
			if _, err := s.db.ExecContext(ctx, `UPDATE file_scope_evidence SET crate_root=? WHERE repo_id=? AND file_id IN (SELECT id FROM files WHERE repo_id=? AND path=?) AND crate_root=''`, root, repoID, repoID, path); err != nil {
				return nil, err
			}
		}
	}
	return roots, nil
}

func (s *Store) rustNamesForChangedPaths(ctx context.Context, repoID int64, roots map[string]struct{}) ([]string, error) {
	return rustNamesForChangedPaths(ctx, s.db, repoID, roots)
}

func rustNamesForChangedPaths(ctx context.Context, q execQuerier, repoID int64, roots map[string]struct{}) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	batches, err := rustRootBatches(1, sortedRustRoots(roots))
	if err != nil {
		return nil, err
	}
	// Every batch is read before anything is returned, and the names are
	// collapsed into a set and sorted afterwards, so the result is exactly what
	// one hypothetical unlimited DISTINCT ... ORDER BY statement would produce.
	unique := map[string]struct{}{}
	for _, batch := range batches {
		predicate, predicateArgs := rustCrateRootPredicate("f", "e", batch)
		query := `SELECT DISTINCT x.dst_name FROM edges x JOIN files f ON f.id=x.file_id JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE x.repo_id=? AND f.language='rust' AND x.dst_name!='' AND ` + predicate
		if err := sqliteScanRows(ctx, q, query, append([]any{repoID}, predicateArgs...), func(rows *sql.Rows) error {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			unique[name] = struct{}{}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if len(unique) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (s *Store) invalidateRustBindingsForRoots(ctx context.Context, repoID int64, roots map[string]struct{}) error {
	return invalidateRustBindingsForRoots(ctx, s.db, repoID, roots)
}

func invalidateRustBindingsForRoots(ctx context.Context, q execQuerier, repoID int64, roots map[string]struct{}) error {
	if len(roots) == 0 {
		return nil
	}
	batches, err := rustRootBatches(2, sortedRustRoots(roots))
	if err != nil {
		return err
	}
	// Clearing is idempotent, so a row matched by two batches is simply cleared
	// twice; what matters is that every batch runs, or a crate past the first
	// batch would keep stale bindings.
	for _, batch := range batches {
		predicate, predicateArgs := rustCrateRootPredicate("f", "se", batch)
		query := `UPDATE edges SET ` + resolverClearResolutionSQL + ` WHERE repo_id=? AND resolution_strategy IN ('rust_module_scope','rust_use_scope','rust_associated_function') AND file_id IN (SELECT f.id FROM files f JOIN file_scope_evidence se ON se.file_id=f.id AND se.repo_id=f.repo_id WHERE f.repo_id=? AND f.language='rust' AND ` + predicate + `)`
		args := append([]any{repoID, repoID}, predicateArgs...)
		if _, err := q.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// resolveRustModuleScope is deliberately conservative: Rust edges are decided
// here or remain unresolved; generic repository-name strategies never see them.
func resolveRustModuleScope(ctx context.Context, tx *sql.Tx, repoID int64, only map[int64]struct{}) (map[int64]struct{}, error) {
	return resolveRustModuleScopeWithStats(ctx, tx, repoID, only, nil)
}

func resolveRustModuleScopeWithStats(ctx context.Context, tx *sql.Tx, repoID int64, only map[int64]struct{}, stats *RustResolutionStats) (map[int64]struct{}, error) {
	rootPath := conventionalRustRoot
	// Incremental scope is bounded by the selected callers' proven crate roots.
	// Newly written evidence may not have crate_root yet, so conventional root
	// paths are accepted as a temporary seed and are validated by the graph.
	roots := map[string]struct{}{}
	if only != nil && len(only) > 0 {
		ids := make([]int64, 0, len(only))
		for id := range only {
			ids = append(ids, id)
		}
		for _, chunk := range chunkInt64s(ids, sqliteInClauseBatchSize) {
			rows, err := tx.QueryContext(ctx, `SELECT f.path,COALESCE(e.crate_root,'') FROM edges x JOIN files f ON f.id=x.file_id LEFT JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE x.repo_id=? AND x.id IN (`+sqlitePlaceholders(len(chunk))+`)`, append([]any{repoID}, int64SliceToAny(chunk)...)...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var path, root string
				if err := rows.Scan(&path, &root); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if root == "" {
					root = rootPath(path)
				}
				if root != "" {
					roots[root] = struct{}{}
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
		}
	}
	rootList := sortedRustRoots(roots)
	if stats != nil {
		stats.AffectedCrates = len(rootList)
	}
	// The crate-root fan-out is resolved once, into a file-id set, instead of
	// being re-bound (three parameters per root) into every statement below.
	// That keeps an arbitrary number of affected crates inside the shared
	// parameter budget and, more importantly, keeps the SQL batch boundary out
	// of the semantics: the complete file set is known before anything is
	// loaded, so no decision is ever taken on a partial view.
	filtered := only != nil
	var scopedFiles []any
	if filtered && len(rootList) > 0 {
		scoped, err := rustScopedFileIDs(ctx, tx, repoID, rootList)
		if err != nil {
			return nil, err
		}
		ids := make([]int64, 0, len(scoped))
		for id := range scoped {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		scopedFiles = int64SliceToAny(ids)
	}
	const scopeClause = " AND f.id IN (%s)"

	files := map[int64]rustScopeFile{}
	if err := sqliteBatchedQuery(ctx, tx,
		`SELECT f.id,f.path,e.module_path,e.crate_root FROM files f JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE f.repo_id=? AND f.language='rust' AND f.is_deleted=0`,
		scopeClause, []any{repoID}, scopedFiles, filtered,
		func(rows *sql.Rows) error {
			var f rustScopeFile
			if err := rows.Scan(&f.id, &f.path, &f.module, &f.root); err != nil {
				return err
			}
			files[f.id] = f
			return nil
		}); err != nil {
		return nil, err
	}
	if stats != nil {
		stats.AffectedModules = len(files)
	}
	symbols := map[int64]rustScopeSymbol{}
	var byQ map[string][]rustScopeSymbol
	if err := sqliteBatchedQuery(ctx, tx,
		`SELECT s.id,s.file_id,s.name,s.qualified_name,s.container_name,s.visibility,s.kind FROM symbols s JOIN files f ON f.id=s.file_id JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE s.repo_id=? AND s.language='rust'`,
		scopeClause, []any{repoID}, scopedFiles, filtered,
		func(rows *sql.Rows) error {
			var s rustScopeSymbol
			if err := rows.Scan(&s.id, &s.file, &s.name, &s.qualified, &s.container, &s.visibility, &s.kind); err != nil {
				return err
			}
			symbols[s.id] = s
			return nil
		}); err != nil {
		return nil, err
	}
	imports := []rustScopeImport{}
	if err := sqliteBatchedQuery(ctx, tx,
		`SELECT i.file_id,i.owner_module,i.source_specifier,i.local_name,i.wildcard,i.is_reexport FROM scope_import_evidence i JOIN files f ON f.id=i.file_id JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE i.repo_id=? AND i.language='rust'`,
		scopeClause, []any{repoID}, scopedFiles, filtered,
		func(rows *sql.Rows) error {
			var i rustScopeImport
			var glob, re int
			if err := rows.Scan(&i.file, &i.owner, &i.source, &i.local, &glob, &re); err != nil {
				return err
			}
			i.glob = glob != 0
			i.reexport = re != 0
			imports = append(imports, i)
			return nil
		}); err != nil {
		return nil, err
	}
	if stats != nil {
		stats.ReExportEvidenceLoads = 1
	}
	importsBy := map[int64][]rustScopeImport{}
	for _, i := range imports {
		importsBy[i.file] = append(importsBy[i.file], i)
	}
	// A module path is only meaningful inside its crate.  Keep the root in the
	// key; `crate::util` in two targets of one repository is not one scope.
	//
	// Membership is derived from primary evidence only: a conventional crate
	// root proves itself, and the declaration graph below propagates from
	// there. The persisted crate_root deliberately seeds nothing -- it is a
	// cache of a previous recomputation, and letting it seed membership would
	// let it prove itself after the `mod` declaration behind it disappeared.
	moduleFiles := map[string][]int64{}
	rootOfFile := map[int64]string{}
	for _, f := range files {
		if root := rootPath(f.path); root != "" {
			rootOfFile[f.id] = root
			moduleFiles[root+"\x00crate"] = append(moduleFiles[root+"\x00crate"], f.id)
		}
	}
	decls := []rustScopeModule{}
	if err := sqliteBatchedQuery(ctx, tx,
		`SELECT m.file_id,m.owner_module,m.module_name,m.external_path,m.is_inline,m.visibility FROM rust_module_evidence m JOIN files f ON f.id=m.file_id JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE m.repo_id=?`,
		scopeClause, []any{repoID}, scopedFiles, filtered,
		func(rows *sql.Rows) error {
			var m rustScopeModule
			var inline int
			if err := rows.Scan(&m.file, &m.owner, &m.name, &m.external, &inline, &m.visibility); err != nil {
				return err
			}
			m.inline = inline != 0
			decls = append(decls, m)
			return nil
		}); err != nil {
		return nil, err
	}
	for changed := true; changed; {
		changed = false
		for _, m := range decls {
			root := rootOfFile[m.file]
			if root == "" {
				continue
			}
			key := root + "\x00" + m.owner + "::" + m.name
			if m.inline {
				if len(moduleFiles[key]) == 0 {
					moduleFiles[key] = []int64{m.file}
					changed = true
				}
				continue
			}
			base := strings.TrimSuffix(filepathSlash(m.external), "/")
			matches := []int64{}
			for id, f := range files {
				path := filepathSlash(f.path)
				stem := strings.TrimSuffix(path, ".rs")
				if strings.HasSuffix(stem, "/mod") {
					stem = strings.TrimSuffix(stem, "/mod")
				}
				if path == base+".rs" || path == base+"/mod.rs" ||
					strings.HasSuffix(path, "/"+base+".rs") || strings.HasSuffix(path, "/"+base+"/mod.rs") ||
					strings.HasSuffix(base, "/"+stem) {
					matches = append(matches, id)
				}
			}
			if len(matches) == 1 && len(moduleFiles[key]) == 0 {
				moduleFiles[key] = matches
				if prior := rootOfFile[matches[0]]; prior != "" && prior != root {
					rootOfFile[matches[0]] = "" // incompatible crate memberships
				} else {
					rootOfFile[matches[0]] = root
				}
				changed = true
			}
		}
	}
	moduleProven := func(root, module string) bool {
		if root == "" {
			return false
		}
		parts := strings.Split(module, "::")
		if len(parts) == 0 || parts[0] != "crate" {
			return false
		}
		return len(moduleFiles[root+"\x00"+module]) == 1
	}
	// Persist over every loaded file, not only the ones with a proven root: a
	// file that lost its membership must have its cached crate_root cleared, or
	// the next incremental run would still see it in the crate.
	persistIDs := make([]int64, 0, len(files))
	for id := range files {
		persistIDs = append(persistIDs, id)
	}
	slices.Sort(persistIDs)
	if err := updateRustCrateRoots(ctx, tx, repoID, persistIDs, rootOfFile); err != nil {
		return nil, err
	}
	for id, f := range files {
		f.root = rootOfFile[id]
		files[id] = f
	}
	// Parser paths are provisional for files that can also be bin roots (for
	// example src/bin/util.rs). The declaration graph is authoritative. A file
	// may safely have several memberships, so add one lookup view per proven
	// membership rather than overwriting the persisted symbol identity.
	byQ = map[string][]rustScopeSymbol{}
	for _, original := range symbols {
		added := false
		for key, members := range moduleFiles {
			sep := strings.IndexByte(key, 0)
			if sep < 0 || len(members) != 1 || members[0] != original.file {
				continue
			}
			module := key[sep+1:]
			s := original
			if module != "crate" {
				s.qualified = module + "::" + s.name
				if s.container != "" && s.container != files[s.file].module && s.container != module {
					s.qualified = module + "::" + s.container + "::" + s.name
				}
			}
			byQ[s.qualified] = append(byQ[s.qualified], s)
			added = true
		}
		if !added {
			byQ[original.qualified] = append(byQ[original.qualified], original)
		}
	}
	moduleMember := func(module string, id int64, caller int64) bool {
		root := rootOfFile[caller]
		for _, member := range moduleFiles[root+"\x00"+module] {
			if member == id && root != "" && rootOfFile[member] == root {
				return true
			}
		}
		return false
	}
	candidateModule := func(c rustScopeSymbol) string {
		module := strings.TrimSuffix(c.qualified, "::"+c.name)
		if c.container != "" && strings.Contains(c.qualified, "::"+c.container+"::") {
			module = strings.TrimSuffix(c.qualified, "::"+c.container+"::"+c.name)
		}
		return module
	}
	eligible := func(c rustScopeSymbol, caller rustScopeFile) bool {
		module := candidateModule(c)
		if c.visibility == "private" && caller.module != module && !strings.HasPrefix(caller.module, module+"::") {
			return false
		}
		if c.visibility == "public" {
			return true
		}
		if c.visibility == "restricted:crate" {
			return rootOfFile[c.file] != "" && rootOfFile[c.file] == rootOfFile[caller.id]
		}
		if c.visibility == "restricted:super" {
			parent := module[:max(0, strings.LastIndex(module, "::"))]
			return parent != "" && (caller.module == parent || strings.HasPrefix(caller.module, parent+"::"))
		}
		return false
	}
	resolvePath := func(raw, owner string) string {
		p := strings.Split(strings.TrimPrefix(raw, "::"), "::")
		if len(p) == 0 || p[0] == "" {
			return ""
		}
		if p[0] == "crate" {
			return "crate::" + strings.Join(p[1:], "::")
		}
		if p[0] == "self" {
			return owner + "::" + strings.Join(p[1:], "::")
		}
		for len(p) > 0 && p[0] == "super" {
			if i := strings.LastIndex(owner, "::"); i >= 0 {
				owner = owner[:i]
			} else {
				owner = "crate"
			}
			p = p[1:]
		}
		return owner + "::" + strings.Join(p, "::")
	}
	var exportCandidates func(string, string, map[string]struct{}) []rustScopeSymbol
	exportCandidates = func(module, name string, seen map[string]struct{}) []rustScopeSymbol {
		key := module + "::" + name
		if _, ok := seen[key]; ok {
			return nil
		}
		seen[key] = struct{}{}
		if stats != nil {
			stats.ReExportNodesVisited++
		}
		// The recursion is over an in-memory export relation loaded above; this
		// count is the observable proof that traversal is not DB-per-hop.
		out := append([]rustScopeSymbol(nil), byQ[key]...)
		for _, im := range imports {
			if im.owner != module || !im.reexport {
				continue
			}
			if im.glob {
				out = append(out, exportCandidates(resolvePath(im.source, module), name, seen)...)
			} else if im.local == name {
				raw := resolvePath(im.source, module)
				parts := strings.Split(raw, "::")
				if len(parts) > 1 {
					out = append(out, exportCandidates(strings.Join(parts[:len(parts)-1], "::"), parts[len(parts)-1], seen)...)
				}
			}
		}
		return out
	}
	if only == nil {
		if stats != nil {
			stats.BatchInvalidationOps = 1
		}
		if _, err := tx.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=NULL,resolution_strategy='',resolution_confidence='' WHERE repo_id=? AND file_id IN (SELECT id FROM files WHERE repo_id=? AND language='rust') AND dst_symbol_id IS NOT NULL AND resolution_strategy NOT IN ('rust_module_scope','rust_use_scope','rust_associated_function')`, repoID, repoID); err != nil {
			return nil, err
		}
	}
	type rustResolution struct {
		edge, symbol int64
		strategy     string
	}
	resolutions := make([]rustResolution, 0)
	// The selected-edge set is unbounded, so the id fan is batched by the same
	// budget. Every batch is scanned before any resolution is applied, and each
	// edge id appears in exactly one batch, so batching changes nothing about
	// which edges are decided.
	edgeBatches := [][]any{nil}
	filteredEdges := only != nil
	if filteredEdges {
		if len(only) == 0 {
			return map[int64]struct{}{}, nil
		}
		ids := make([]int64, 0, len(only))
		for id := range only {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		var err error
		if edgeBatches, err = sqliteBatchArgs(1, int64SliceToAny(ids)); err != nil {
			return nil, err
		}
	}
	const edgeQuery = `SELECT e.id,e.file_id,e.src_symbol_id,e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='rust' AND e.dst_symbol_id IS NULL`
	for _, edgeBatch := range edgeBatches {
		query := edgeQuery
		if filteredEdges {
			query += ` AND e.id IN (` + sqlitePlaceholders(len(edgeBatch)) + `)`
		}
		rows, err := tx.QueryContext(ctx, query, append([]any{repoID}, edgeBatch...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, file, src int64
			var dst string
			if err := rows.Scan(&id, &file, &src, &dst); err != nil {
				return nil, err
			}
			if only != nil {
				if _, ok := only[id]; !ok {
					continue
				}
			}
			if stats != nil {
				stats.AffectedEdges++
			}
			caller, ok := files[file]
			if !ok {
				continue
			}
			ss := symbols[src]
			if strings.Contains(dst, ".") {
				continue
			}
			owner := ss.container
			if owner == "" {
				owner = caller.module
			}
			path := dst
			strategy := ResolutionStrategyRustModuleScope
			var globCandidates []rustScopeSymbol
			for _, im := range importsBy[file] {
				if im.owner != owner {
					continue
				}
				if im.glob {
					module := resolvePath(im.source, owner)
					for _, c := range byQ[module+"::"+dst] {
						if stats != nil {
							stats.CandidateRows++
						}
						if eligible(c, caller) {
							globCandidates = append(globCandidates, c)
						}
					}
					continue
				}
				if im.local == dst {
					path = resolvePath(im.source, owner)
					strategy = ResolutionStrategyRustUseScope
					break
				}
			}
			if path == dst && strings.Contains(dst, "::") {
				path = resolvePath(dst, owner)
			} else if path == dst {
				path = owner + "::" + dst
			}
			if path == owner+"::"+dst && len(globCandidates) > 0 {
				path = ""
				strategy = ResolutionStrategyRustUseScope
			}
			var chosen int64
			count := 0
			for _, c := range globCandidates {
				if c.file == caller.id || (moduleProven(rootOfFile[caller.id], candidateModule(c)) && moduleMember(candidateModule(c), c.file, caller.id)) {
					count++
					chosen = c.id
				}
			}
			for _, c := range byQ[path] {
				if stats != nil {
					stats.CandidateRows++
				}
				module := candidateModule(c)
				if eligible(c, caller) && (c.file == caller.id || (moduleProven(rootOfFile[caller.id], module) && moduleMember(module, c.file, caller.id))) {
					count++
					chosen = c.id
				}
			}
			if count == 0 {
				parts := strings.Split(path, "::")
				if len(parts) > 1 {
					for _, c := range exportCandidates(strings.Join(parts[:len(parts)-1], "::"), parts[len(parts)-1], map[string]struct{}{}) {
						if stats != nil {
							stats.CandidateRows++
						}
						module := candidateModule(c)
						if eligible(c, caller) && moduleProven(rootOfFile[caller.id], module) && moduleMember(module, c.file, caller.id) {
							count++
							chosen = c.id
						}
					}
				}
			}
			if count != 1 {
				continue
			}
			if strings.Contains(dst, "::") {
				for _, c := range byQ[path] {
					if c.id == chosen && c.kind == "function" && c.container != candidateModule(c) {
						strategy = ResolutionStrategyRustAssociatedFunction
					}
				}
			}
			resolutions = append(resolutions, rustResolution{edge: id, symbol: chosen, strategy: strategy})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	if len(resolutions) == 0 {
		return map[int64]struct{}{}, nil
	}
	if stats != nil {
		stats.BatchApplyOps = 1
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE rust_scope_resolution(edge_id INTEGER PRIMARY KEY, symbol_id INTEGER NOT NULL, strategy TEXT NOT NULL, confidence TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return nil, err
	}
	defer tx.ExecContext(ctx, `DROP TABLE rust_scope_resolution`)
	for start := 0; start < len(resolutions); start += 300 {
		end := min(start+300, len(resolutions))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*4)
		for _, result := range resolutions[start:end] {
			values = append(values, "(?,?,?,?)")
			args = append(args, result.edge, result.symbol, result.strategy, "high")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO rust_scope_resolution(edge_id,symbol_id,strategy,confidence) VALUES `+strings.Join(values, ","), args...); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT symbol_id FROM rust_scope_resolution r WHERE r.edge_id=edges.id), resolution_strategy=(SELECT strategy FROM rust_scope_resolution r WHERE r.edge_id=edges.id), resolution_confidence=(SELECT confidence FROM rust_scope_resolution r WHERE r.edge_id=edges.id) WHERE repo_id=? AND dst_symbol_id IS NULL AND id IN (SELECT edge_id FROM rust_scope_resolution)`, repoID); err != nil {
		return nil, err
	}
	bound := make(map[int64]struct{}, len(resolutions))
	for _, result := range resolutions {
		bound[result.edge] = struct{}{}
	}
	return bound, nil
}

func (s *Store) resolveRustModuleScopeStandalone(ctx context.Context, repoID int64, ids map[int64]struct{}) (map[int64]struct{}, error) {
	return s.resolveRustModuleScopeStandaloneWithStats(ctx, repoID, ids, nil)
}

func (s *Store) resolveRustModuleScopeStandaloneWithStats(ctx context.Context, repoID int64, ids map[int64]struct{}, stats *RustResolutionStats) (map[int64]struct{}, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	bound, err := resolveRustModuleScopeWithStats(ctx, tx, repoID, ids, stats)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return bound, nil
}

// dropRustEdgesOutsideScope removes from stale every Rust edge that the
// crate-scoped pass owns but will not look at again, because its file is not
// covered by the affected crate roots. It is the guard that keeps the
// repo-wide, name-based invalidation from unbinding crates nothing will rebind.
//
// Only the three strategies invalidateRustBindingsForRoots itself clears are
// protected. A Rust edge bound by any other redecidable strategy is not owned
// by that pass, so exempting it would leave it permanently stale instead of
// converging on the unresolved state a fresh index produces.
//
// The query is driven by the stale ids rather than by the Rust edge population,
// so the work is proportional to the invalidation batch and not to the size of
// the repository.
func (s *Store) dropRustEdgesOutsideScope(ctx context.Context, repoID int64, scope map[int64]struct{}, stale map[int64]struct{}) error {
	ids := make([]int64, 0, len(stale))
	for id := range stale {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return sqliteBatchedQuery(ctx, s.db,
		`SELECT e.id,e.file_id FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='rust' AND e.resolution_strategy IN ('rust_module_scope','rust_use_scope','rust_associated_function')`,
		` AND e.id IN (%s)`, []any{repoID}, int64SliceToAny(ids), true,
		func(rows *sql.Rows) error {
			var id, file int64
			if err := rows.Scan(&id, &file); err != nil {
				return err
			}
			if _, ok := scope[file]; !ok {
				delete(stale, id)
			}
			return nil
		})
}
