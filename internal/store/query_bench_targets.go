package store

import (
	"context"
	"database/sql"
	"errors"
)

// QueryBenchTargets names the representative query arguments selected from an
// indexed graph.
//
// A query benchmark is only reproducible if it asks the same questions on every
// run, and only honest if those questions come from the graph rather than from
// the benchmark author's imagination. Every field here is derived by a
// deterministic query over the repository's own rows; an empty field means the
// graph genuinely has nothing to select, and the caller is expected to skip
// that scenario rather than invent a target.
//
// Selection deliberately favours the *worst* representative case (the symbol
// with the most callers, the file with the most linked tests) rather than a
// median one. A benchmark that only measures easy targets reports a latency
// nobody experiences at the moment it matters.
type QueryBenchTargets struct {
	// CallerTarget is the symbol with the highest resolved in-degree.
	CallerTarget string
	// CalleeSource is the symbol with the highest resolved out-degree.
	CalleeSource string
	// TestLinkedFile is the file with the most test links pointing at it.
	TestLinkedFile string
	// SearchTerm is a term guaranteed to match at least one symbol: the short
	// name of CallerTarget.
	SearchTerm string
}

// QueryBenchTargets selects deterministic representative targets for a query
// benchmark. It is read-only and safe on a database opened with OpenReadOnly.
func (s *Store) QueryBenchTargets(ctx context.Context, repoID int64) (QueryBenchTargets, error) {
	ctx, tx, err := s.beginParserSemanticGraphRead(ctx, repoID)
	if err != nil {
		return QueryBenchTargets{}, err
	}
	if tx != nil {
		defer tx.Rollback()
	}
	var out QueryBenchTargets

	caller, err := s.extremeDegreeSymbol(ctx, repoID, "dst_symbol_id")
	if err != nil {
		return QueryBenchTargets{}, err
	}
	out.CallerTarget = caller
	if caller != "" {
		out.SearchTerm = lookupSymbolShortName(caller)
	}

	callee, err := s.extremeDegreeSymbol(ctx, repoID, "src_symbol_id")
	if err != nil {
		return QueryBenchTargets{}, err
	}
	out.CalleeSource = callee

	file, err := s.mostTestLinkedFile(ctx, repoID)
	if err != nil {
		return QueryBenchTargets{}, err
	}
	out.TestLinkedFile = file

	return out, nil
}

// extremeDegreeSymbol returns the qualified name of the symbol with the highest
// edge degree in one direction among symbols whose qualified name is unique.
//
// The scenarios look the target up by name, and a name shared by several
// symbols (C++ TEST macros, overloads) is refused as ambiguous, so only names
// that resolve to exactly one live symbol are eligible. Ties are broken by
// qualified name rather than row id, keeping the choice stable across
// re-indexes, which assign different row ids to the same code.
func (s *Store) extremeDegreeSymbol(ctx context.Context, repoID int64, degreeCol string) (string, error) {
	if degreeCol != "dst_symbol_id" && degreeCol != "src_symbol_id" {
		return "", errors.New("store: unsupported degree column")
	}
	var qname string
	err := s.parserSemanticQueryer(ctx).QueryRowContext(ctx, `
		SELECT s.qualified_name
		FROM (
			SELECT `+degreeCol+` AS sid, COUNT(1) AS degree
			FROM edges
			WHERE repo_id = ? AND `+degreeCol+` IS NOT NULL
			GROUP BY `+degreeCol+`
		) agg
		JOIN symbols s ON s.id = agg.sid
		JOIN files f ON f.id = s.file_id AND f.repo_id = s.repo_id AND f.is_deleted = 0
		WHERE s.repo_id = ? AND NOT EXISTS (
			SELECT 1
			FROM symbols o
			JOIN files fo ON fo.id = o.file_id AND fo.repo_id = o.repo_id AND fo.is_deleted = 0
			WHERE o.repo_id = s.repo_id AND o.qualified_name = s.qualified_name AND o.id <> s.id
		)
		ORDER BY agg.degree DESC, s.qualified_name ASC, s.id ASC
		LIMIT 1
	`, repoID, repoID).Scan(&qname)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return qname, nil
}

// mostTestLinkedFile returns the repo-relative path of the file with the most
// test links pointing at it, ties broken by path.
func (s *Store) mostTestLinkedFile(ctx context.Context, repoID int64) (string, error) {
	var path string
	err := s.parserSemanticQueryer(ctx).QueryRowContext(ctx, `
		SELECT f.path
		FROM test_links t
		JOIN files f ON f.id = t.target_file_id
		WHERE t.repo_id = ? AND t.target_file_id IS NOT NULL AND f.is_deleted = 0
		GROUP BY f.path
		ORDER BY COUNT(1) DESC, f.path ASC
		LIMIT 1
	`, repoID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return path, nil
}
