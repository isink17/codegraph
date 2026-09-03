package store

import (
	"context"
	"database/sql"
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
	AffectedCrates       int
	AffectedModules      int
	AffectedEdges        int
	CandidateRows        int
	ReExportNodesVisited int
	ReExportEvidenceLoads int
	BatchInvalidationOps int
	BatchApplyOps        int
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
			rows, err := s.db.QueryContext(ctx, `SELECT path FROM files WHERE repo_id=? AND language='rust' AND is_deleted=0 AND (path LIKE '%/lib.rs' OR path LIKE '%/main.rs' OR path IN ('lib.rs','main.rs'))`, repoID)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var path string
				if err := rows.Scan(&path); err != nil {
					_ = rows.Close()
					return nil, err
				}
				candidates = append(candidates, path)
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
	if len(roots) == 0 {
		return nil, nil
	}
	values := make([]string, 0, len(roots))
	for root := range roots {
		values = append(values, root)
	}
	sort.Strings(values)
	parts := make([]string, 0, len(values))
	args := make([]any, 0, len(values)+1)
	args = append(args, repoID)
	for _, root := range values {
		dir := root[:strings.LastIndex(root, "/")+1]
		parts = append(parts, "(e.crate_root=? OR (e.crate_root='' AND (f.path=? OR f.path LIKE ?)))")
		args = append(args, root, root, dir+"%")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT x.dst_name FROM edges x JOIN files f ON f.id=x.file_id JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE x.repo_id=? AND f.language='rust' AND x.dst_name!='' AND (`+strings.Join(parts, " OR ")+") ORDER BY x.dst_name", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (s *Store) invalidateRustBindingsForRoots(ctx context.Context, repoID int64, roots map[string]struct{}) error {
	if len(roots) == 0 {
		return nil
	}
	values := make([]string, 0, len(roots))
	args := make([]any, 0, len(roots)+2)
	args = append(args, repoID, repoID)
	for root := range roots {
		values = append(values, root)
	}
	sort.Strings(values)
	parts := make([]string, 0, len(values))
	for _, root := range values {
		dir := root[:strings.LastIndex(root, "/")+1]
		parts = append(parts, "(se.crate_root=? OR (se.crate_root='' AND (f.path=? OR f.path LIKE ?)))")
		args = append(args, root, root, dir+"%")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+` WHERE repo_id=? AND resolution_strategy IN ('rust_module_scope','rust_use_scope','rust_associated_function') AND file_id IN (SELECT f.id FROM files f JOIN file_scope_evidence se ON se.file_id=f.id AND se.repo_id=f.repo_id WHERE f.repo_id=? AND f.language='rust' AND (`+strings.Join(parts, " OR ")+`))`, args...)
	return err
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
	rootList := make([]string, 0, len(roots))
	for root := range roots {
		rootList = append(rootList, root)
	}
	sort.Strings(rootList)
	if stats != nil {
		stats.AffectedCrates = len(rootList)
	}
	scope := func(fileAlias, evidenceAlias string) (string, []any) {
		if only == nil {
			return "", nil
		}
		if len(rootList) == 0 {
			return " AND 0", nil
		}
		parts := make([]string, 0, len(rootList)*2)
		args := make([]any, 0, len(rootList)*3)
		for _, root := range rootList {
			dir := root[:strings.LastIndex(root, "/")+1]
			parts = append(parts, evidenceAlias+`.crate_root=? OR (`+evidenceAlias+`.crate_root='' AND (`+fileAlias+`.path=? OR `+fileAlias+`.path LIKE ?))`)
			args = append(args, root, root, dir+"%")
		}
		return " AND (" + strings.Join(parts, " OR ") + ")", args
	}

	files := map[int64]rustScopeFile{}
	fileScope, fileArgs := scope("f", "e")
	rows, err := tx.QueryContext(ctx, `SELECT f.id,f.path,e.module_path,e.crate_root FROM files f JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE f.repo_id=? AND f.language='rust' AND f.is_deleted=0`+fileScope, append([]any{repoID}, fileArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f rustScopeFile
		if err := rows.Scan(&f.id, &f.path, &f.module, &f.root); err != nil {
			return nil, err
		}
		files[f.id] = f
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if stats != nil {
		stats.AffectedModules = len(files)
	}
	symbols := map[int64]rustScopeSymbol{}
	byQ := map[string][]rustScopeSymbol{}
	symbolScope, symbolArgs := scope("f", "e")
	rows, err = tx.QueryContext(ctx, `SELECT s.id,s.file_id,s.name,s.qualified_name,s.container_name,s.visibility,s.kind FROM symbols s JOIN files f ON f.id=s.file_id JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE s.repo_id=? AND s.language='rust'`+symbolScope, append([]any{repoID}, symbolArgs...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s rustScopeSymbol
		if err := rows.Scan(&s.id, &s.file, &s.name, &s.qualified, &s.container, &s.visibility, &s.kind); err != nil {
			return nil, err
		}
		symbols[s.id] = s
		byQ[s.qualified] = append(byQ[s.qualified], s)
	}
	rows.Close()
	imports := []rustScopeImport{}
	importScope, importArgs := scope("f", "e")
	rows, err = tx.QueryContext(ctx, `SELECT i.file_id,i.owner_module,i.source_specifier,i.local_name,i.wildcard,i.is_reexport FROM scope_import_evidence i JOIN files f ON f.id=i.file_id JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE i.repo_id=? AND i.language='rust'`+importScope, append([]any{repoID}, importArgs...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var i rustScopeImport
		var glob, re int
		if err := rows.Scan(&i.file, &i.owner, &i.source, &i.local, &glob, &re); err != nil {
			return nil, err
		}
		i.glob = glob != 0
		i.reexport = re != 0
		imports = append(imports, i)
	}
	rows.Close()
	if stats != nil {
		stats.ReExportEvidenceLoads = 1
	}
	importsBy := map[int64][]rustScopeImport{}
	for _, i := range imports {
		importsBy[i.file] = append(importsBy[i.file], i)
	}
	// A module path is only meaningful inside its crate.  Keep the root in the
	// key; `crate::util` in two targets of one repository is not one scope.
	moduleFiles := map[string][]int64{}
	rootOfFile := map[int64]string{}
	for _, f := range files {
		if f.root != "" {
			rootOfFile[f.id] = f.root
			module := f.module
			if module == "" {
				module = "crate"
			}
			moduleFiles[f.root+"\x00"+module] = append(moduleFiles[f.root+"\x00"+module], f.id)
		}
		if root := rootPath(f.path); root != "" {
			rootOfFile[f.id] = root
			moduleFiles[root+"\x00crate"] = append(moduleFiles[root+"\x00crate"], f.id)
		}
	}
	moduleScope, moduleArgs := scope("f", "e")
	rows, err = tx.QueryContext(ctx, `SELECT m.file_id,m.owner_module,m.module_name,m.external_path,m.is_inline,m.visibility FROM rust_module_evidence m JOIN files f ON f.id=m.file_id JOIN file_scope_evidence e ON e.file_id=f.id AND e.repo_id=f.repo_id WHERE m.repo_id=?`+moduleScope, append([]any{repoID}, moduleArgs...)...)
	if err != nil {
		return nil, err
	}
	decls := []rustScopeModule{}
	for rows.Next() {
		var m rustScopeModule
		var inline int
		if err := rows.Scan(&m.file, &m.owner, &m.name, &m.external, &inline, &m.visibility); err != nil {
			return nil, err
		}
		m.inline = inline != 0
		decls = append(decls, m)
	}
	rows.Close()
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
	if len(rootOfFile) > 0 {
		ids := make([]int64, 0, len(rootOfFile))
		for id := range rootOfFile {
			ids = append(ids, id)
		}
		for _, chunk := range chunkInt64s(ids, 300) {
			args := make([]any, 0, len(chunk)*2)
			cases := make([]string, 0, len(chunk))
			for _, id := range chunk {
				cases = append(cases, "WHEN ? THEN ?")
				args = append(args, id, rootOfFile[id])
			}
			where := make([]string, 0, len(chunk))
			for _, id := range chunk {
				where = append(where, "?")
				args = append(args, id)
			}
			query := `UPDATE file_scope_evidence SET crate_root=CASE file_id ` + strings.Join(cases, " ") + ` ELSE crate_root END WHERE repo_id=? AND file_id IN (` + strings.Join(where, ",") + `)`
			// The repo id is last because the CASE parameters precede the WHERE.
			queryArgs := make([]any, 0, len(args))
			queryArgs = append(queryArgs, args[1:]...)
			queryArgs = append(queryArgs, repoID)
			queryArgs = append(queryArgs, int64SliceToAny(chunk)...)
			if _, err := tx.ExecContext(ctx, query, queryArgs...); err != nil {
				return nil, err
			}
		}
	}
	for id, root := range rootOfFile {
		f := files[id]
		f.root = root
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
	edgeScope := ""
	edgeArgs := []any{repoID}
	if only != nil {
		if len(only) == 0 {
			return map[int64]struct{}{}, nil
		}
		ids := make([]int64, 0, len(only))
		for id := range only {
			ids = append(ids, id)
		}
		edgeScope = ` AND e.id IN (` + sqlitePlaceholders(len(ids)) + `)`
		edgeArgs = append(edgeArgs, int64SliceToAny(ids)...)
	}
	rows, err = tx.QueryContext(ctx, `SELECT e.id,e.file_id,e.src_symbol_id,e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='rust' AND e.dst_symbol_id IS NULL`+edgeScope, edgeArgs...)
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
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
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
