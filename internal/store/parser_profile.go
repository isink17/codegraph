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

// FileParserOwnedEvidencePredicate is the SQL fragment that decides whether a
// file still holds parser-owned semantic evidence, correlated against a `files`
// row aliased `f`.
//
// It is a package-level constant because doctor inspects the same population
// over a read-only handle it will not migrate (internal/doctor.
// queryParserProfileGroups). Two hand-copied predicates would drift, and a
// doctor that disagrees with the indexer about what is pinned reports a
// repository as converged while the indexer refuses to touch it.
//
// The population is "files that currently hold parser-owned graph evidence",
// and NOT `parse_state`. That distinction is the contract, and it is not
// cosmetic: `TouchFilesMetadataBatch` writes `parse_state = ParseStateSkipped`
// for a file whose content hash is unchanged but whose mtime moved -- the
// ordinary result of a checkout, rebase or rebuild -- while leaving every row
// it declared in place. Filtering on `parse_state = 'indexed'` therefore made a
// whole repository look provenance-free after one `git checkout`, and a
// downgrade the indexer must refuse would have run unopposed.
//
// Every table listed here is PRIMARY parser evidence: a row exists in it
// because `insertParsedFileGraph` wrote it straight from a `graph.ParsedFile`,
// so reparsing the file under a different adapter can change or destroy it.
// The provenance question is "would reparsing this file potentially change
// persisted primary syntax facts", not "is every column of the row
// parser-owned" -- `edges` counts even though the resolver later overwrites
// `dst_symbol_id`, `resolution_strategy` and `resolution_confidence` on the
// very same row, because the row's existence, source attribution, kind, line
// and destination name are the parser's.
//
// Four tables are deliberately excluded, and NOT because a parser never
// touches them -- three of the four are written by `insertParsedFileGraph` as
// well. They are excluded because a reparse under a different profile cannot
// change them:
//
//   - `symbol_tokens` and `symbol_fts` are tokenizations OF `symbols`. They
//     cannot be a file's only evidence, because the `symbols` term above
//     already fires for every file that has one.
//   - `file_tokens` is `texttoken.Weights(content)` -- a tokenization of the
//     raw file text for search ranking, computed identically by every adapter
//     (parser/treesitter/common.go, parser/python, parser/golang,
//     parser/heuristic). It is a function of the bytes on disk and of nothing
//     the adapter decided, so it holds no fact a profile transition could
//     alter. Adding it would also make the population "every non-empty source
//     file", which permanently pins comments-only files -- the exact
//     overcorrection the paragraph below exists to prevent.
//   - `symbol_embeddings` is written by the embedding pass, not by a parser.
//
// `edges` is filtered rather than taken whole. Every parser-written edge counts,
// but `ResolveCrossLanguageLinks` also inserts `EdgeKindCrossLanguageRef` rows
// carrying the source symbol's `file_id` (cross_language_links.go), and those
// are resolver output. Today such a row implies the file has symbols anyway, so
// the filter changes no answer; it is here so that stops being an invariant the
// predicate silently depends on.
//
// Restricting the population to `symbols` -- which is what this predicate used
// to be -- was too narrow, and silently so. A TypeScript file whose whole
// content is `export * from "./api"` parses to zero symbols, zero imports and
// one re-export row in `scope_import_evidence`; an `import "./bootstrap"` or a
// Python `from pkg import plugin` parses to zero symbols, one `file_imports`
// row and one `scope_import_evidence` row. All of that is evidence a different
// parser would write differently, yet none of it made the file visible to
// provenance, so a profile transition could reparse and rewrite it with no
// guard and no refusal.
//
// What the predicate still excludes is exactly right. A file over the size cap
// never reached a parser and, since P22.35, owns nothing: its old graph is
// retired rather than left standing, so it can never pin its language to a
// stale profile. The same now holds for a file whose parse failed under
// `best_effort` -- RetireFileGraphsBatch drops the previous parser's rows with
// the rest, because a successful scan must not keep serving them as a
// description of bytes no parser accepted. Neither can a genuinely empty or
// comments-only source file, which owns no row in any table below.
//
// A file behind a `languages` allowlist is the one case that still keeps its
// rows, and deliberately so: a `--languages go` run makes no claim about Java,
// so the Java evidence a previous run wrote is still the best answer anyone
// has, and it keeps pinning its profile.
const FileParserOwnedEvidencePredicate = `(
		   EXISTS (SELECT 1 FROM symbols t WHERE t.file_id = f.id)
		OR EXISTS (SELECT 1 FROM references_tbl t WHERE t.file_id = f.id)
		OR EXISTS (SELECT 1 FROM edges t WHERE t.file_id = f.id AND t.edge_kind <> '` + EdgeKindCrossLanguageRef + `')
		OR EXISTS (SELECT 1 FROM file_imports t WHERE t.file_id = f.id)
		OR EXISTS (SELECT 1 FROM file_scope_evidence t WHERE t.repo_id = f.repo_id AND t.file_id = f.id)
		OR EXISTS (SELECT 1 FROM scope_import_evidence t WHERE t.repo_id = f.repo_id AND t.file_id = f.id)
		OR EXISTS (SELECT 1 FROM scope_module_candidate_evidence t WHERE t.repo_id = f.repo_id AND t.source_file_id = f.id)
		OR EXISTS (SELECT 1 FROM rust_module_evidence t WHERE t.repo_id = f.repo_id AND t.file_id = f.id)
		OR EXISTS (SELECT 1 FROM go_local_binding_evidence t WHERE t.repo_id = f.repo_id AND t.file_id = f.id)
		OR EXISTS (SELECT 1 FROM test_links t WHERE t.test_file_id = f.id)
	)`

// FileParserProfileGroups returns the repository's persisted parser provenance,
// grouped so a whole scan costs ONE query rather than one per file.
//
// The population is FileParserOwnedEvidencePredicate; see it for what counts as
// parser-owned evidence and why.
func (s *Store) FileParserProfileGroups(ctx context.Context, repoID int64) ([]FileParserProfileGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.language, f.parser_profile, f.parser_call_edges, COUNT(*)
		FROM files f
		WHERE f.repo_id = ? AND f.is_deleted = 0
		  AND `+FileParserOwnedEvidencePredicate+`
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
