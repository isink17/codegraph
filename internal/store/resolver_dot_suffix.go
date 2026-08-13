package store

import (
	"context"
	"database/sql"
	"strings"
)

// tmpDotSuffixNamesTable holds the distinct unresolved multi-dot dst_names the
// dot-suffix strategy still has to match, each tagged with the prefilter tier
// it qualifies for (see dotSuffixPrefilter).
const tmpDotSuffixNamesTable = "temp.tmp_resolver_dot_suffix_names"

// Prefilter tiers for one dst_name. Both apply the same
// `qualified_name LIKE '%.' || dst_name` join condition; they differ only in
// whether an indexed necessary condition narrows the symbols side first.
const (
	// dotSuffixTierExact: the last two segments hold no LIKE wildcard, so a
	// matching symbol's `dot_tail2` equals them (case-insensitively).
	dotSuffixTierExact = 1
	// dotSuffixTierScan: the last two segments hold a LIKE wildcard. Both `%`
	// and `_` can match '.' or '/', which moves or erases the boundary
	// `dot_tail2` is derived from, so nothing about the symbol's tail is
	// implied — not its content and not its length. These names keep the
	// original scan-per-name form.
	dotSuffixTierScan = 2
)

// populateDotSuffixCandidates fills `tmp_edge_dot_suffix` with one candidate
// group per (dst_name, candidate language).
//
// The matching predicate is unchanged: a symbol is a candidate exactly when
// `qualified_name LIKE '%.' || dst_name`, and the P7 aggregates and P3-facing
// HAVING are the shared fragments every strategy uses. What changes is how the
// candidate symbols are *reached*.
//
// Driving the join off the LIKE alone forces a full `symbols` scan per distinct
// name, which profiling of a full index measured as the single largest cost in
// Pass 2. Names with no LIKE wildcard in their last two dot-separated segments
// admit an indexed prefilter: the matched suffix is then a literal
// `'.' || dst_name`, so it contains no '/', lies wholly inside the symbol's
// after-slash portion, and ends in dst_name's own last two segments — which is
// exactly what migration 017 persists as `symbols.dot_tail2`. Wildcards earlier
// in the name cannot move that boundary, so they do not disqualify a name.
//
// That condition is necessary only. The LIKE stays in the join, so the
// prefilter can never admit a match the old form rejected, and by construction
// it never rejects one the old form admitted. It is compared under NOCASE
// because SQLite's LIKE is ASCII case-insensitive, and is served by
// `idx_symbols_repo_dot_tail2_nocase` (migration 021).
//
// A wildcard *inside* those two segments admits nothing, because `%` and `_`
// both match '.' and '/': a symbol matching `x.a_b.c` may have its own
// `dot_tail2` be `b.c` (the `_` matched a '.') or empty (it matched a '/').
// Neither the content nor the length of the symbol's tail is constrained, so
// those names run the unfiltered scan.
//
// The two name sets are disjoint and both write into the same candidate table,
// so its (dst_name, dst_language) primary key cannot collide.
func (s *Store) populateDotSuffixCandidates(ctx context.Context, tx *sql.Tx, repoID int64) error {
	names, err := s.dotSuffixNames(ctx, tx, repoID)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+tmpDotSuffixNamesTable); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_resolver_dot_suffix_names`+
		`(dst_name TEXT PRIMARY KEY, tier INTEGER NOT NULL, tail2 TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return err
	}
	dropped := false
	defer func() {
		if dropped {
			return
		}
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+tmpDotSuffixNamesTable)
	}()

	tiers, err := insertDotSuffixNames(ctx, tx, names)
	if err != nil {
		return err
	}

	for _, pass := range []struct {
		tier int
		join string
	}{
		// `dot_tail2 != ''` is what lets SQLite use the partial prefilter
		// index from migration 021: a partial index is only usable when the
		// query implies its WHERE clause. It is also implied by the equality
		// itself (tail2 is never empty in this tier), so it never narrows the
		// result. The scan tier must not carry it: a wildcard in the pattern
		// can match a '/', so a matching symbol's after-slash portion may
		// legitimately hold no '.'.
		{dotSuffixTierExact, `AND s.dot_tail2 != '' AND s.dot_tail2 = n.tail2 COLLATE NOCASE`},
		{dotSuffixTierScan, ``},
	} {
		if !tiers[pass.tier] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tmp_edge_dot_suffix(dst_name, dst_language, any_symbol_id, production_symbol_id)
			SELECT n.dst_name, s.language,
				`+resolverCandidateAggregatesSQL+`
			FROM `+tmpDotSuffixNamesTable+` n
			JOIN symbols s
			  ON s.repo_id = ?
			 `+pass.join+`
			 AND s.qualified_name LIKE '%.' || n.dst_name
			`+resolverCandidateJoinSQL+`
			WHERE n.tier = ? AND s.language != ''
			GROUP BY n.dst_name, s.language
			`+resolverCandidateHavingSQL+`
		`, repoID, pass.tier); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE `+tmpDotSuffixNamesTable); err != nil {
		return err
	}
	dropped = true
	return nil
}

