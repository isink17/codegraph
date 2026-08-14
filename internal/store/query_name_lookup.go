package store

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Batched multi-name symbol lookup.
//
// `lookupSymbolIDs` resolves one name through a four-stage cascade: exact
// qualified name, exact name, qualified-name suffix, then the short name. Only
// the first stage that matches contributes. That is fine for one name.
//
// `lookupSymbolIDsForNames` used to call it once per name, and `FindCallees`
// calls it with every distinct unresolved destination a symbol has. A symbol
// calling 300 vendored functions therefore ran 300 cascades, and because the
// third stage is a leading-wildcard LIKE that no index can serve, each of those
// was a full scan of the symbol table. On the 100k fixture that measured 8.8
// *seconds* for a single find_callees call -- three orders of magnitude over
// the budget, and a cliff that grows with both repo size and fan-out.
//
// This file resolves the same cascade for all names at once:
//
//	stage 1 and 2  -- indexed equality, one statement per chunk of names
//	stage 3        -- ONE scan of the symbol table for every remaining name,
//	                  matched in Go against the same LIKE patterns the
//	                  per-name SQL used
//	stage 4        -- indexed equality on the short names
//
// The per-name semantics are unchanged: a name still takes the ids of the first
// stage that yields any, and a name that resolves early never reaches a later
// stage. What changes is that stage 3 costs one scan for the whole call instead
// of one scan per name.

// nameLookupChunk bounds how many names go into a single IN list.
const nameLookupChunk = 400

// asciiLower lowercases ASCII letters and leaves every other byte alone, which
// is exactly the folding SQLite's LIKE applies.
func asciiLower(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + 'a' - 'A'
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// suffixMatcher evaluates stage 3's two LIKE patterns -- `'%.'||short` and
// `'%::'||short` -- for a whole set of short names in a single pass over a
// qualified name.
//
// Getting this exactly right matters more than it looks, because two obvious
// shortcuts are both wrong:
//
//   - "the tail after the last separator equals short". False whenever short
//     itself contains a separator: `lookupSymbolShortName` splits on the last
//     "::" first, so `ns::Foo.Bar` yields the short `Foo.Bar`, and
//     `pkg.Foo.Bar LIKE '%.Foo.Bar'` matches while its after-last-dot tail is
//     just `Bar`.
//   - "route anything containing a LIKE metacharacter to the old per-name SQL".
//     `_` is a LIKE metacharacter and also the single most common character in
//     identifiers outside Go, so every snake_case name would take the per-name
//     full-scan path -- which is the exact cliff this file exists to remove.
//
// So the patterns are evaluated here instead. `_` matches one character and
// therefore leaves the pattern fixed-length, which is what lets the common case
// stay a length-bucketed lookup rather than a scan over every short name. `%`
// is the only genuinely variable-length case and is rare enough in an
// identifier to be handled by a general matcher.
//
// All comparisons are ASCII case-insensitive, because SQLite's LIKE is.
type suffixMatcher struct {
	// literal maps a folded fixed-length short name to the original spellings
	// that fold to it. A slice, not a single value, because two different
	// shorts can fold together (`Foo` and `foo`) and each still needs its own
	// hit list.
	literal map[string][]string
	// underscore holds folded fixed-length shorts that contain '_', bucketed by
	// length so a row only compares against candidates that could fit.
	underscore map[int][]string
	// percent holds folded shorts containing '%', the only variable-length
	// case.
	percent []string
	// lengths is every distinct fixed length in literal/underscore, so a row
	// checks each candidate suffix length exactly once.
	lengths []int
	// original maps a folded short back to its original spellings.
	original map[string][]string
}

func newSuffixMatcher(shorts []string) *suffixMatcher {
	m := &suffixMatcher{
		literal:    map[string][]string{},
		underscore: map[int][]string{},
		original:   map[string][]string{},
	}
	seenLen := map[int]bool{}
	addLen := func(n int) {
		if !seenLen[n] {
			seenLen[n] = true
			m.lengths = append(m.lengths, n)
		}
	}
	for _, short := range shorts {
		folded := asciiLower(short)
		m.original[folded] = append(m.original[folded], short)
		switch {
		case strings.ContainsRune(short, '%'):
			m.percent = append(m.percent, folded)
		case strings.ContainsRune(short, '_'):
			m.underscore[len(folded)] = append(m.underscore[len(folded)], folded)
			addLen(len(folded))
		default:
			m.literal[folded] = append(m.literal[folded], short)
			addLen(len(folded))
		}
	}
	return m
}

func (m *suffixMatcher) empty() bool {
	return len(m.literal) == 0 && len(m.underscore) == 0 && len(m.percent) == 0
}

// match calls hit(short) once for every short name whose pattern matches
// foldedQName. A short may be reported more than once per row (both separators
// can match); callers deduplicate.
func (m *suffixMatcher) match(foldedQName string, hit func(short string)) {
	n := len(foldedQName)
	// Literal candidates are exact on any input: equality is byte equality, and
	// the separator bytes '.' and ':' cannot occur inside a multi-byte rune.
	// Only the '_' patterns depend on characters lining up with bytes.
	asciiOnly := isASCII(foldedQName)
	for _, l := range m.lengths {
		for _, sep := range [2]string{".", "::"} {
			start := n - l
			sepStart := start - len(sep)
			if sepStart < 0 || foldedQName[sepStart:start] != sep {
				continue
			}
			suffix := foldedQName[start:]
			for _, short := range m.literal[suffix] {
				hit(short)
			}
			if !asciiOnly {
				continue
			}
			for _, cand := range m.underscore[l] {
				if underscoreEqual(suffix, cand) {
					for _, short := range m.original[cand] {
						hit(short)
					}
				}
			}
		}
	}
	if !asciiOnly {
		// Rare path: fall back to the general matcher for every '_' pattern,
		// because byte-length bucketing cannot select candidates here.
		for _, cands := range m.underscore {
			for _, cand := range cands {
				if likeMatch(foldedQName, "%."+cand) || likeMatch(foldedQName, "%::"+cand) {
					for _, short := range m.original[cand] {
						hit(short)
					}
				}
			}
		}
	}
	for _, cand := range m.percent {
		if likeMatch(foldedQName, "%."+cand) || likeMatch(foldedQName, "%::"+cand) {
			for _, short := range m.original[cand] {
				hit(short)
			}
		}
	}
}

// underscoreEqual reports whether s matches pattern, where '_' in pattern
// matches exactly one character.
//
// Only valid when both sides are ASCII: '_' matches one *character* in SQLite,
// not one byte, so on multi-byte input the byte-length bucketing that selects
// candidates would be wrong. `suffixMatcher.match` routes non-ASCII rows to
// likeMatch instead.
func underscoreEqual(s, pattern string) bool {
	if len(s) != len(pattern) {
		return false
	}
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '_' && pattern[i] != s[i] {
			return false
		}
	}
	return true
}

