package store

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Cross-language linking.
//
// `cross_language_ref` is the one edge kind this package creates already bound,
// from its own evidence, rather than by binding an edge a parser emitted. That
// makes it the one place allowed to cross a language boundary (see
// resolver_language.go), so what counts as evidence here has to be stated
// exactly -- an exemption from the language gate is not a licence to guess.
//
// The only cross-language fact this repository extracts is an import: a
// `file_imports` row whose specifier names a file written in another language.
// Nothing else -- no FFI, binding, or codegen marker -- reaches the database.
// So one rule covers every link:
//
//	A cross-language link requires an import bridge: file A holds an import
//	specifier that resolves to exactly one active file B, A and B are written in
//	different languages, and no same-language file answers that specifier.
//
// Within a bridge the *target symbol* still has to be identified, and the
// ambiguity rule from the implicit resolver applies unchanged: one candidate
// binds, two candidates abstain. A source symbol in A binds to
//
//   - the symbol in B that uniquely shares its name (`cross_language_shared_name`),
//     the import bridge being what makes the shared name evidence rather than
//     coincidence; or
//   - B's single eligible symbol when no name matches (`cross_language_import_path`),
//     where the bridge alone already leaves no other candidate.
//
// Anything else -- two same-named candidates, no name match in a B that holds
// several symbols, an import that resolves to two foreign files -- abstains and
// writes nothing. Deliberately *not* evidence:
//
//   - a bare name shared across languages with no bridge. Until this pass was
//     fixed that alone created an edge, so a Python `RenderReport` was wired to
//     an unrelated TypeScript `RenderReport` in another directory.
//   - a specifier that only looks path-shaped after its dots are read as an
//     extension. `import app.models` is a module path, not `app.ts`.
//
// The pass is a rebuild, not an append: it computes the link set the current
// evidence supports and makes the stored set equal it inside one transaction.
// That is what makes it idempotent and what retires links whose evidence is
// gone, and it is why no query here carries a LIMIT -- the old per-file-pair
// `LIMIT 50` was a cap on a symbol cross product that no longer exists, and a
// cap on a write path silently drops real links.
//
// Lifecycle, unchanged by this fix and worth knowing: nothing in the indexer
// calls this pass. Only the `cross_language_links` MCP tool does, and
// re-indexing a bridged file drops the links it owns (the source side always
// did, through the ordinary per-file edge delete; the destination side now does
// too, because an unbound cross-language row is worse than none). So these
// links live until the next index of either endpoint and are recreated by the
// next invocation. Putting the pass into the index lifecycle is a product
// decision, not a correctness one.
const (
	// crossLangEligibleKinds are the symbol kinds a cross-language link may
	// connect, unchanged from the original pass.
	crossLangEligibleKinds = `('function', 'method', 'class', 'type', 'struct', 'interface')`

	// crossLangEdgeValuesBatchRows controls multi-row inserts into edges where
	// each cross-language row uses 10 parameters. 98*10=980 variables, staying
	// under sqliteDefaultMaxVariables.
	crossLangEdgeValuesBatchRows = 98
)

// xlangFile is an active file, identified by its slash-form path rather than by
// its row id: two indexes of the same tree agree on the path and not on the id.
type xlangFile struct {
	id       int64
	path     string
	language string
}

// xlangSymbol is a link-eligible symbol. Order and identity come from the
// semantic columns; `id` is carried only to write the edge.
type xlangSymbol struct {
	id        int64
	fileID    int64
	name      string
	qualified string
	kind      string
	startLine int
	startCol  int
	stableKey string
}

// xlangBridge is one direction of one import bridge: symbols in `src` may link
// to symbols in `dst`.
type xlangBridge struct {
	src xlangFile
	dst xlangFile
}

// xlangLink is a link the evidence supports, with the provenance it must carry.
type xlangLink struct {
	srcSymbolID int64
	dstSymbolID int64
	srcFileID   int64
	dstName     string
	evidence    string
	strategy    string
	confidence  string
	// sortKey orders the link set by semantic identity only.
	sortKey string
}

// ResolveCrossLanguageLinks makes the repository's `cross_language_ref` edges
// equal the set the current import evidence supports. It returns the number of
// links that did not already exist, so a rerun against an unchanged graph
// reports zero.
func (s *Store) ResolveCrossLanguageLinks(ctx context.Context, repoID int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	// Candidates are computed inside the same transaction that writes them, so
	// the link set cannot describe a graph that stopped existing before the
	// write: a concurrent indexing commit makes this pass fail and roll back
	// rather than bind a symbol that is already gone.
	links, err := crossLanguageLinkSet(ctx, tx, repoID)
	if err == nil {
		var created int
		created, err = applyCrossLanguageLinksTx(ctx, tx, repoID, links)
		if err == nil {
			if err = tx.Commit(); err == nil {
				return created, nil
			}
			return 0, err
		}
	}
	_ = tx.Rollback()
	return 0, err
}

