package texttoken

import "sort"

// Semantic tokenization is the retrieval contract: the rules `symbol_tokens`
// is written under and that `Store.SemanticSearch` tokenizes a query with. It
// is deliberately a separate surface from `Weights`, which stays byte-for-byte
// what it always was.
//
// `Weights` has two other callers -- every parser adapter, which feeds
// `file_tokens`, and the parser-profile contract in store/parser_profile.go,
// which excludes `file_tokens` from parser provenance precisely because every
// adapter computes it identically from the raw bytes. Widening `Weights` would
// change both, for a table no query reads. So the decomposition lives here and
// reaches exactly two call sites.
//
// What it adds to the whole-token rules is identifier decomposition. The whole
// token is still emitted, so an exact reference keeps scoring on it; the
// components are emitted beside it, so a query that names one part of a
// compound identifier can reach the symbol at all. Two examples from the
// pinned corpora, under the old rules one token each:
//
//	okhttp3.internal.connection.RealConnectionPool.callAcquirePooledConnection
//	crate::output::help_template::HelpTemplate::write_templated_help
//
// Decomposition is two passes. First the whole token is cut at `_`, `-`, `.`
// and `/` -- which `Weights` treats as token characters, so a dotted qualified
// name or a slashed path is one token there and nothing in a query can match a
// suffix of it. Then each separator-free run is cut at case and digit
// boundaries:
//
//	lower -> upper              getURL      -> get, URL
//	upper -> upper+lower        HTTPServer  -> HTTP, Server
//	digit -> upper              HTTP2Writer -> http2, Writer
//
// Digits attach to the letters before them, so `HTTP2Writer` keeps `http2`
// rather than scattering a bare `2` into the index, and `utf8Decode` keeps
// `utf8`. A run with no boundary at all yields itself and nothing more, which
// is what keeps ordinary prose from being cut into substrings: `connection` is
// one component, never `conn` + `ection`.
//
// Components under two characters are dropped, the same floor whole tokens
// have always had. Non-ASCII bytes are delimiters here exactly as they are in
// `Weights`; this layer adds no Unicode case handling and does not change
// where a multi-byte rune ends a token.
//
// Only a symbol's identity is decomposed -- its name and qualified name --
// and not its signature or doc. That boundary is measured, not aesthetic. A
// signature carries a whole method body in the Kotlin, Java and Python
// adapters, so decomposing it hands the longest test methods in a repository
// the largest share of the new tokens, and it is the small implementation
// symbol next to them that the query was looking for. Decomposing all four
// fields was run against all three datasets beside this; it is worse on
// ContextForTask everywhere (v1 recall@5 0.469 against 0.499, rust-v1
// recall@10 0.300 against 0.450) and it is what turns one clap query's
// context recall from 0.50 into 0.00 instead of leaving it at 0.50.
//
// Weighting is otherwise ordinary: a component counts 1 per occurrence like
// any other term, so term frequency, document frequency and document length
// stay one consistent accounting and BM25 simply sees a document with more
// terms in it. The alternative -- counting components as frequency but not
// as length, on the view that a component is a synonym rather than content
// -- was implemented and measured too, and was worse on seven of eight
// aggregate figures (challenge-v1 search recall@10 0.635 against 0.535,
// rust-v1 0.400 against 0.250): it hands a long compound name the whole
// frequency gain with none of the length that pays for it, which is the
// domination failure in the other direction.
//
// Every figure above comes from tools/retrievalbench; the reports are in
// codegraph-evidence/v2-cg144-identifier-tokens-20261104/raw.
//
// Repetition inside the identity -- `connection` appears in the package
// path, the type name and the method name of the symbol above -- is real
// repetition, and k1 saturation is what answers it.

// SemanticTokenVersion identifies these rules. It is persisted per repository
// in `repos.semantic_token_version`; a stored version that is not this one
// means the rows in `symbol_tokens` were written under different rules and
// cannot be scored against a query tokenized with these.
//
// 0 is "unknown": every index written before this column existed.
// 1 is the whole-token-only rules those indexes actually hold, and is never
// written -- it exists so the refusal can name what is there.
// 2 is identifier decomposition.
const SemanticTokenVersion = 2

