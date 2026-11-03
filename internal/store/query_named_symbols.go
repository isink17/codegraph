package store

import (
	"context"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// SymbolsForNamedReferences resolves names a task text spelled out -- a bare
// identifier (`CacheInterceptor`) or a dotted tail of a qualified name
// (`RealCall.execute`) -- to the symbols that carry them.
//
// Matching is exact and never fuzzy. A reference matches a symbol when it
// equals the symbol's qualified name, equals the portion of it after the last
// '/' (`qualified_suffix`), or equals the last one, two or three
// dot-separated segments of that portion. The suffix legs are the ones that
// matter in practice: a task writes `RealCall.execute` while the index stores
// `okhttp3.internal.connection.RealCall.execute`, which equals neither the
// `name` column (`execute`) nor the qualified name.
//
// Every leg is an indexed equality against a persisted column
// (`idx_symbols_repo_qname`, `idx_symbols_repo_qsuffix`,
// `idx_symbols_repo_name`, `idx_symbols_repo_dot_tail2`,
// `idx_symbols_repo_dot_tail3`), and each leg stops at the cap. A reference
// therefore costs a bounded number of index seeks whatever its popularity in
// the repository, and never a repository-wide scan.
//
// Comparison is case-sensitive, like those indexes: a reference is evidence
// because the task quoted an identifier as the source spells it, and a
// case-folded guess is not the same evidence.
//
// perNameLimit bounds the symbols one reference may contribute. A reference
// matching more is ambiguous -- `intercept` names a method on every
// interceptor -- so it contributes nothing rather than an arbitrary subset.
// The caller bounds how many references are offered.
func (s *Store) SymbolsForNamedReferences(ctx context.Context, repoID int64, names []string, perNameLimit int) ([]graph.Symbol, error) {
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

	// Each leg is its own indexed SELECT. An `OR` over several indexed columns
	// is not enough: SQLite plans it as one pass over the repository's symbols,
	// while a UNION of single-column equalities keeps every leg on its index.
	//
	// Every leg carries its own cap, and the live-file join sits inside it. A
	// single `LIMIT` after the `ORDER BY` would bound only the rows returned:
	// the sort, and the list the legs feed it, would still materialize every
	// match, which is unbounded in a reference's popularity rather than in the
	// cap. A name every file declares -- `__init__`, `setUp`, `onCreate` --
	// is the normal case for that, not a contrived one. Measured on a
	// 206k-symbol index with 200k declarations of one name: 43 ms for the
	// unbounded shape, 0.07 ms for this one.
	//
	// The caps cannot change an answer. A leg truncates only when it alone
	// already exceeds perNameLimit, and the reference is then discarded unread
	// at any cap; at or below the cap every leg returns its complete match set,
	// the CTE holds their exact union, and the outer ORDER BY is total over it.
	// Keeping the live-file join inside each leg is what makes that true in the
	// presence of soft-deleted files: a cap applied before that filter could
	// fill itself with dead rows and hide the live declaration behind them.
	const legSelect = `SELECT id FROM (
				SELECT s.id AS id
				FROM symbols s
				JOIN files f ON f.id = s.file_id AND f.repo_id = s.repo_id AND f.is_deleted = 0
				WHERE s.repo_id = ? AND `
	const legEnd = `
				LIMIT ?)`
	// `!= ''` restates each partial index's own predicate, which is what keeps
	// that index usable here.
	const (
		legQualifiedName   = "s.qualified_name = ?"
		legQualifiedSuffix = "s.qualified_suffix != '' AND s.qualified_suffix = ?"
		legName            = "s.name = ?"
		legDotTail2        = "s.dot_tail2 != '' AND s.dot_tail2 = ?"
		legDotTail3        = "s.dot_tail3 != '' AND s.dot_tail3 = ?"
	)
	const tail = `
			LIMIT ?
		)
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name,
		       s.signature, s.visibility, s.start_line, s.start_col, s.end_line, s.end_col,
		       s.doc_summary, s.stable_key, f.path
		FROM hit
		JOIN symbols s ON s.id = hit.id
		JOIN files f ON f.id = s.file_id AND f.repo_id = s.repo_id AND f.is_deleted = 0
		ORDER BY f.path ASC, s.qualified_name ASC, s.kind ASC, s.start_line ASC, s.start_col ASC,
		         s.end_line ASC, s.end_col ASC, s.stable_key ASC
	`
	seen := make(map[int64]bool)
	out := make([]graph.Symbol, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		predicates := []string{legQualifiedName, legQualifiedSuffix}
		switch strings.Count(name, ".") + 1 {
		case 1:
			predicates = append(predicates, legName)
		case 2:
			predicates = append(predicates, legDotTail2)
		case 3:
			predicates = append(predicates, legDotTail3)
		}
		// Reading one row past the cap is what distinguishes "exactly at the
		// cap" from "too many to trust".
		probe := perNameLimit + 1
		legs := make([]string, 0, len(predicates))
		args := make([]any, 0, len(predicates)*3+1)
		for _, predicate := range predicates {
			legs = append(legs, legSelect+predicate+legEnd)
			args = append(args, repoID, name, probe)
		}
		args = append(args, probe)
		rows, err := s.parserSemanticQueryer(ctx).QueryContext(ctx,
			"\n\t\tWITH hit(id) AS (\n\t\t\t"+strings.Join(legs, "\n\t\t\tUNION ")+tail, args...)
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
			out = append(out, sym)
		}
	}
	return out, nil
}
