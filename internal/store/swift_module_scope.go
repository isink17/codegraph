package store

import (
	"context"
)

type swiftBuildScope struct {
	packageID string
	module    string
}

type swiftBuildScopes struct {
	files   map[int64]swiftBuildScope
	imports map[int64]map[string]bool
}

type swiftScopeRelation uint8

const (
	swiftScopeUnknown swiftScopeRelation = iota
	swiftScopeSameModule
	swiftScopeSamePackage
	swiftScopeDifferentPackage
	swiftScopeIneligible
)

func loadSwiftBuildScopes(ctx context.Context, q javaQuery, repoID int64) (swiftBuildScopes, error) {
	s := swiftBuildScopes{files: map[int64]swiftBuildScope{}, imports: map[int64]map[string]bool{}}
	rows, err := q.QueryContext(ctx, `SELECT file_id,package_name,module_path FROM file_scope_evidence WHERE repo_id=?`, repoID)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var file int64
		var scope swiftBuildScope
		if err := rows.Scan(&file, &scope.packageID, &scope.module); err != nil {
			return s, err
		}
		s.files[file] = scope
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	imports, err := q.QueryContext(ctx, `SELECT file_id,source_specifier FROM scope_import_evidence WHERE repo_id=? AND language='swift' AND source_specifier<>''`, repoID)
	if err != nil {
		return s, err
	}
	defer imports.Close()
	for imports.Next() {
		var file int64
		var module string
		if err := imports.Scan(&file, &module); err != nil {
			return s, err
		}
		if s.imports[file] == nil {
			s.imports[file] = map[string]bool{}
		}
		s.imports[file][module] = true
	}
	return s, imports.Err()
}

func (s swiftBuildScopes) relation(caller, candidate int64) swiftScopeRelation {
	a, aOK := s.files[caller]
	b, bOK := s.files[candidate]
	if !aOK || !bOK || a.packageID == "" || b.packageID == "" {
		return swiftScopeUnknown
	}
	if a.packageID != b.packageID {
		return swiftScopeDifferentPackage
	}
	if a.module == "" || b.module == "" {
		return swiftScopeUnknown
	}
	if a.module == b.module {
		return swiftScopeSameModule
	}
	return swiftScopeSamePackage
}

func (s swiftBuildScopes) candidateEligible(caller, candidate int64, visibility string) swiftScopeRelation {
	relation := s.relation(caller, candidate)
	if relation == swiftScopeUnknown {
		return relation
	}
	if visibility == "private" || visibility == "fileprivate" {
		return swiftScopeIneligible
	}
	if relation == swiftScopeSameModule {
		return relation
	}
	if visibility == "internal" {
		return swiftScopeIneligible
	}
	candidateScope := s.files[candidate]
	if !s.imports[caller][candidateScope.module] {
		return swiftScopeIneligible
	}
	if relation == swiftScopeSamePackage && visibility == "package" {
		return relation
	}
	if visibility == "public" || visibility == "open" {
		return relation
	}
	return swiftScopeIneligible
}

func swiftScopeKnownIneligible(relation swiftScopeRelation) bool {
	return relation == swiftScopeIneligible
}
