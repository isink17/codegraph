package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/isink17/codegraph/internal/graph"
)

type ImpactSeedPresence struct {
	Requested int      `json:"requested"`
	Found     int      `json:"found"`
	Missing   []string `json:"missing"`
}

func (s *Store) SearchSymbolsResult(ctx context.Context, repoID int64, query string, limit, offset int) (SymbolSearchResult, error) {
	return s.searchSymbolsWithPresence(ctx, repoID, query, limit, offset)
}

// searchSymbolsWithPresence evaluates the candidate CTE once. The metadata
// row keeps presence available even when the requested page is empty.
func (s *Store) searchSymbolsWithPresence(ctx context.Context, repoID int64, query string, limit, offset int) (SymbolSearchResult, error) {
	search := func(sqlText string, args ...any) (SymbolSearchResult, error) {
		rows, err := s.db.QueryContext(ctx, sqlText, args...)
		if err != nil {
			return SymbolSearchResult{}, err
		}
		defer rows.Close()
		var result SymbolSearchResult
		for rows.Next() {
			var matched int
			var sym graph.Symbol
			var id, fileID, startLine, startCol, endLine, endCol sql.NullInt64
			var language, kind, name, qualifiedName, container, signature, visibility, docSummary, stableKey, filePath sql.NullString
			if err := rows.Scan(&matched, &id, &fileID, &language, &kind, &name, &qualifiedName, &container, &signature, &visibility,
				&startLine, &startCol, &endLine, &endCol, &docSummary, &stableKey, &filePath); err != nil {
				return SymbolSearchResult{}, err
			}
			result.Matched = matched != 0
			if !id.Valid {
				continue
			}
			sym.ID, sym.FileID = id.Int64, fileID.Int64
			sym.Language, sym.Kind, sym.Name, sym.QualifiedName = language.String, kind.String, name.String, qualifiedName.String
			sym.ContainerName, sym.Signature, sym.Visibility = container.String, signature.String, visibility.String
			sym.Range.StartLine, sym.Range.StartCol = int(startLine.Int64), int(startCol.Int64)
			sym.Range.EndLine, sym.Range.EndCol = int(endLine.Int64), int(endCol.Int64)
			sym.DocSummary, sym.StableKey, sym.FilePath = docSummary.String, stableKey.String, filePath.String
			result.Matches = append(result.Matches, sym)
		}
		return result, rows.Err()
	}

	args := []any{repoID, quoteFTS(query), safeLimit(limit), safeOffset(offset)}
	result, err := search(`
		WITH matches AS MATERIALIZED (
			SELECT s.id
			FROM symbol_fts fts JOIN symbols s ON s.id = fts.symbol_id
			WHERE s.repo_id = ? AND symbol_fts MATCH ?
		), page AS (
			SELECT matches.id FROM matches
			JOIN symbols s ON s.id = matches.id
			ORDER BY s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
			LIMIT ? OFFSET ?
		), meta AS (SELECT EXISTS(SELECT 1 FROM matches) AS matched)
		SELECT meta.matched, s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM meta
		LEFT JOIN page p ON 1 = 1
		LEFT JOIN symbols s ON s.id = p.id
		LEFT JOIN files f ON f.id = s.file_id
		ORDER BY CASE WHEN s.id IS NULL THEN 0 ELSE 1 END, s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
	`, args...)
	if err == nil {
		return result, nil
	}

	return search(`
		WITH matches AS MATERIALIZED (
			SELECT s.id FROM symbols s
			WHERE s.repo_id = ? AND (s.name LIKE ? OR s.qualified_name LIKE ?)
		), page AS (
			SELECT matches.id FROM matches
			JOIN symbols s ON s.id = matches.id
			ORDER BY s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
			LIMIT ? OFFSET ?
		), meta AS (SELECT EXISTS(SELECT 1 FROM matches) AS matched)
		SELECT meta.matched, s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM meta
		LEFT JOIN page p ON 1 = 1
		LEFT JOIN symbols s ON s.id = p.id
		LEFT JOIN files f ON f.id = s.file_id
		ORDER BY CASE WHEN s.id IS NULL THEN 0 ELSE 1 END, s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
	`, repoID, "%"+query+"%", "%"+query+"%", safeLimit(limit), safeOffset(offset))
}

func (s *Store) FindSymbolExactResult(ctx context.Context, repoID int64, query string, limit, offset int) (SymbolSearchResult, error) {
	matches, err := s.FindSymbolExact(ctx, repoID, query, limit, offset)
	if err != nil {
		return SymbolSearchResult{}, err
	}
	var found int
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM symbols WHERE repo_id = ? AND (name = ? OR qualified_name = ?))`, repoID, query, query).Scan(&found)
	return SymbolSearchResult{Matched: found != 0, Matches: matches}, err
}

func (s *Store) searchSymbolsMatched(ctx context.Context, repoID int64, query string) (bool, error) {
	var matched int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM symbol_fts fts JOIN symbols s ON s.id = fts.symbol_id
		WHERE s.repo_id = ? AND symbol_fts MATCH ?
	)`, repoID, quoteFTS(query)).Scan(&matched)
	if err == nil {
		return matched != 0, nil
	}
	return s.likeSymbolsMatched(ctx, repoID, query)
}

