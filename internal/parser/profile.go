package parser

import "strings"

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
//   - import, re-export or scope evidence changes
//   - any other parser-owned primary semantic evidence
//
// That last clause is not filler. The set of persisted facts a profile covers
// is exactly store.FileParserOwnedEvidencePredicate, which is wider than
// "symbols and calls": a file whose whole content is `export * from "./api"`
// declares no symbol and no import, only a re-export row, and it is still a
// persisted fact a different adapter would write differently.
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

	// SemanticEpoch orders parser-owned graph semantics across implementation
	// families. Unlike ID, it is language-wide: tree-sitter and fallback
	// adapters that implement the same safety generation share an epoch.
	SemanticEpoch int
}

// SemanticEpochForProfile is the directional compatibility contract for one
// parser family. Bump its language entry whenever that family changes
// persisted semantics. Different families share a value only while they
// implement the same safety generation.
func SemanticEpochForProfile(id string) int {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) < 2 {
		return 0
	}
	return parserSemanticEpochs[parts[0]+":"+parts[1]]
}

// SemanticEpochs returns a copy of the language-level compatibility contract.
func SemanticEpochs() map[string]int {
	out := make(map[string]int)
	for familyLanguage, epoch := range parserSemanticEpochs {
		language := familyLanguage[strings.IndexByte(familyLanguage, ':')+1:]
		if epoch > out[language] {
			out[language] = epoch
		}
	}
	return out
}

var parserSemanticEpochs = map[string]int{
	"treesitter:cpp": 1, "treesitter:csharp": 1, "treesitter:go": 1,
	"treesitter:hcl":  1,
	"treesitter:java": 1, "treesitter:kotlin": 1, "treesitter:php": 1,
	"treesitter:dart":   1,
	"treesitter:lua":    1,
	"treesitter:python": 2, "treesitter:ruby": 1, "treesitter:rust": 1,
	"treesitter:scala": 1,
	"treesitter:swift": 1, "treesitter:typescript": 1,
	"python-regex:python": 2,
	"heuristic:cpp":       1, "heuristic:csharp": 1, "heuristic:go": 1,
	"heuristic:hcl":  1,
	"heuristic:dart": 1,
	"heuristic:lua":  1,
	"heuristic:java": 1, "heuristic:kotlin": 1, "heuristic:php": 1,
	"heuristic:python": 1, "heuristic:ruby": 1, "heuristic:rust": 1,
	"heuristic:scala": 1,
	"heuristic:swift": 1, "heuristic:typescript": 1,
	"go-ast:go": 1,
}

// NewProfile constructs a production profile with the current family/language
// semantic epoch. Profile ID changes still control which files are reparsed.
func NewProfile(language, id string, emitsCallEdges bool) Profile {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) < 2 || parts[1] != language {
		return Profile{ID: id, EmitsCallEdges: emitsCallEdges}
	}
	return Profile{ID: id, EmitsCallEdges: emitsCallEdges, SemanticEpoch: SemanticEpochForProfile(id)}
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
