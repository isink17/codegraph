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
// It aggregates degree on the edge side, finds the Nth degree cutoff, and joins
// semantic identity only for rows at or above that cutoff. The final public
// limit uses degree plus semantic identity, never a database row ID.
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
	otherCol := "src_symbol_id"
	oppositeVisible := "osf.id IS NOT NULL"
	if degreeCol == "src_symbol_id" {
		otherCol = "dst_symbol_id"
		oppositeVisible = "e.dst_symbol_id IS NULL OR osf.id IS NOT NULL"
	}
	out := []map[string]any{}
	rows, err := s.db.QueryContext(ctx, `
		WITH degrees AS (
			SELECT e.`+degreeCol+` AS sid, COUNT(1) AS degree
			FROM edges e
			-- Degree is reported over the active resolved graph. Both the
			-- counted symbol and, when the far end is resolved, its counterpart
			-- must live in an active file: an edge only one visible symbol
			-- takes part in is not evidence the user can see, and a ghost
			-- symbol must not reach the cutoff and consume a top-N slot.
			-- Unresolved far ends keep counting exactly as before.
			JOIN symbols ds ON ds.id = e.`+degreeCol+`
			JOIN files df ON df.id = ds.file_id AND df.is_deleted = 0
			JOIN files ef ON ef.id = e.file_id AND ef.repo_id = e.repo_id AND ef.is_deleted = 0
			LEFT JOIN symbols os ON os.id = e.`+otherCol+`
			LEFT JOIN files osf ON osf.id = os.file_id AND osf.is_deleted = 0
			WHERE e.repo_id = ? AND e.`+degreeCol+` IS NOT NULL
			  AND (`+oppositeVisible+`)
			GROUP BY e.`+degreeCol+`
		), cutoff AS (
			SELECT MIN(degree) AS degree FROM (
				SELECT degree FROM degrees
			ORDER BY degree DESC
				LIMIT ?
			)
		)
		SELECT s.qualified_name, s.kind, f.path, d.degree
		FROM degrees d
		JOIN symbols s ON s.id = d.sid
		JOIN files f ON f.id = s.file_id AND f.is_deleted = 0
		WHERE s.repo_id = ? AND d.degree >= (SELECT degree FROM cutoff)
		ORDER BY d.degree DESC, f.path ASC,
		         s.qualified_name ASC, s.kind ASC, s.signature ASC,
		         s.stable_key ASC, s.start_line ASC, s.start_col ASC,
		         s.end_line ASC, s.end_col ASC
		LIMIT ?
	`, repoID, limit, repoID, limit)
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
		// `file` is the stored `files.path`: a logical repository identity, kept
		// byte for byte like `top_directories` in the same overview. A backslash
		// is filename data, so it is not rewritten here.
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
	otherCol := "src_symbol_id"
	oppositeVisible := "osf.id IS NOT NULL"
	if degreeCol == "src_symbol_id" {
		otherCol = "dst_symbol_id"
		oppositeVisible = "e.dst_symbol_id IS NULL OR osf.id IS NOT NULL"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.qualified_name, s.kind, f.path
		FROM symbols s
		JOIN files f ON f.id = s.file_id AND f.is_deleted = 0
		WHERE s.repo_id = ?
		  -- Mirrors the degrees CTE: an edge that no longer counts towards
		  -- degree must not disqualify its symbol from the zero-degree padding
		  -- either, or a symbol whose only edges point at ghosts would vanish
		  -- from the overview rather than appear with degree 0.
		  AND NOT EXISTS (
			  SELECT 1 FROM edges e
			  JOIN files ef ON ef.id = e.file_id AND ef.repo_id = e.repo_id AND ef.is_deleted = 0
		      LEFT JOIN symbols os ON os.id = e.`+otherCol+`
		      LEFT JOIN files osf ON osf.id = os.file_id AND osf.is_deleted = 0
		      WHERE e.repo_id = ? AND e.`+degreeCol+` = s.id
		        AND (`+oppositeVisible+`)
		  )
		ORDER BY f.path ASC, s.qualified_name ASC,
		         s.kind ASC, s.signature ASC, s.stable_key ASC,
		         s.start_line ASC, s.start_col ASC, s.end_line ASC, s.end_col ASC
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