// isASCII reports whether s contains only single-byte runes.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// likeMatch implements SQLite's LIKE for '%' and '_' over ASCII-folded inputs.
//
// Two details are easy to get wrong and both silently drop rows:
//
//   - '%' must be tested before the literal comparison. Testing "literal or
//     '_'" first means a '%' in the pattern facing a literal '%' in the value
//     is consumed as a literal, no backtrack point is recorded, and a later
//     mismatch can never recover. SQLite matches `'x.a%%b' LIKE '%.a%b'`; a
//     literal-first matcher does not.
//   - '_' consumes one character, not one byte, and so does the resumption
//     point after a '%'. Byte-wise advancing loses hits on any identifier
//     containing a non-ASCII rune.
func likeMatch(s, pattern string) bool {
	starP, starS := -1, 0
	si, pi := 0, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && pattern[pi] == '%':
			starP, starS = pi, si
			pi++
		case pi < len(pattern) && pattern[pi] == '_':
			_, size := utf8.DecodeRuneInString(s[si:])
			si += size
			pi++
		case pi < len(pattern) && pattern[pi] == s[si]:
			si++
			pi++
		case starP >= 0:
			pi = starP + 1
			_, size := utf8.DecodeRuneInString(s[starS:])
			starS += size
			si = starS
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '%' {
		pi++
	}
	return pi == len(pattern)
}

// trimLookupName is the normalization every name lookup applies before it
// matches anything. It is named because two callers now have to agree on it:
// lookupSymbolIDsByName keys its result on the trimmed form, so a caller that
// looks the result up by the raw name silently gets nothing.
func trimLookupName(name string) string {
	return strings.TrimSpace(strings.TrimPrefix(name, "::"))
}

