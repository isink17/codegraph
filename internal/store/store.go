package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/isink17/codegraph/internal/graph"
)

//go:embed schema/*.sql
var migrationFS embed.FS

type Store struct {
	db *sql.DB
}

type FileRecord struct {
	ID          int64
	Path        string
	Language    string
	SizeBytes   int64
	MtimeUnixNS int64
	ContentHash string
	IsDeleted   bool
}

type ScanSummary struct {
	RepoID       int64 `json:"repo_id"`
	ScanID       int64 `json:"scan_id"`
	FilesSeen    int   `json:"files_seen"`
	FilesIndexed int   `json:"files_indexed"`
	FilesSkipped int   `json:"files_skipped"`
	FilesChanged int   `json:"files_changed"`
	FilesDeleted int   `json:"files_deleted"`
	DurationMS   int64 `json:"duration_ms"`
}

type RelatedTest struct {
	File   string  `json:"file"`
	Symbol string  `json:"symbol"`
	Reason string  `json:"reason"`
	Score  float64 `json:"score"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000;`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.Migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Migrate() error {
	entries, err := fs.ReadDir(migrationFS, "schema")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		name := entry.Name()
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			continue
		}
		exists, err := hasMigration(s.db, version)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlBytes, err := migrationFS.ReadFile(filepath.ToSlash(filepath.Join("schema", name)))
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func hasMigration(db *sql.DB, version int) (bool, error) {
	var exists int
	err := db.QueryRow(`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, version).Scan(&exists)
	if err == nil {
		return exists > 0, nil
	}
	if strings.Contains(err.Error(), "no such table") {
		return false, nil
	}
	return false, err
}

func CanonicalRepoPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	eval, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(eval), nil
	}
	return filepath.Clean(abs), nil
}

func DBFileNameForRepo(repoRoot string) string {
	sum := sha256.Sum256([]byte(repoRoot))
	return hex.EncodeToString(sum[:8]) + ".sqlite"
}

func (s *Store) UpsertRepo(ctx context.Context, rootPath string) (graph.Repo, error) {
	canonical, err := CanonicalRepoPath(rootPath)
	if err != nil {
		return graph.Repo{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO repos(root_path, canonical_path, created_at, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(canonical_path) DO UPDATE SET root_path=excluded.root_path, updated_at=excluded.updated_at
	`, rootPath, canonical, now, now); err != nil {
		return graph.Repo{}, err
	}
	var repo graph.Repo
	if err := s.db.QueryRowContext(ctx, `SELECT id, root_path, canonical_path FROM repos WHERE canonical_path = ?`, canonical).Scan(&repo.ID, &repo.RootPath, &repo.CanonicalPath); err != nil {
		return graph.Repo{}, err
	}
	return repo, nil
}

func (s *Store) ExistingFiles(ctx context.Context, repoID int64) (map[string]FileRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, path, language, size_bytes, mtime_unix_ns, content_sha256, is_deleted
		FROM files
		WHERE repo_id = ?
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]FileRecord{}
	for rows.Next() {
		var rec FileRecord
		var isDeleted int
		if err := rows.Scan(&rec.ID, &rec.Path, &rec.Language, &rec.SizeBytes, &rec.MtimeUnixNS, &rec.ContentHash, &isDeleted); err != nil {
			return nil, err
		}
		rec.IsDeleted = isDeleted == 1
		out[rec.Path] = rec
	}
	return out, rows.Err()
}

func (s *Store) BeginScan(ctx context.Context, repoID int64, kind string) (int64, time.Time, error) {
	started := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO scans(repo_id, scan_kind, started_at, status)
		VALUES(?, ?, ?, 'running')
	`, repoID, kind, started.Format(time.RFC3339))
	if err != nil {
		return 0, time.Time{}, err
	}
	id, err := res.LastInsertId()
	return id, started, err
}

func (s *Store) CompleteScan(ctx context.Context, scanID int64, summary ScanSummary, started time.Time, status string, errText string) error {
	finished := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE scans
		SET finished_at = ?, status = ?, files_seen = ?, files_changed = ?, files_deleted = ?, error_text = ?
		WHERE id = ?
	`, finished.Format(time.RFC3339), status, summary.FilesSeen, summary.FilesChanged, summary.FilesDeleted, errText, scanID)
	return err
}

