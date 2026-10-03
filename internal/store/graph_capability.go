package store

import (
	"context"
	"sort"
	"strings"
)

// Persisted graph capability: what the STORED graph can answer about call
// relationships, derived from the parser profiles recorded per file. It is a
// property of the database, not of the running binary -- a non-cgo binary can
// read a graph a cgo binary wrote, and the other way round.
const (
	// GraphCallCapable: every file of the language was written by one known
	// parser profile that emits call edges.
	GraphCallCapable = "call_capable"
	// GraphSymbolsOnly: every file was written by one known profile that emits
	// no call edges. Relationship queries over it return nothing, and that
	// nothing is not evidence of absence.
	GraphSymbolsOnly = "symbols_only"
	// GraphUnknownProvenance: at least one file predates recorded parser
	// provenance, so whether its call edges are complete is unknowable.
	GraphUnknownProvenance = "unknown_provenance"
	// GraphMixed: the language's files were written by more than one profile or
	// with more than one capability. At graph level: languages disagree.
	GraphMixed = "mixed"
)

// LanguageCapability is one language's persisted capability.
type LanguageCapability struct {
	Language   string   `json:"language"`
	Capability string   `json:"graph_capability"`
	Profiles   []string `json:"parser_profiles"`
}

// GraphCapability is the repository's persisted capability. State is empty
// when the graph holds no parser-owned evidence at all.
type GraphCapability struct {
	State     string               `json:"state"`
	Languages []LanguageCapability `json:"languages"`
}

// GraphLimitation is one disclosed reason why a relationship answer may be
// incomplete. It is reported only when it applies; its absence means the
// persisted graph is call-capable for every language it holds.
type GraphLimitation struct {
	Language   string `json:"language"`
	Capability string `json:"graph_capability"`
	Effect     string `json:"effect"`
}

// approximateCallProfilePrefixes are call-capable profiles whose call graph is
// known to be incomplete: the regex Python fallback misses call sites on
// bracket-continuation lines and inside f-string interpolations. They emit
// call edges, so they classify as call_capable, but an answer built on them
// must not read as complete.
var approximateCallProfilePrefixes = []string{"python-regex:"}

func approximateCallProfile(profile string) bool {
	for _, prefix := range approximateCallProfilePrefixes {
		if strings.HasPrefix(profile, prefix) {
			return true
		}
	}
	return false
}

// ClassifyGraphCapability folds persisted provenance groups into the
// per-language and graph-level capability. Groups with no language or no files
// are ignored. Pure: it reads nothing but its argument.
func ClassifyGraphCapability(groups []FileParserProfileGroup) GraphCapability {
	type acc struct {
		profiles  map[string]struct{}
		unknown   bool
		callEdges map[bool]struct{}
	}
	byLanguage := map[string]*acc{}
	for _, group := range groups {
		if group.Language == "" || group.Files == 0 {
			continue
		}
		a := byLanguage[group.Language]
		if a == nil {
			a = &acc{profiles: map[string]struct{}{}, callEdges: map[bool]struct{}{}}
			byLanguage[group.Language] = a
		}
		if group.Profile == "" {
			a.unknown = true
			a.profiles["unknown"] = struct{}{}
			continue
		}
		a.profiles[group.Profile] = struct{}{}
		a.callEdges[group.CallEdges] = struct{}{}
	}
	var out GraphCapability
	for language, a := range byLanguage {
		var capability string
		switch {
		case a.unknown:
			capability = GraphUnknownProvenance
		case len(a.profiles) > 1 || len(a.callEdges) > 1:
			capability = GraphMixed
		case hasKey(a.callEdges, true):
			capability = GraphCallCapable
		default:
			capability = GraphSymbolsOnly
		}
		profiles := make([]string, 0, len(a.profiles))
		for profile := range a.profiles {
			profiles = append(profiles, profile)
		}
		sort.Strings(profiles)
		out.Languages = append(out.Languages, LanguageCapability{Language: language, Capability: capability, Profiles: profiles})
	}
	sort.Slice(out.Languages, func(i, j int) bool { return out.Languages[i].Language < out.Languages[j].Language })
	for i, language := range out.Languages {
		if i == 0 {
			out.State = language.Capability
		} else if out.State != language.Capability {
			out.State = GraphMixed
		}
	}
	return out
}

func hasKey(m map[bool]struct{}, key bool) bool {
	_, ok := m[key]
	return ok
}

// Limitations lists, per language, why a relationship answer over this graph
// may be incomplete. Nil when every language is call-capable through a
// complete parser -- callers rely on that to leave rich responses unchanged.
func (g GraphCapability) Limitations() []GraphLimitation {
	var out []GraphLimitation
	for _, language := range g.Languages {
		effect := ""
		switch language.Capability {
		case GraphSymbolsOnly:
			effect = "call edges absent: indexed symbols-only; an empty or short result is not evidence of no relationship"
		case GraphUnknownProvenance:
			effect = "parser provenance unknown: call edges may be incomplete; run a full update with a call-capable build to record it"
		case GraphMixed:
			effect = "files indexed by different parser profiles: call edges may be incomplete or inconsistent; run a full update to converge"
		case GraphCallCapable:
			if len(language.Profiles) == 1 && approximateCallProfile(language.Profiles[0]) {
				effect = "call edges from an approximate fallback parser: some call sites are not recorded; an empty or short result is not evidence of no relationship"
			}
		}
		if effect == "" {
			continue
		}
		out = append(out, GraphLimitation{Language: language.Language, Capability: language.Capability, Effect: effect})
	}
	return out
}

// GraphCapability reports the repository's persisted capability over the same
// file population the indexer's downgrade guard and doctor inspect
// (FileParserOwnedEvidencePredicate), so the three never disagree about which
// files count.
func (s *Store) GraphCapability(ctx context.Context, repoID int64) (GraphCapability, error) {
	groups, err := s.FileParserProfileGroups(ctx, repoID)
	if err != nil {
		return GraphCapability{}, err
	}
	return ClassifyGraphCapability(groups), nil
}

// GraphLimitations returns the limitations a relationship answer over this
// repository must disclose, or nil when there are none.
//
// It first classifies every live file without the evidence predicate. That
// population is a superset of the predicate's, per language, so if the
// superset needs no disclosure neither does the exact one: a subset of "one
// complete call-capable profile" is that profile or nothing. A rich graph
// therefore answers from one cheap grouped scan of `files`; only a graph that
// may need disclosure pays for the exact population.
func (s *Store) GraphLimitations(ctx context.Context, repoID int64) ([]GraphLimitation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT language, parser_profile, parser_call_edges, COUNT(*)
		FROM files
		WHERE repo_id = ? AND is_deleted = 0
		GROUP BY language, parser_profile, parser_call_edges
	`, repoID)
	if err != nil {
		return nil, err
	}
	var all []FileParserProfileGroup
	for rows.Next() {
		var group FileParserProfileGroup
		var callEdges int
		if err := rows.Scan(&group.Language, &group.Profile, &callEdges, &group.Files); err != nil {
			rows.Close()
			return nil, err
		}
		group.CallEdges = callEdges != 0
		all = append(all, group)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ClassifyGraphCapability(all).Limitations()) == 0 {
		return nil, nil
	}
	exact, err := s.GraphCapability(ctx, repoID)
	if err != nil {
		return nil, err
	}
	return exact.Limitations(), nil
}