// dotSuffixNamesPerStatement keeps the seeding INSERT's parameter count
// (3 per name) under sqliteDefaultMaxVariables, and bounds how many rows are
// bound as arguments at once regardless of repository size.
const dotSuffixNamesPerStatement = 300

// insertDotSuffixNames seeds the name table and reports which prefilter tiers
// are actually present, so the caller can skip the pass nothing would feed.
func insertDotSuffixNames(ctx context.Context, tx *sql.Tx, names []string) (map[int]bool, error) {
	const width = 3
	tiers := map[int]bool{}
	args := make([]any, 0, dotSuffixNamesPerStatement*width)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		var b strings.Builder
		b.WriteString(`INSERT OR IGNORE INTO ` + tmpDotSuffixNamesTable + `(dst_name, tier, tail2) VALUES `)
		for i := 0; i < len(args)/width; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?,?,?)")
		}
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return err
		}
		args = args[:0]
		return nil
	}
	for _, name := range names {
		tier, tail2 := dotSuffixPrefilter(name)
		tiers[tier] = true
		args = append(args, name, tier, tail2)
		if len(args) >= dotSuffixNamesPerStatement*width {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return tiers, nil
}

// dotSuffixNames returns the distinct unresolved dst_names the dot-suffix
// strategy applies to: at least two dots and no slash.
//
// The names are materialised in Go so the tier classification can be done with
// Go string handling rather than nested SQL substring arithmetic. That is one
// string per distinct unresolved multi-dot name, not per edge, and it is the
// same set the old form's inline `SELECT DISTINCT` produced.
func (s *Store) dotSuffixNames(ctx context.Context, tx *sql.Tx, repoID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT dst_name
		FROM edges
		WHERE repo_id = ? AND dst_symbol_id IS NULL
		AND instr(dst_name, '.') > 0 AND instr(dst_name, '/') = 0
		AND instr(substr(dst_name, instr(dst_name, '.') + 1), '.') > 0
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// dotSuffixPrefilter classifies one dst_name and returns the `dot_tail2` value
// the exact tier compares against (empty for the scan tier).
//
// Only the last two segments matter: those are the segments the prefilter makes
// a claim about. A wildcard earlier in the name only affects the part of the
// pattern before that tail, and cannot move the tail boundary.
//
// Three things disqualify a tail, each because it breaks the derivation rather
// than merely because it is unusual:
//
//	'%' or '_'  either can match '.' or '/', which moves or erases the
//	            boundary `dot_tail2` is derived from.
//	empty       the name has fewer than two segments after its last '/', so no
//	            tail is derivable; a scan is the safe reading.
//
// A '/' in the name needs no special handling: dotTail2 applies the same
// after-last-slash rule to the name that migration 017 applied to the symbol,
// so both sides drop the same prefix and the returned tail is slash-free by
// construction.
func dotSuffixPrefilter(dstName string) (tier int, tail2 string) {
	tail := dotTail2(dstName)
	if tail == "" || strings.ContainsAny(tail, "%_") {
		return dotSuffixTierScan, ""
	}
	return dotSuffixTierExact, tail
}
