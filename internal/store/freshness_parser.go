package store

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
)

// SetParserProfiles records the parser registry of the binary using this
// store (parser.Registry.LanguageProfiles), the same authority the indexer
// plans profile transitions with. Freshness compares stored provenance with
// it; without it that comparison reads unknown.
func (s *Store) SetParserProfiles(profiles map[string]parser.Profile) {
	clone := maps.Clone(profiles)
	s.parserProfiles.Store(&clone)
}

// Parser-profile compatibility states and reasons; see docs/freshness.md.
const (
	parserCompatCurrent  = "current"
	parserCompatObsolete = "obsolete"
)

// parserProfileCompatibility compares each language's stored profiles with
// this binary's. A language is obsolete only when every profile that differs
// is an older version of the same parser family, so the next scan by this
// binary reparses it. It is unknown when this binary cannot say which profile
// is current for that data: no adapter for the language, a refused downgrade
// (the data came from a call-capable build, as tree-sitter data read by a
// build without cgo), another parser family, a newer version, or no recorded
// provenance.
func parserProfileCompatibility(groups []FileParserProfileGroup, current map[string]parser.Profile) graph.FreshnessParserCompatibility {
	byLanguage := map[string][]FileParserProfileGroup{}
	for _, g := range groups {
		if g.Language != "" && g.Files > 0 {
			byLanguage[g.Language] = append(byLanguage[g.Language], g)
		}
	}
	out := graph.FreshnessParserCompatibility{State: parserCompatCurrent, Reason: "matches_this_binary", Languages: []graph.FreshnessParserLanguage{}}
	for _, language := range slices.Sorted(maps.Keys(byLanguage)) {
		lang := classifyParserLanguage(language, byLanguage[language], current)
		if lang.State == parserCompatCurrent {
			continue
		}
		out.Languages = append(out.Languages, lang)
		if out.State == parserCompatCurrent || (lang.State == parserCompatObsolete && out.State != parserCompatObsolete) {
			out.State, out.Reason = lang.State, lang.Reason
		}
	}
	return out
}

func classifyParserLanguage(language string, groups []FileParserProfileGroup, current map[string]parser.Profile) graph.FreshnessParserLanguage {
	out := graph.FreshnessParserLanguage{Language: language, State: parserCompatCurrent, Stored: []string{}}
	for _, g := range groups {
		stored := g.Profile
		if stored == "" {
			stored = freshnessUnknown
		}
		if !slices.Contains(out.Stored, stored) {
			out.Stored = append(out.Stored, stored)
		}
	}
	slices.Sort(out.Stored)
	profile, ok := current[language]
	if !ok {
		out.State, out.Reason = freshnessUnknown, "no_adapter"
		return out
	}
	out.Current = profile.ID
	unknownReason := ""
	for _, g := range groups {
		if g.Profile == profile.ID {
			continue
		}
		reason := ""
		switch {
		case !profile.EmitsCallEdges && (g.Profile == "" || g.CallEdges):
			// planParserProfiles refuses this as a downgrade.
			reason = "call_capable_build_required"
		case g.Profile == "":
			reason = "unrecorded_provenance"
		default:
			reason = compareProfileVersions(g.Profile, profile.ID)
		}
		if reason == "" {
			out.State, out.Reason = parserCompatObsolete, "older_version"
			return out
		}
		if unknownReason == "" {
			unknownReason = reason
		}
	}
	if unknownReason != "" {
		out.State, out.Reason = freshnessUnknown, unknownReason
	}
	return out
}

// compareProfileVersions returns "" when stored is an older version of
// current's family ("<family>:<language>:v<n>"), and why not otherwise.
func compareProfileVersions(stored, current string) string {
	storedFamily, storedVersion, ok1 := splitProfileID(stored)
	currentFamily, currentVersion, ok2 := splitProfileID(current)
	switch {
	case !ok1 || !ok2 || storedFamily != currentFamily:
		return "other_parser_family"
	case storedVersion > currentVersion:
		return "newer_version"
	}
	return ""
}

func splitProfileID(id string) (family string, version int, ok bool) {
	i := strings.LastIndexByte(id, ':')
	if i < 0 || !strings.HasPrefix(id[i+1:], "v") {
		return "", 0, false
	}
	version, err := strconv.Atoi(id[i+2:])
	if err != nil {
		return "", 0, false
	}
	return id[:i], version, true
}