// xlangQueryer is the read surface the candidate phase needs, satisfied by both
// *sql.DB and *sql.Tx.
type xlangQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// crossLanguageLinkSet computes the deterministic, complete link set. It only
// reads; the caller's transaction does the writing.
func crossLanguageLinkSet(ctx context.Context, q xlangQueryer, repoID int64) ([]xlangLink, error) {
	files, byFullPath, byBase, err := crossLanguageFiles(ctx, q, repoID)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	bridges, err := crossLanguageBridges(ctx, q, repoID, byFullPath, byBase)
	if err != nil {
		return nil, err
	}
	if len(bridges) == 0 {
		return nil, nil
	}

	involved := map[int64]bool{}
	for _, b := range bridges {
		involved[b.src.id] = true
		involved[b.dst.id] = true
	}
	symbolsByFile, err := crossLanguageSymbols(ctx, q, repoID, involved)
	if err != nil {
		return nil, err
	}

	// One target per source symbol per bridge, so a pair cannot be proposed
	// twice by the same bridge; two imports naming the same file can still
	// propose it, which this set collapses. Fan-in across *different* bridges is
	// still ambiguity, and the filter after the loop drops it.
	seen := make(map[[2]int64]bool)
	// One name index per imported file, not per bridge: several files importing
	// the same foreign file is the common shape, and rebuilding the index for
	// each of them is the only part of this pass that would grow faster than the
	// symbols it reads.
	byNameCache := map[int64]map[string][]xlangSymbol{}
	var links []xlangLink
	for _, bridge := range bridges {
		dstSymbols := symbolsByFile[bridge.dst.id]
		if len(dstSymbols) == 0 {
			continue
		}
		byName, ok := byNameCache[bridge.dst.id]
		if !ok {
			byName = make(map[string][]xlangSymbol, len(dstSymbols))
			for _, sym := range dstSymbols {
				byName[sym.name] = append(byName[sym.name], sym)
			}
			byNameCache[bridge.dst.id] = byName
		}
		srcSymbols := symbolsByFile[bridge.src.id]
		for _, src := range srcSymbols {
			target, strategy, ok := crossLanguageTarget(src, len(srcSymbols), dstSymbols, byName)
			if !ok {
				continue
			}
			key := [2]int64{src.id, target.id}
			if seen[key] {
				continue
			}
			seen[key] = true
			var prefix string
			if strategy == ResolutionStrategyCrossLanguageSharedName {
				prefix = "shared_name:"
			} else {
				prefix = "import_path:"
			}
			links = append(links, xlangLink{
				srcSymbolID: src.id,
				dstSymbolID: target.id,
				srcFileID:   bridge.src.id,
				dstName:     target.name,
				evidence:    prefix + bridge.src.language + "→" + bridge.dst.language,
				strategy:    strategy,
				confidence:  resolutionConfidenceFor(strategy),
				sortKey: strings.Join([]string{
					bridge.src.path, src.qualified, src.kind, fmt.Sprintf("%09d:%09d", src.startLine, src.startCol), src.stableKey,
					bridge.dst.path, target.qualified, target.kind, fmt.Sprintf("%09d:%09d", target.startLine, target.startCol), target.stableKey,
					strategy,
				}, "\x00"),
			})
		}
	}
	links = dropAmbiguousCrossLanguageSources(links)
	sort.Slice(links, func(i, j int) bool { return links[i].sortKey < links[j].sortKey })
	return links, nil
}

// dropAmbiguousCrossLanguageSources removes every link of a source symbol that
// two bridges each answered. One file importing two foreign files that both
// define the source's name is two candidates for one claim, and the rule that
// two candidates abstain does not stop applying because the candidates arrived
// through different imports.
func dropAmbiguousCrossLanguageSources(links []xlangLink) []xlangLink {
	targets := make(map[int64]int, len(links))
	for _, l := range links {
		targets[l.srcSymbolID]++
	}
	kept := links[:0]
	for _, l := range links {
		if targets[l.srcSymbolID] == 1 {
			kept = append(kept, l)
		}
	}
	return kept
}

