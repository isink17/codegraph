package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

// ParserInfo is doctor's one parser section. It answers two questions and
// nothing else: what this binary can parse, and whether the repository's
// persisted graph was produced by the same parsers.
//
// It is deliberately not a parser report dump -- per-language profile listings
// belong to the `supported_languages` tool.
type ParserInfo struct {
	// Degraded lists the languages this binary indexes symbols-only, with no
	// call graph. Non-empty is not an error (a fresh non-cgo index is allowed),
	// but it must never be invisible.
	Degraded []string `json:"degraded_languages,omitempty"`
	// Provenance summarises the repository's persisted parser state. One of:
	// "current", "upgrade_required", "downgrade_refused", "unknown_legacy",
	// "mixed", or "" when no repository database was inspected.
	Provenance string `json:"provenance,omitempty"`
	// Languages carries the per-language detail behind Provenance, only for the
	// languages that are not already current.
	Languages []ParserLanguageState `json:"languages,omitempty"`
}

type ParserLanguageState struct {
	Language string `json:"language"`
	// State is the per-language version of Provenance.
	State string `json:"state"`
	// Stored are the distinct persisted profiles for the language, with
	// "unknown" standing for pre-migration-036 rows.
	Stored []string `json:"stored_profiles,omitempty"`
	// Current is this binary's profile for the language.
	Current string `json:"current_profile,omitempty"`
}

const (
	parserStateCurrent          = "current"
	parserStateUpgradeRequired  = "upgrade_required"
	parserStateDowngradeRefused = "downgrade_refused"
	parserStateUnknownLegacy    = "unknown_legacy"
	parserStateMixed            = "mixed"
)

// parserSeverity orders the per-language states so the repository-level
// summary reports the worst one rather than the last one seen.
var parserSeverity = map[string]int{
	parserStateCurrent:          0,
	parserStateUpgradeRequired:  1,
	parserStateMixed:            2,
	parserStateUnknownLegacy:    3,
	parserStateDowngradeRefused: 4,
}

