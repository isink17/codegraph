package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

var simpleJVMType = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(\.[A-Za-z_$][A-Za-z0-9_$]*)*$`)

type JVMTypeDependency struct {
	ConsumerKey       string
	ConsumerPath      string
	LookupKey         string
	Kind              string
	TargetKey         string
	TargetFingerprint string
	SourceLanguage    string
	TypePosition      string
	TypeOrdinal       int
	State             string
	Provenance        string
}

func (s *Store) JVMTypeDependencies(ctx context.Context, repoID int64) ([]JVMTypeDependency, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT consumer_key,consumer_path,lookup_key,dependency_kind,target_key,target_fingerprint,source_language,type_position,type_ordinal,state,provenance FROM jvm_type_dependencies WHERE repo_id=? ORDER BY consumer_key,lookup_key,type_position,type_ordinal`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JVMTypeDependency
	for rows.Next() {
		var d JVMTypeDependency
		if err := rows.Scan(&d.ConsumerKey, &d.ConsumerPath, &d.LookupKey, &d.Kind, &d.TargetKey, &d.TargetFingerprint, &d.SourceLanguage, &d.TypePosition, &d.TypeOrdinal, &d.State, &d.Provenance); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type jvmTypeDeclaration struct {
	evidenceKey string
	key         string
	lookup      string
	path        string
	fingerprint string
	language    string
	kind        string
}

type jvmTypeDependency struct {
	consumerKey  string
	consumerPath string
	lookup       string
	kind         string
	target       string
	fingerprint  string
	language     string
	position     string
	ordinal      int
	provenance   string
}

type jvmTypeImport struct {
	source   string
	local    string
	wildcard bool
	static   bool
}

type jvmScopeState struct {
	source, generated, excluded, dependencies, metadata, compiler string
}

func markJVMDependencyPathDirtyTx(ctx context.Context, tx *sql.Tx, repoID int64, path string) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO jvm_type_dependency_pending_paths(repo_id,path) VALUES(?,?)`, repoID, path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jvm_type_dependencies SET state='dirty' WHERE repo_id=? AND consumer_path=?`, repoID, path); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE jvm_type_dependencies SET state='dirty' WHERE repo_id=? AND lookup_key IN (SELECT lookup_key FROM jvm_type_declarations WHERE repo_id=? AND file_path=?)`, repoID, repoID, path)
	return err
}

func markJVMDependencyParsedKeysDirtyTx(ctx context.Context, tx *sql.Tx, repoID int64, path string, parsed graph.ParsedFile) error {
	// Any semantic edit can alter a lookup without changing the old provider
	// key: imports may add a higher-priority candidate, and declaration moves
	// may change package ownership. Keep the invalidation keyed to consumers in
	// the edited file plus the prior and new lookup keys, while treating changed
	// import sets conservatively for same-package short-name lookups.
	if _, err := tx.ExecContext(ctx, `UPDATE jvm_type_dependencies SET state='dirty' WHERE repo_id=? AND consumer_path=?`, repoID, path); err != nil {
		return err
	}
	for _, fact := range parsed.JVMTypeEvidence {
		name, owner := "", fact.OwnerName
		if fact.Kind == "typealias" && fact.SymbolIndex < 0 {
			name = evidenceName(fact.EvidenceKey)
		} else if fact.SymbolIndex >= 0 && fact.SymbolIndex < len(parsed.Symbols) && strings.HasPrefix(parsed.Symbols[fact.SymbolIndex].StableKey, "type:") {
			symbol := parsed.Symbols[fact.SymbolIndex]
			name = symbol.Name
			if owner == "" {
				if dot := strings.LastIndexByte(symbol.QualifiedName, '.'); dot >= 0 {
					owner = symbol.QualifiedName[:dot]
				}
			}
		}
		if name == "" {
			continue
		}
		key := jvmTypeLookupKey(owner, name)
		if _, err := tx.ExecContext(ctx, `UPDATE jvm_type_dependencies SET state='dirty' WHERE repo_id=? AND lookup_key=?`, repoID, key); err != nil {
			return err
		}
	}
	return nil
}

// RebuildJVMTypeDependencies invalidates observations by stable lookup key,
// then recomputes them from persisted source facts. Dirty markers commit first,
// so a failed recomputation cannot leave old proofs certified as current.
func (s *Store) RebuildJVMTypeDependencies(ctx context.Context, repoID int64, changedPaths []string) (int, error) {
	paths := jvmUniqueSorted(changedPaths)
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO jvm_type_dependency_index_state(repo_id, initialized) VALUES(?, 0)`, repoID); err != nil {
		return 0, err
	}
	var initialized int
	if err := s.db.QueryRowContext(ctx, `SELECT initialized FROM jvm_type_dependency_index_state WHERE repo_id=?`, repoID).Scan(&initialized); err != nil {
		return 0, err
	}
	paths, err := s.jvmRelevantDependencyPaths(ctx, repoID, paths)
	if err != nil {
		return 0, err
	}
	pending, err := s.pendingJVMTypeDependencyPaths(ctx, repoID)
	if err != nil {
		return 0, err
	}
	paths = jvmMergeSortedUnique(paths, pending)
	var dirty int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jvm_type_dependencies WHERE repo_id=? AND state='dirty'`, repoID).Scan(&dirty); err != nil {
		return 0, err
	}
	if initialized != 0 && dirty == 0 && len(paths) == 0 {
		return 0, nil
	}
	all := initialized == 0
	oldDecls, err := s.loadJVMTypeDeclarations(ctx, s.db, repoID, paths, all)
	if err != nil {
		return 0, err
	}
	newDecls, err := s.loadJVMTypeDeclarationsFromFacts(ctx, s.db, repoID, paths, all)
	if err != nil {
		return 0, err
	}
	changedKeys := changedJVMTypeLookupKeys(oldDecls, newDecls)

	// Dirty state commits separately from recomputation. A crash or SQL error
	// after this point leaves the affected observations unusable until retry.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	if all {
		if _, err = tx.ExecContext(ctx, `UPDATE jvm_type_dependencies SET state='dirty' WHERE repo_id=?`, repoID); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
	} else {
		for _, path := range paths {
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO jvm_type_dependency_pending_paths(repo_id,path) VALUES(?,?)`, repoID, path); err != nil {
				_ = tx.Rollback()
				return 0, err
			}
		}
		for _, path := range paths {
			if _, err = tx.ExecContext(ctx, `UPDATE jvm_type_dependencies SET state='dirty' WHERE repo_id=? AND consumer_path=?`, repoID, path); err != nil {
				_ = tx.Rollback()
				return 0, err
			}
		}
		for _, key := range changedKeys {
			if _, err = tx.ExecContext(ctx, `UPDATE jvm_type_dependencies SET state='dirty' WHERE repo_id=? AND lookup_key=?`, repoID, key); err != nil {
				_ = tx.Rollback()
				return 0, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}

	consumerPaths, err := s.dirtyJVMDependencyPaths(ctx, repoID)
	if err != nil {
		return 0, err
	}
	consumerPaths = jvmMergeSortedUnique(consumerPaths, paths)
	if all {
		consumerPaths, err = s.jvmTypeEvidencePaths(ctx, repoID)
		if err != nil {
			return 0, err
		}
	}
	if len(consumerPaths) == 0 && len(paths) == 0 && dirty == 0 {
		// Initialize the declaration snapshot even for repositories without JVM
		// facts. This keeps subsequent no-op updates constant-time.
		consumerPaths = []string{}
	}

	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	if err = s.replaceJVMTypeDeclarations(ctx, tx, repoID, paths, all, newDecls); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if all {
		if _, err = tx.ExecContext(ctx, `DELETE FROM jvm_type_dependencies WHERE repo_id=?`, repoID); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
	} else {
		for _, path := range consumerPaths {
			if _, err = tx.ExecContext(ctx, `DELETE FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path=?`, repoID, path); err != nil {
				_ = tx.Rollback()
				return 0, err
			}
		}
	}
	declarations, err := s.loadJVMTypeDeclarationIndex(ctx, tx, repoID)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	scope, err := loadJVMScopeState(ctx, tx, repoID)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	var count int
	for _, path := range consumerPaths {
		deps, err := s.jvmDependenciesForPath(ctx, tx, repoID, path, declarations, scope)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		for _, dep := range deps {
			if _, err := tx.ExecContext(ctx, `INSERT INTO jvm_type_dependencies(repo_id,consumer_key,consumer_path,lookup_key,dependency_kind,target_key,target_fingerprint,source_language,type_position,type_ordinal,state,provenance) VALUES(?,?,?,?,?,?,?,?,?,?, 'current', ?)`, repoID, dep.consumerKey, dep.consumerPath, dep.lookup, dep.kind, dep.target, dep.fingerprint, dep.language, dep.position, dep.ordinal, dep.provenance); err != nil {
				_ = tx.Rollback()
				return 0, err
			}
			count++
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jvm_type_dependency_index_state SET initialized=1 WHERE repo_id=?`, repoID); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if all {
		if _, err := tx.ExecContext(ctx, `DELETE FROM jvm_type_dependency_pending_paths WHERE repo_id=?`, repoID); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
	} else {
		for _, path := range paths {
			if _, err := tx.ExecContext(ctx, `DELETE FROM jvm_type_dependency_pending_paths WHERE repo_id=? AND path=?`, repoID, path); err != nil {
				_ = tx.Rollback()
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) jvmRelevantDependencyPaths(ctx context.Context, repoID int64, paths []string) ([]string, error) {
	var out []string
	for _, path := range paths {
		var language string
		err := s.db.QueryRowContext(ctx, `SELECT language FROM files WHERE repo_id=? AND path=?`, repoID, path).Scan(&language)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		if language == "java" || language == "kotlin" {
			out = append(out, path)
		}
	}
	return out, nil
}

func (s *Store) loadJVMTypeDeclarations(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, repoID int64, paths []string, all bool) ([]jvmTypeDeclaration, error) {
	query := `SELECT evidence_key,declaration_key,lookup_key,file_path,fingerprint,source_language,declaration_kind FROM jvm_type_declarations WHERE repo_id=?`
	args := []any{repoID}
	if !all {
		if len(paths) == 0 {
			return nil, nil
		}
		query += ` AND file_path IN (` + jvmPlaceholders(len(paths)) + `)`
		for _, path := range paths {
			args = append(args, path)
		}
	}
	query += ` ORDER BY lookup_key,declaration_key,evidence_key`
	return scanJVMTypeDeclarations(ctx, q, query, args...)
}

func (s *Store) loadJVMTypeDeclarationsFromFacts(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, repoID int64, paths []string, all bool) ([]jvmTypeDeclaration, error) {
	query := `SELECT e.evidence_key, e.source_language, e.declaration_kind, e.modifiers, e.has_type_parameters, e.underlying_type, e.underlying_state, e.alias_target, e.syntax_state, e.provenance, e.owner_name, f.path, s.stable_key, s.qualified_name FROM jvm_type_evidence e JOIN files f ON f.id=e.file_id LEFT JOIN symbols s ON s.id=e.symbol_id WHERE e.repo_id=? AND f.is_deleted=0 AND ((e.declaration_kind='typealias' AND e.symbol_id IS NULL) OR (e.symbol_id IS NOT NULL AND s.stable_key LIKE 'type:%'))`
	args := []any{repoID}
	if !all {
		if len(paths) == 0 {
			return nil, nil
		}
		query += ` AND f.path IN (` + jvmPlaceholders(len(paths)) + `)`
		for _, path := range paths {
			args = append(args, path)
		}
	}
	query += ` ORDER BY f.path,e.evidence_key`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []jvmTypeDeclaration
	for rows.Next() {
		var evidenceKey, lang, kind, modifiers, underlying, underlyingState, aliasTarget, syntaxState, provenance, owner, path string
		var typeParams int
		var stable, qualified sql.NullString
		if err := rows.Scan(&evidenceKey, &lang, &kind, &modifiers, &typeParams, &underlying, &underlyingState, &aliasTarget, &syntaxState, &provenance, &owner, &path, &stable, &qualified); err != nil {
			return nil, err
		}
		name := ""
		key := ""
		if kind == "typealias" && !stable.Valid {
			name = evidenceName(evidenceKey)
			key = "jvm-type/v1|" + lang + "|" + owner + "." + name + "|typealias"
		} else if stable.Valid {
			key = "jvm-type/v1|" + lang + "|" + stable.String
			qualifiedName := qualified.String
			if dot := strings.LastIndexByte(qualifiedName, '.'); dot >= 0 {
				name = qualifiedName[dot+1:]
			} else {
				name = qualifiedName
			}
		}
		if key == "" || name == "" {
			continue
		}
		lookup := jvmTypeLookupKey(owner, name)
		fingerprint := hashStrings(kind, modifiers, fmt.Sprint(typeParams), underlying, underlyingState, aliasTarget, syntaxState, provenance)
		out = append(out, jvmTypeDeclaration{evidenceKey: evidenceKey, key: key, lookup: lookup, path: path, fingerprint: fingerprint, language: lang, kind: kind})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) replaceJVMTypeDeclarations(ctx context.Context, tx *sql.Tx, repoID int64, paths []string, all bool, declarations []jvmTypeDeclaration) error {
	if all {
		if _, err := tx.ExecContext(ctx, `DELETE FROM jvm_type_declarations WHERE repo_id=?`, repoID); err != nil {
			return err
		}
	} else {
		for _, path := range paths {
			if _, err := tx.ExecContext(ctx, `DELETE FROM jvm_type_declarations WHERE repo_id=? AND file_path=?`, repoID, path); err != nil {
				return err
			}
		}
	}
	for _, d := range declarations {
		if !all && !jvmSlicesContains(paths, d.path) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jvm_type_declarations(repo_id,evidence_key,declaration_key,lookup_key,file_path,fingerprint,source_language,declaration_kind) VALUES(?,?,?,?,?,?,?,?)`, repoID, d.evidenceKey, d.key, d.lookup, d.path, d.fingerprint, d.language, d.kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadJVMTypeDeclarationIndex(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, repoID int64) (map[string][]jvmTypeDeclaration, error) {
	rows, err := q.QueryContext(ctx, `SELECT evidence_key,declaration_key,lookup_key,file_path,fingerprint,source_language,declaration_kind FROM jvm_type_declarations WHERE repo_id=? ORDER BY lookup_key,declaration_key,evidence_key`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]jvmTypeDeclaration)
	for rows.Next() {
		var d jvmTypeDeclaration
		if err := rows.Scan(&d.evidenceKey, &d.key, &d.lookup, &d.path, &d.fingerprint, &d.language, &d.kind); err != nil {
			return nil, err
		}
		out[d.lookup] = append(out[d.lookup], d)
	}
	return out, rows.Err()
}

func (s *Store) dirtyJVMDependencyPaths(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT consumer_path FROM jvm_type_dependencies WHERE repo_id=? AND state='dirty' ORDER BY consumer_path`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

func (s *Store) pendingJVMTypeDependencyPaths(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM jvm_type_dependency_pending_paths WHERE repo_id=? ORDER BY path`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

func (s *Store) jvmTypeEvidencePaths(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT f.path FROM jvm_type_evidence e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.is_deleted=0 ORDER BY f.path`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

func (s *Store) jvmDependenciesForPath(ctx context.Context, tx *sql.Tx, repoID int64, path string, declarations map[string][]jvmTypeDeclaration, scope jvmScopeState) ([]jvmTypeDependency, error) {
	var deps []jvmTypeDependency
	imports, pkg, err := loadJVMImports(ctx, tx, repoID, path)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.evidence_key,s.stable_key,s.qualified_name,s.signature,s.container_name,e.source_language,e.has_type_parameters,c.position,c.ordinal,c.syntax,c.syntax_state FROM jvm_callable_type_evidence c JOIN symbols s ON s.id=c.symbol_id JOIN jvm_type_evidence e ON e.repo_id=c.repo_id AND e.symbol_id=c.symbol_id JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.path=? AND f.is_deleted=0 ORDER BY e.evidence_key,c.position,c.ordinal`, repoID, path)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var evidenceKey, stable, qualified, signature, container, language, position, syntax, syntaxState string
		var typeParams, ordinal int
		if err := rows.Scan(&evidenceKey, &stable, &qualified, &signature, &container, &language, &typeParams, &position, &ordinal, &syntax, &syntaxState); err != nil {
			rows.Close()
			return nil, err
		}
		consumer := "jvm-callable/v1|" + path + "|" + language + "|" + stable + "|" + signature + "|" + evidenceKey
		callScope := scope
		if container != pkg || typeParams != 0 {
			callScope = jvmScopeState{}
		}
		observations := resolveJVMTypeSyntax(syntax, syntaxState, language, pkg, imports, declarations, callScope)
		for idx, o := range observations {
			deps = append(deps, jvmTypeDependency{consumerKey: consumer, consumerPath: path, lookup: o.lookup, kind: o.kind, target: o.target, fingerprint: o.fingerprint, language: language, position: position, ordinal: ordinal*8 + idx, provenance: "jvm-type-syntax:v1"})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT e.evidence_key,e.source_language,e.owner_name,e.alias_target,e.syntax_state,e.has_type_parameters FROM jvm_type_evidence e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.path=? AND f.is_deleted=0 AND e.declaration_kind='typealias' AND e.symbol_id IS NULL ORDER BY e.evidence_key`, repoID, path)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var evidenceKey, language, owner, syntax, syntaxState string
		var typeParams int
		if err := rows.Scan(&evidenceKey, &language, &owner, &syntax, &syntaxState, &typeParams); err != nil {
			rows.Close()
			return nil, err
		}
		name := evidenceName(evidenceKey)
		consumer := "jvm-typealias/v1|" + language + "|" + owner + "." + name + "|" + evidenceKey
		aliasScope := scope
		if typeParams != 0 {
			aliasScope = jvmScopeState{}
		}
		aliasTargetState := syntaxState
		if simpleJVMType.MatchString(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(syntax), "?"))) {
			aliasTargetState = "known"
		}
		observations := resolveJVMTypeSyntax(syntax, aliasTargetState, language, owner, imports, declarations, aliasScope)
		for idx, o := range observations {
			deps = append(deps, jvmTypeDependency{consumerKey: consumer, consumerPath: path, lookup: o.lookup, kind: o.kind, target: o.target, fingerprint: o.fingerprint, language: language, position: "alias_target", ordinal: idx, provenance: "jvm-typealias-syntax:v1"})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return deps, nil
}

type jvmTypeObservation struct{ lookup, kind, target, fingerprint string }

func resolveJVMTypeSyntax(syntax, syntaxState, language, packageName string, imports []jvmTypeImport, declarations map[string][]jvmTypeDeclaration, scope jvmScopeState) []jvmTypeObservation {
	typeName := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(syntax), "?"))
	if syntaxState != "known" || !simpleJVMType.MatchString(typeName) {
		return []jvmTypeObservation{{lookup: "jvm-type-lookup/v1|unsupported|" + hashStrings(language, packageName, syntax), kind: "unknown"}}
	}
	qualified := strings.Contains(typeName, ".")
	short := typeName
	wildcard := false
	if dot := strings.LastIndexByte(short, '.'); dot >= 0 {
		short = short[dot+1:]
	}
	keys := []string{}
	if qualified {
		keys = append(keys, jvmTypeLookupKey(strings.TrimSuffix(typeName, "."+short), short))
	} else {
		matches := map[string]struct{}{}
		for _, imp := range imports {
			if imp.static {
				continue
			}
			if imp.wildcard {
				wildcard = true
				continue
			}
			if imp.local == short {
				matches[imp.source] = struct{}{}
			}
		}
		for source := range matches {
			if dot := strings.LastIndexByte(source, '.'); dot > 0 {
				keys = append(keys, jvmTypeLookupKey(source[:dot], source[dot+1:]))
			}
		}
		// Keep the same-package lookup as an explicit absence dependency even
		// when an import is present. A later local declaration can shadow or
		// conflict with the imported spelling without touching the consumer.
		keys = append(keys, jvmTypeLookupKey(packageName, short))
		if wildcard {
			keys = append(keys, "jvm-type-lookup/v1|wildcard|"+hashStrings(language, packageName, short))
		}
	}
	keys = jvmUniqueSorted(keys)
	allCandidates := map[string]struct{}{}
	for _, key := range keys {
		for _, candidate := range declarations[key] {
			allCandidates[candidate.evidenceKey] = struct{}{}
		}
	}
	conflicted := false
	if !qualified {
		conflicted, _ = conflictingJVMImports(imports, short)
	}
	ambiguous := len(allCandidates) > 1 || conflicted
	var out []jvmTypeObservation
	for _, key := range keys {
		candidates := declarations[key]
		kind := "unknown"
		var target, fingerprint string
		if len(candidates) > 1 {
			kind = "ambiguous"
		} else if len(candidates) == 1 && allJVMscopeComplete(scope) {
			kind = "positive"
			target = candidates[0].key
			fingerprint = candidates[0].fingerprint
		} else if allJVMscopeComplete(scope) && !strings.Contains(key, "|wildcard|") {
			kind = "negative"
		}
		if ambiguous && !strings.Contains(key, "|wildcard|") {
			kind = "ambiguous"
			target = ""
			fingerprint = ""
		}
		if wildcard && !qualified && !strings.Contains(key, "|wildcard|") {
			kind = "unknown"
			target = ""
			fingerprint = ""
		}
		out = append(out, jvmTypeObservation{lookup: key, kind: kind, target: target, fingerprint: fingerprint})
	}
	return out
}

func loadJVMImports(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, repoID int64, path string) ([]jvmTypeImport, string, error) {
	var pkg string
	if err := q.QueryRowContext(ctx, `SELECT package_name FROM file_scope_evidence f JOIN files x ON x.id=f.file_id WHERE f.repo_id=? AND x.path=?`, repoID, path).Scan(&pkg); err != nil && err != sql.ErrNoRows {
		return nil, "", err
	}
	rows, err := q.QueryContext(ctx, `SELECT source_specifier,local_name,wildcard,is_static FROM scope_import_evidence i JOIN files f ON f.id=i.file_id WHERE i.repo_id=? AND f.path=? AND i.language IN ('java','kotlin') ORDER BY source_specifier,local_name`, repoID, path)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var imports []jvmTypeImport
	for rows.Next() {
		var i jvmTypeImport
		var wildcard, static int
		if err := rows.Scan(&i.source, &i.local, &wildcard, &static); err != nil {
			return nil, "", err
		}
		i.wildcard = wildcard != 0
		i.static = static != 0
		imports = append(imports, i)
	}
	return imports, pkg, rows.Err()
}

func loadJVMScopeState(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, repoID int64) (jvmScopeState, error) {
	var s jvmScopeState
	err := q.QueryRowContext(ctx, `SELECT source_state,generated_state,excluded_state,dependency_state,external_metadata_state,compiler_identity_state FROM jvm_compilation_scope_evidence WHERE repo_id=?`, repoID).Scan(&s.source, &s.generated, &s.excluded, &s.dependencies, &s.metadata, &s.compiler)
	return s, err
}
func allJVMscopeComplete(s jvmScopeState) bool {
	return s.source == "complete" && s.generated == "complete" && s.excluded == "complete" && s.dependencies == "complete" && s.metadata == "complete" && s.compiler == "complete"
}
func conflictingJVMImports(imports []jvmTypeImport, short string) (bool, []string) {
	var found []string
	for _, i := range imports {
		if !i.static && !i.wildcard && i.local == short {
			found = append(found, i.source)
		}
	}
	found = jvmUniqueSorted(found)
	return len(found) > 1, found
}

func changedJVMTypeLookupKeys(oldDecls, newDecls []jvmTypeDeclaration) []string {
	old := map[string][]string{}
	next := map[string][]string{}
	for _, d := range oldDecls {
		old[d.evidenceKey] = []string{d.lookup, d.key, d.fingerprint}
	}
	for _, d := range newDecls {
		next[d.evidenceKey] = []string{d.lookup, d.key, d.fingerprint}
	}
	changed := map[string]struct{}{}
	for evidence, v := range old {
		n, ok := next[evidence]
		if !ok || strings.Join(v, "\x00") != strings.Join(n, "\x00") {
			changed[v[0]] = struct{}{}
			if ok {
				changed[n[0]] = struct{}{}
			}
		}
	}
	for evidence, v := range next {
		if _, ok := old[evidence]; !ok {
			changed[v[0]] = struct{}{}
		}
	}
	out := make([]string, 0, len(changed))
	for k := range changed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func scanJVMTypeDeclarations(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]jvmTypeDeclaration, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []jvmTypeDeclaration
	for rows.Next() {
		var d jvmTypeDeclaration
		if err := rows.Scan(&d.evidenceKey, &d.key, &d.lookup, &d.path, &d.fingerprint, &d.language, &d.kind); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func evidenceName(key string) string {
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		return key[i+1:]
	}
	return ""
}
func jvmTypeLookupKey(pkg, name string) string { return "jvm-type-lookup/v1|" + pkg + "|" + name }
func hashStrings(values ...string) string {
	h := sha256.New()
	for _, v := range values {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
func jvmPlaceholders(n int) string {
	if n < 1 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
func jvmSlicesContains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func jvmUniqueSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	out := values[:0]
	for _, v := range values {
		if v != "" && (len(out) == 0 || out[len(out)-1] != v) {
			out = append(out, v)
		}
	}
	return out
}
func jvmMergeSortedUnique(a, b []string) []string { return jvmUniqueSorted(append(a, b...)) }