// crossLanguageTarget applies the ambiguity rule to one source symbol. It
// returns the single target the evidence identifies, or ok=false to abstain.
//
// Order never breaks a tie here: a lexical winner among two equally valid
// targets would be a deterministic wrong answer, which is worse than the
// non-deterministic one it replaced.
func crossLanguageTarget(src xlangSymbol, srcEligible int, dstSymbols []xlangSymbol, byName map[string][]xlangSymbol) (xlangSymbol, string, bool) {
	switch named := byName[src.name]; len(named) {
	case 1:
		return named[0], ResolutionStrategyCrossLanguageSharedName, true
	case 0:
		// No name correspondence, so the bridge is the only evidence and it has
		// to identify both ends by itself: one eligible symbol on each side. A
		// file-level import does not say which of several source symbols is the
		// one that uses the imported file, and asserting it for each of them
		// would fan one fact out into an edge per symbol.
		if len(dstSymbols) == 1 && srcEligible == 1 {
			return dstSymbols[0], ResolutionStrategyCrossLanguageImportPath, true
		}
		return xlangSymbol{}, "", false
	default:
		// Several same-named candidates in the imported file: ambiguous.
		return xlangSymbol{}, "", false
	}
}

// crossLanguageFiles loads the active files and the two path indexes the
// specifier matcher needs. Paths are compared in slash form so a Windows index
// resolves the same specifiers as a POSIX one.
func crossLanguageFiles(ctx context.Context, q xlangQueryer, repoID int64) ([]xlangFile, map[string][]xlangFile, map[string][]xlangFile, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, path, language FROM files
		WHERE repo_id = ? AND is_deleted = 0
		ORDER BY path, id
	`, repoID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cross-language files query: %w", err)
	}
	defer rows.Close()

	var files []xlangFile
	byFullPath := map[string][]xlangFile{}
	byBase := map[string][]xlangFile{}
	for rows.Next() {
		var f xlangFile
		var stored string
		if err := rows.Scan(&f.id, &stored, &f.language); err != nil {
			return nil, nil, nil, fmt.Errorf("cross-language file scan: %w", err)
		}
		f.path = CanonicalRelPath(stored)
		files = append(files, f)
		byFullPath[f.path] = append(byFullPath[f.path], f)
		if base := strings.TrimSuffix(f.path, path.Ext(f.path)); base != "" && base != f.path {
			byBase[base] = append(byBase[base], f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("cross-language file rows: %w", err)
	}
	// Sorted input keeps every bucket ordered by path, so candidate inspection
	// is order-independent even before the ambiguity check.
	return files, byFullPath, byBase, nil
}

// crossLanguageBridges resolves every import specifier to at most one foreign
// file and returns the resulting bridges in semantic order.
func crossLanguageBridges(
	ctx context.Context,
	q xlangQueryer,
	repoID int64,
	byFullPath, byBase map[string][]xlangFile,
) ([]xlangBridge, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT f.id, f.path, f.language, fi.import_path
		FROM file_imports fi
		JOIN files f ON f.id = fi.file_id
		WHERE f.repo_id = ? AND f.is_deleted = 0
		ORDER BY f.path, fi.import_path, f.id
	`, repoID)
	if err != nil {
		return nil, fmt.Errorf("cross-language imports query: %w", err)
	}
	defer rows.Close()

	seen := map[[2]int64]bool{}
	var bridges []xlangBridge
	for rows.Next() {
		var importer xlangFile
		var stored, specifier string
		if err := rows.Scan(&importer.id, &stored, &importer.language, &specifier); err != nil {
			return nil, fmt.Errorf("cross-language import scan: %w", err)
		}
		importer.path = CanonicalRelPath(stored)
		target, ok := crossLanguageImportTarget(importer, specifier, byFullPath, byBase)
		if !ok {
			continue
		}
		key := [2]int64{importer.id, target.id}
		if seen[key] {
			continue
		}
		seen[key] = true
		bridges = append(bridges, xlangBridge{src: importer, dst: target})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cross-language import rows: %w", err)
	}
	sort.Slice(bridges, func(i, j int) bool {
		if bridges[i].src.path != bridges[j].src.path {
			return bridges[i].src.path < bridges[j].src.path
		}
		return bridges[i].dst.path < bridges[j].dst.path
	})
	return bridges, nil
}

