package store

import (
	"context"
	"fmt"
)

// architectureTopN is how many entry points / hub symbols the architecture
// overview reports. It was a literal 15 in both queries before P12; naming it
// keeps the two lists the same length by construction.
const architectureTopN = 15

// topDegreeSymbols returns the N symbols with the highest edge degree in one
// direction, as `{qualified_name, kind, file, <countKey>}` maps.
//
// The pre-P12 query said what it meant -- `symbols LEFT JOIN edges GROUP BY
// s.id ORDER BY count DESC LIMIT 15` -- and paid for it: SQLite had to visit
// every symbol in the repository, build one group per symbol, and sort all of
// them to find fifteen. On a 100k-symbol graph that was the single slowest
// query in the whole local surface (~400ms, four times the next worst).
//
// Aggregating on the *edge* side inverts the cost. Only symbols that actually
// have an edge in that direction can be in the top N, so the grouping runs over
// the edge index and produces at most one row per referenced symbol; the symbol
// and file rows are then fetched for fifteen ids.
//
// The one case where that is not equivalent is a repository with fewer than N
// referenced symbols: the old query padded the list with zero-degree symbols,
// because a LEFT JOIN keeps them. `fillZeroDegree` reproduces that padding, so
// a small or edge-less repository still gets an N-length list. Its scan only
// runs in exactly that case.
func (s *Store) topDegreeSymbols(ctx context.Context, repoID int64, degreeCol, countKey string, limit int) ([]map[string]any, error) {
	if degreeCol != "dst_symbol_id" && degreeCol != "src_symbol_id" {
		return nil, fmt.Errorf("topDegreeSymbols: unsupported column %q", degreeCol)
	}
	out := []map[string]any{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.qualified_name, s.kind, f.path, agg.degree
		FROM (
			SELECT e.`+degreeCol+` AS sid, COUNT(1) AS degree
			FROM edges e
			WHERE e.repo_id = ? AND e.`+degreeCol+` IS NOT NULL
			GROUP BY e.`+degreeCol+`
			-- sid is the tie-break so which symbols make the cut is at least
			-- deterministic for a given database. It cannot be qualified_name:
			-- that would mean joining before limiting, which is the whole cost
			-- this shape exists to avoid.
			ORDER BY degree DESC, sid ASC
			LIMIT ?
		) agg
		JOIN symbols s ON s.id = agg.sid
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ?
		ORDER BY agg.degree DESC, s.qualified_name ASC
	`, repoID, limit, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var qname, kind, path string
		var degree int
		if err := rows.Scan(&qname, &kind, &path, &degree); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"qualified_name": qname, "kind": kind, "file": path, countKey: degree,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) >= limit {
		return out, nil
	}
	return s.fillZeroDegree(ctx, repoID, degreeCol, countKey, limit-len(out), out)
}

// fillZeroDegree tops a short top-degree list up with symbols that have no edge
// in that direction, preserving the pre-P12 behaviour of always returning a
// full-length list on graphs with few edges.
func (s *Store) fillZeroDegree(ctx context.Context, repoID int64, degreeCol, countKey string, need int, out []map[string]any) ([]map[string]any, error) {
	if need <= 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.qualified_name, s.kind, f.path
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM edges e WHERE e.repo_id = ? AND e.`+degreeCol+` = s.id
		  )
		ORDER BY s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
		LIMIT ?
	`, repoID, repoID, need)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var qname, kind, path string
		if err := rows.Scan(&qname, &kind, &path); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"qualified_name": qname, "kind": kind, "file": path, countKey: 0,
		})
	}
	return out, rows.Err()
}
