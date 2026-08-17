package store

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// SymbolRef identifies a symbol the way semantic search reports it: by
// containing file path and qualified name. Both semantic paths (token overlap
// and hybrid) group their results on exactly this pair, so it is the natural
// join key back to a real symbol row -- and, unlike a bare name, it is not
// ambiguous across packages.
//
// File is a repository-relative path in canonical form: forward slashes,
// whatever the host's separator. Equality of two refs is therefore
// platform-independent, and callers may build one from either form -- see
// CanonicalRelPath.
type SymbolRef struct {
	File          string
	QualifiedName string
}

// CanonicalRelPath is the logical form of a repository-relative path: forward
// slashes, no leading "./".
//
// The repository has two path forms and both are legitimate. `files.path` is
// stored in the host's native form, because the indexer derives it with
// filepath.Rel/filepath.Clean; every value that leaves the store for a client is
// slash-normalized instead (scanSymbol, RelatedTests, the export paths). On
// Linux and macOS the two coincide, which is why mixing them was invisible; on
// Windows `paymentsvc\service.go` and `paymentsvc/service.go` are different map
// keys for the same file.
//
// Use this wherever a relative path is an identity -- a map key, a comparison, a
// ranking tie-break -- and keep native form only for the SQL predicates that
// have to match the stored bytes.
// It deliberately does not trim whitespace and does not Clean: a leading or
// trailing space is a legal part of a POSIX filename, and trimming one here
// would silently fail to resolve that file. Argument hygiene belongs at the tool
// boundary (normalizeRepoRelPath, FileSourceStates), not in the identity
// function.
func CanonicalRelPath(path string) string {
	slashed := filepath.ToSlash(path)
	if slashed == "" || slashed == "." {
		return ""
	}
	return strings.TrimPrefix(slashed, "./")
}

// canonicalStoredPath interprets either native separator spelling at the
// compatibility boundary. CanonicalRelPath itself keeps POSIX identity rules;
// this helper is only for bytes read from persisted files.path values.
func canonicalStoredPath(path string) string {
	return CanonicalRelPath(strings.ReplaceAll(path, `\`, `/`))
}

// storedPathVariants returns slash, current-host native, and Windows-native
// forms a `files.path` column may hold. This keeps readers compatible with
// existing databases until canonical persistence is handled by P23.
func storedPathVariants(canonical string) []string {
	native := filepath.FromSlash(canonical)
	windows := strings.ReplaceAll(canonical, "/", `\`)
	variants := []string{canonical}
	for _, variant := range []string{native, windows} {
		if variant != canonical && !containsString(variants, variant) {
			variants = append(variants, variant)
		}
	}
	return variants
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

	// Two representations, kept apart deliberately: `wanted` (and the returned
	// map) is keyed canonically, because that is what a scanned symbol carries and
	// what every caller compares; the IN list binds the stored forms, because that
	// is what the column holds.
	wanted := make(map[SymbolRef]struct{}, len(refs))
	pathSet := make(map[string]struct{}, len(refs))
	nameSet := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		canonical := CanonicalRelPath(ref.File)
		if canonical == "" || ref.QualifiedName == "" {
			continue
		}
		wanted[SymbolRef{File: canonical, QualifiedName: ref.QualifiedName}] = struct{}{}
		for _, variant := range storedPathVariants(canonical) {
			pathSet[variant] = struct{}{}
		}
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
			hasGlobal := false
			for _, name := range nameChunk {
				if strings.HasPrefix(name, "::") {
					hasGlobal = true
					break
				}
			}

			args := make([]any, 0, len(pathChunk)+len(nameChunk)*2+1)
			args = append(args, repoID)
			for _, p := range pathChunk {
				args = append(args, p)
			}
			for _, n := range nameChunk {
				args = append(args, n)
			}
			if hasGlobal {
				for _, n := range nameChunk {
					args = append(args, n)
				}
			}
			nameMatch := `s.qualified_name IN (` + placeholders(len(nameChunk)) + `)`
			if hasGlobal {
				nameMatch = `(` + nameMatch + ` OR (s.language = 'cpp' AND s.container_name = '' AND '::' || s.qualified_name IN (` + placeholders(len(nameChunk)) + `)))`
			}
			query := `
				SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
				       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
				FROM symbols s
				JOIN files f ON f.id = s.file_id
				WHERE s.repo_id = ?
				  AND f.path IN (` + placeholders(len(pathChunk)) + `)
				  AND ` + nameMatch + `
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
				// scanSymbols already slash-normalizes FilePath; canonicalizing again
				// costs nothing and keeps this independent of that.
				ref := SymbolRef{File: CanonicalRelPath(sym.FilePath), QualifiedName: sym.QualifiedName}
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
