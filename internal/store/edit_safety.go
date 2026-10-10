package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/platform"
)

// EditSafetyTarget is the graph state an edit-safety assessment reads for one
// target: its symbols and the edges that reach them, from one read snapshot.
type EditSafetyTarget struct {
	// FileFound reports, for a file target, that the path is a live indexed
	// file. A live file may declare no symbols.
	FileFound bool
	// FileLanguage is the file target's indexed language.
	FileLanguage string
	// Symbols are the target symbols: the one exact symbol, or every symbol
	// the file declares, ordered by position.
	Symbols []graph.Symbol
	// Dependents are the resolved edges into Symbols from outside the target,
	// ordered by source path, line, source symbol and edge kind.
	Dependents []EditSafetyDependent
	// UnresolvedCandidates counts edges with no resolved target whose
	// recorded name equals a target symbol's name or qualified name, or ends
	// in "." followed by its name. Any of them may reach the target.
	UnresolvedCandidates int
}

// EditSafetyDependent is one resolved edge into an edit-safety target.
type EditSafetyDependent struct {
	EdgeKind             string `json:"edge_kind"`
	SourcePath           string `json:"source_path"`
	SourceLine           int64  `json:"source_line"`
	SourceSymbol         string `json:"source_symbol"`
	SourceStableKey      string `json:"source_stable_key"`
	SourceIsTest         bool   `json:"source_is_test"`
	TargetSymbol         string `json:"target_symbol"`
	TargetStableKey      string `json:"target_stable_key"`
	ResolutionStrategy   string `json:"resolution_strategy"`
	ResolutionConfidence string `json:"resolution_confidence"`
}