func (s *Store) TouchFileMetadata(ctx context.Context, repoID, scanID int64, path, language string, sizeBytes, mtimeUnixNS int64, contentHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files(repo_id, path, language, size_bytes, mtime_unix_ns, content_sha256, parse_state, last_scan_id, indexed_at, is_deleted)
		VALUES(?, ?, ?, ?, ?, ?, 'skipped', ?, ?, 0)
		ON CONFLICT(repo_id, path)
		DO UPDATE SET
			language = excluded.language,
			size_bytes = excluded.size_bytes,
			mtime_unix_ns = excluded.mtime_unix_ns,
			content_sha256 = excluded.content_sha256,
			parse_state = 'skipped',
			last_scan_id = excluded.last_scan_id,
			indexed_at = excluded.indexed_at,
			is_deleted = 0
	`, repoID, path, language, sizeBytes, mtimeUnixNS, contentHash, scanID, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) ReplaceFileGraph(ctx context.Context, repoID, scanID int64, path, language string, sizeBytes, mtimeUnixNS int64, contentHash string, parsed graph.ParsedFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO files(repo_id, path, language, size_bytes, mtime_unix_ns, content_sha256, parse_state, last_scan_id, indexed_at, is_deleted)
		VALUES(?, ?, ?, ?, ?, ?, 'indexed', ?, ?, 0)
		ON CONFLICT(repo_id, path)
		DO UPDATE SET
			language = excluded.language,
			size_bytes = excluded.size_bytes,
			mtime_unix_ns = excluded.mtime_unix_ns,
			content_sha256 = excluded.content_sha256,
			parse_state = 'indexed',
			last_scan_id = excluded.last_scan_id,
			indexed_at = excluded.indexed_at,
			is_deleted = 0
	`, repoID, path, language, sizeBytes, mtimeUnixNS, contentHash, scanID, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	var fileID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM files WHERE repo_id = ? AND path = ?`, repoID, path).Scan(&fileID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := deleteFileGraph(ctx, tx, fileID); err != nil {
		_ = tx.Rollback()
		return err
	}

	insertSymbolStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name, signature, visibility, start_line, start_col, end_line, end_col, doc_summary, stable_key)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer insertSymbolStmt.Close()

	insertSymbolFTSStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO symbol_fts(repo_id, symbol_id, name, qualified_name, signature, doc_summary)
		VALUES(?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer insertSymbolFTSStmt.Close()

	insertSymbolTokenStmt, err := tx.PrepareContext(ctx, `INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer insertSymbolTokenStmt.Close()

	insertReferenceStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO references_tbl(repo_id, file_id, symbol_id, ref_kind, name, qualified_name, start_line, start_col, end_line, end_col, context_symbol_id)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer insertReferenceStmt.Close()

	insertEdgeStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer insertEdgeStmt.Close()

	insertImportStmt, err := tx.PrepareContext(ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES(?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer insertImportStmt.Close()

	insertFileTokenStmt, err := tx.PrepareContext(ctx, `INSERT INTO file_tokens(file_id, token, weight) VALUES(?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer insertFileTokenStmt.Close()

	insertTestLinkStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer insertTestLinkStmt.Close()

	stableToID := map[string]int64{}
	for _, sym := range parsed.Symbols {
		res, err := insertSymbolStmt.ExecContext(ctx, repoID, fileID, sym.Language, sym.Kind, sym.Name, sym.QualifiedName, sym.ContainerName, sym.Signature, sym.Visibility, sym.Range.StartLine, sym.Range.StartCol, sym.Range.EndLine, sym.Range.EndCol, sym.DocSummary, sym.StableKey)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		symbolID, err := res.LastInsertId()
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		stableToID[sym.StableKey] = symbolID
		if _, err := insertSymbolFTSStmt.ExecContext(ctx, repoID, symbolID, sym.Name, sym.QualifiedName, sym.Signature, sym.DocSummary); err != nil {
			_ = tx.Rollback()
			return err
		}
		for token, weight := range tokenizeForSearch(sym.Name + " " + sym.QualifiedName + " " + sym.Signature + " " + sym.DocSummary) {
			if _, err := insertSymbolTokenStmt.ExecContext(ctx, symbolID, token, weight); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	contextSymbolID := firstFunctionID(stableToID, parsed.Symbols)
	for _, ref := range parsed.References {
		var symbolID any
		if ref.SymbolID != nil {
			symbolID = *ref.SymbolID
		}
		var contextID any
		if contextSymbolID != 0 {
			contextID = contextSymbolID
		}
		if _, err := insertReferenceStmt.ExecContext(ctx, repoID, fileID, symbolID, ref.Kind, ref.Name, ref.QualifiedName, ref.Range.StartLine, ref.Range.StartCol, ref.Range.EndLine, ref.Range.EndCol, contextID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for _, edge := range parsed.Edges {
		srcID := chooseSrcSymbolID(stableToID, parsed.Symbols, edge.Line)
		if srcID == 0 {
			continue
		}
		if _, err := insertEdgeStmt.ExecContext(ctx, repoID, srcID, edge.DstName, edge.Kind, edge.Evidence, fileID, edge.Line); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for _, imp := range parsed.Imports {
		if _, err := insertImportStmt.ExecContext(ctx, repoID, fileID, imp); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for token, weight := range parsed.FileTokens {
		if _, err := insertFileTokenStmt.ExecContext(ctx, fileID, token, weight); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for _, link := range parsed.TestLinks {
		var testSymbolID any
		var targetSymbolID any
		if id := stableToID[link.TestSymbolKey]; id != 0 {
			testSymbolID = id
		}
		if id, ok, err := s.resolveSymbolByStableKey(ctx, repoID, link.TargetStableKey); err == nil && ok {
			targetSymbolID = id
		}
		if _, err := insertTestLinkStmt.ExecContext(ctx, repoID, fileID, testSymbolID, targetSymbolID, link.Reason, link.Score); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func deleteFileGraph(ctx context.Context, tx *sql.Tx, fileID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM symbols WHERE file_id = ?`, fileID)
	if err != nil {
		return err
	}
	var symbolIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		symbolIDs = append(symbolIDs, id)
	}
	_ = rows.Close()

	deleteSymbolTokensStmt, err := tx.PrepareContext(ctx, `DELETE FROM symbol_tokens WHERE symbol_id = ?`)
	if err != nil {
		return err
	}
	defer deleteSymbolTokensStmt.Close()

	deleteSymbolFTSStmt, err := tx.PrepareContext(ctx, `DELETE FROM symbol_fts WHERE symbol_id = ?`)
	if err != nil {
		return err
	}
	defer deleteSymbolFTSStmt.Close()

	for _, symbolID := range symbolIDs {
		if _, err := deleteSymbolTokensStmt.ExecContext(ctx, symbolID); err != nil {
			return err
		}
		if _, err := deleteSymbolFTSStmt.ExecContext(ctx, symbolID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM references_tbl WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM file_imports WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM file_tokens WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM test_links WHERE test_file_id = ?`, fileID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM symbols WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	return nil
}

func firstFunctionID(stableToID map[string]int64, symbols []graph.Symbol) int64 {
	for _, sym := range symbols {
		if sym.Kind == "function" {
			return stableToID[sym.StableKey]
		}
	}
	return 0
}

func chooseSrcSymbolID(stableToID map[string]int64, symbols []graph.Symbol, line int) int64 {
	for _, sym := range symbols {
		if sym.Kind != "function" {
			continue
		}
		if line >= sym.Range.StartLine && line <= sym.Range.EndLine {
			return stableToID[sym.StableKey]
		}
	}
	return firstFunctionID(stableToID, symbols)
}

func (s *Store) MarkMissingDeleted(ctx context.Context, repoID, scanID int64, seen map[string]struct{}) (int, error) {
	records, err := s.ExistingFiles(ctx, repoID)
	if err != nil {
		return 0, err
	}
	count := 0
	for path, rec := range records {
		if _, ok := seen[path]; ok {
			continue
		}
		if rec.IsDeleted {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
			UPDATE files SET is_deleted = 1, parse_state = 'deleted', last_scan_id = ?
			WHERE id = ?
		`, scanID, rec.ID); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *Store) ResolveEdges(ctx context.Context, repoID int64) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, dst_name FROM edges WHERE repo_id = ? AND dst_symbol_id IS NULL`, repoID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type target struct {
		edgeID  int64
		dstName string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.edgeID, &t.dstName); err != nil {
			return err
		}
		targets = append(targets, t)
	}
	for _, t := range targets {
		id, ok, err := s.resolveTargetSymbol(ctx, repoID, t.dstName)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, id, t.edgeID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) resolveTargetSymbol(ctx context.Context, repoID int64, name string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM symbols WHERE repo_id = ? AND qualified_name = ? LIMIT 1`, repoID, name).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	parts := strings.Split(name, ".")
	short := parts[len(parts)-1]
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM symbols WHERE repo_id = ? AND name = ? LIMIT 2`, repoID, short)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var candidate int64
		if err := rows.Scan(&candidate); err != nil {
			return 0, false, err
		}
		ids = append(ids, candidate)
	}
	if len(ids) == 1 {
		return ids[0], true, nil
	}
	return 0, false, nil
}

func (s *Store) resolveSymbolByStableKey(ctx context.Context, repoID int64, stableKey string) (int64, bool, error) {
	if stableKey == "" {
		return 0, false, nil
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM symbols WHERE repo_id = ? AND stable_key = ? LIMIT 1`, repoID, stableKey).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return 0, false, err
}

