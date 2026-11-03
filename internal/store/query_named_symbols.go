package store

import (
	"context"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// NamedSymbolMatch is one resolved reference to a symbol a task named.
type NamedSymbolMatch struct {
	// Name is the reference exactly as the request spelled it.
	Name   string
	Symbol graph.Symbol
}

// maxNamedReferenceSegments bounds the dot-delimited suffix legs this lookup
// serves from an index. One, two and three segments are persisted as
// `symbols.name`, `symbols.dot_tail2` and `symbols.dot_tail3`; a longer
// reference has no persisted tail to join against, so it is matched only
// against the whole qualified name rather than by a repository-wide scan.
const maxNamedReferenceSegments = 3

// SymbolsForNamedReferences resolves names a task text spelled out -- a bare
// identifier (`CacheInterceptor`) or a dotted tail of a qualified name
// (`RealCall.execute`) -- to the symbols that carry them.
//
// Matching is exact and never fuzzy: a reference matches a symbol when it
// equals the symbol's `qualified_name`, or equals the last one, two or three
// dot-separated segments of it. The suffix legs are the ones that matter in
// practice: a task writes `RealCall.execute` while the index stores
// `okhttp3.internal.connection.RealCall.execute`, which equals neither the
// `name` column (`execute`) nor the qualified name.
//
// Every leg is an indexed equality against a persisted column
// (`idx_symbols_repo_name`, `idx_symbols_repo_qname`,
// `idx_symbols_repo_dot_tail2`, `idx_symbols_repo_dot_tail3`), so the cost is
// one indexed lookup per reference and never a repository-wide scan.
// Comparison is case-sensitive, like those indexes: a reference is evidence
// because the task quoted an identifier as the source spells it, and a
// case-folded guess is not the same evidence.
//
// perNameLimit bounds the rows read for one reference. A reference matching
// more rows than that is ambiguous -- `intercept` names a method on every
// interceptor -- so it contributes nothing rather than an arbitrary subset.
// The caller bounds how many references are offered.
func (s *Store) SymbolsForNamedReferences(ctx context.Context, repoID int64, names []string, perNameLimit int) ([]NamedSymbolMatch, error) {
	if len(names) == 0 || perNameLimit <= 0 {
		return nil, nil
	}
	ctx, tx, err := s.beginParserSemanticGraphRead(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if tx != nil {
		defer tx.Rollback()
	}

	// Each leg is its own indexed SELECT. An `OR` over two indexed columns is
	// not enough: SQLite plans it as one pass over the repository's symbols,
	// while a UNION of single-column equalities keeps both legs on their index.
	const head = `
		WITH hit(id) AS (
			SELECT id FROM symbols WHERE repo_id = ? AND qualified_name = ?`
	const tail = `
		)
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name,
		       s.signature, s.visibility, s.start_line, s.start_col, s.end_line, s.end_col,
		       s.doc_summary, s.stable_key, f.path
		FROM hit
		JOIN symbols s ON s.id = hit.id
		JOIN files f ON f.id = s.file_id AND f.repo_id = s.repo_id AND f.is_deleted = 0
		ORDER BY f.path ASC, s.qualified_name ASC, s.kind ASC, s.start_line ASC, s.start_col ASC,
		         s.end_line ASC, s.end_col ASC, s.stable_key ASC
		LIMIT ?
	`
	seen := make(map[int64]bool)
	out := make([]NamedSymbolMatch, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		suffixLeg := ""
		switch strings.Count(name, ".") + 1 {
		case 1:
			suffixLeg = "\n\t\t\tUNION SELECT id FROM symbols WHERE repo_id = ? AND name = ?"
		case 2:
			// `!= ''` restates the partial index's own predicate, which is what
			// keeps idx_symbols_repo_dot_tail2 usable here.
			suffixLeg = "\n\t\t\tUNION SELECT id FROM symbols WHERE repo_id = ? AND dot_tail2 != '' AND dot_tail2 = ?"
		case maxNamedReferenceSegments:
			suffixLeg = "\n\t\t\tUNION SELECT id FROM symbols WHERE repo_id = ? AND dot_tail3 != '' AND dot_tail3 = ?"
		}
		args := []any{repoID, name}
		if suffixLeg != "" {
			args = append(args, repoID, name)
		}
		// Reading one row past the cap is what distinguishes "exactly at the
		// cap" from "too many to trust".
		args = append(args, perNameLimit+1)
		rows, err := s.parserSemanticQueryer(ctx).QueryContext(ctx, head+suffixLeg+tail, args...)
		if err != nil {
			return nil, err
		}
		syms, err := scanSymbols(rows)
		if err != nil {
			return nil, err
		}
		if len(syms) > perNameLimit {
			continue
		}
		for _, sym := range syms {
			if seen[sym.ID] {
				continue
			}
			seen[sym.ID] = true
			out = append(out, NamedSymbolMatch{Name: name, Symbol: sym})
		}
	}
	return out, nil
}
