package parser

// Profile is the semantic identity of the adapter that produced a file's
// persisted graph. It answers one question the filesystem cannot: "would this
// binary's parser have written the same facts as the one that indexed this
// file?".
//
// A profile is NOT a build fingerprint. It deliberately excludes the executable
// bytes, the git SHA, the build timestamp, the Go type name, and CGO_ENABLED,
// because none of those change when and only when persisted graph semantics
// change. Two different builds of the same adapter must report the same ID, and
// one build must report a different ID once its output would differ.
//
// # Versioning rule
//
// Bump the version suffix of a profile ID whenever an UNCHANGED source file
// could produce different persisted graph semantics under the new
// implementation, so that already-indexed files have to reach the parser again:
//
//   - call extraction changes (new, removed, or differently spelled call edges)
//   - symbol identity or extraction changes (names, qualified names, kinds)
//   - import or scope evidence changes
//
// Do NOT bump for changes that cannot alter persisted output:
//
//   - internal refactors with identical output
//   - performance-only changes
//
// Bumping a profile ID is a language-scoped reparse, not a repair. Explicit
// repair migrations (Python 033/034, Go 035) remain the right tool when the
// persisted rows themselves are wrong and must be unbound; a profile bump only
// says "ask the parser again".
type Profile struct {
	// ID is deterministic, implementation-specific, and deliberately versioned.
	// Convention: "<implementation>:<language>:v<n>", e.g. "treesitter:java:v1".
	// The empty string means "unknown provenance" and is never a valid ID for a
	// production adapter.
	ID string

	// EmitsCallEdges reports whether this adapter builds a call graph. It is the
	// one capability persisted alongside the ID, because losing it is the
	// destructive downgrade this contract exists to refuse: a symbols-only
	// heuristic adapter replacing a tree-sitter graph deletes every call edge
	// for the language without saying so.
	EmitsCallEdges bool
}

// Known reports whether the profile identifies an adapter at all.
func (p Profile) Known() bool { return p.ID != "" }

// ProfileProvider is implemented by adapters whose persisted output has a
// stable semantic identity. It is deliberately optional on Adapter: the test
// suites in this repository construct many synthetic adapters that have no
// meaningful provenance, and requiring the method would rewrite all of them.
//
// Every adapter in a PRODUCTION default registry must implement it. See
// Registry.LanguagesMissingProfile and the registry contract tests.
type ProfileProvider interface {
	Profile() Profile
}