func (s *Store) likeSymbolsMatched(ctx context.Context, repoID int64, query string) (bool, error) {
	var matched int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM symbols WHERE repo_id = ? AND (name LIKE ? OR qualified_name LIKE ?))`, repoID, "%"+query+"%", "%"+query+"%").Scan(&matched)
	return matched != 0, err
}

func (s *Store) FindCallersResult(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) (NeighborResult, error) {
	var ids []int64
	if symbolID != 0 {
		identity, ok, err := s.lookupSymbolIdentity(ctx, repoID, symbolID)
		if err != nil {
			return NeighborResult{}, err
		}
		if !ok {
			return NeighborResult{Callers: []graph.Symbol{}}, nil
		}
		symbol = identity.QualifiedName
		ids = []int64{identity.ID}
	} else {
		var err error
		ids, err = s.lookupSymbolIDs(ctx, repoID, symbol, 0)
		if err != nil {
			return NeighborResult{}, err
		}
	}
	items, err := s.findCallersResolved(ctx, repoID, symbol, ids, limit, offset)
	if err != nil {
		return NeighborResult{}, err
	}
	result := NeighborResult{TargetFound: len(ids) > 0}
	if result.TargetFound {
		result.Callers = items
	} else {
		result.UnresolvedHints = items
	}
	return result, nil
}

func (s *Store) FindCalleesResult(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) (NeighborResult, error) {
	var ids []int64
	if symbolID != 0 {
		identity, ok, err := s.lookupSymbolIdentity(ctx, repoID, symbolID)
		if err != nil {
			return NeighborResult{}, err
		}
		if !ok {
			return NeighborResult{Callees: []graph.Symbol{}}, nil
		}
		symbol = identity.QualifiedName
		ids = []int64{identity.ID}
	} else {
		var err error
		ids, err = s.lookupSymbolIDs(ctx, repoID, symbol, 0)
		if err != nil {
			return NeighborResult{}, err
		}
	}
	items, err := s.findCalleesResolved(ctx, repoID, ids, limit, offset)
	if err != nil {
		return NeighborResult{}, err
	}
	return NeighborResult{TargetFound: len(ids) > 0, Callees: items}, nil
}

func (s *Store) RelatedTestsResult(ctx context.Context, repoID int64, symbol, file string, limit, offset int) (RelatedTestsResult, error) {
	if symbol != "" {
		targetID, err := s.lookupSymbolID(ctx, repoID, symbol, 0)
		found := err == nil
		if errors.Is(err, ErrSymbolNotFound) {
			err = nil
		}
		if err != nil {
			return RelatedTestsResult{}, err
		}
		if !found {
			return RelatedTestsResult{Tests: []RelatedTest{}}, nil
		}
		tests, err := s.relatedTests(ctx, repoID, symbol, file, limit, offset, targetID, true)
		return RelatedTestsResult{TargetFound: found, Tests: tests}, err
	}
	found, err := s.filePresent(ctx, repoID, file)
	if err != nil {
		return RelatedTestsResult{}, err
	}
	tests, err := s.RelatedTests(ctx, repoID, symbol, file, limit, offset)
	return RelatedTestsResult{TargetFound: found, Tests: tests}, err
}

// RelatedTestFilesPresent resolves all requested file identities with one
// indexed lookup; the test-result aggregation remains owned by query.Service.
func (s *Store) RelatedTestFilesPresent(ctx context.Context, repoID int64, files []string) ([]bool, error) {
	present := make([]bool, len(files))
	variants := make([]string, 0, len(files)*2)
	seen := map[string]struct{}{}
	for _, file := range files {
		canonical := CanonicalRelPath(normalizeRepoRelPath(file))
		for _, variant := range storedPathVariants(canonical) {
			if variant == "" {
				continue
			}
			if _, ok := seen[variant]; !ok {
				seen[variant] = struct{}{}
				variants = append(variants, variant)
			}
		}
	}
	if len(variants) == 0 {
		return present, nil
	}
	args := make([]any, 0, len(variants)+1)
	args = append(args, repoID)
	for _, variant := range variants {
		args = append(args, variant)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM files WHERE repo_id = ? AND path IN (`+sqlPlaceholders(len(variants))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]struct{}{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		found[path] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, file := range files {
		for _, variant := range storedPathVariants(CanonicalRelPath(normalizeRepoRelPath(file))) {
			if _, ok := found[variant]; ok {
				present[i] = true
				break
			}
		}
	}
	return present, nil
}

func (s *Store) filePresent(ctx context.Context, repoID int64, file string) (bool, error) {
	canonical := CanonicalRelPath(normalizeRepoRelPath(file))
	if canonical == "" {
		return false, nil
	}
	variants := storedPathVariants(canonical)
	args := make([]any, 0, len(variants)+1)
	args = append(args, repoID)
	for _, v := range variants {
		args = append(args, v)
	}
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM files WHERE repo_id = ? AND path IN (`+sqlPlaceholders(len(variants))+`) LIMIT 1`, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && id.Valid, err
}

func (s *Store) TraceDependenciesResult(ctx context.Context, repoID int64, symbol, direction string, maxDepth, limit, offset int) (TraceResult, error) {
	return s.traceDependencies(ctx, repoID, symbol, direction, maxDepth, limit, offset)
}
