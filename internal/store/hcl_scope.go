package store

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/terraform"
)

// Terraform reference ownership.
//
// A Terraform module is a directory: every .tf file in it shares one
// namespace, and no other directory's declarations are visible. An HCL edge
// binds only to the single declaration of its exact address in its own
// file's directory; a duplicate, a missing declaration, a dynamic traversal,
// or a directory holding a Terraform file the grammar could not parse leaves
// it unresolved. No repo-wide strategy may answer an HCL edge: a name lookup
// would cross module directories.
//
// The decision for an edge depends on every file of its directory, so the
// pass re-decides every HCL edge of the repository whenever it runs.

// hclScopeVetoSQL keeps every repo-wide strategy off HCL edges.
const hclScopeVetoSQL = `NOT (f.language = 'hcl')`

// hclScopeOwned is the Go-side twin of hclScopeVetoSQL.
func hclScopeOwned(t edgeTarget) bool { return t.srcLanguage == "hcl" }

// hclTerraformDeclarationKinds are the symbol kinds a reference can name.
var hclTerraformDeclarationKinds = map[string]bool{
	terraform.KindResource: true, terraform.KindData: true, terraform.KindVariable: true,
	terraform.KindLocal: true, terraform.KindModule: true,
}

// isHCLPath reports whether a changed path can change an HCL decision.
func isHCLPath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".tf", ".tfvars", ".hcl":
		return true
	}
	return false
}

type hclScopeKey struct{ dir, address string }

func resolveHCLScope(ctx context.Context, q execQuerier, repoID int64) (int, error) {
	if _, err := q.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+`
		WHERE repo_id = ? AND file_id IN (SELECT id FROM files WHERE repo_id = ? AND language = 'hcl')`, repoID, repoID); err != nil {
		return 0, err
	}
	declarations := map[hclScopeKey][]int64{}
	broken := map[string]bool{}
	rows, err := q.QueryContext(ctx, `SELECT s.id, s.kind, s.qualified_name, f.path
		FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ? AND f.language = 'hcl' AND f.is_deleted = 0`, repoID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id int64
		var kind, qname, filePath string
		if err := rows.Scan(&id, &kind, &qname, &filePath); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if !terraform.IsTerraformPath(filePath) {
			continue
		}
		dir := path.Dir(filePath)
		switch {
		case kind == terraform.KindSyntaxError:
			broken[dir] = true
		case hclTerraformDeclarationKinds[kind]:
			key := hclScopeKey{dir, qname}
			declarations[key] = append(declarations[key], id)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	type pending struct{ edgeID, symbolID int64 }
	var binds []pending
	rows, err = q.QueryContext(ctx, `SELECT e.id, e.dst_name, f.path
		FROM edges e JOIN files f ON f.id = e.file_id
		WHERE e.repo_id = ? AND f.language = 'hcl' AND f.is_deleted = 0
		  AND e.edge_kind = '`+EdgeKindReferences+`' AND e.evidence = '`+graph.HCLTerraformReferenceEvidence+`'
		ORDER BY e.id`, repoID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var edgeID int64
		var dst, filePath string
		if err := rows.Scan(&edgeID, &dst, &filePath); err != nil {
			_ = rows.Close()
			return 0, err
		}
		dir := path.Dir(filePath)
		if !terraform.IsTerraformPath(filePath) || broken[dir] {
			continue
		}
		if ids := declarations[hclScopeKey{dir, dst}]; len(ids) == 1 {
			binds = append(binds, pending{edgeID, ids[0]})
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	confidence := resolutionConfidenceFor(ResolutionStrategyTerraformModuleScope)
	for _, b := range binds {
		// ponytail: one UPDATE per bound edge, like the Lua pass; batch through
		// a temp table if Terraform-heavy repositories make this measurable.
		if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ? WHERE id = ?`,
			b.symbolID, ResolutionStrategyTerraformModuleScope, confidence, b.edgeID); err != nil {
			return 0, err
		}
	}
	return len(binds), nil
}

// resolveHCLScopeStandalone runs the pass in its own transaction, for callers
// holding a pooled *sql.DB.
func (s *Store) resolveHCLScopeStandalone(ctx context.Context, repoID int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := resolveHCLScope(ctx, tx, repoID)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
