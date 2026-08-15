package store

import (
	"context"
	"database/sql"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

type goModule struct {
	path string
	root string
}

func (s *Store) resolveOwnModuleImportsStandalone(ctx context.Context, repoID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_own_module_veto(edge_id INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	if _, err := s.resolveOwnModuleImports(ctx, tx, repoID); err != nil {
		return err
	}
	for _, table := range []string{"tmp_resolver_own_module_veto", "tmp_resolver_own_module_targets", "tmp_resolver_own_module_resolution"} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+table); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// resolveOwnModuleImports is one batched, high-evidence pass. It deliberately
// reads module declarations from the repository, not from the Go toolchain.
func (s *Store) resolveOwnModuleImports(ctx context.Context, tx *sql.Tx, repoID int64) (int, error) {
	modules, err := repoGoModules(ctx, tx, repoID)
	if err != nil || len(modules) == 0 {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_own_module_veto(edge_id INTEGER PRIMARY KEY)`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolver_own_module_veto`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_own_module_targets(package_dir TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(package_dir, name))`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolver_own_module_targets`); err != nil {
		return 0, err
	}

	type edgeFact struct {
		id   int64
		dir  string
		name string
	}
	byKey := map[string][]edgeFact{}
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.dst_name, fi.import_path
		FROM edges e
		JOIN files sf ON sf.id = e.file_id AND sf.repo_id = e.repo_id
		JOIN file_imports fi ON fi.file_id = sf.id AND fi.repo_id = e.repo_id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL
		  AND sf.language = 'go' AND instr(e.dst_name, '/') > 0
	`, repoID)
	if err != nil {
		return 0, err
	}
	seenEdges := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		var dst, imported string
		if err := rows.Scan(&id, &dst, &imported); err != nil {
			return 0, err
		}
		dot := strings.LastIndexByte(dst, '.')
		if dot <= 0 || dot == len(dst)-1 || dst[:dot] != imported {
			continue
		}
		dir, ok := modulePackageDir(modules, imported)
		if !ok {
			continue
		}
		fact := edgeFact{id: id, dir: dir, name: dst[dot+1:]}
		key := dir + "\x00" + fact.name
		byKey[key] = append(byKey[key], fact)
		seenEdges[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	vetoRows := make([][]any, 0, len(seenEdges))
	for id := range seenEdges {
		vetoRows = append(vetoRows, []any{id})
	}
	if err := insertModuleRows(ctx, tx, "tmp_resolver_own_module_veto", "edge_id", vetoRows); err != nil {
		return 0, err
	}

	targetRows := make([][]any, 0, len(byKey))
	for key := range byKey {
		parts := strings.SplitN(key, "\x00", 2)
		targetRows = append(targetRows, []any{parts[0], parts[1]})
	}
	if err := insertModuleRows(ctx, tx, "tmp_resolver_own_module_targets", "package_dir, name", targetRows); err != nil {
		return 0, err
	}
	if len(byKey) == 0 {
		return 0, nil
	}

	type candidate struct{ id int64 }
	candidates := map[string][]candidate{}
	rows, err = tx.QueryContext(ctx, `
		SELECT t.package_dir, t.name, s.id, replace(f.path, '\\', '/')
		FROM tmp_resolver_own_module_targets t
		JOIN files f ON f.repo_id = ? AND f.is_deleted = 0
		JOIN symbols s ON s.repo_id = ? AND s.file_id = f.id AND s.name = t.name
		WHERE s.language = 'go'
		  AND (s.container_name = '' OR s.qualified_name = s.container_name || '.' || s.name)
		  AND s.kind IN ('function', 'type', 'struct', 'interface')
	`, repoID, repoID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var dir, name, filePath string
		var id int64
		if err := rows.Scan(&dir, &name, &id, &filePath); err != nil {
			return 0, err
		}
		if path.Dir(filePath) != dir || IsTestFilePath(filePath) {
			continue
		}
		key := dir + "\x00" + name
		candidates[key] = append(candidates[key], candidate{id: id})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_own_module_resolution(edge_id INTEGER PRIMARY KEY, dst_symbol_id INTEGER NOT NULL)`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolver_own_module_resolution`); err != nil {
		return 0, err
	}
	resolved := 0
	resolutionRows := make([][]any, 0, len(byKey))
	for key, facts := range byKey {
		cs := candidates[key]
		if len(cs) != 1 {
			continue
		}
		for _, fact := range facts {
			resolutionRows = append(resolutionRows, []any{fact.id, cs[0].id})
			resolved++
		}
	}
	if err := insertModuleRows(ctx, tx, "tmp_resolver_own_module_resolution", "edge_id, dst_symbol_id", resolutionRows); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE edges
		SET dst_symbol_id = r.dst_symbol_id,
		    resolution_strategy = 'module_import',
		    resolution_confidence = 'high'
		FROM tmp_resolver_own_module_resolution r
		WHERE edges.id = r.edge_id AND edges.dst_symbol_id IS NULL
	`); err != nil {
		return 0, err
	}
	return resolved, nil
}

func insertModuleRows(ctx context.Context, tx *sql.Tx, table, columns string, rows [][]any) error {
	for start := 0; start < len(rows); start += 400 {
		end := start + 400
		if end > len(rows) {
			end = len(rows)
		}
		var b strings.Builder
		b.WriteString(`INSERT OR IGNORE INTO ` + table + `(` + columns + `) VALUES `)
		args := make([]any, 0, (end-start)*len(rows[start]))
		for i, row := range rows[start:end] {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(")
			for j := range row {
				if j > 0 {
					b.WriteByte(',')
				}
				b.WriteByte('?')
				args = append(args, row[j])
			}
			b.WriteString(")")
		}
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

func repoGoModules(ctx context.Context, tx *sql.Tx, repoID int64) ([]goModule, error) {
	var root string
	if err := tx.QueryRowContext(ctx, `SELECT root_path FROM repos WHERE id = ?`, repoID).Scan(&root); err != nil {
		return nil, err
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var modules []goModule
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if filePath != root && (entry.Name() == ".git" || entry.Name() == ".codegraph") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "go.mod" {
			return nil
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		parsed, err := modfile.Parse(filePath, data, nil)
		if err != nil || parsed.Module == nil || parsed.Module.Mod.Path == "" {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(filePath))
		if err != nil {
			return err
		}
		modules = append(modules, goModule{path: parsed.Module.Mod.Path, root: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return modules, nil
}

func modulePackageDir(modules []goModule, importPath string) (string, bool) {
	best := goModule{}
	for _, module := range modules {
		if importPath != module.path && !strings.HasPrefix(importPath, module.path+"/") {
			continue
		}
		if len(module.path) > len(best.path) {
			best = module
		} else if len(module.path) == len(best.path) && (module.path != best.path || module.root != best.root) {
			return "", false
		}
	}
	if best.path == "" {
		return "", false
	}
	rel := strings.TrimPrefix(importPath, best.path)
	rel = strings.TrimPrefix(rel, "/")
	if best.root == "." || best.root == "" {
		return rel, true
	}
	if rel == "" {
		return best.root, true
	}
	return path.Join(best.root, rel), true
}
