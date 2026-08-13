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
// edge degree in one direction.
//
// The maximum degree is found first and the name is then chosen among the
// symbols that have exactly that degree, ordered by qualified name. Picking the
// name that way -- rather than taking whatever row a `ORDER BY count DESC LIMIT
// 1` happens to yield -- keeps the choice stable across re-indexes, which
// assign different row ids to the same code.
func (s *Store) extremeDegreeSymbol(ctx context.Context, repoID int64, degreeCol string) (string, error) {
	if degreeCol != "dst_symbol_id" && degreeCol != "src_symbol_id" {
		return "", errors.New("store: unsupported degree column")
	}
	var maxDegree sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT MAX(degree) FROM (
			SELECT COUNT(1) AS degree
			FROM edges
			WHERE repo_id = ? AND `+degreeCol+` IS NOT NULL
			GROUP BY `+degreeCol+`
		)
	`, repoID).Scan(&maxDegree); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !maxDegree.Valid || maxDegree.Int64 == 0 {
		return "", nil
	}

	var qname string
	err := s.db.QueryRowContext(ctx, `
		SELECT s.qualified_name
		FROM (
			SELECT `+degreeCol+` AS sid, COUNT(1) AS degree
			FROM edges
			WHERE repo_id = ? AND `+degreeCol+` IS NOT NULL
			GROUP BY `+degreeCol+`
			HAVING degree = ?
		) agg
		JOIN symbols s ON s.id = agg.sid
		WHERE s.repo_id = ?
		ORDER BY s.qualified_name ASC, s.id ASC
		LIMIT 1
	`, repoID, maxDegree.Int64, repoID).Scan(&qname)
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
	err := s.db.QueryRowContext(ctx, `
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
