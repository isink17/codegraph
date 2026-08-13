package store

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/classify"
)

// errMismatchedExportEvidence guards the positional pairing between an edge page
// and its classification evidence. It fails the export rather than classifying
// one file's call with another file's imports.
var errMismatchedExportEvidence = errors.New("store: export edge evidence length does not match edge page")

// Target classification of exported edges (P8).
//
// `ExportEdge.TargetClassification` says what an edge points at: `project` when
// a destination is bound, and otherwise the strongest honest description of the
// unresolved name -- `builtin`, `stdlib`, `external`, or `unknown`. The rules
// themselves live in internal/classify; this file is only the batched evidence
// loader that feeds them.
//
// # Computed, not persisted
//
// There is no `edges.target_classification` column and no migration 020. The
// classification is derived on read instead, for one reason: it is a pure
// function of data already persisted (`files.language`, `file_imports`,
// `edges.dst_name`, `symbols.name`), so persisting it would add a second copy
// that can disagree with the first.
//
// The disagreement would be real, not theoretical. A stored classification would
// need clearing or recomputing at every one of:
//
//   - each of the two unbind sites (deleteFileGraphsBatch,
//     deleteFileGraphsBatchFromTemp), where a stale `project` must not survive
//     its destination's deletion;
//   - every resolver strategy UPDATE, where a new binding must flip the value to
//     `project`;
//   - re-index of any *other* file, which can add or remove an import and change
//     a classification in a file that itself did not change;
//   - and it would be skipped entirely whenever prepareResolverTables takes its
//     no-unresolved-edges early-out.
//
// Deriving on read makes all four impossible: there is no stored value to go
// stale, `Resolved` is read from the same row as `dst_symbol_id`, and imports are
// read as they are now. The cost is two batched queries per exported page
// (§ evidence, below), which is cheaper than the invariants the column would
// have needed.
//
// # Cost
//
// Per page of edges, not per edge: one query for the imports of the distinct
// source files on the page, and one -- only when the page actually contains a
// builtin-candidate name -- for same-language project symbols claiming those
// names. Both are chunked IN-lists over existing indexes
// (`idx_file_imports_file_id`, `idx_symbols_repo_name`), and the second is scoped
// to the languages actually on the page. Import normalization runs once per file,
// not once per edge. No filesystem access, no per-edge query, and the language
// sets in internal/classify are read-only package-level maps, so no table is
// copied per call.

// exportEdgeEvidence is the per-edge classification input that is read from the
// edges query but is not part of the exported JSON contract: the source file's
// id and persisted language, plus the recorded call-site text. Kept beside the
// edge rather than on ExportEdge so the wire shape stays unchanged.
type exportEdgeEvidence struct {
	fileID      int64
	srcLanguage string
	callSite    string
}

// exportEdgeColumnsSQL is the one column list every ExportEdge query selects, in
// the exact order scanExportEdges reads. The three trailing columns are
// classification evidence rather than wire fields; `files` is LEFT JOINed, so a
// missing file row yields an empty language and the classifier falls back to
// `unknown` -- the same fail-closed behaviour as the P2 language gate.
const exportEdgeColumnsSQL = `e.id, e.src_symbol_id, COALESCE(src.qualified_name, ''), e.dst_symbol_id, ` +
	`COALESCE(dst.qualified_name, ''), e.dst_name, e.edge_kind, COALESCE(f.path, ''), e.line, ` +
	`e.resolution_strategy, e.resolution_confidence, e.file_id, COALESCE(f.language, ''), e.evidence`