// crossLanguageImportTarget resolves one specifier. It abstains unless the
// specifier is path-shaped, resolves inside the repository, and names exactly
// one foreign file with no same-language file answering it.
func crossLanguageImportTarget(
	importer xlangFile,
	specifier string,
	byFullPath, byBase map[string][]xlangFile,
) (xlangFile, bool) {
	resolved, ok := resolveImportSpecifierPath(importer.path, specifier)
	if !ok {
		return xlangFile{}, false
	}

	// A specifier may name the file directly, name it without its extension, or
	// carry a foreign extension that has to come off first (a TypeScript import
	// of './model.ts' answered by model.py). All three are the same claim about
	// one path, so their candidates are one set.
	candidates := map[int64]xlangFile{}
	add := func(matches []xlangFile) {
		for _, m := range matches {
			if m.id != importer.id {
				candidates[m.id] = m
			}
		}
	}
	add(byFullPath[resolved])
	add(byBase[resolved])
	if ext := path.Ext(resolved); ext != "" {
		stripped := strings.TrimSuffix(resolved, ext)
		add(byFullPath[stripped])
		add(byBase[stripped])
	}
	if len(candidates) == 0 {
		return xlangFile{}, false
	}

	var foreign []xlangFile
	for _, c := range candidates {
		if c.language == importer.language {
			// A same-language file answers this import, so the ordinary
			// same-language resolver owns it: claiming a cross-language target
			// as well would be inventing a second meaning for one specifier.
			return xlangFile{}, false
		}
		foreign = append(foreign, c)
	}
	if len(foreign) != 1 {
		// Zero is nothing to link; two or more is ambiguous, and picking either
		// would be a guess.
		return xlangFile{}, false
	}
	return foreign[0], true
}

// resolveImportSpecifierPath turns an import specifier into a repository
// relative slash path, or reports that it is not a path at all.
//
// `file_imports.import_path` holds whatever the parser saw: `numpy as np`,
// `std::{fs, io}`, `com.foo.Bar`, `./model`, `src/shared/model.ts`. Only the
// path-shaped ones can name a file, and reading dots as extensions is what let
// `import app.models` match a root-level `app.ts`.
func resolveImportSpecifierPath(importerPath, specifier string) (string, bool) {
	spec := filepath.ToSlash(strings.TrimSpace(specifier))
	if spec == "" || strings.ContainsAny(spec, " \t\"'()[]{}<>*:;,") {
		return "", false
	}
	relative := strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../")
	if !relative && !strings.Contains(spec, "/") {
		// Not path-shaped: a bare or dotted module name.
		return "", false
	}
	var resolved string
	if relative {
		resolved = path.Join(path.Dir(importerPath), spec)
	} else {
		resolved = path.Clean(spec)
	}
	resolved = strings.TrimPrefix(resolved, "/")
	if resolved == "" || resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		// Escapes the repository: no file of ours answers it.
		return "", false
	}
	return resolved, true
}

// crossLanguageSymbols loads the link-eligible symbols of the bridged files,
// each file's slice ordered by semantic identity.
func crossLanguageSymbols(ctx context.Context, q xlangQueryer, repoID int64, fileIDs map[int64]bool) (map[int64][]xlangSymbol, error) {
	ids := make([]int64, 0, len(fileIDs))
	for id := range fileIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	byFile := map[int64][]xlangSymbol{}
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		batch := ids[start:end]
		args := make([]any, 0, len(batch)+1)
		args = append(args, repoID)
		for _, id := range batch {
			args = append(args, id)
		}
		query := `
			SELECT id, file_id, name, qualified_name, kind, start_line, start_col, stable_key
			FROM symbols
			WHERE repo_id = ? AND file_id IN (` + sqlitePlaceholders(len(batch)) + `)
			AND kind IN ` + crossLangEligibleKinds
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("cross-language symbols query: %w", err)
		}
		for rows.Next() {
			var sym xlangSymbol
			if err := rows.Scan(&sym.id, &sym.fileID, &sym.name, &sym.qualified, &sym.kind,
				&sym.startLine, &sym.startCol, &sym.stableKey); err != nil {
				rows.Close()
				return nil, fmt.Errorf("cross-language symbol scan: %w", err)
			}
			byFile[sym.fileID] = append(byFile[sym.fileID], sym)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("cross-language symbol rows: %w", err)
		}
		rows.Close()
	}
	for fileID := range byFile {
		syms := byFile[fileID]
		sort.Slice(syms, func(i, j int) bool { return xlangSymbolLess(syms[i], syms[j]) })
		byFile[fileID] = syms
	}
	return byFile, nil
}

