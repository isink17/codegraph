package indexer

import (
	"errors"
	"fmt"
	"sort"

	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

// Sentinel errors for the two ways a parser-profile transition can stop a scan.
// Callers (CLI, MCP, watcher) branch on these with errors.Is; nothing in this
// repository is allowed to match on the message text, which exists only to be
// actionable to a human.
var (
	// ErrParserDowngradeRefused: the persisted graph for a language was produced
	// by a call-capable parser (or by an unknown one, which might have been),
	// and this binary's adapter for that language emits no call edges. Replacing
	// it would delete call evidence that cannot be rebuilt without the other
	// binary, so the scan refuses BEFORE mutating anything.
	ErrParserDowngradeRefused = errors.New("parser downgrade refused")

	// ErrParserProfileTransitionRequired: a path-scoped scan (watcher flush,
	// explicit paths) is about to touch a language whose existing files were
	// indexed by a different -- or unknown -- parser. Honouring it would convert
	// the repository one file at a time and leave a same-language mixed graph,
	// so the scan refuses and asks for one full run to converge the language.
	ErrParserProfileTransitionRequired = errors.New("parser profile transition required")
)

// ParserProfileError carries the language and the two profile identities
// involved, so a caller can report exactly which language is blocked without
// parsing a sentence. Unwrap yields one of the sentinels above.
type ParserProfileError struct {
	Reason   error
	Language string
	// Stored is the profile persisted for the language's existing files. Empty
	// means unknown legacy provenance (pre-migration-036 rows).
	Stored string
	// StoredCallEdges reports whether the persisted parser claimed call-graph
	// support. Meaningless when Stored is empty.
	StoredCallEdges bool
	// Current is this binary's profile for the language.
	Current parser.Profile
}

func (e *ParserProfileError) Unwrap() error { return e.Reason }

func (e *ParserProfileError) Error() string {
	stored := e.Stored
	if stored == "" {
		stored = "unknown (indexed before parser provenance was recorded)"
	}
	switch {
	case errors.Is(e.Reason, ErrParserDowngradeRefused):
		return fmt.Sprintf(
			"refusing to reindex %s with %q: it emits no call edges and the existing graph was produced by %s; run this scan with a call-capable build, or delete the index and rebuild if a symbols-only graph is intended",
			e.Language, e.Current.ID, stored,
		)
	default:
		return fmt.Sprintf(
			"parser profile for %s changed (stored %s, current %q); run a full `codegraph update <repo>` with this binary before resuming incremental updates",
			e.Language, stored, e.Current.ID,
		)
	}
}

// parserProfilePlan is the decision a scan makes about parser provenance before
// it writes anything.
type parserProfilePlan struct {
	// reparseLanguages are the languages whose files must reach the parser again
	// even though their size, mtime and content hash are unchanged. A parser
	// profile change is a semantic input change; filesystem equality is not
	// sufficient to skip.
	reparseLanguages map[string]struct{}
}

func (p parserProfilePlan) languages() []string {
	out := make([]string, 0, len(p.reparseLanguages))
	for language := range p.reparseLanguages {
		out = append(out, language)
	}
	sort.Strings(out)
	return out
}

// planParserProfiles compares persisted per-file provenance against the current
// registry, language by language, and returns either the set of languages this
// scan must reconverge or a typed refusal.
//
// Language scoping is the whole point: a Java profile change must not make Go
// files reparse, and a Go-only path-scoped update must not fail because Java is
// stale. `affected` decides which languages this particular scan can mutate --
// every language with an adapter for a full run, only the candidate paths'
// languages for a path-scoped one.
//
// The five cases, in the order they are decided:
//
//	A. stored == current for every group          -> nothing; fast paths stay valid
//	C. stored proves call edges, current has none -> ErrParserDowngradeRefused
//	E. stored unknown, current has no call edges  -> ErrParserDowngradeRefused
//	   (an unknown graph may have been richer; "it has no edges right now" is
//	    not proof that it never did, so this fails closed)
//	   path-scoped, any transition                -> ErrParserProfileTransitionRequired
//	B/D. otherwise                                -> reparse the language
func planParserProfiles(
	groups []store.FileParserProfileGroup,
	current map[string]parser.Profile,
	affected func(language string) bool,
	pathScoped bool,
) (parserProfilePlan, error) {
	plan := parserProfilePlan{reparseLanguages: map[string]struct{}{}}
	byLanguage := map[string][]store.FileParserProfileGroup{}
	for _, group := range groups {
		if group.Language == "" || group.Files == 0 {
			continue
		}
		byLanguage[group.Language] = append(byLanguage[group.Language], group)
	}

	languages := make([]string, 0, len(byLanguage))
	for language := range byLanguage {
		languages = append(languages, language)
	}
	sort.Strings(languages)

	for _, language := range languages {
		profile, ok := current[language]
		if !ok {
			// No adapter for the language in this binary, so this scan cannot
			// touch its files at all -- they are neither reparsed nor at risk.
			continue
		}
		if !affected(language) {
			continue
		}
		stale := make([]store.FileParserProfileGroup, 0, len(byLanguage[language]))
		for _, group := range byLanguage[language] {
			if group.Profile != profile.ID {
				stale = append(stale, group)
			}
		}
		if len(stale) == 0 {
			continue
		}
		if !profile.EmitsCallEdges {
			// Deterministic worst case first: a proven call-capable predecessor
			// outranks an unknown one in the message, and unknown outranks a
			// proven symbols-only one (which is not a downgrade at all, only a
			// different implementation).
			var blocking *store.FileParserProfileGroup
			for i := range stale {
				group := stale[i]
				switch {
				case group.Profile != "" && group.CallEdges:
					blocking = &stale[i]
				case group.Profile == "" && (blocking == nil || !blocking.CallEdges):
					blocking = &stale[i]
				}
				if blocking != nil && blocking.Profile != "" && blocking.CallEdges {
					break
				}
			}
			if blocking != nil && (blocking.Profile == "" || blocking.CallEdges) {
				return parserProfilePlan{}, &ParserProfileError{
					Reason:          ErrParserDowngradeRefused,
					Language:        language,
					Stored:          blocking.Profile,
					StoredCallEdges: blocking.CallEdges,
					Current:         profile,
				}
			}
		}
		if pathScoped {
			return parserProfilePlan{}, &ParserProfileError{
				Reason:          ErrParserProfileTransitionRequired,
				Language:        language,
				Stored:          stale[0].Profile,
				StoredCallEdges: stale[0].CallEdges,
				Current:         profile,
			}
		}
		plan.reparseLanguages[language] = struct{}{}
	}
	return plan, nil
}