func (s *Store) Stats(ctx context.Context, repoID int64) (graph.Stats, error) {
	var stats graph.Stats
	stats.RepoID = repoID
	if err := s.db.QueryRowContext(ctx, `SELECT root_path FROM repos WHERE id = ?`, repoID).Scan(&stats.RepoRoot); err != nil {
		return graph.Stats{}, err
	}
	queries := []struct {
		sql  string
		dest *int64
	}{
		{`SELECT COUNT(1) FROM files WHERE repo_id = ? AND is_deleted = 0`, &stats.Files},
		{`SELECT COUNT(1) FROM symbols WHERE repo_id = ?`, &stats.Symbols},
		{`SELECT COUNT(1) FROM references_tbl WHERE repo_id = ?`, &stats.References},
		{`SELECT COUNT(1) FROM edges WHERE repo_id = ?`, &stats.Edges},
		{`SELECT COUNT(1) FROM dirty_files WHERE repo_id = ?`, &stats.DirtyFiles},
	}
	for _, q := range queries {
		if err := s.db.QueryRowContext(ctx, q.sql, repoID).Scan(q.dest); err != nil {
			return graph.Stats{}, err
		}
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM scans WHERE repo_id = ?`, repoID).Scan(&stats.LastScanID); err != nil {
		return graph.Stats{}, err
	}
	var indexedAt sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT indexed_at FROM files WHERE repo_id = ? AND indexed_at <> '' ORDER BY indexed_at DESC LIMIT 1`, repoID).Scan(&indexedAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return graph.Stats{}, err
	}
	if indexedAt.Valid {
		stats.LastIndexedAt = indexedAt.String
	}
	stats.Languages = map[string]int{}
	rows, err := s.db.QueryContext(ctx, `SELECT language, COUNT(1) FROM files WHERE repo_id = ? AND is_deleted = 0 GROUP BY language`, repoID)
	if err != nil {
		return graph.Stats{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var lang string
		var count int
		if err := rows.Scan(&lang, &count); err != nil {
			return graph.Stats{}, err
		}
		stats.Languages[lang] = count
	}
	return stats, nil
}

func (s *Store) SearchSymbols(ctx context.Context, repoID int64, query string, limit int) ([]graph.Symbol, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM symbol_fts fts
		JOIN symbols s ON s.id = fts.symbol_id
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ? AND symbol_fts MATCH ?
		LIMIT ?
	`, repoID, quoteFTS(query), limit)
	if err != nil {
		rows, err = s.db.QueryContext(ctx, `
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND (s.name LIKE ? OR s.qualified_name LIKE ?)
			LIMIT ?
		`, repoID, "%"+query+"%", "%"+query+"%", limit)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()
	var out []graph.Symbol
	for rows.Next() {
		sym, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sym)
	}
	return out, rows.Err()
}

func (s *Store) FindSymbol(ctx context.Context, repoID int64, query string, limit int) ([]graph.Symbol, error) {
	return s.SearchSymbols(ctx, repoID, query, limit)
}

func (s *Store) FindCallers(ctx context.Context, repoID int64, symbol string, symbolID int64, limit int) ([]graph.Symbol, error) {
	targetID, err := s.lookupSymbolID(ctx, repoID, symbol, symbolID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM edges e
		JOIN symbols s ON s.id = e.src_symbol_id
		JOIN files f ON f.id = s.file_id
		WHERE e.repo_id = ? AND e.dst_symbol_id = ?
		LIMIT ?
	`, repoID, targetID, safeLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Symbol
	for rows.Next() {
		sym, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sym)
	}
	return out, rows.Err()
}