// lookupSymbolIDsForNames resolves every name through the same cascade
// lookupSymbolIDs applies, and returns the union of the resulting ids in
// first-seen order.
func (s *Store) lookupSymbolIDsForNames(ctx context.Context, repoID int64, names []string) ([]int64, error) {
	resolved, err := s.lookupSymbolIDsByName(ctx, repoID, names)
	if err != nil || len(resolved) == 0 {
		return nil, err
	}
	seen := map[int64]struct{}{}
	var out []int64
	for _, name := range dedupeLookupNames(names) {
		for _, id := range resolved[name] {
			if id == 0 {
				continue
			}
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// dedupeLookupNames trims and deduplicates while preserving order, so the
// returned id order matches the pre-P12 per-name loop for the same input.
func dedupeLookupNames(names []string) []string {
	ordered := make([]string, 0, len(names))
	pending := make(map[string]bool, len(names))
	for _, name := range names {
		trimmed := trimLookupName(name)
		if trimmed == "" || pending[trimmed] {
			continue
		}
		pending[trimmed] = true
		ordered = append(ordered, trimmed)
	}
	return ordered
}

// lookupSymbolIDsByName runs the same cascade and keeps the per-name result
// instead of flattening it.
//
// Batched context expansion needs the association: a seed's unresolved callee
// names must resolve to that seed's callees, and merging thirty seeds' names
// into one id union would give every seed everyone else's graph. The flat
// lookupSymbolIDsForNames is now a thin wrapper over this, so the two cannot
// diverge in cascade semantics.
//
// Keys are the trimmed names -- see trimLookupName.
func (s *Store) lookupSymbolIDsByName(ctx context.Context, repoID int64, names []string) (map[string][]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	ordered := dedupeLookupNames(names)
	if len(ordered) == 0 {
		return nil, nil
	}

	resolved := make(map[string][]int64, len(ordered))
	remaining := ordered

	// Stages 1 and 2: exact qualified_name, then exact name.
	for _, column := range []string{"qualified_name", "name"} {
		if len(remaining) == 0 {
			break
		}
		hits, err := s.symbolIDsByColumn(ctx, repoID, column, remaining)
		if err != nil {
			return nil, err
		}
		remaining = assignHits(resolved, remaining, hits, func(name string) string { return name })
	}

	// Stage 3: qualified-name suffix match on the short name. One scan for all
	// remaining names, whatever their spelling.
	var shorts []string
	seenShort := map[string]bool{}
	for _, name := range remaining {
		short := lookupSymbolShortName(name)
		if short == "" || seenShort[short] {
			continue
		}
		seenShort[short] = true
		shorts = append(shorts, short)
	}
	if len(shorts) > 0 {
		hits, err := s.symbolIDsBySuffixScan(ctx, repoID, shorts)
		if err != nil {
			return nil, err
		}
		remaining = assignHits(resolved, remaining, hits, lookupSymbolShortName)
	}

	// Stage 4: exact name match on the short name, for names whose short name
	// actually differs from the name itself (the pre-P12 condition).
	if len(remaining) > 0 {
		var stage4 []string
		byShort := map[string][]string{}
		for _, name := range remaining {
			short := lookupSymbolShortName(name)
			if short == "" || short == name {
				continue
			}
			if len(byShort[short]) == 0 {
				stage4 = append(stage4, short)
			}
			byShort[short] = append(byShort[short], name)
		}
		if len(stage4) > 0 {
			hits, err := s.symbolIDsByColumn(ctx, repoID, "name", stage4)
			if err != nil {
				return nil, err
			}
			for short, ids := range hits {
				for _, name := range byShort[short] {
					if _, done := resolved[name]; !done {
						resolved[name] = ids
					}
				}
			}
		}
	}

	return resolved, nil
}

// assignHits records ids for every still-unresolved name whose key appears in
// hits, and returns the names that remain unresolved.
func assignHits(resolved map[string][]int64, remaining []string, hits map[string][]int64, key func(string) string) []string {
	if len(hits) == 0 {
		return remaining
	}
	out := remaining[:0:0]
	for _, name := range remaining {
		if ids, ok := hits[key(name)]; ok && len(ids) > 0 {
			resolved[name] = ids
			continue
		}
		out = append(out, name)
	}
	return out
}

// symbolIDsByColumn resolves names against an indexed equality column,
// returning column value -> ids.
func (s *Store) symbolIDsByColumn(ctx context.Context, repoID int64, column string, names []string) (map[string][]int64, error) {
	if column != "qualified_name" && column != "name" {
		return nil, nil
	}
	out := map[string][]int64{}
	for start := 0; start < len(names); start += nameLookupChunk {
		end := min(start+nameLookupChunk, len(names))
		chunk := names[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		rows, err := s.neighborQuery(ctx, `
			SELECT s.`+column+`, s.id
			FROM symbols s
			WHERE s.repo_id = ? AND s.`+column+` IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
			ORDER BY s.`+column+` ASC, s.id ASC
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key string
			var id int64
			if err := rows.Scan(&key, &id); err != nil {
				rows.Close()
				return nil, err
			}
			out[key] = append(out[key], id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// symbolIDsBySuffixScan performs stage 3 for every short name in one pass.
//
// It reads (qualified_name, id) for the repository -- a covering scan of
// idx_symbols_repo_qname -- and evaluates both LIKE patterns for every short
// name against each row. One scan replaces one-scan-per-name.
//
// The scan deliberately does not join `files`, which the per-name SQL did. The
// join could only ever exclude a symbol whose `file_id` names no row, which
// `PRAGMA foreign_keys = ON` makes unreachable, and the page query that
// consumes these ids joins `files` again anyway -- so the observable result is
// the same and the scan stays covering.
func (s *Store) symbolIDsBySuffixScan(ctx context.Context, repoID int64, shorts []string) (map[string][]int64, error) {
	matcher := newSuffixMatcher(shorts)
	if matcher.empty() {
		return nil, nil
	}

	rows, err := s.neighborQuery(ctx, `
		SELECT s.qualified_name, s.id
		FROM symbols s
		WHERE s.repo_id = ?
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]int64{}
	lastID := map[string]int64{}
	for rows.Next() {
		var qname string
		var id int64
		if err := rows.Scan(&qname, &id); err != nil {
			return nil, err
		}
		folded := asciiLower(qname)
		matcher.match(folded, func(short string) {
			// Both separators can match the same row; keep one id per short.
			if seen, ok := lastID[short]; ok && seen == id {
				return
			}
			lastID[short] = id
			out[short] = append(out[short], id)
		})
	}
	return out, rows.Err()
}