// SymbolsByQualifiedName returns up to limit live symbols whose qualified
// name is exactly name, ordered by path and position. It never matches a
// short name or suffix.
func (s *Store) SymbolsByQualifiedName(ctx context.Context, repoID int64, name string, limit int) ([]graph.Symbol, error) {
	rows, err := s.parserSemanticQueryer(ctx).QueryContext(ctx, `
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM symbols s
		JOIN files f ON f.id = s.file_id AND f.repo_id = s.repo_id AND f.is_deleted = 0
		WHERE s.repo_id = ? AND s.qualified_name = ?
		ORDER BY f.path ASC, s.start_line ASC, s.start_col ASC, s.kind ASC, s.stable_key ASC
		LIMIT ?
	`, repoID, name, limit)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// EditSafetySymbol reads the target state for one symbol id. ok is false when
// no live symbol has that id.
func (s *Store) EditSafetySymbol(ctx context.Context, repoID, symbolID int64) (EditSafetyTarget, bool, error) {
	ctx, tx, err := s.beginParserSemanticGraphRead(ctx, repoID)
	if err != nil {
		return EditSafetyTarget{}, false, err
	}
	if tx != nil {
		defer tx.Rollback()
	}
	syms, err := s.SymbolsForIDs(ctx, repoID, []int64{symbolID})
	if err != nil {
		return EditSafetyTarget{}, false, err
	}
	sym, ok := syms[symbolID]
	if !ok {
		return EditSafetyTarget{}, false, nil
	}
	out := EditSafetyTarget{Symbols: []graph.Symbol{sym}}
	if err := s.editSafetyEdges(ctx, repoID, &out, false); err != nil {
		return EditSafetyTarget{}, false, err
	}
	return out, true, nil
}

// EditSafetyFile reads the target state for one repository-relative file.
// An invalid spelling or a path that is not a live indexed file returns
// FileFound false.
func (s *Store) EditSafetyFile(ctx context.Context, repoID int64, file string) (EditSafetyTarget, error) {
	logical, err := platform.PublicRepositoryPath(file)
	if err != nil {
		return EditSafetyTarget{}, nil
	}
	ctx, tx, err := s.beginParserSemanticGraphRead(ctx, repoID)
	if err != nil {
		return EditSafetyTarget{}, err
	}
	if tx != nil {
		defer tx.Rollback()
	}
	var out EditSafetyTarget
	err = s.parserSemanticQueryer(ctx).QueryRowContext(ctx,
		`SELECT language FROM files WHERE repo_id = ? AND path = ? AND is_deleted = 0`, repoID, logical).Scan(&out.FileLanguage)
	if errors.Is(err, sql.ErrNoRows) {
		return EditSafetyTarget{}, nil
	}
	if err != nil {
		return EditSafetyTarget{}, err
	}
	out.FileFound = true
	rows, err := s.parserSemanticQueryer(ctx).QueryContext(ctx, `
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM symbols s JOIN files f ON f.repo_id = s.repo_id AND f.id = s.file_id
		WHERE s.repo_id = ? AND f.path = ? AND f.is_deleted = 0
		ORDER BY s.start_line, s.start_col, s.end_line, s.end_col, s.qualified_name, s.kind, s.stable_key
	`, repoID, logical)
	if err != nil {
		return EditSafetyTarget{}, err
	}
	if out.Symbols, err = scanSymbols(rows); err != nil {
		return EditSafetyTarget{}, err
	}
	if err := s.editSafetyEdges(ctx, repoID, &out, true); err != nil {
		return EditSafetyTarget{}, err
	}
	return out, nil
}

// editSafetyEdges fills Dependents and UnresolvedCandidates for t.Symbols.
// Edges from a target symbol to itself are not dependents; with sameFile,
// neither are edges from any symbol of the target file.
func (s *Store) editSafetyEdges(ctx context.Context, repoID int64, t *EditSafetyTarget, sameFile bool) error {
	t.Dependents = []EditSafetyDependent{}
	if len(t.Symbols) == 0 {
		return nil
	}
	q := s.parserSemanticQueryer(ctx)
	ids := make([]int64, 0, len(t.Symbols))
	for _, sym := range t.Symbols {
		ids = append(ids, sym.ID)
	}
	for _, chunk := range chunkInt64s(ids, sqliteInClauseBatchSize) {
		args := []any{repoID}
		for _, id := range chunk {
			args = append(args, id)
		}
		exclude := `e.src_symbol_id <> e.dst_symbol_id`
		if sameFile {
			exclude = `src.file_id <> dst.file_id`
		}
		rows, err := q.QueryContext(ctx, `
			SELECT e.edge_kind, sf.path, e.line, src.qualified_name, src.stable_key,
			       dst.qualified_name, dst.stable_key, e.resolution_strategy, e.resolution_confidence
			FROM edges e
			JOIN symbols src ON src.repo_id = e.repo_id AND src.id = e.src_symbol_id
			JOIN files sf ON sf.repo_id = src.repo_id AND sf.id = src.file_id AND sf.is_deleted = 0
			JOIN symbols dst ON dst.repo_id = e.repo_id AND dst.id = e.dst_symbol_id
			WHERE e.repo_id = ? AND e.dst_symbol_id IN (`+placeholders(len(chunk))+`) AND `+exclude, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d EditSafetyDependent
			if err := rows.Scan(&d.EdgeKind, &d.SourcePath, &d.SourceLine, &d.SourceSymbol, &d.SourceStableKey,
				&d.TargetSymbol, &d.TargetStableKey, &d.ResolutionStrategy, &d.ResolutionConfidence); err != nil {
				rows.Close()
				return err
			}
			d.SourceIsTest = IsTestFilePath(d.SourcePath)
			t.Dependents = append(t.Dependents, d)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	sortEditSafetyDependents(t.Dependents)

	// Every spelling an unresolved edge could use for a target symbol. The
	// suffix test is substr(...) rather than LIKE so names holding % or _
	// need no escaping.
	seen := map[string]bool{}
	var exact, suffixes []string
	for _, sym := range t.Symbols {
		for _, n := range []string{sym.Name, sym.QualifiedName} {
			if n != "" && !seen[n] {
				seen[n] = true
				exact = append(exact, n)
			}
		}
		if sym.Name != "" && !seen["."+sym.Name] {
			seen["."+sym.Name] = true
			suffixes = append(suffixes, "."+sym.Name)
		}
	}
	type cond struct {
		sql  string
		args []any
	}
	conds := make([]cond, 0, len(exact)+len(suffixes))
	for _, n := range exact {
		conds = append(conds, cond{`e.dst_name = ?`, []any{n}})
	}
	for _, sfx := range suffixes {
		conds = append(conds, cond{`substr(e.dst_name, -?) = ?`, []any{len(sfx), sfx}})
	}
	const batch = sqliteInClauseBatchSize / 2
	for start := 0; start < len(conds); start += batch {
		part := conds[start:min(start+batch, len(conds))]
		where := make([]string, len(part))
		args := []any{repoID}
		for i, c := range part {
			where[i] = c.sql
			args = append(args, c.args...)
		}
		var n int
		// Only edges from live files count; for a file target, not edges
		// from the file itself.
		self := ``
		if sameFile {
			self = ` AND src.file_id <> ?`
			args = append(args, t.Symbols[0].FileID)
		}
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges e
			JOIN symbols src ON src.repo_id = e.repo_id AND src.id = e.src_symbol_id
			JOIN files sf ON sf.repo_id = src.repo_id AND sf.id = src.file_id AND sf.is_deleted = 0
			WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND (`+strings.Join(where, " OR ")+`)`+self, args...).Scan(&n); err != nil {
			return err
		}
		t.UnresolvedCandidates += n
	}
	return nil
}

func sortEditSafetyDependents(ds []EditSafetyDependent) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		switch {
		case a.SourcePath != b.SourcePath:
			return a.SourcePath < b.SourcePath
		case a.SourceLine != b.SourceLine:
			return a.SourceLine < b.SourceLine
		case a.SourceStableKey != b.SourceStableKey:
			return a.SourceStableKey < b.SourceStableKey
		case a.TargetStableKey != b.TargetStableKey:
			return a.TargetStableKey < b.TargetStableKey
		case a.EdgeKind != b.EdgeKind:
			return a.EdgeKind < b.EdgeKind
		default:
			return a.ResolutionStrategy < b.ResolutionStrategy
		}
	})
}
