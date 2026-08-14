package store

import (
	"context"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// SymbolRef identifies a symbol the way semantic search reports it: by
// containing file path and qualified name. Both semantic paths (token overlap
// and hybrid) group their results on exactly this pair, so it is the natural
// join key back to a real symbol row -- and, unlike a bare name, it is not
// ambiguous across packages.
type SymbolRef struct {
	File          string
	QualifiedName string
}

// SymbolsForRefs resolves search-result refs to full symbol rows in one query
// per chunk. It exists so seed expansion can use a symbol's real identity
// (symbol_id, stable_key) instead of a second, ambiguous bare-name lookup, and
// so a 30-seed context request does not become 30 round trips.
//
// Refs with an empty file or qualified name cannot identify a row and are
// skipped. When a single (file, qualified_name) pair matches more than one row
// -- overloads and same-named methods in one file -- the choice is the row that
// carries a stable key, then the lowest stable key, then the earliest position.
// Row ids are never used to break ties.
func (s *Store) SymbolsForRefs(ctx context.Context, repoID int64, refs []SymbolRef) (map[SymbolRef]graph.Symbol, error) {
	out := make(map[SymbolRef]graph.Symbol, len(refs))
	if len(refs) == 0 {
		return out, nil
	}

	wanted := make(map[SymbolRef]struct{}, len(refs))
	pathSet := make(map[string]struct{}, len(refs))
	nameSet := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.File == "" || ref.QualifiedName == "" {
			continue
		}
		wanted[ref] = struct{}{}
		pathSet[ref.File] = struct{}{}
		nameSet[ref.QualifiedName] = struct{}{}
	}
	if len(wanted) == 0 {
		return out, nil
	}
	paths := sortedKeys(pathSet)
	names := sortedKeys(nameSet)

	// Both IN lists are bound into one statement, so each loop takes half the
	// single-list chunk budget rather than all of it.
	const refChunk = sqliteInClauseBatchSize / 2

	// The cross product of the two IN lists over-selects (a path may hold a
	// qualified name wanted for a different path), which the wanted-set filter
	// below discards. That costs one query instead of one per ref, and SQLite
	// row-value IN support is not portable across the drivers this project
	// builds with.
	best := map[SymbolRef]graph.Symbol{}
	for pathStart := 0; pathStart < len(paths); pathStart += refChunk {
		pathChunk := paths[pathStart:min(pathStart+refChunk, len(paths))]
		for nameStart := 0; nameStart < len(names); nameStart += refChunk {
			nameChunk := names[nameStart:min(nameStart+refChunk, len(names))]

			args := make([]any, 0, len(pathChunk)+len(nameChunk)+1)
			args = append(args, repoID)
			for _, p := range pathChunk {
				args = append(args, p)
			}
			for _, n := range nameChunk {
				args = append(args, n)
			}
			query := `
				SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
				       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
				FROM symbols s
				JOIN files f ON f.id = s.file_id
				WHERE s.repo_id = ?
				  AND f.path IN (` + placeholders(len(pathChunk)) + `)
				  AND s.qualified_name IN (` + placeholders(len(nameChunk)) + `)
			`
			rows, err := s.db.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, err
			}
			syms, err := scanSymbols(rows)
			if err != nil {
				return nil, err
			}
			for _, sym := range syms {
				ref := SymbolRef{File: sym.FilePath, QualifiedName: sym.QualifiedName}
				if _, ok := wanted[ref]; !ok {
					continue
				}
				existing, seen := best[ref]
				if !seen || preferSeedSymbol(sym, existing) {
					best[ref] = sym
				}
			}
		}
	}
	for ref, sym := range best {
		out[ref] = sym
	}
	return out, nil
}

// preferSeedSymbol reports whether candidate should replace existing as the
// resolution of one (file, qualified_name) pair.
func preferSeedSymbol(candidate, existing graph.Symbol) bool {
	// A row without a stable key cannot be drilled into, so it never wins over
	// one that can -- and lexical order would otherwise put "" first.
	if (candidate.StableKey == "") != (existing.StableKey == "") {
		return existing.StableKey == ""
	}
	if candidate.StableKey != existing.StableKey {
		return candidate.StableKey < existing.StableKey
	}
	if candidate.Range.StartLine != existing.Range.StartLine {
		return candidate.Range.StartLine < existing.Range.StartLine
	}
	return candidate.Range.StartCol < existing.Range.StartCol
}

// SymbolNameCounts counts the symbols carrying each of the given short names,
// in one query. Context expansion uses it to decide whether a seed's name may
// take part in a lookup at all: FindCallers/FindCallees match unresolved edges
// by name, and a name shared by two packages would attribute one package's
// callers to the other's symbol.
func (s *Store) SymbolNameCounts(ctx context.Context, repoID int64, names []string) (map[string]int, error) {
	out := make(map[string]int, len(names))
	set := map[string]struct{}{}
	for _, n := range names {
		if n != "" {
			set[n] = struct{}{}
		}
	}
	if len(set) == 0 {
		return out, nil
	}
	unique := sortedKeys(set)
	for start := 0; start < len(unique); start += sqliteInClauseBatchSize {
		chunk := unique[start:min(start+sqliteInClauseBatchSize, len(unique))]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, n := range chunk {
			args = append(args, n)
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT s.name, COUNT(*)
			FROM symbols s
			WHERE s.repo_id = ? AND s.name IN (`+placeholders(len(chunk))+`)
			GROUP BY s.name
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			var count int
			if err := rows.Scan(&name, &count); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[name] = count
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LastScanID returns the highest scan id recorded for the repository, or 0 when
// it has never been scanned. It is the cheap graph-generation identity: Stats
// reports the same number but pays for several COUNT(*) scans to do it.
//
// Every scan row counts, including a failed or in-progress one. A failed scan
// can still have written rows before it gave up, so treating it as a new
// generation invalidates outstanding context cursors rather than letting one
// resume against a graph that may have moved underneath it.
func (s *Store) LastScanID(ctx context.Context, repoID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM scans WHERE repo_id = ?`, repoID).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func placeholders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}