func (s *Store) FindCallees(ctx context.Context, repoID int64, symbol string, symbolID int64, limit int) ([]graph.Symbol, error) {
	srcID, err := s.lookupSymbolID(ctx, repoID, symbol, symbolID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM edges e
		JOIN symbols s ON s.id = e.dst_symbol_id
		JOIN files f ON f.id = s.file_id
		WHERE e.repo_id = ? AND e.src_symbol_id = ?
		LIMIT ?
	`, repoID, srcID, safeLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Symbol
	for rows.Next() {
		sym, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sym)
	}
	return out, rows.Err()
}

func (s *Store) ImpactRadius(ctx context.Context, repoID int64, symbols []string, files []string, depth int) (map[string]any, error) {
	affected := map[int64]graph.Symbol{}
	queue := []int64{}
	for _, name := range symbols {
		id, err := s.lookupSymbolID(ctx, repoID, name, 0)
		if err != nil {
			continue
		}
		queue = append(queue, id)
	}
	for _, file := range files {
		rows, err := s.db.QueryContext(ctx, `
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM symbols s JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND f.path = ?
		`, repoID, file)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			sym, err := scanSymbol(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			affected[sym.ID] = sym
			queue = append(queue, sym.ID)
		}
		_ = rows.Close()
	}
	if depth <= 0 {
		depth = 2
	}
	seen := map[int64]struct{}{}
	for level := 0; level < depth && len(queue) > 0; level++ {
		current := queue
		queue = nil
		for _, id := range current {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			for _, direction := range []string{"caller", "callee"} {
				query := `
					SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
					       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
					FROM edges e
					JOIN symbols s ON s.id = e.src_symbol_id
					JOIN files f ON f.id = s.file_id
					WHERE e.repo_id = ? AND e.dst_symbol_id = ?
				`
				arg := id
				if direction == "callee" {
					query = `
						SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
						       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
						FROM edges e
						JOIN symbols s ON s.id = e.dst_symbol_id
						JOIN files f ON f.id = s.file_id
						WHERE e.repo_id = ? AND e.src_symbol_id = ? AND e.dst_symbol_id IS NOT NULL
					`
				}
				rows, err := s.db.QueryContext(ctx, query, repoID, arg)
				if err != nil {
					return nil, err
				}
				for rows.Next() {
					sym, err := scanSymbol(rows)
					if err != nil {
						_ = rows.Close()
						return nil, err
					}
					affected[sym.ID] = sym
					queue = append(queue, sym.ID)
				}
				_ = rows.Close()
			}
		}
	}
	filesSet := map[string]struct{}{}
	var fileList []string
	var symbolList []graph.Symbol
	for _, sym := range affected {
		symbolList = append(symbolList, sym)
		if _, ok := filesSet[sym.FilePath]; !ok {
			filesSet[sym.FilePath] = struct{}{}
			fileList = append(fileList, sym.FilePath)
		}
	}
	return map[string]any{
		"symbols": symbolList,
		"files":   fileList,
		"summary": map[string]any{
			"affected_symbols": len(symbolList),
			"affected_files":   len(fileList),
		},
	}, nil
}

func (s *Store) RelatedTests(ctx context.Context, repoID int64, symbol, file string, limit int) ([]RelatedTest, error) {
	var rows *sql.Rows
	var err error
	if file != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT f.path, COALESCE(s.qualified_name, ''), t.reason, t.score
			FROM test_links t
			JOIN files f ON f.id = t.test_file_id
			LEFT JOIN symbols s ON s.id = t.test_symbol_id
			WHERE t.repo_id = ? AND t.test_file_id IN (
				SELECT id FROM files WHERE repo_id = ? AND path LIKE '%_test.go'
			)
			LIMIT ?
		`, repoID, repoID, safeLimit(limit))
	} else {
		targetID, err := s.lookupSymbolID(ctx, repoID, symbol, 0)
		if err != nil {
			return nil, err
		}
		rows, err = s.db.QueryContext(ctx, `
			SELECT f.path, COALESCE(s.qualified_name, ''), t.reason, t.score
			FROM test_links t
			JOIN files f ON f.id = t.test_file_id
			LEFT JOIN symbols s ON s.id = t.test_symbol_id
			WHERE t.repo_id = ? AND t.target_symbol_id = ?
			LIMIT ?
		`, repoID, targetID, safeLimit(limit))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelatedTest
	for rows.Next() {
		var item RelatedTest
		if err := rows.Scan(&item.File, &item.Symbol, &item.Reason, &item.Score); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) SemanticSearch(ctx context.Context, repoID int64, query string, limit int) ([]map[string]any, error) {
	tokens := tokenizeForSearch(query)
	if len(tokens) == 0 {
		return nil, nil
	}
	tokenList := make([]string, 0, len(tokens))
	for token := range tokens {
		tokenList = append(tokenList, token)
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(tokenList)), ",")
	sqlQuery := `
		SELECT f.path, COALESCE(s.qualified_name, ''), SUM(st.weight) AS score
		FROM symbol_tokens st
		JOIN symbols s ON s.id = st.symbol_id
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ? AND st.token IN (` + placeholders + `)
		GROUP BY f.path, s.qualified_name
		ORDER BY score DESC
		LIMIT ?
	`
	args := make([]any, 0, len(tokenList)+2)
	args = append(args, repoID)
	for _, token := range tokenList {
		args = append(args, token)
	}
	args = append(args, safeLimit(limit))
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type candidate struct {
		file   string
		symbol string
		score  float64
	}
	out := make([]map[string]any, 0, safeLimit(limit))
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.file, &item.symbol, &item.score); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"file":   item.file,
			"symbol": item.symbol,
			"score":  item.score,
			"why":    []string{"token_overlap"},
		})
	}
	return out, rows.Err()
}