// classifyExportEdges fills TargetClassification for every edge in the page.
// Both ExportEdge producers call it, so export, JSONL, DOT, and the resolver
// audit all read one classifier rather than three approximations of it.
//
// evidence is positional: evidence[i] belongs to edges[i].
func (s *Store) classifyExportEdges(ctx context.Context, repoID int64, edges []ExportEdge, evidence []exportEdgeEvidence) error {
	if len(edges) == 0 {
		return nil
	}
	if len(evidence) != len(edges) {
		// Programming error in a caller: classifying with mismatched evidence
		// would attribute one file's imports to another file's edges.
		return errMismatchedExportEvidence
	}

	fileIDs := make([]int64, 0, len(edges))
	seenFile := map[int64]struct{}{}
	builtinNames := map[string]struct{}{}
	builtinLanguages := map[string]struct{}{}
	for i := range edges {
		if edges[i].DstSymbolID != nil {
			// Resolved edges need no evidence beyond the binding itself.
			continue
		}
		if id := evidence[i].fileID; id != 0 {
			if _, ok := seenFile[id]; !ok {
				seenFile[id] = struct{}{}
				fileIDs = append(fileIDs, id)
			}
		}
		if name, ok := classify.BuiltinCandidate(evidence[i].srcLanguage, edges[i].DstName); ok {
			builtinNames[name] = struct{}{}
			builtinLanguages[evidence[i].srcLanguage] = struct{}{}
		}
	}

	rawImportsByFile, err := s.importsForFiles(ctx, repoID, fileIDs)
	if err != nil {
		return err
	}
	shadowed, err := s.languageNamesDefinedInProject(ctx, repoID, builtinNames, builtinLanguages)
	if err != nil {
		return err
	}

	// Normalize each file's imports once, not once per edge: a file's edges all
	// see the same import list, and normalization allocates (Python alias
	// stripping, Rust brace expansion). Keyed by file id alone -- normalization
	// is language-specific, but `files.language` is single-valued, so every edge
	// of a file normalizes under the same rules.
	normalizedByFile := make(map[int64][]string, len(rawImportsByFile))

	for i := range edges {
		fileID := evidence[i].fileID
		language := evidence[i].srcLanguage
		imports, cached := normalizedByFile[fileID]
		if !cached {
			imports = classify.NormalizeImports(language, rawImportsByFile[fileID])
			normalizedByFile[fileID] = imports
		}
		ev := classify.Evidence{
			Resolved: edges[i].DstSymbolID != nil,
			Language: language,
			DstName:  edges[i].DstName,
			CallSite: evidence[i].callSite,
			Imports:  imports,
		}
		if name, ok := classify.BuiltinCandidate(language, ev.DstName); ok {
			_, ev.ProjectDefinesTarget = shadowed[symbolLangKey{name: name, language: language}]
		}
		edges[i].TargetClassification = classify.Target(ev)
	}
	return nil
}

// importsForFiles loads `file_imports.import_path` for the given files in one
// chunked query, preserving each file's stored order. Files with no imports are
// simply absent from the result, and a nil map read yields a nil slice, which
// classify.Evidence accepts as "no import evidence".
func (s *Store) importsForFiles(ctx context.Context, repoID int64, fileIDs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	for _, chunk := range chunkInt64s(fileIDs, sqliteInClauseBatchSize) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT file_id, import_path
			FROM file_imports
			WHERE repo_id = ? AND file_id IN (`+placeholders+`)
			ORDER BY id ASC
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var fileID int64
			var importPath string
			if err := rows.Scan(&fileID, &importPath); err != nil {
				rows.Close()
				return nil, err
			}
			out[fileID] = append(out[fileID], importPath)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// languageNamesDefinedInProject reports which (language, name) pairs the
// repository actually defines, for the builtin candidate names on this page
// only. It is the evidence behind classify.Evidence.ProjectDefinesTarget.
//
// The language is carried through deliberately: a Swift `sorted` must not
// suppress the builtin classification of a Python call to `sorted`. That is the
// same cross-language name coincidence the P2 gate refuses to bind on, and it is
// the exact shape of the adversarial audit's case A.
//
// No symbol-kind filter is applied, and no `files.is_deleted` filter either. The
// abstention set must be a SUPERSET of the resolver's candidate set, and
// resolveSymbolCandidates filters on neither: between MarkFilesDeletedBatch and
// PurgeDeletedFileGraphsForScan the symbol rows still exist and can still be the
// P3 ambiguity that left this edge unresolved. Narrowing the query here would
// hide exactly those rows and report `builtin` for an ambiguity. Both omissions
// keep the rule conservative: it can only ever turn a `builtin` into an
// `unknown`, never the reverse.
//
// Scoping by the languages actually present on the page keeps the index range
// scan off symbols of unrelated languages.
func (s *Store) languageNamesDefinedInProject(ctx context.Context, repoID int64, names map[string]struct{}, languages map[string]struct{}) (map[symbolLangKey]struct{}, error) {
	out := map[symbolLangKey]struct{}{}
	if len(names) == 0 || len(languages) == 0 {
		return out, nil
	}
	// Sorted so the statement text and its chunk boundaries are reproducible.
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	orderedLanguages := make([]string, 0, len(languages))
	for language := range languages {
		orderedLanguages = append(orderedLanguages, language)
	}
	sort.Strings(orderedLanguages)
	languagePlaceholders := strings.TrimRight(strings.Repeat("?,", len(orderedLanguages)), ",")

	for _, chunk := range chunkStrings(ordered, sqliteInClauseBatchSize) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+len(orderedLanguages)+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		for _, language := range orderedLanguages {
			args = append(args, language)
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT DISTINCT s.language, s.name
			FROM symbols s
			WHERE s.repo_id = ? AND s.name IN (`+placeholders+`)
			  AND s.language IN (`+languagePlaceholders+`)
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key symbolLangKey
			if err := rows.Scan(&key.language, &key.name); err != nil {
				rows.Close()
				return nil, err
			}
			out[key] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}
