package store

import (
	"context"
	"sort"
)

// FileParserProfileGroup is one (language, parser profile) population inside a
// repository, with the call-graph capability the parser that wrote those rows
// claimed at the time.
//
// Profile == "" means UNKNOWN provenance: rows written before migration 036.
// It is deliberately distinguishable from a known symbols-only parser -- the
// indexer refuses different things for the two.
type FileParserProfileGroup struct {
	Language  string `json:"language"`
	Profile   string `json:"parser_profile"`
	CallEdges bool   `json:"call_edges"`
	Files     int    `json:"files"`
}

// FileParserProfileGroups returns the repository's persisted parser provenance,
// grouped so a whole scan costs ONE query rather than one per file.
//
// The population is "files that currently hold graph evidence", proven by an
// indexed EXISTS probe against `symbols`, and NOT by `parse_state`. That
// distinction is the contract, and it is not cosmetic: `TouchFilesMetadataBatch`
// writes `parse_state = 'skipped'` for a file whose content hash is unchanged
// but whose mtime moved -- the ordinary result of a checkout, rebase or rebuild
// -- while leaving every symbol and edge it declared in place. Filtering on
// `parse_state = 'indexed'` therefore made a whole repository look
// provenance-free after one `git checkout`, and a downgrade the indexer must
// refuse would have run unopposed.
//
// What the probe excludes is exactly right: a file over the size cap or behind
// a languages allowlist never reached a parser and declares nothing, so it can
// never pin its language to a stale profile. A file that failed to parse but
// still holds the previous parser's symbols DOES count -- that evidence is real
// and was produced by that parser, so a half-converged language keeps reporting
// itself as mixed until the failure is fixed.
//
// A file whose only persisted rows are imports is not counted. It declares no
// symbol, so it holds no call evidence a downgrade could destroy.
func (s *Store) FileParserProfileGroups(ctx context.Context, repoID int64) ([]FileParserProfileGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.language, f.parser_profile, f.parser_call_edges, COUNT(*)
		FROM files f
		WHERE f.repo_id = ? AND f.is_deleted = 0
		  AND EXISTS (SELECT 1 FROM symbols s WHERE s.file_id = f.id)
		GROUP BY f.language, f.parser_profile, f.parser_call_edges
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileParserProfileGroup
	for rows.Next() {
		var group FileParserProfileGroup
		var callEdges int
		if err := rows.Scan(&group.Language, &group.Profile, &callEdges, &group.Files); err != nil {
			return nil, err
		}
		group.CallEdges = callEdges != 0
		out = append(out, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Language != out[j].Language {
			return out[i].Language < out[j].Language
		}
		return out[i].Profile < out[j].Profile
	})
	return out, nil
}
