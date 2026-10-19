package store

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"slices"
	"strings"
)

// GoModuleEvidence is one go.mod (Kind "module") or go.work (Kind
// "workspace") of a repository. Root is its directory relative to the
// repository, "." at the top. ModulePath is empty for a go.mod without a
// usable module directive; LocalReplace reports a replace by a directory.
type GoModuleEvidence struct {
	Kind         string
	Root         string
	ModulePath   string
	LocalReplace bool
}

// DiscoverGoModuleEvidence reads every go.mod and go.work under root, the
// same files the Go import resolver reads.
func DiscoverGoModuleEvidence(root string) ([]GoModuleEvidence, error) {
	modules, workspaces, err := goModuleTreeUnderRoot(root)
	if err != nil {
		return nil, err
	}
	out := make([]GoModuleEvidence, 0, len(modules)+len(workspaces))
	for _, m := range modules {
		out = append(out, GoModuleEvidence{Kind: "module", Root: m.root, ModulePath: m.path, LocalReplace: m.localReplace})
	}
	for _, w := range workspaces {
		out = append(out, GoModuleEvidence{Kind: "workspace", Root: w})
	}
	slices.SortFunc(out, func(a, b GoModuleEvidence) int {
		return strings.Compare(a.Kind+"\x00"+a.Root, b.Kind+"\x00"+b.Root)
	})
	return out, nil
}

func goModuleEvidenceKey(repoID int64) string { return fmt.Sprintf("go.module_evidence.%d", repoID) }

// ReplaceGoModuleEvidence records evidence as the repository's Go modules and
// marks them known. An unchanged set writes nothing.
func (s *Store) ReplaceGoModuleEvidence(ctx context.Context, repoID int64, evidence []GoModuleEvidence) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, known, err := goModuleEvidence(ctx, tx, repoID)
	if err != nil {
		return err
	}
	if known && slices.Equal(current, evidence) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM go_module_evidence WHERE repo_id=?`, repoID); err != nil {
		return err
	}
	for _, e := range evidence {
		if _, err := tx.ExecContext(ctx, `INSERT INTO go_module_evidence(repo_id,kind,root,module_path,local_replace) VALUES(?,?,?,?,?)`, repoID, e.Kind, e.Root, e.ModulePath, boolInt(e.LocalReplace)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,'v1') ON CONFLICT(key) DO UPDATE SET value=excluded.value`, goModuleEvidenceKey(repoID)); err != nil {
		return err
	}
	return tx.Commit()
}

func goModuleEvidence(ctx context.Context, q *sql.Tx, repoID int64) ([]GoModuleEvidence, bool, error) {
	var marker string
	if err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, goModuleEvidenceKey(repoID)).Scan(&marker); err == sql.ErrNoRows {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	rows, err := q.QueryContext(ctx, `SELECT kind,root,module_path,local_replace FROM go_module_evidence WHERE repo_id=? ORDER BY kind,root`, repoID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []GoModuleEvidence{}
	for rows.Next() {
		var e GoModuleEvidence
		var replace int
		if err := rows.Scan(&e.Kind, &e.Root, &e.ModulePath, &replace); err != nil {
			return nil, false, err
		}
		e.LocalReplace = replace != 0
		out = append(out, e)
	}
	return out, marker == "v1", rows.Err()
}

// SemanticGoModules is the repository's Go module layout as last indexed.
type SemanticGoModules struct {
	Modules    []SemanticGoModule `json:"modules"`
	Workspaces []string           `json:"workspaces"`
}

type SemanticGoModule struct {
	Root         string `json:"root"`
	Path         string `json:"path"`
	LocalReplace bool   `json:"local_replace,omitempty"`
}

// SemanticGoFile is a Go file's package identity. ImportPath is set only when
// ModuleState is GoModuleComplete.
type SemanticGoFile struct {
	PackageClause   string `json:"package_clause"`
	BuildConstraint string `json:"build_constraint"`
	ExternalTest    bool   `json:"external_test"`
	ModuleState     string `json:"module_state"`
	ModuleRoot      string `json:"module_root,omitempty"`
	ModulePath      string `json:"module_path,omitempty"`
	ImportPath      string `json:"import_path,omitempty"`
}

const (
	GoModuleComplete    = "complete"
	GoModuleUnknown     = "module_evidence_unavailable"
	GoModuleNone        = "no_module"
	GoModuleNotDeclared = "module_path_missing"
)

// goFileIdentity derives a file's module and import path from the nearest
// go.mod at or above its directory, as the go command does.
func goFileIdentity(filePath, pkg, build string, modules *SemanticGoModules) *SemanticGoFile {
	out := &SemanticGoFile{PackageClause: pkg, BuildConstraint: build, ModuleState: GoModuleUnknown,
		ExternalTest: strings.HasSuffix(filePath, "_test.go") && strings.HasSuffix(pkg, "_test")}
	if modules == nil {
		return out
	}
	dir := path.Dir(filePath)
	var nearest *SemanticGoModule
	depth := -1
	for i, m := range modules.Modules {
		d := len(m.Root) + 1
		switch {
		case m.Root == ".":
			d = 0
		case dir != m.Root && !strings.HasPrefix(dir, m.Root+"/"):
			continue
		}
		if d > depth {
			nearest, depth = &modules.Modules[i], d
		}
	}
	switch {
	case nearest == nil:
		out.ModuleState = GoModuleNone
	case nearest.Path == "":
		out.ModuleState, out.ModuleRoot = GoModuleNotDeclared, nearest.Root
	default:
		out.ModuleState, out.ModuleRoot, out.ModulePath, out.ImportPath = GoModuleComplete, nearest.Root, nearest.Path, nearest.Path
		rel := dir
		if nearest.Root != "." {
			rel = strings.TrimPrefix(strings.TrimPrefix(dir, nearest.Root), "/")
		}
		if rel != "" && rel != "." {
			out.ImportPath += "/" + rel
		}
	}
	return out
}