// inspectParser builds the parser section. `languages` is the running binary's
// registry view; `dbPath` may be empty, in which case only the degraded-parser
// half is reported.
func inspectParser(ctx context.Context, languages []parser.LanguageSupport, dbPath string) (*ParserInfo, []string) {
	if len(languages) == 0 && strings.TrimSpace(dbPath) == "" {
		return nil, nil
	}
	info := &ParserInfo{}
	current := make(map[string]parser.LanguageSupport, len(languages))
	for _, lang := range languages {
		current[lang.Language] = lang
		if lang.ParserProfile != "" && !lang.CallEdges {
			info.Degraded = append(info.Degraded, lang.Language)
		}
	}
	sort.Strings(info.Degraded)

	var recommendations []string
	if len(info.Degraded) > 0 {
		recommendations = append(recommendations, fmt.Sprintf(
			"this binary parses %s symbols-only: no call graph is produced for those languages",
			strings.Join(info.Degraded, ", "),
		))
	}
	if strings.TrimSpace(dbPath) == "" {
		return info, recommendations
	}

	groups, err := queryParserProfileGroups(ctx, dbPath)
	if err != nil {
		// A database written before migration 036 has no such column. That is
		// itself the answer, not a failure to report.
		if strings.Contains(err.Error(), "no such column") {
			info.Provenance = parserStateUnknownLegacy
			recommendations = append(recommendations, "repo DB predates parser provenance; run a full `codegraph update <repo>` to record it")
			return info, recommendations
		}
		recommendations = append(recommendations, "parser provenance inspect failed: "+err.Error())
		return info, recommendations
	}

	type repoLanguage struct {
		repoID   int64
		language string
	}
	byLanguage := map[repoLanguage][]store.FileParserProfileGroup{}
	for _, group := range groups {
		if group.Language == "" || group.Files == 0 {
			continue
		}
		key := repoLanguage{repoID: group.RepoID, language: group.Language}
		byLanguage[key] = append(byLanguage[key], group.FileParserProfileGroup)
	}
	worst := parserStateCurrent
	names := make([]repoLanguage, 0, len(byLanguage))
	for key := range byLanguage {
		names = append(names, key)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].language != names[j].language {
			return names[i].language < names[j].language
		}
		return names[i].repoID < names[j].repoID
	})
	seen := map[string]struct{}{}
	for _, key := range names {
		language := key.language
		active, known := current[language]
		if !known || active.ParserProfile == "" {
			// No adapter in this binary: nothing to converge and nothing at risk.
			continue
		}
		stored := map[string]struct{}{}
		storedCallCapable := false
		storedUnknown := false
		for _, group := range byLanguage[key] {
			if group.Profile == "" {
				stored["unknown"] = struct{}{}
				storedUnknown = true
				continue
			}
			stored[group.Profile] = struct{}{}
			if group.CallEdges {
				storedCallCapable = true
			}
		}
		if len(stored) == 1 {
			if _, only := stored[active.ParserProfile]; only {
				continue
			}
		}
		state := parserStateUpgradeRequired
		switch {
		case !active.CallEdges && (storedCallCapable || storedUnknown):
			state = parserStateDowngradeRefused
		case storedUnknown:
			state = parserStateUnknownLegacy
		case len(stored) > 1:
			state = parserStateMixed
		}
		list := make([]string, 0, len(stored))
		for profile := range stored {
			list = append(list, profile)
		}
		sort.Strings(list)
		// One entry per language: two repositories in the same state say the
		// same thing twice, and the section is a diagnostic, not an inventory.
		if _, duplicate := seen[language+"|"+state]; !duplicate {
			seen[language+"|"+state] = struct{}{}
			info.Languages = append(info.Languages, ParserLanguageState{
				Language: language,
				State:    state,
				Stored:   list,
				Current:  active.ParserProfile,
			})
		}
		if parserSeverity[state] > parserSeverity[worst] {
			worst = state
		}
	}
	info.Provenance = worst
	switch worst {
	case parserStateDowngradeRefused:
		recommendations = append(recommendations, "this binary would delete call evidence for an indexed language; scans are refused until a call-capable build runs, or the index is rebuilt from scratch")
	case parserStateUnknownLegacy, parserStateMixed, parserStateUpgradeRequired:
		recommendations = append(recommendations, "parser profile changed since this repo was indexed; run a full `codegraph update <repo>` with this binary")
	}
	return info, recommendations
}

// repoParserProfileGroup is a provenance group tagged with the repository it
// belongs to. A database can hold several repositories, and a language that is
// tree-sitter in one and heuristic in another is two independent states, not
// one mixed one -- aggregating across repositories would invent a conflict no
// single scan will ever face.
type repoParserProfileGroup struct {
	RepoID int64
	store.FileParserProfileGroup
}

// queryParserProfileGroups mirrors store.FileParserProfileGroups over a
// read-only handle, sharing its population predicate verbatim rather than
// restating it -- a doctor that disagreed with the indexer about which files
// are pinned would report a repository as converged while the indexer refuses
// to touch it. Doctor never opens the store proper, because it must be able to
// inspect a database it will not migrate.
func queryParserProfileGroups(ctx context.Context, dbPath string) ([]repoParserProfileGroup, error) {
	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, true)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
		SELECT f.repo_id, f.language, f.parser_profile, f.parser_call_edges, COUNT(*)
		FROM files f
		WHERE f.is_deleted = 0
		  AND `+store.FileParserOwnedEvidencePredicate+`
		GROUP BY f.repo_id, f.language, f.parser_profile, f.parser_call_edges
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []repoParserProfileGroup
	for rows.Next() {
		var group repoParserProfileGroup
		var callEdges int
		if err := rows.Scan(&group.RepoID, &group.Language, &group.Profile, &callEdges, &group.Files); err != nil {
			return nil, err
		}
		group.CallEdges = callEdges != 0
		out = append(out, group)
	}
	return out, rows.Err()
}
