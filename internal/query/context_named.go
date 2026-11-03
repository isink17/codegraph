package query

import "strings"

const (
	// maxNamedReferences bounds how many symbol references one task may spend
	// on the named-reference leg. Each costs one indexed lookup.
	maxNamedReferences = 12
	// namedReferenceFanout bounds the symbols one reference may contribute. A
	// reference matching more than this is ambiguous evidence and contributes
	// nothing; see Store.SymbolsForNamedReferences.
	namedReferenceFanout = 3
)

// extractNamedReferences pulls the symbol references a task spelled out of its
// text, in first-appearance order, deduplicated and capped.
//
// It is deliberately conservative, because a reference becomes a top-ranked
// seed and prose must not. Two shapes qualify:
//
//   - a dotted reference whose every segment is identifier-shaped
//     (`RealCall.execute`, `OkHttpClient.newCall`), and
//   - a bare identifier carrying an uppercase letter somewhere other than its
//     first position (`CacheInterceptor`, `newCall`) or an underscore
//     (`process_payment`).
//
// The second rule is what keeps ordinary capitalized prose out: `Trace`,
// `Explain` and `Exchange` open sentences or name concepts and are
// indistinguishable from a one-hump type name, so they are not admitted on
// their own. A task that means such a symbol still reaches it through semantic
// search and through graph expansion from the references that did qualify.
//
// Nothing here decides relevance: a reference is only a lookup key, and a key
// that resolves to no indexed symbol contributes nothing at all.
func extractNamedReferences(task string) []string {
	seen := map[string]bool{}
	var out []string
	for _, word := range splitIdentifierRuns(task) {
		word = strings.Trim(word, ".")
		if word == "" || seen[word] || !isNamedReference(word) {
			continue
		}
		seen[word] = true
		out = append(out, word)
		if len(out) == maxNamedReferences {
			break
		}
	}
	return out
}

// splitIdentifierRuns cuts the text into maximal runs of characters that can
// occur inside a dotted identifier reference.
func splitIdentifierRuns(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case r == '_' || r == '.':
			return false
		default:
			return true
		}
	})
}

func isNamedReference(word string) bool {
	if strings.Contains(word, ".") {
		for _, seg := range strings.Split(word, ".") {
			if !isIdentifier(seg) {
				return false
			}
		}
		return true
	}
	if !isIdentifier(word) {
		return false
	}
	if strings.Contains(word, "_") {
		return true
	}
	for i := 1; i < len(word); i++ {
		if word[i] >= 'A' && word[i] <= 'Z' {
			return true
		}
	}
	return false
}

func isIdentifier(seg string) bool {
	if seg == "" {
		return false
	}
	c := seg[0]
	if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	for i := 1; i < len(seg); i++ {
		c := seg[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}
