package store

// Resolver language compatibility (P2).
//
// Implicit resolver strategies bind `edges.dst_symbol_id` by matching names
// (exact, qualified, dot-tail, slash-suffix, receiver). Name matching alone is
// not evidence that two symbols are related: a Python call to `sorted` and a
// Swift `SortKitA.sorted` share nothing but a string.
//
// The rule, applied identically by every implicit strategy and by both the
// full/repo resolver and the path-/name-scoped incremental resolver:
//
//	A candidate may bind only when the source language and the destination
//	language are the same persisted CodeGraph language, and both are known.
//
// Consequences:
//
//   - Ecosystem interop (Java/Kotlin, C/C++, JS/TS, ...) is NOT compatibility.
//     Two languages are compatible only when their persisted language strings
//     are equal.
//   - Unknown language on either side fails closed: no implicit binding.
//   - No alias/normalization table exists or is introduced. Language strings
//     are produced by `parser.Adapter.Language()` and persisted verbatim into
//     `files.language` / `symbols.language`; the same adapter constants are
//     used by the cgo (tree-sitter) and non-cgo registries, so persisted values
//     are already canonical. (C is persisted as `cpp` and JS as `typescript`
//     because a single adapter owns those extensions -- that collapsing happens
//     at parse time, not here.)
//   - Explicit cross-language edges are out of scope: `ResolveCrossLanguageLinks`
//     creates `cross_language_ref` edges from its own evidence -- a file-level
//     import bridge, never a shared name on its own -- and is unaffected by this
//     gate. See cross_language_links.go for what it accepts as evidence.
//
// Source language is read from `files.language` of the edge's own `file_id`;
// destination language from `symbols.language`. Both are persisted canonical
// fields, so the resolver never infers language from a filename extension.

// resolverLanguageCompatible reports whether an implicit resolver strategy may
// bind a destination symbol in dstLanguage to an edge originating in
// srcLanguage. It is the Go-side twin of resolverSrcLanguageSQL-based SQL
// gating and must stay semantically identical to it.
func resolverLanguageCompatible(srcLanguage, dstLanguage string) bool {
	if srcLanguage == "" || dstLanguage == "" {
		return false
	}
	return srcLanguage == dstLanguage
}

// resolverLanguageGateSQL is the one SQL predicate that enforces the rule above
// for every implicit strategy. It is spliced into each resolver UPDATE and
// requires exactly three things of the surrounding statement:
//
//   - the update target is `edges`
//   - `files` is joined in as `f`
//   - the candidate relation is joined in as `r` and exposes `dst_language`
//
// Candidate relations are always built with `language != ”`, and the join to
// `files` drops edges whose file row is missing, so an unknown language on
// either side yields no match rather than a guess.
const resolverLanguageGateSQL = `f.id = edges.file_id AND r.dst_language = f.language AND f.language <> 'rust'`

// symbolLangKey keys candidate lookups by (name, language) so that Go-side
// resolution can only ever pick a same-language destination.
type symbolLangKey struct {
	name     string
	language string
}
