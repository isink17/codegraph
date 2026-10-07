package store

import (
	"context"
	"database/sql"

	"github.com/isink17/codegraph/internal/graph"
)

type JVMCompilationScope struct {
	Domain     string
	Version    int
	State      string
	Provenance string
}

// JVMTypeEvidence returns source facts in a stable path/key order. The rows
// describe syntax and provenance; they do not contain canonical JVM identity.
func (s *Store) JVMTypeEvidence(ctx context.Context, repoID int64) ([]graph.JVMTypeEvidence, error) {
	rows, err := s.parserSemanticQueryer(ctx).QueryContext(ctx, `SELECT e.evidence_key,e.owner_name,e.source_language,e.declaration_kind,e.modifiers,e.has_type_parameters,e.underlying_type,e.underlying_state,e.alias_target,e.syntax_state,e.provenance,f.path,c.position,c.syntax,c.syntax_state FROM jvm_type_evidence e JOIN files f ON f.id=e.file_id LEFT JOIN jvm_callable_type_evidence c ON c.repo_id=e.repo_id AND c.symbol_id=e.symbol_id WHERE e.repo_id=? AND f.is_deleted=0 ORDER BY f.path,e.evidence_key,c.position,c.ordinal`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]graph.JVMTypeEvidence, 0)
	byKey := make(map[string]int)
	for rows.Next() {
		var fact graph.JVMTypeEvidence
		var hasTypeParams int
		var position, syntax, syntaxState sql.NullString
		if err := rows.Scan(&fact.EvidenceKey, &fact.OwnerName, &fact.SourceLanguage, &fact.Kind, &fact.Modifiers, &hasTypeParams, &fact.UnderlyingType, &fact.UnderlyingState, &fact.AliasTarget, &fact.SyntaxState, &fact.Provenance, &fact.FilePath, &position, &syntax, &syntaxState); err != nil {
			return nil, err
		}
		idx, ok := byKey[fact.EvidenceKey]
		if !ok {
			fact.SymbolIndex = -1
			fact.TypeParams = hasTypeParams != 0
			idx = len(out)
			byKey[fact.EvidenceKey] = idx
			out = append(out, fact)
		}
		if position.Valid {
			out[idx].Params = append(out[idx].Params, graph.JVMCallableTypeEvidence{Position: position.String, Syntax: syntax.String, SyntaxState: syntaxState.String})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) JVMCompilationScope(ctx context.Context, repoID int64) (JVMCompilationScope, error) {
	var scope JVMCompilationScope
	err := s.parserSemanticQueryer(ctx).QueryRowContext(ctx, `SELECT evidence_domain,evidence_version,state,provenance FROM jvm_compilation_scope_evidence WHERE repo_id=?`, repoID).Scan(&scope.Domain, &scope.Version, &scope.State, &scope.Provenance)
	return scope, err
}
