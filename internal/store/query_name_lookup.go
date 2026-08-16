package store

import (
	"context"
	"sort"
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
// A name still takes the ids of the first stage that yields any, and a name
// that resolves early never reaches a later stage; stage 3 costs one scan for
// the whole call instead of one scan per name.
//
// This batched cascade serves EDGE EVIDENCE (unresolved dst_names feeding
// FindCallees and context expansion), and since P22.1 it deliberately differs
// from the single-name cascade `lookupSymbolIDs` runs for user-typed input:
// member-qualified spellings (see memberQualifiedName) never degrade to their
// bare tail here, because a call site's qualifier is evidence and discarding
// it fabricates relationships (`rows.Close` claiming every project Close).
// User input keeps the forgiving cascade.
//
// P22.7 makes the split explicit in the type system. There are two contracts,
// and conflating them is the defect this file used to carry:
//
//	DISCOVERY  a user types a name and asks what the repository calls that.
//	           No language is supplied and none may be invented, so every
//	           language's answer is a legitimate result. `lookupSymbolIDs`
//	           (store.go) owns this, and nothing here narrows it.
//	IDENTITY   a persisted source spelled a name and CodeGraph is deciding
//	           WHICH project symbol that spelling denotes. The source's
//	           persisted language is evidence, and a candidate in another
//	           language is not the thing that was named -- exactly the P2 rule
//	           the resolver applies to implicit bindings.
//
// Everything below is the identity contract. It takes `[]nameLanguages` rather
// than `[]string` so a caller cannot reach it without saying whose language the
// spelling carries, and a name whose languages are empty resolves to nothing:
// an unknown source language fails closed rather than matching everything.
// Explicit cross-language links are unaffected -- they are `cross_language_ref`
// edges with a bound `dst_symbol_id`, created by ResolveCrossLanguageLinks from
// import-bridge evidence, and no name cascade runs for them.

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
		case memberQualifiedName(short):
			// Member-qualified full spellings are evidence, not patterns
			// (P22.1): their '_' and '%' bytes match literally, exactly like
			// the escaped LIKE legs FindCallers runs for the same rule.
			// Treating `config.load_config` as a wildcard would let it claim
			// `x.config.loadXconfig` -- a fabrication the caller side refuses.
			m.literal[folded] = append(m.literal[folded], short)
			addLen(len(folded))
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

// memberQualifiedName reports whether a destination name is member/scope-
// qualified syntax whose qualifier is evidence: it contains a '.' or a "::"
// and no '/'. Such a spelling must never degrade to its bare tail (P22.1) --
// `rows.Close` does not spell any project symbol named Close; it spells a
// member of `rows`. Import-path spellings (containing '/') keep the legacy
// short-name fallback: they are package-qualified by construction and the
// own-module import mapping that would resolve them properly is deferred.
func memberQualifiedName(name string) bool {
	return !strings.ContainsRune(name, '/') &&
		(strings.ContainsRune(name, '.') || strings.Contains(name, "::"))
}

// lookupSuffixKey returns the string a name contributes to suffix evidence:
// the full spelling for member-qualified names (so the qualifier stays part of
// the match), the short name for everything else (unchanged legacy behavior).
func lookupSuffixKey(name string) string {
	if memberQualifiedName(name) {
		return name
	}
	return lookupSymbolShortName(name)
}

// boundaryProperSuffixes returns the proper suffixes of a qualified spelling
// cut at '.', "::", and '/' boundaries, keeping only suffixes that are still
// qualified (contain a '.' or "::"). The bare tail is deliberately excluded:
// a spelling that shares only its tail with an identity shares no qualifier
// evidence with it.
func boundaryProperSuffixes(qname string) []string {
	var out []string
	for i := 0; i < len(qname); i++ {
		switch {
		case qname[i] == ':' && i+1 < len(qname) && qname[i+1] == ':':
			if s := qname[i+2:]; s != "" && memberSeparated(s) {
				out = append(out, s)
			}
			i++
		case qname[i] == '.' || qname[i] == '/':
			if s := qname[i+1:]; s != "" && memberSeparated(s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// memberSeparated reports whether a spelling still carries a qualifier
// separator.
func memberSeparated(s string) bool {
	return strings.ContainsRune(s, '.') || strings.Contains(s, "::")
}

// foldedExtendsAtBoundary reports whether dst spells a deeper form of qname:
// dst ends with qname preceded by a '.', "::", or '/' boundary. Comparison is
// ASCII case-insensitive, the folding SQLite's LIKE would have applied.
func foldedExtendsAtBoundary(dst, qname string) bool {
	return prefoldedExtendsAtBoundary(asciiLower(dst), asciiLower(qname))
}

// prefoldedExtendsAtBoundary is foldedExtendsAtBoundary over inputs the caller
// has already folded, so a scan can fold each row once instead of once per
// (row, identity) pair.
func prefoldedExtendsAtBoundary(d, q string) bool {
	if q == "" || len(d) <= len(q) {
		return false
	}
	if !strings.HasSuffix(d, q) {
		return false
	}
	switch boundary := d[:len(d)-len(q)]; {
	case strings.HasSuffix(boundary, "::"), strings.HasSuffix(boundary, "."), strings.HasSuffix(boundary, "/"):
		return true
	}
	return false
}

// nameLanguages is one lookup name together with the persisted languages of the
// sources that spelled it -- the evidence that decides which candidates may
// answer it.
//
// It is a distinct type, and every identity-side entry point takes a slice of
// it, so a caller physically cannot ask this cascade a question without saying
// whose language the spelling carries. An empty `languages` is a legitimate
// answer meaning "the source has no persisted language", and it resolves to
// nothing rather than to everything.
type nameLanguages struct {
	name      string
	languages []string
}

// symbolIDLang is a candidate symbol id tagged with its own persisted language,
// so the per-name language decision can be made once over rows the batched
// statements already had to read.
type symbolIDLang struct {
	id       int64
	language string
}

// sortedLanguages renders a language set in a deterministic order. Languages
// are semantic strings, so ordering by them can never make an answer depend on
// a row id or on insertion order.
func sortedLanguages(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for lang := range set {
		if lang == "" {
			continue
		}
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

// lookupSymbolIDsForNameLanguages resolves every (name, source language) pair
// through the batched edge-evidence cascade (lookupSymbolIDsByNameLanguage --
// NOT the forgiving language-agnostic cascade lookupSymbolIDs runs for user
// input; member-qualified spellings deliberately diverge, see the file header)
// and returns the union of the resulting ids in first-seen order.
func (s *Store) lookupSymbolIDsForNameLanguages(ctx context.Context, repoID int64, names []nameLanguages) ([]int64, error) {
	resolved, err := s.lookupSymbolIDsByNameLanguage(ctx, repoID, names)
	if err != nil || len(resolved) == 0 {
		return nil, err
	}
	seen := map[int64]struct{}{}
	var out []int64
	for _, key := range dedupeNameLanguages(names) {
		for _, id := range resolved[key] {
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

// dedupeNameLanguages trims and deduplicates (name, language) pairs while
// preserving input order, so the returned id order is a function of the caller's
// evidence order and not of map iteration.
func dedupeNameLanguages(names []nameLanguages) []symbolLangKey {
	ordered := make([]symbolLangKey, 0, len(names))
	pending := make(map[symbolLangKey]bool, len(names))
	for _, entry := range names {
		trimmed := trimLookupName(entry.name)
		if trimmed == "" {
			continue
		}
		for _, language := range entry.languages {
			if language == "" {
				// No persisted language is not a wildcard: it is an absence,
				// and an absent source language may not select a candidate.
				continue
			}
			key := symbolLangKey{name: trimmed, language: language}
			if pending[key] {
				continue
			}
			pending[key] = true
			ordered = append(ordered, key)
		}
	}
	return ordered
}

// lookupSymbolIDsByNameLanguage runs the same cascade and keeps the per-pair
// result instead of flattening it.
//
// Batched context expansion needs the association: a seed's unresolved callee
// names must resolve to that seed's callees, and merging thirty seeds' names
// into one id union would give every seed everyone else's graph. The flat
// lookupSymbolIDsForNameLanguages is now a thin wrapper over this, so the two
// cannot diverge in cascade semantics.
//
// The key is (trimmed name, source language) rather than the name alone,
// because the same spelling written by a Python seed and by a Go seed is two
// different questions with two different right answers. Keying on the name
// would hand whichever seed asked first the other's candidates back.
//
// The cascade is evaluated per pair, not per name: `Foo` may be answered by
// exact-name equality in one language and only by suffix evidence in another,
// and each language is entitled to the strongest evidence IT has.
func (s *Store) lookupSymbolIDsByNameLanguage(ctx context.Context, repoID int64, names []nameLanguages) (map[symbolLangKey][]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	ordered := dedupeNameLanguages(names)
	if len(ordered) == 0 {
		return nil, nil
	}
	resolved := make(map[symbolLangKey][]int64, len(ordered))
	remaining := ordered

	// Stages 1 and 2: exact qualified_name, then exact name.
	//
	// The language set is recomputed from what is still unresolved at each
	// stage, so a stage never reads candidate rows in a language no remaining
	// pair could accept.
	for _, column := range []string{"qualified_name", "name"} {
		if len(remaining) == 0 {
			break
		}
		// P22.9: a BARE spelling may not claim a type. The spellings are split
		// so the exclusion follows the syntax rather than the stage -- a
		// qualifier-bearing spelling keeps every candidate it had, in both
		// stages, because the qualifier is evidence the source itself wrote.
		//
		// This is the identity contract, not discovery: the caller is a
		// persisted source that spelled a name, so it must answer what the
		// resolver would have bound (resolver_type_scope.go). Without it a
		// Kotlin `A()` still reached `class A.A` through find_callees and
		// context expansion after the resolver had refused it.
		// The split is by spelling AND by language, because the exclusion only
		// applies where CodeGraph can decide cross-file visibility: a bare
		// Kotlin or Swift spelling keeps every candidate it had, since those
		// languages make a class visible across files with no import at all.
		bare, qualified := splitBareNameLanguages(remaining)
		bareGated, bareUngated := splitTypeScopeGatedLanguages(lookupKeyLanguages(bare))
		hits := map[string][]symbolIDLang{}
		for _, part := range []struct {
			names           []string
			languages       []string
			excludeTypeKind bool
		}{
			{lookupKeyNames(qualified), lookupKeyLanguages(qualified), false},
			{lookupKeyNames(bare), bareGated, true},
			{lookupKeyNames(bare), bareUngated, false},
		} {
			if len(part.names) == 0 || len(part.languages) == 0 {
				continue
			}
			partHits, err := s.symbolIDsByColumn(ctx, repoID, column,
				part.names, part.languages, part.excludeTypeKind)
			if err != nil {
				return nil, err
			}
			for name, candidates := range partHits {
				hits[name] = mergeCandidatesPreservingOrder(hits[name], candidates)
			}
		}
		remaining = assignHits(resolved, remaining, hits, func(name string) string { return name })
	}

	// Stage 3: qualified-name suffix evidence. One scan for all remaining
	// names, whatever their spelling. Member-qualified names contribute their
	// FULL spelling as the suffix (lookupSuffixKey), so `rows.Close` can only
	// match a deeper identity that ends in `.rows.Close` -- never every symbol
	// whose tail happens to be Close. They are additionally matched the other
	// way around: an identity that IS a qualified suffix of the spelling
	// (`other.pkg.Target` naming `pkg.Target`) is equality on qualified_name
	// over the spelling's own boundary suffixes. Both directions preserve the
	// qualifier; both were previously drowned in the bare-tail matches.
	var shorts []string
	seenShort := map[string]bool{}
	var memberNames []string
	seenMember := map[string]bool{}
	for _, key := range remaining {
		if memberQualifiedName(key.name) && !seenMember[key.name] {
			seenMember[key.name] = true
			memberNames = append(memberNames, key.name)
		}
		short := lookupSuffixKey(key.name)
		if short == "" || seenShort[short] {
			continue
		}
		seenShort[short] = true
		shorts = append(shorts, short)
	}
	if len(shorts) > 0 {
		languages := lookupKeyLanguages(remaining)
		hits, err := s.symbolIDsBySuffixScan(ctx, repoID, shorts, languages)
		if err != nil {
			return nil, err
		}
		if deeper, err := s.memberSuffixSpellingHits(ctx, repoID, memberNames, languages); err != nil {
			return nil, err
		} else {
			for name, ids := range deeper {
				hits[name] = mergeCandidatesPreservingOrder(hits[name], ids)
			}
		}
		remaining = assignHits(resolved, remaining, hits, lookupSuffixKey)
	}

	// Stage 4: exact name match on the short name, for names whose short name
	// actually differs from the name itself (the pre-P12 condition).
	// Member-qualified names are excluded: equality on the bare tail is
	// exactly the qualifier-discarding match stage 3 no longer performs.
	//
	// Every key that reaches this stage has a short name DIFFERENT from its own
	// spelling, so the spelling is qualifier-bearing and the stage degrades it to
	// a bare tail. That is deliberately stricter than the resolver, which exempts
	// qualifier-bearing spellings: the exemption there is about a spelling that
	// MATCHED with its qualifier intact, and this stage matched without it. A
	// qualified spelling that really named a class was bound by qualified
	// evidence and never reaches here, so a bare-tail match on a class is the
	// same fabrication stage 3 stopped performing for member spellings (P22.1).
	if len(remaining) > 0 {
		var stage4 []string
		seenStage4 := map[string]bool{}
		byShort := map[string][]symbolLangKey{}
		for _, key := range remaining {
			if memberQualifiedName(key.name) {
				continue
			}
			short := lookupSymbolShortName(key.name)
			if short == "" || short == key.name {
				continue
			}
			if !seenStage4[short] {
				seenStage4[short] = true
				stage4 = append(stage4, short)
			}
			byShort[short] = append(byShort[short], key)
		}
		if len(stage4) > 0 {
			gated, ungated := splitTypeScopeGatedLanguages(lookupKeyLanguages(remaining))
			hits := map[string][]symbolIDLang{}
			for _, part := range []struct {
				languages       []string
				excludeTypeKind bool
			}{{gated, true}, {ungated, false}} {
				if len(part.languages) == 0 {
					continue
				}
				partHits, err := s.symbolIDsByColumn(ctx, repoID, "name", stage4, part.languages, part.excludeTypeKind)
				if err != nil {
					return nil, err
				}
				for name, candidates := range partHits {
					hits[name] = mergeCandidatesPreservingOrder(hits[name], candidates)
				}
			}
			for _, short := range stage4 {
				for _, key := range byShort[short] {
					if _, done := resolved[key]; done {
						continue
					}
					if ids := candidateIDsInLanguage(hits[short], key.language); len(ids) > 0 {
						resolved[key] = ids
					}
				}
			}
		}
	}

	return resolved, nil
}

// lookupKeyNames returns the distinct names of a pair set, preserving order.
func lookupKeyNames(keys []symbolLangKey) []string {
	out := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key.name] {
			continue
		}
		seen[key.name] = true
		out = append(out, key.name)
	}
	return out
}

// lookupKeyLanguages returns the distinct languages of a pair set in sorted
// order. It narrows the candidate rows each statement reads to the languages
// some caller actually asked about; the exact per-pair decision is still made
// against each candidate's own language.
func lookupKeyLanguages(keys []symbolLangKey) []string {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key.language] = struct{}{}
	}
	return sortedLanguages(set)
}

// candidateIDsInLanguage keeps the candidates whose persisted language is the
// one the spelling's source was written in. This is the whole gate: a candidate
// in another language is not the symbol that was named.
func candidateIDsInLanguage(candidates []symbolIDLang, language string) []int64 {
	if language == "" {
		return nil
	}
	var out []int64
	for _, cand := range candidates {
		if cand.language == language {
			out = append(out, cand.id)
		}
	}
	return out
}

// assignHits records ids for every still-unresolved pair whose key appears in
// hits with a same-language candidate, and returns the pairs that remain
// unresolved.
func assignHits(resolved map[symbolLangKey][]int64, remaining []symbolLangKey, hits map[string][]symbolIDLang, key func(string) string) []symbolLangKey {
	if len(hits) == 0 {
		return remaining
	}
	out := remaining[:0:0]
	for _, pair := range remaining {
		if ids := candidateIDsInLanguage(hits[key(pair.name)], pair.language); len(ids) > 0 {
			resolved[pair] = ids
			continue
		}
		out = append(out, pair)
	}
	return out
}

// symbolIDsByColumn resolves names against an indexed equality column,
// returning column value -> candidates. Rows outside `languages` are excluded
// in SQL, which can only shrink the read set; the per-name language decision is
// still made in Go, because different names in one batch can carry different
// source languages.
//
// When the batch asks about ONE language the SQL predicate is already exact, so
// the column is not read back at all and every row is tagged with the language
// that was asked for. That is the common shape -- one repository, one seed
// language -- and it keeps the statement reading exactly the columns it read
// before P22.7.
// `excludeTypeKinds` drops type declarations from the candidate set. It is set
// only for bare spellings (P22.9, resolver_type_scope.go); a bare name that
// really denotes a class has already been bound by the resolver and reaches the
// answer through the destination id, so a bare-name candidate of type kind here
// is by construction one the resolver refused.
func (s *Store) symbolIDsByColumn(ctx context.Context, repoID int64, column string, names, languages []string, excludeTypeKinds bool) (map[string][]symbolIDLang, error) {
	if column != "qualified_name" && column != "name" {
		return nil, nil
	}
	if len(languages) == 0 {
		return map[string][]symbolIDLang{}, nil
	}
	single := len(languages) == 1
	langColumn := ", s.language"
	if single {
		langColumn = ""
	}
	out := map[string][]symbolIDLang{}
	for start := 0; start < len(names); start += nameLookupChunk {
		end := min(start+nameLookupChunk, len(names))
		chunk := names[start:end]
		args := make([]any, 0, len(chunk)+len(languages)+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		for _, language := range languages {
			args = append(args, language)
		}
		kindFilter := ""
		if excludeTypeKinds {
			kindFilter = ` AND s.kind NOT IN ` + typeSymbolKindsSQL
		}
		rows, err := s.neighborQuery(ctx, `
			SELECT s.`+column+`, s.id`+langColumn+`
			FROM symbols s
			WHERE s.repo_id = ? AND s.`+column+` IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
			  AND s.language IN (`+strings.TrimRight(strings.Repeat("?,", len(languages)), ",")+`)`+kindFilter+`
			ORDER BY s.`+column+` ASC, s.id ASC
		`, args...)
		if err != nil {
			return nil, err
		}
		var key, language string
		var id int64
		dest := []any{&key, &id}
		if !single {
			dest = append(dest, &language)
		} else {
			language = languages[0]
		}
		for rows.Next() {
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return nil, err
			}
			out[key] = append(out[key], symbolIDLang{id: id, language: language})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// memberSuffixSpellingHits resolves the reverse direction of stage 3 for
// member-qualified names: symbols whose full qualified_name is one of the
// spelling's own boundary suffixes (`other.pkg.Target` names `pkg.Target`).
// Indexed equality, one statement per chunk; ids arrive per name in
// longest-suffix-first order so the result cannot depend on map iteration.
func (s *Store) memberSuffixSpellingHits(ctx context.Context, repoID int64, memberNames, languages []string) (map[string][]symbolIDLang, error) {
	if len(memberNames) == 0 {
		return nil, nil
	}
	suffixesByName := make(map[string][]string, len(memberNames))
	var lookups []string
	seen := map[string]bool{}
	for _, name := range memberNames {
		suffixes := boundaryProperSuffixes(name)
		suffixesByName[name] = suffixes
		for _, suffix := range suffixes {
			if !seen[suffix] {
				seen[suffix] = true
				lookups = append(lookups, suffix)
			}
		}
	}
	if len(lookups) == 0 {
		return nil, nil
	}
	bySuffix, err := s.symbolIDsByColumn(ctx, repoID, "qualified_name", lookups, languages, false)
	if err != nil {
		return nil, err
	}
	out := map[string][]symbolIDLang{}
	for _, name := range memberNames {
		for _, suffix := range suffixesByName[name] {
			out[name] = mergeCandidatesPreservingOrder(out[name], bySuffix[suffix])
		}
		if len(out[name]) == 0 {
			delete(out, name)
		}
	}
	return out, nil
}

// mergeCandidatesPreservingOrder appends candidates whose ids are not already
// present, keeping first-seen order deterministic.
func mergeCandidatesPreservingOrder(base, extra []symbolIDLang) []symbolIDLang {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[int64]bool, len(base))
	for _, cand := range base {
		seen[cand.id] = true
	}
	for _, cand := range extra {
		if !seen[cand.id] {
			seen[cand.id] = true
			base = append(base, cand)
		}
	}
	return base
}

// mergeIDsPreservingOrder appends ids not already present, keeping first-seen
// order deterministic.
func mergeIDsPreservingOrder(base, extra []int64) []int64 {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[int64]bool, len(base))
	for _, id := range base {
		seen[id] = true
	}
	for _, id := range extra {
		if !seen[id] {
			seen[id] = true
			base = append(base, id)
		}
	}
	return base
}

// symbolIDsBySuffixScan performs stage 3 for every short name in one pass.
//
// It reads (qualified_name, id, language) for the repository's requested
// languages and evaluates both LIKE patterns for every short name against each
// row. One scan replaces one-scan-per-name.
//
// The scan deliberately does not join `files`, which the per-name SQL did. The
// join could only ever exclude a symbol whose `file_id` names no row, which
// `PRAGMA foreign_keys = ON` makes unreachable, and the page query that
// consumes these ids joins `files` again anyway -- so the observable result is
// the same.
//
// P22.7 added a language predicate. `idx_symbols_repo_qname` does not carry
// `language`, so the scan is no longer index-only
// (`SEARCH s USING INDEX idx_symbols_repo_qname (repo_id=?)`, not `USING
// COVERING INDEX`); it was measured neutral on the 100k fixture, because the
// predicate drops at least as many rows as the row fetch costs. Making it
// covering again would need `language` in the index -- a migration, and a
// decision for whatever phase wants one.
//
// The column is only read back when the batch asks about more than one
// language. With one language the SQL predicate already answers the question,
// so every row is tagged with the requested language and nothing extra is
// scanned.
func (s *Store) symbolIDsBySuffixScan(ctx context.Context, repoID int64, shorts, languages []string) (map[string][]symbolIDLang, error) {
	matcher := newSuffixMatcher(shorts)
	if matcher.empty() || len(languages) == 0 {
		// An empty map, never nil: the caller merges into this result in place.
		return map[string][]symbolIDLang{}, nil
	}

	single := len(languages) == 1
	langColumn := ", s.language"
	if single {
		langColumn = ""
	}
	args := make([]any, 0, len(languages)+1)
	args = append(args, repoID)
	for _, language := range languages {
		args = append(args, language)
	}
	rows, err := s.neighborQuery(ctx, `
		SELECT s.qualified_name, s.id, s.kind`+langColumn+`
		FROM symbols s
		WHERE s.repo_id = ?
		  AND s.language IN (`+strings.TrimRight(strings.Repeat("?,", len(languages)), ",")+`)
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]symbolIDLang{}
	lastID := map[string]int64{}
	var qname, kind, language string
	var id int64
	dest := []any{&qname, &id, &kind}
	if !single {
		dest = append(dest, &language)
	} else {
		language = languages[0]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		folded := asciiLower(qname)
		isType := isTypeSymbolKind(kind)
		matcher.match(folded, func(short string) {
			// P22.9: a BARE short reaching a type through `%.short` is the
			// bare-tail fabrication in its class/constructor form -- it is how
			// `A` still claimed `A.A` after the resolver had refused it. A
			// member-qualified spelling keeps its qualifier and its candidates.
			if isType && typeScopeGatedLanguage(language) && goBareCallName(short) {
				return
			}
			// Both separators can match the same row; keep one id per short.
			if seen, ok := lastID[short]; ok && seen == id {
				return
			}
			lastID[short] = id
			out[short] = append(out[short], symbolIDLang{id: id, language: language})
		})
	}
	return out, rows.Err()
}

// splitBareNameLanguages partitions lookup keys by whether their spelling
// carries a qualifier, preserving input order in both halves so statement text
// stays a function of the input. Only the bare half is subject to P22.9's
// type-target exclusion.
func splitBareNameLanguages(keys []symbolLangKey) (bare, qualified []symbolLangKey) {
	for _, key := range keys {
		if goBareCallName(key.name) {
			bare = append(bare, key)
		} else {
			qualified = append(qualified, key)
		}
	}
	return bare, qualified
}

// splitTypeScopeGatedLanguages partitions a language list into the ones P22.9
// governs and the rest, preserving order so statement text stays a function of
// the input.
func splitTypeScopeGatedLanguages(languages []string) (gated, ungated []string) {
	for _, language := range languages {
		if typeScopeGatedLanguage(language) {
			gated = append(gated, language)
		} else {
			ungated = append(ungated, language)
		}
	}
	return gated, ungated
}