func (s *Store) QueueDirtyFile(ctx context.Context, repoID int64, path, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dirty_files(repo_id, path, reason, queued_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(repo_id, path) DO UPDATE SET reason=excluded.reason, queued_at=excluded.queued_at
	`, repoID, path, reason, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) DrainDirtyFiles(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM dirty_files WHERE repo_id = ? ORDER BY queued_at`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dirty_files WHERE repo_id = ?`, repoID); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) lookupSymbolID(ctx context.Context, repoID int64, symbol string, symbolID int64) (int64, error) {
	if symbolID != 0 {
		return symbolID, nil
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM symbols WHERE repo_id = ? AND (qualified_name = ? OR name = ?) LIMIT 1
	`, repoID, symbol, symbol).Scan(&id)
	return id, err
}

func scanSymbol(scanner interface{ Scan(dest ...any) error }) (graph.Symbol, error) {
	var sym graph.Symbol
	if err := scanner.Scan(
		&sym.ID, &sym.FileID, &sym.Language, &sym.Kind, &sym.Name, &sym.QualifiedName, &sym.ContainerName, &sym.Signature, &sym.Visibility,
		&sym.Range.StartLine, &sym.Range.StartCol, &sym.Range.EndLine, &sym.Range.EndCol, &sym.DocSummary, &sym.StableKey, &sym.FilePath,
	); err != nil {
		return graph.Symbol{}, err
	}
	return sym, nil
}

func safeLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	return limit
}

func quoteFTS(query string) string {
	tokens := strings.Fields(query)
	for i, token := range tokens {
		tokens[i] = fmt.Sprintf(`"%s"*`, strings.ReplaceAll(token, `"`, ""))
	}
	return strings.Join(tokens, " ")
}

func tokenizeForSearch(text string) map[string]float64 {
	text = strings.ToLower(text)
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '/' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	out := map[string]float64{}
	for _, field := range fields {
		if len(field) < 2 {
			continue
		}
		out[field] += 1
	}
	return out
}
