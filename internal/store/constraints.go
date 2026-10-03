package store

import "context"

// The reads in this file serve `check_constraints`. Every statement is a plain
// SELECT scoped to one repository: the evaluator must never mutate, re-resolve
// or index, and it must never see another repository's rows.

// ConstraintFile is one live file as the constraint evaluator sees it.
type ConstraintFile struct {
	Path            string
	Language        string
	ParserProfile   string
	ParserCallEdges bool
}

// ConstraintEdge is one dependency row with both endpoints projected to their
// files. Resolved is false when dst_symbol_id is NULL; the target fields are
// then empty.
type ConstraintEdge struct {
	SourcePath      string
	Line            int64
	EdgeKind        string
	SourceSymbol    string
	SourceStartLine int64
	SourceStableKey string
	Resolved        bool
	TargetPath      string
	TargetSymbol    string
	TargetStartLine int64
	TargetStableKey string
	Strategy        string
	Confidence      string
	Evidence        string
}

// ConstraintIndexInfo is the freshness metadata a constraints result reports.
type ConstraintIndexInfo struct {
	LastIndexedAt string
	LastScanID    int64
	DirtyFiles    int64
}

// ConstraintIndexInfo reads the last scan id, the newest indexed_at and the
// watcher's dirty-file count for one repository.
func (s *Store) ConstraintIndexInfo(ctx context.Context, repoID int64) (ConstraintIndexInfo, error) {
	var info ConstraintIndexInfo
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COALESCE(MAX(id), 0) FROM scans WHERE repo_id = ?),
			(SELECT COUNT(1) FROM dirty_files WHERE repo_id = ?),
			COALESCE((SELECT MAX(indexed_at) FROM files WHERE repo_id = ? AND indexed_at <> ''), '')`,
		repoID, repoID, repoID).Scan(&info.LastScanID, &info.DirtyFiles, &info.LastIndexedAt)
	return info, err
}

// ConstraintFiles lists the repository's live files ordered by path.
func (s *Store) ConstraintFiles(ctx context.Context, repoID int64) ([]ConstraintFile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, language, parser_profile, parser_call_edges
		FROM files WHERE repo_id = ? AND is_deleted = 0
		ORDER BY path`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConstraintFile
	for rows.Next() {
		var f ConstraintFile
		var callEdges int64
		if err := rows.Scan(&f.Path, &f.Language, &f.ParserProfile, &callEdges); err != nil {
			return nil, err
		}
		f.ParserCallEdges = callEdges != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// ConstraintEdges lists every edge whose source symbol file and evidence file
// are live. A resolved edge is returned only when its target symbol exists in a
// live file of the same repository; a resolved edge into a deleted file or a
// missing symbol is dropped rather than reported as unresolved, because it is
// neither a trustworthy dependency nor a blind spot of the parser.
func (s *Store) ConstraintEdges(ctx context.Context, repoID int64) ([]ConstraintEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT srcf.path, e.line, e.edge_kind, src.qualified_name, src.start_line, src.stable_key,
		       e.dst_symbol_id IS NOT NULL,
		       COALESCE(dstf.path, ''), COALESCE(dst.qualified_name, ''), COALESCE(dst.start_line, 0), COALESCE(dst.stable_key, ''),
		       e.resolution_strategy, e.resolution_confidence, e.evidence
		FROM edges e
		JOIN symbols src ON src.id = e.src_symbol_id AND src.repo_id = e.repo_id
		JOIN files srcf ON srcf.id = src.file_id AND srcf.repo_id = e.repo_id AND srcf.is_deleted = 0
		JOIN files f ON f.id = e.file_id AND f.repo_id = e.repo_id AND f.is_deleted = 0
		LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id AND dst.repo_id = e.repo_id
		LEFT JOIN files dstf ON dstf.id = dst.file_id AND dstf.repo_id = e.repo_id AND dstf.is_deleted = 0
		WHERE e.repo_id = ? AND (e.dst_symbol_id IS NULL OR dstf.id IS NOT NULL)`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConstraintEdge
	for rows.Next() {
		var e ConstraintEdge
		if err := rows.Scan(&e.SourcePath, &e.Line, &e.EdgeKind, &e.SourceSymbol, &e.SourceStartLine, &e.SourceStableKey,
			&e.Resolved, &e.TargetPath, &e.TargetSymbol, &e.TargetStartLine, &e.TargetStableKey,
			&e.Strategy, &e.Confidence, &e.Evidence); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