// xlangSymbolLess orders symbols by identity columns only: qualified name,
// kind, position, then stable_key as the last resort. A row id would reintroduce
// exactly the insertion-order dependence this pass exists to remove.
func xlangSymbolLess(a, b xlangSymbol) bool {
	if a.qualified != b.qualified {
		return a.qualified < b.qualified
	}
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	if a.startLine != b.startLine {
		return a.startLine < b.startLine
	}
	if a.startCol != b.startCol {
		return a.startCol < b.startCol
	}
	return a.stableKey < b.stableKey
}

// applyCrossLanguageLinksTx makes the stored `cross_language_ref` set equal
// `links`, and returns how many links were newly written.
func applyCrossLanguageLinksTx(ctx context.Context, tx *sql.Tx, repoID int64, links []xlangLink) (int, error) {
	type storedLink struct {
		id         int64
		dstName    string
		evidence   string
		strategy   string
		confidence string
		fileID     int64
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, src_symbol_id, dst_symbol_id, dst_name, evidence, resolution_strategy, resolution_confidence, file_id
		FROM edges
		WHERE repo_id = ? AND edge_kind = ?
		ORDER BY id
	`, repoID, EdgeKindCrossLanguageRef)
	if err != nil {
		return 0, fmt.Errorf("cross-language existing query: %w", err)
	}
	stored := map[[2]int64]storedLink{}
	var obsolete []int64
	for rows.Next() {
		var id, src int64
		var dst sql.NullInt64
		var l storedLink
		if err := rows.Scan(&id, &src, &dst, &l.dstName, &l.evidence, &l.strategy, &l.confidence, &l.fileID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("cross-language existing scan: %w", err)
		}
		if !dst.Valid {
			// An unbound cross-language row carries no call site and no
			// evidence, and the implicit resolver would happily rebind it
			// same-language. Drop it; this pass recreates it if the evidence
			// still supports it.
			obsolete = append(obsolete, id)
			continue
		}
		l.id = id
		key := [2]int64{src, dst.Int64}
		if previous, ok := stored[key]; ok {
			// A pre-fix database can hold the same pair twice (duplicate
			// file_imports rows each inserted their own copy). Keep one and
			// retire the rest, or the duplicate would be invisible to this
			// reconciliation forever and every rerun would report a change.
			obsolete = append(obsolete, id)
			_ = previous
			continue
		}
		stored[key] = l
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("cross-language existing rows: %w", err)
	}
	rows.Close()

	wanted := make(map[[2]int64]bool, len(links))
	var insert []xlangLink
	for _, l := range links {
		key := [2]int64{l.srcSymbolID, l.dstSymbolID}
		wanted[key] = true
		existing, ok := stored[key]
		if ok &&
			existing.dstName == l.dstName &&
			existing.evidence == l.evidence &&
			existing.strategy == l.strategy &&
			existing.confidence == l.confidence &&
			existing.fileID == l.srcFileID {
			continue
		}
		if ok {
			// Same pair, different provenance: replace it so the row states the
			// evidence that actually supports it now.
			obsolete = append(obsolete, existing.id)
		}
		insert = append(insert, l)
	}
	for key, existing := range stored {
		if !wanted[key] {
			obsolete = append(obsolete, existing.id)
		}
	}
	sort.Slice(obsolete, func(i, j int) bool { return obsolete[i] < obsolete[j] })

	const deleteChunk = 400
	for start := 0; start < len(obsolete); start += deleteChunk {
		end := min(start+deleteChunk, len(obsolete))
		batch := obsolete[start:end]
		args := make([]any, 0, len(batch)+1)
		args = append(args, repoID)
		for _, id := range batch {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM edges WHERE repo_id = ? AND id IN (`+sqlitePlaceholders(len(batch))+`)`,
			args...); err != nil {
			return 0, fmt.Errorf("cross-language delete obsolete: %w", err)
		}
	}

	const columns = "repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line," +
		" resolution_strategy, resolution_confidence"
	args := make([]any, 0, crossLangEdgeValuesBatchRows*10)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		if err := execBatchInsert(ctx, tx, "edges", columns, 10, args, nil); err != nil {
			return fmt.Errorf("cross-language insert: %w", err)
		}
		args = args[:0]
		return nil
	}
	for _, l := range insert {
		args = append(args, repoID, l.srcSymbolID, l.dstSymbolID, l.dstName, EdgeKindCrossLanguageRef,
			l.evidence, l.srcFileID, 0, l.strategy, l.confidence)
		if len(args) == crossLangEdgeValuesBatchRows*10 {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return len(insert), nil
}