// SemanticSymbolWeightsInto accumulates one symbol's retrieval tokens.
//
// It is the single definition of what `symbol_tokens` holds, so the indexing
// path and the refresh path cannot drift apart about which fields are
// decomposed.
func SemanticSymbolWeightsInto(dst map[string]float64, name, qualifiedName, signature, docSummary string) {
	SemanticWeightsStringsInto(dst, name, qualifiedName)
	WeightsStringsInto(dst, signature, docSummary)
}

// SemanticWeightsString tokenizes a query under the retrieval contract. A
// query needs the token set only, so the masses are not returned: nothing
// about the query contributes to a document's length or frequency.
func SemanticWeightsString(text string) []string {
	acc := map[string]float64{}
	SemanticWeightsStringsInto(acc, text)
	out := make([]string, 0, len(acc))
	for token := range acc {
		out = append(out, token)
	}
	sort.Strings(out)
	return out
}

// SemanticWeightsStringsInto accumulates the retrieval tokens of every part
// into dst.
//
// The token is accumulated in its original case and lowercased only as each
// string is emitted: a camel boundary is the case change itself, so folding
// the bytes on the way in would destroy the only evidence of where the
// components are.
func SemanticWeightsStringsInto(dst map[string]float64, parts ...string) {
	var buf [128]byte
	token := buf[:0]
	flush := func() {
		if len(token) >= 2 {
			dst[lowerString(token)] += 1
			emitComponents(dst, token)
		}
		token = token[:0]
	}
	for _, part := range parts {
		for i := 0; i < len(part); i++ {
			b := part[i]
			if isTokenByte(b) {
				token = append(token, b)
				continue
			}
			if len(token) > 0 {
				flush()
			}
		}
		if len(token) > 0 {
			flush()
		}
	}
}

// emitComponents decomposes one whole token and adds every component.
//
// A component equal to the whole token is not re-emitted: `connection` would
// otherwise count twice for a token that has no boundary to cut at, which
// would make an undecomposable word outweigh a decomposable one for no reason
// a reader of the corpus could point at.
func emitComponents(dst map[string]float64, token []byte) {
	whole := len(token)
	for start := 0; start < len(token); {
		end := start
		for end < len(token) && !isSeparatorByte(token[end]) {
			end++
		}
		if run := token[start:end]; len(run) >= 2 {
			// The separator-free run itself: `RealConnectionPool` out of
			// `okhttp3.internal.connection.RealConnectionPool.put`, which is
			// what an explicit `RealConnectionPool.put` query matches on.
			if len(run) != whole {
				dst[lowerString(run)] += 1
			}
			emitCaseComponents(dst, run)
		}
		start = end
		for start < len(token) && isSeparatorByte(token[start]) {
			start++
		}
	}
}

// emitCaseComponents cuts one separator-free run at its case and digit
// boundaries.
func emitCaseComponents(dst map[string]float64, run []byte) {
	start := 0
	for i := 1; i <= len(run); i++ {
		if i < len(run) && !isComponentBoundary(run, i) {
			continue
		}
		// A segment spanning the whole run is not emitted: the caller has
		// already counted the run, or -- when the run is the whole token --
		// flush has.
		if seg := run[start:i]; len(seg) >= 2 && len(seg) != len(run) {
			dst[lowerString(seg)] += 1
		}
		start = i
	}
}

// isComponentBoundary reports whether a component starts at run[i].
//
//	lower|digit -> upper     getURL, http2Writer
//	upper -> upper + lower   HTTPServer, at the S of Server
//
// A digit never starts a component, so `HTTP2Writer` yields `http2` and
// `Writer` rather than a bare `2`, and `utf8Decode` keeps `utf8` whole.
func isComponentBoundary(run []byte, i int) bool {
	prev, cur := run[i-1], run[i]
	if !isUpper(cur) {
		return false
	}
	if isLower(prev) || isDigit(prev) {
		return true
	}
	return isUpper(prev) && i+1 < len(run) && isLower(run[i+1])
}

func isSeparatorByte(b byte) bool {
	return b == '_' || b == '-' || b == '.' || b == '/'
}

func isUpper(b byte) bool { return b >= 'A' && b <= 'Z' }
func isLower(b byte) bool { return b >= 'a' && b <= 'z' }
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func lowerString(b []byte) string {
	needs := false
	for _, c := range b {
		if isUpper(c) {
			needs = true
			break
		}
	}
	if !needs {
		return string(b)
	}
	out := make([]byte, len(b))
	for i, c := range b {
		if isUpper(c) {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
