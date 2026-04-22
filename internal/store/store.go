package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/texttoken"
)

//go:embed schema/*.sql
var migrationFS embed.FS

type Store struct {
	db *sql.DB
}

type OpenOptions struct {
	PerformanceProfile string
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

type FileMetadataUpdate struct {
	Path        string
	Language    string
	SizeBytes   int64
	MtimeUnixNS int64
	ContentHash string
}

type ScanSummary struct {
	RepoID           int64                     `json:"repo_id"`
	ScanID           int64                     `json:"scan_id"`
	FilesSeen        int                       `json:"files_seen"`
	FilesIndexed     int                       `json:"files_indexed"`
	FilesSkipped     int                       `json:"files_skipped"`
	FilesChanged     int                       `json:"files_changed"`
	FilesDeleted     int                       `json:"files_deleted"`
	FilesTotal       int                       `json:"files_total,omitempty"`
	FilesDeletedPct  float64                   `json:"files_deleted_pct,omitempty"`
	ParseErrors      int                       `json:"parse_errors,omitempty"`
	ParseSamples     []string                  `json:"parse_samples,omitempty"`
	LanguageCoverage map[string]LanguageCounts `json:"language_coverage,omitempty"`
	ExistingLoadMS   int64                     `json:"existing_load_ms,omitempty"`
	WalkMS           int64                     `json:"walk_ms,omitempty"`
	ProcessWallMS    int64                     `json:"process_wall_ms,omitempty"`
	ParseMS          int64                     `json:"parse_ms,omitempty"`
	ReadMS           int64                     `json:"read_ms,omitempty"`
	HashMS           int64                     `json:"hash_ms,omitempty"`
	AdapterParseMS   int64                     `json:"adapter_parse_ms,omitempty"`
	WriteMS          int64                     `json:"write_ms,omitempty"`
	WriteMetadataMS  int64                     `json:"write_metadata_ms,omitempty"`
	WriteReplaceMS   int64                     `json:"write_replace_ms,omitempty"`
	EmbedMS          int64                     `json:"embed_ms,omitempty"`
	MarkMissingMS    int64                     `json:"mark_missing_ms,omitempty"`
	ResolveMS        int64                     `json:"resolve_ms,omitempty"`
	DurationMS       int64                     `json:"duration_ms"`
}

type LanguageCounts struct {
	Seen        int `json:"seen"`
	Indexed     int `json:"indexed"`
	Skipped     int `json:"skipped"`
	ParseFailed int `json:"parse_failed"`
}

type ScanRecord struct {
	ID               int64                     `json:"id"`
	RepoID           int64                     `json:"repo_id"`
	ScanKind         string                    `json:"scan_kind"`
	StartedAt        string                    `json:"started_at"`
	FinishedAt       string                    `json:"finished_at,omitempty"`
	Status           string                    `json:"status"`
	FilesSeen        int64                     `json:"files_seen"`
	FilesChanged     int64                     `json:"files_changed"`
	FilesDeleted     int64                     `json:"files_deleted"`
	ErrorText        string                    `json:"error_text,omitempty"`
	LanguageCoverage map[string]LanguageCounts `json:"language_coverage,omitempty"`
}

type RelatedTest struct {
	File   string  `json:"file"`
	Symbol string  `json:"symbol"`
	Reason string  `json:"reason"`
	Score  float64 `json:"score"`
}

type ExportEdge struct {
	ID               int64  `json:"edge_id"`
	SrcSymbolID      int64  `json:"src_symbol_id"`
	SrcQualifiedName string `json:"src_qualified_name"`
	DstSymbolID      *int64 `json:"dst_symbol_id,omitempty"`
	DstQualifiedName string `json:"dst_qualified_name,omitempty"`
	DstName          string `json:"dst_name,omitempty"`
	Kind             string `json:"kind"`
	FilePath         string `json:"file,omitempty"`
	Line             int    `json:"line"`
}

type ReplaceFileGraphInput struct {
	Path        string
	Language    string
	SizeBytes   int64
	MtimeUnixNS int64
	ContentHash string
	Parsed      graph.ParsedFile
}

func Open(path string) (*Store, error) {
	return OpenWithOptions(path, OpenOptions{})
}

func OpenWithOptions(path string, opts OpenOptions) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := applyPragmas(db, opts.PerformanceProfile); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.Migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func applyPragmas(db *sql.DB, profile string) error {
	base := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA busy_timeout = 5000;`,
		`PRAGMA foreign_keys = ON;`,
	}
	perf := strings.ToLower(strings.TrimSpace(profile))
	switch perf {
	case "", "balanced":
		base = append(base,
			`PRAGMA synchronous = NORMAL;`,
			`PRAGMA temp_store = MEMORY;`,
		)
	case "durable":
		base = append(base, `PRAGMA synchronous = FULL;`)
	case "fast":
		base = append(base,
			`PRAGMA synchronous = OFF;`,
			`PRAGMA temp_store = MEMORY;`,
		)
	default:
		base = append(base,
			`PRAGMA synchronous = NORMAL;`,
			`PRAGMA temp_store = MEMORY;`,
		)
	}
	for _, pragma := range base {
		if _, err := db.Exec(pragma); err != nil {
			return err
		}
	}
	return nil
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

func (s *Store) PrimaryRepo(ctx context.Context) (graph.Repo, bool, error) {
	var repo graph.Repo
	err := s.db.QueryRowContext(ctx, `
		SELECT id, root_path, canonical_path
		FROM repos
		ORDER BY id ASC
		LIMIT 1
	`).Scan(&repo.ID, &repo.RootPath, &repo.CanonicalPath)
	if errors.Is(err, sql.ErrNoRows) {
		return graph.Repo{}, false, nil
	}
	if err != nil {
		return graph.Repo{}, false, err
	}
	return repo, true, nil
}

func (s *Store) ListRepos(ctx context.Context, limit, offset int) ([]graph.Repo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, root_path, canonical_path
		FROM repos
		ORDER BY id ASC
		LIMIT ?
		OFFSET ?
	`, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repos []graph.Repo
	for rows.Next() {
		var repo graph.Repo
		if err := rows.Scan(&repo.ID, &repo.RootPath, &repo.CanonicalPath); err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

func (s *Store) ListScans(ctx context.Context, repoID int64, limit, offset int) ([]ScanRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, repo_id, scan_kind, started_at, COALESCE(finished_at, ''), status, files_seen, files_changed, files_deleted, error_text
		FROM scans
		WHERE repo_id = ?
		ORDER BY id DESC
		LIMIT ?
		OFFSET ?
	`, repoID, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanScanRecords(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachScanLanguageCoverage(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) LatestScanErrors(ctx context.Context, repoID int64, limit, offset int) ([]ScanRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, repo_id, scan_kind, started_at, COALESCE(finished_at, ''), status, files_seen, files_changed, files_deleted, error_text
		FROM scans
		WHERE repo_id = ? AND status = 'failed' AND error_text <> ''
		ORDER BY id DESC
		LIMIT ?
		OFFSET ?
	`, repoID, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanScanRecords(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachScanLanguageCoverage(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
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
	return scanExistingFiles(rows)
}

func (s *Store) ExistingFilesForPaths(ctx context.Context, repoID int64, paths []string) (map[string]FileRecord, error) {
	out := make(map[string]FileRecord, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	const chunkSize = 400
	for start := 0; start < len(paths); start += chunkSize {
		end := min(start+chunkSize, len(paths))
		chunk := paths[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			SELECT id, path, language, size_bytes, mtime_unix_ns, content_sha256, is_deleted
			FROM files
			WHERE repo_id = ? AND path IN (` + placeholders + `)
		`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, path := range chunk {
			args = append(args, path)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		records, err := scanExistingFiles(rows)
		if err != nil {
			return nil, err
		}
		for k, v := range records {
			out[k] = v
		}
	}
	return out, nil
}

func scanExistingFiles(rows *sql.Rows) (map[string]FileRecord, error) {
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

func scanScanRecords(rows *sql.Rows) ([]ScanRecord, error) {
	var out []ScanRecord
	for rows.Next() {
		var rec ScanRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.RepoID,
			&rec.ScanKind,
			&rec.StartedAt,
			&rec.FinishedAt,
			&rec.Status,
			&rec.FilesSeen,
			&rec.FilesChanged,
			&rec.FilesDeleted,
			&rec.ErrorText,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) attachScanLanguageCoverage(ctx context.Context, scans []ScanRecord) error {
	if len(scans) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(scans))
	indexByID := make(map[int64]int, len(scans))
	for i, scan := range scans {
		ids = append(ids, scan.ID)
		indexByID[scan.ID] = i
	}
	for _, chunk := range chunkInt64s(ids, 250) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			SELECT scan_id, language, seen, indexed, skipped, parse_failed
			FROM scan_language_coverage
			WHERE scan_id IN (` + placeholders + `)
		`
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var scanID int64
			var lang string
			var cov LanguageCounts
			if err := rows.Scan(&scanID, &lang, &cov.Seen, &cov.Indexed, &cov.Skipped, &cov.ParseFailed); err != nil {
				_ = rows.Close()
				return err
			}
			idx, ok := indexByID[scanID]
			if !ok {
				continue
			}
			if scans[idx].LanguageCoverage == nil {
				scans[idx].LanguageCoverage = map[string]LanguageCounts{}
			}
			scans[idx].LanguageCoverage[lang] = cov
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE scans
		SET finished_at = ?, status = ?, files_seen = ?, files_changed = ?, files_deleted = ?, error_text = ?
		WHERE id = ?
	`, finished.Format(time.RFC3339), status, summary.FilesSeen, summary.FilesChanged, summary.FilesDeleted, errText, scanID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scan_language_coverage WHERE scan_id = ?`, scanID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if len(summary.LanguageCoverage) > 0 {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO scan_language_coverage(scan_id, language, seen, indexed, skipped, parse_failed)
			VALUES(?, ?, ?, ?, ?, ?)
		`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		defer stmt.Close()
		for lang, cov := range summary.LanguageCoverage {
			if _, err := stmt.ExecContext(ctx, scanID, lang, cov.Seen, cov.Indexed, cov.Skipped, cov.ParseFailed); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) MarkFilesSeenBatch(ctx context.Context, repoID, scanID int64, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
		UPDATE files
		SET last_scan_id = ?, is_deleted = 0
		WHERE repo_id = ? AND path = ?
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, path := range paths {
		if _, err := stmt.ExecContext(ctx, scanID, repoID, path); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) TouchFilesMetadataBatch(ctx context.Context, repoID, scanID int64, updates []FileMetadataUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
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
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	indexedAt := time.Now().UTC().Format(time.RFC3339)
	for _, update := range updates {
		if _, err := stmt.ExecContext(
			ctx,
			repoID,
			update.Path,
			update.Language,
			update.SizeBytes,
			update.MtimeUnixNS,
			update.ContentHash,
			scanID,
			indexedAt,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) MarkFilesParseFailedBatch(ctx context.Context, repoID, scanID int64, updates []FileMetadataUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO files(repo_id, path, language, size_bytes, mtime_unix_ns, content_sha256, parse_state, last_scan_id, indexed_at, is_deleted)
		VALUES(?, ?, ?, ?, ?, ?, 'failed', ?, ?, 0)
		ON CONFLICT(repo_id, path)
		DO UPDATE SET
			language = excluded.language,
			size_bytes = excluded.size_bytes,
			mtime_unix_ns = excluded.mtime_unix_ns,
			content_sha256 = excluded.content_sha256,
			parse_state = 'failed',
			last_scan_id = excluded.last_scan_id,
			indexed_at = excluded.indexed_at,
			is_deleted = 0
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	indexedAt := time.Now().UTC().Format(time.RFC3339)
	for _, update := range updates {
		if _, err := stmt.ExecContext(
			ctx,
			repoID,
			update.Path,
			update.Language,
			update.SizeBytes,
			update.MtimeUnixNS,
			update.ContentHash,
			scanID,
			indexedAt,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ReplaceFileGraph(ctx context.Context, repoID, scanID int64, path, language string, sizeBytes, mtimeUnixNS int64, contentHash string, parsed graph.ParsedFile) error {
	_, err := s.ReplaceFileGraphsBatch(ctx, repoID, scanID, []ReplaceFileGraphInput{{
		Path:        path,
		Language:    language,
		SizeBytes:   sizeBytes,
		MtimeUnixNS: mtimeUnixNS,
		ContentHash: contentHash,
		Parsed:      parsed,
	}})
	return err
}

func (s *Store) ReplaceFileGraphsBatch(ctx context.Context, repoID, scanID int64, inputs []ReplaceFileGraphInput) ([]int64, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}

	upsertFileStmt, err := tx.PrepareContext(ctx, `
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
		RETURNING id
	`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	defer upsertFileStmt.Close()

	insertSymbolStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name, signature, visibility, start_line, start_col, end_line, end_col, doc_summary, stable_key)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	defer insertSymbolStmt.Close()

	insertSymbolFTSStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO symbol_fts(repo_id, symbol_id, name, qualified_name, signature, doc_summary)
		VALUES(?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	defer insertSymbolFTSStmt.Close()

	insertSymbolTokenStmt, err := tx.PrepareContext(ctx, `INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	defer insertSymbolTokenStmt.Close()

	insertReferenceStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO references_tbl(repo_id, file_id, symbol_id, ref_kind, name, qualified_name, start_line, start_col, end_line, end_col, context_symbol_id)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	defer insertReferenceStmt.Close()

	insertEdgeStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	defer insertEdgeStmt.Close()

	insertImportStmt, err := tx.PrepareContext(ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES(?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	defer insertImportStmt.Close()

	insertFileTokenStmt, err := tx.PrepareContext(ctx, `INSERT INTO file_tokens(file_id, token, weight) VALUES(?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	defer insertFileTokenStmt.Close()

	insertTestLinkStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	defer insertTestLinkStmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	fileIDs := make([]int64, 0, len(inputs))
	for _, input := range inputs {
		var fileID int64
		if err := upsertFileStmt.QueryRowContext(ctx, repoID, input.Path, input.Language, input.SizeBytes, input.MtimeUnixNS, input.ContentHash, scanID, now).Scan(&fileID); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		fileIDs = append(fileIDs, fileID)
	}

	if err := deleteFileGraphsBatch(ctx, tx, fileIDs); err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	for idx, input := range inputs {
		fileID := fileIDs[idx]
		if err := insertParsedFileGraph(
			ctx,
			tx,
			repoID,
			fileID,
			input.Parsed,
			insertSymbolStmt,
			insertSymbolFTSStmt,
			insertSymbolTokenStmt,
			insertReferenceStmt,
			insertEdgeStmt,
			insertImportStmt,
			insertFileTokenStmt,
			insertTestLinkStmt,
		); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return fileIDs, nil
}

func deleteFileGraphsBatch(ctx context.Context, tx *sql.Tx, fileIDs []int64) error {
	if len(fileIDs) == 0 {
		return nil
	}

	const maxVars = 900

	execInChunks := func(sqlPrefix, sqlSuffix string, ids []int64) error {
		for start := 0; start < len(ids); start += maxVars {
			end := start + maxVars
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			placeholders := strings.Repeat("?,", len(chunk))
			placeholders = strings.TrimSuffix(placeholders, ",")
			query := sqlPrefix + placeholders + sqlSuffix
			args := make([]any, 0, len(chunk))
			for _, id := range chunk {
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
		}
		return nil
	}

	// Dependent tables that reference symbols must be deleted before deleting symbols.
	if err := execInChunks(
		`DELETE FROM symbol_tokens WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id IN (`,
		`))`,
		fileIDs,
	); err != nil {
		return err
	}
	if err := execInChunks(
		`DELETE FROM symbol_fts WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id IN (`,
		`))`,
		fileIDs,
	); err != nil {
		return err
	}

	if err := execInChunks(`DELETE FROM edges WHERE file_id IN (`, `)`, fileIDs); err != nil {
		return err
	}
	if err := execInChunks(`DELETE FROM references_tbl WHERE file_id IN (`, `)`, fileIDs); err != nil {
		return err
	}
	if err := execInChunks(`DELETE FROM file_imports WHERE file_id IN (`, `)`, fileIDs); err != nil {
		return err
	}
	if err := execInChunks(`DELETE FROM file_tokens WHERE file_id IN (`, `)`, fileIDs); err != nil {
		return err
	}
	if err := execInChunks(`DELETE FROM test_links WHERE test_file_id IN (`, `)`, fileIDs); err != nil {
		return err
	}
	if err := execInChunks(`DELETE FROM symbol_embeddings WHERE file_id IN (`, `)`, fileIDs); err != nil {
		return err
	}

	return execInChunks(`DELETE FROM symbols WHERE file_id IN (`, `)`, fileIDs)
}

func deleteFileGraph(ctx context.Context, tx *sql.Tx, fileID int64) error {
	deleteSymbolTokensStmt, err := tx.PrepareContext(ctx, `
		DELETE FROM symbol_tokens
		WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id = ?)
	`)
	if err != nil {
		return err
	}
	defer deleteSymbolTokensStmt.Close()

	deleteSymbolFTSStmt, err := tx.PrepareContext(ctx, `
		DELETE FROM symbol_fts
		WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id = ?)
	`)
	if err != nil {
		return err
	}
	defer deleteSymbolFTSStmt.Close()

	return deleteFileGraphWithStmts(ctx, tx, fileID, deleteSymbolTokensStmt, deleteSymbolFTSStmt)
}

func deleteFileGraphWithStmts(ctx context.Context, tx *sql.Tx, fileID int64, deleteSymbolTokensStmt, deleteSymbolFTSStmt *sql.Stmt) error {
	if _, err := deleteSymbolTokensStmt.ExecContext(ctx, fileID); err != nil {
		return err
	}
	if _, err := deleteSymbolFTSStmt.ExecContext(ctx, fileID); err != nil {
		return err
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM symbol_embeddings WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM symbols WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	return nil
}

func insertParsedFileGraph(
	ctx context.Context,
	tx *sql.Tx,
	repoID int64,
	fileID int64,
	parsed graph.ParsedFile,
	insertSymbolStmt *sql.Stmt,
	insertSymbolFTSStmt *sql.Stmt,
	insertSymbolTokenStmt *sql.Stmt,
	insertReferenceStmt *sql.Stmt,
	insertEdgeStmt *sql.Stmt,
	insertImportStmt *sql.Stmt,
	insertFileTokenStmt *sql.Stmt,
	insertTestLinkStmt *sql.Stmt,
) error {
	stableToID := map[string]int64{}
	for _, sym := range parsed.Symbols {
		res, err := insertSymbolStmt.ExecContext(ctx, repoID, fileID, sym.Language, sym.Kind, sym.Name, sym.QualifiedName, sym.ContainerName, sym.Signature, sym.Visibility, sym.Range.StartLine, sym.Range.StartCol, sym.Range.EndLine, sym.Range.EndCol, sym.DocSummary, sym.StableKey)
		if err != nil {
			return err
		}
		symbolID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		stableToID[sym.StableKey] = symbolID
		if _, err := insertSymbolFTSStmt.ExecContext(ctx, repoID, symbolID, sym.Name, sym.QualifiedName, sym.Signature, sym.DocSummary); err != nil {
			return err
		}
		for token, weight := range texttoken.WeightsString(sym.Name + " " + sym.QualifiedName + " " + sym.Signature + " " + sym.DocSummary) {
			if _, err := insertSymbolTokenStmt.ExecContext(ctx, symbolID, token, weight); err != nil {
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
			return err
		}
	}
	for _, edge := range parsed.Edges {
		srcID := chooseSrcSymbolID(stableToID, parsed.Symbols, edge.Line)
		if srcID == 0 {
			continue
		}
		if _, err := insertEdgeStmt.ExecContext(ctx, repoID, srcID, edge.DstName, edge.Kind, edge.Evidence, fileID, edge.Line); err != nil {
			return err
		}
	}
	for _, imp := range parsed.Imports {
		if _, err := insertImportStmt.ExecContext(ctx, repoID, fileID, imp); err != nil {
			return err
		}
	}
	for token, weight := range parsed.FileTokens {
		if _, err := insertFileTokenStmt.ExecContext(ctx, fileID, token, weight); err != nil {
			return err
		}
	}

	targetKeySet := map[string]struct{}{}
	for _, link := range parsed.TestLinks {
		if link.TargetStableKey != "" {
			targetKeySet[link.TargetStableKey] = struct{}{}
		}
	}
	targetKeys := make([]string, 0, len(targetKeySet))
	for key := range targetKeySet {
		targetKeys = append(targetKeys, key)
	}
	targetStableToID, err := resolveSymbolsByStableKeysQuery(ctx, tx, repoID, targetKeys)
	if err != nil {
		return err
	}
	for _, link := range parsed.TestLinks {
		var testSymbolID any
		var targetSymbolID any
		if id := stableToID[link.TestSymbolKey]; id != 0 {
			testSymbolID = id
		}
		if id, ok := targetStableToID[link.TargetStableKey]; ok {
			targetSymbolID = id
		}
		if _, err := insertTestLinkStmt.ExecContext(ctx, repoID, fileID, testSymbolID, targetSymbolID, link.Reason, link.Score); err != nil {
			return err
		}
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

func (s *Store) MarkMissingDeleted(ctx context.Context, repoID, scanID int64) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE files
		SET is_deleted = 1, parse_state = 'deleted', last_scan_id = ?
		WHERE repo_id = ? AND is_deleted = 0 AND last_scan_id <> ?
	`, scanID, repoID, scanID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

type edgeTarget struct {
	edgeID  int64
	dstName string
}

func (s *Store) ResolveEdges(ctx context.Context, repoID int64) (int, error) {
	totalResolved := 0

	// Strategy 1: Exact qualified name match
	res, err := s.db.ExecContext(ctx, `
		UPDATE edges SET dst_symbol_id = (
			SELECT s.id FROM symbols s
			WHERE s.repo_id = ? AND s.qualified_name = edges.dst_name
			LIMIT 1
		)
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL
		AND EXISTS (SELECT 1 FROM symbols s WHERE s.repo_id = ? AND s.qualified_name = edges.dst_name)
	`, repoID, repoID, repoID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalResolved += int(n)
	}

	// Strategy 2: Name match (unqualified)
	res, err = s.db.ExecContext(ctx, `
		UPDATE edges SET dst_symbol_id = (
			SELECT s.id FROM symbols s
			WHERE s.repo_id = ? AND s.name = edges.dst_name
			AND s.kind IN ('function', 'method', 'class', 'type', 'struct', 'interface')
			LIMIT 1
		)
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL
		AND EXISTS (SELECT 1 FROM symbols s WHERE s.repo_id = ? AND s.name = edges.dst_name AND s.kind IN ('function', 'method', 'class', 'type', 'struct', 'interface'))
	`, repoID, repoID, repoID)
	if err != nil {
		return totalResolved, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalResolved += int(n)
	}

	// Strategy 3: Suffix match (e.g., pkg.Func matches github.com/org/repo/pkg.Func)
	res, err = s.db.ExecContext(ctx, `
		UPDATE edges SET dst_symbol_id = (
			SELECT s.id FROM symbols s
			WHERE s.repo_id = ?
			AND (s.qualified_name LIKE '%.' || edges.dst_name OR s.qualified_name LIKE '%/' || edges.dst_name)
			LIMIT 1
		)
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL
		AND EXISTS (
			SELECT 1 FROM symbols s WHERE s.repo_id = ?
			AND (s.qualified_name LIKE '%.' || edges.dst_name OR s.qualified_name LIKE '%/' || edges.dst_name)
		)
	`, repoID, repoID, repoID)
	if err != nil {
		return totalResolved, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalResolved += int(n)
	}

	// Strategy 4: Method receiver match (e.g., DoSomething matches MyStruct.DoSomething)
	res, err = s.db.ExecContext(ctx, `
		UPDATE edges SET dst_symbol_id = (
			SELECT s.id FROM symbols s
			WHERE s.repo_id = ?
			AND s.name = edges.dst_name
			AND s.container_name != ''
			LIMIT 1
		)
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL
		AND EXISTS (
			SELECT 1 FROM symbols s WHERE s.repo_id = ?
			AND s.name = edges.dst_name AND s.container_name != ''
		)
	`, repoID, repoID, repoID)
	if err != nil {
		return totalResolved, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalResolved += int(n)
	}

	return totalResolved, nil
}

func (s *Store) ResolveEdgesForPaths(ctx context.Context, repoID int64, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	uniquePaths := make([]string, 0, len(paths))
	seenPaths := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seenPaths[path]; ok {
			continue
		}
		seenPaths[path] = struct{}{}
		uniquePaths = append(uniquePaths, path)
	}

	const chunkSize = 400
	fileIDs := make([]int64, 0, len(uniquePaths))
	for start := 0; start < len(uniquePaths); start += chunkSize {
		end := min(start+chunkSize, len(uniquePaths))
		chunk := uniquePaths[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `SELECT id FROM files WHERE repo_id = ? AND path IN (` + placeholders + `)`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, path := range chunk {
			args = append(args, path)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			fileIDs = append(fileIDs, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}

	targets := make([]edgeTarget, 0, len(fileIDs))
	for start := 0; start < len(fileIDs); start += chunkSize {
		end := min(start+chunkSize, len(fileIDs))
		chunk := fileIDs[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `SELECT id, dst_name FROM edges WHERE repo_id = ? AND dst_symbol_id IS NULL AND file_id IN (` + placeholders + `)`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		chunkTargets, err := scanEdgeTargets(rows)
		if err != nil {
			return err
		}
		targets = append(targets, chunkTargets...)
	}
	return s.resolveEdgeTargets(ctx, repoID, targets)
}

func scanEdgeTargets(rows *sql.Rows) ([]edgeTarget, error) {
	defer rows.Close()
	var targets []edgeTarget
	for rows.Next() {
		var target edgeTarget
		if err := rows.Scan(&target.edgeID, &target.dstName); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return targets, nil
}

func (s *Store) resolveEdgeTargets(ctx context.Context, repoID int64, targets []edgeTarget) error {
	if len(targets) == 0 {
		return nil
	}
	qualifiedSet := map[string]struct{}{}
	for _, target := range targets {
		if target.dstName != "" {
			qualifiedSet[target.dstName] = struct{}{}
		}
	}
	qualifiedNames := make([]string, 0, len(qualifiedSet))
	for name := range qualifiedSet {
		qualifiedNames = append(qualifiedNames, name)
	}
	byQualified, err := s.resolveSymbolsByQualifiedNames(ctx, repoID, qualifiedNames)
	if err != nil {
		return err
	}

	shortSet := map[string]struct{}{}
	for _, target := range targets {
		if _, ok := byQualified[target.dstName]; ok {
			continue
		}
		parts := strings.Split(target.dstName, ".")
		short := strings.TrimSpace(parts[len(parts)-1])
		if short != "" {
			shortSet[short] = struct{}{}
		}
	}
	shortNames := make([]string, 0, len(shortSet))
	for name := range shortSet {
		shortNames = append(shortNames, name)
	}
	byShort, err := s.resolveUniqueSymbolsByNames(ctx, repoID, shortNames)
	if err != nil {
		return err
	}

	type edgeResolution struct {
		edgeID int64
		dstID  int64
	}
	resolutions := make([]edgeResolution, 0, len(targets))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, target := range targets {
		dstID, ok := byQualified[target.dstName]
		if !ok {
			parts := strings.Split(target.dstName, ".")
			short := strings.TrimSpace(parts[len(parts)-1])
			dstID, ok = byShort[short]
		}
		if !ok || dstID == 0 {
			continue
		}
		resolutions = append(resolutions, edgeResolution{edgeID: target.edgeID, dstID: dstID})
	}

	if len(resolutions) == 0 {
		_ = tx.Rollback()
		return nil
	}

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_edge_resolution(edge_id INTEGER PRIMARY KEY, dst_symbol_id INTEGER NOT NULL)`); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_edge_resolution`); err != nil {
		_ = tx.Rollback()
		return err
	}

	// Keep well under SQLite's default variable limit (999).
	const maxPairsPerInsert = 400
	for start := 0; start < len(resolutions); start += maxPairsPerInsert {
		end := min(start+maxPairsPerInsert, len(resolutions))
		chunk := resolutions[start:end]
		var b strings.Builder
		b.WriteString(`INSERT INTO tmp_edge_resolution(edge_id, dst_symbol_id) VALUES `)
		args := make([]any, 0, len(chunk)*2)
		for i, r := range chunk {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("(?,?)")
			args = append(args, r.edgeID, r.dstID)
		}
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE edges
		SET dst_symbol_id = (SELECT t.dst_symbol_id FROM tmp_edge_resolution t WHERE t.edge_id = edges.id)
		WHERE edges.id IN (SELECT edge_id FROM tmp_edge_resolution)
	`); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE tmp_edge_resolution`); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) resolveSymbolsByQualifiedNames(ctx context.Context, repoID int64, qualifiedNames []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(qualifiedNames) == 0 {
		return out, nil
	}
	const chunkSize = 400
	for start := 0; start < len(qualifiedNames); start += chunkSize {
		end := min(start+chunkSize, len(qualifiedNames))
		chunk := qualifiedNames[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			SELECT qualified_name, id
			FROM symbols
			WHERE repo_id = ? AND qualified_name IN (` + placeholders + `)
		`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var qualified string
			var id int64
			if err := rows.Scan(&qualified, &id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if _, exists := out[qualified]; !exists {
				out[qualified] = id
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) resolveUniqueSymbolsByNames(ctx context.Context, repoID int64, names []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(names) == 0 {
		return out, nil
	}
	const chunkSize = 400
	for start := 0; start < len(names); start += chunkSize {
		end := min(start+chunkSize, len(names))
		chunk := names[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			SELECT name, id
			FROM symbols
			WHERE repo_id = ? AND name IN (` + placeholders + `)
			ORDER BY id ASC
		`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		seenCount := map[string]int{}
		firstID := map[string]int64{}
		for rows.Next() {
			var name string
			var id int64
			if err := rows.Scan(&name, &id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			seenCount[name]++
			if seenCount[name] == 1 {
				firstID[name] = id
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		for name, count := range seenCount {
			if count == 1 {
				out[name] = firstID[name]
			}
		}
	}
	return out, nil
}

func (s *Store) resolveSymbolsByStableKeys(ctx context.Context, repoID int64, stableKeys []string) (map[string]int64, error) {
	return resolveSymbolsByStableKeysQuery(ctx, s.db, repoID, stableKeys)
}

type queryContexter interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func resolveSymbolsByStableKeysQuery(ctx context.Context, q queryContexter, repoID int64, stableKeys []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(stableKeys) == 0 {
		return out, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(stableKeys)), ",")
	query := `
		SELECT stable_key, id
		FROM symbols
		WHERE repo_id = ? AND stable_key IN (` + placeholders + `)
	`
	args := make([]any, 0, len(stableKeys)+1)
	args = append(args, repoID)
	for _, key := range stableKeys {
		args = append(args, key)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var id int64
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		out[key] = id
	}
	return out, rows.Err()
}

func (s *Store) Stats(ctx context.Context, repoID int64) (graph.Stats, error) {
	var stats graph.Stats
	stats.RepoID = repoID
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			r.root_path,
			(SELECT COUNT(1) FROM files f WHERE f.repo_id = r.id AND f.is_deleted = 0) AS files_count,
			(SELECT COUNT(1) FROM symbols s WHERE s.repo_id = r.id) AS symbols_count,
			(SELECT COUNT(1) FROM references_tbl rt WHERE rt.repo_id = r.id) AS refs_count,
			(SELECT COUNT(1) FROM edges e WHERE e.repo_id = r.id) AS edges_count,
			(SELECT COUNT(1) FROM dirty_files d WHERE d.repo_id = r.id) AS dirty_count,
			(SELECT COALESCE(MAX(sc.id), 0) FROM scans sc WHERE sc.repo_id = r.id) AS last_scan_id
		FROM repos r
		WHERE r.id = ?
	`, repoID).Scan(
		&stats.RepoRoot,
		&stats.Files,
		&stats.Symbols,
		&stats.References,
		&stats.Edges,
		&stats.DirtyFiles,
		&stats.LastScanID,
	); err != nil {
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

func (s *Store) SearchSymbols(ctx context.Context, repoID int64, query string, limit, offset int) ([]graph.Symbol, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM symbol_fts fts
		JOIN symbols s ON s.id = fts.symbol_id
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ? AND symbol_fts MATCH ?
		LIMIT ?
		OFFSET ?
	`, repoID, quoteFTS(query), safeLimit(limit), safeOffset(offset))
	if err != nil {
		rows, err = s.db.QueryContext(ctx, `
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND (s.name LIKE ? OR s.qualified_name LIKE ?)
			LIMIT ?
			OFFSET ?
		`, repoID, "%"+query+"%", "%"+query+"%", safeLimit(limit), safeOffset(offset))
		if err != nil {
			return nil, err
		}
	}
	return scanSymbols(rows)
}

func (s *Store) FindSymbol(ctx context.Context, repoID int64, query string, limit, offset int) ([]graph.Symbol, error) {
	return s.SearchSymbols(ctx, repoID, query, limit, offset)
}

func (s *Store) FindSymbolExact(ctx context.Context, repoID int64, query string, limit, offset int) ([]graph.Symbol, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ? AND (s.name = ? OR s.qualified_name = ?)
		LIMIT ?
		OFFSET ?
	`, repoID, query, query, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) FindCallers(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
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
		OFFSET ?
	`, repoID, targetID, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) FindCallees(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
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
		OFFSET ?
	`, repoID, srcID, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) ImpactRadius(ctx context.Context, repoID int64, symbols []string, files []string, depth int) (map[string]any, error) {
	affected := make(map[int64]graph.Symbol, len(symbols))
	queue := make([]int64, 0, len(symbols))
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
		currentSet := map[int64]struct{}{}
		current := make([]int64, 0, len(queue))
		for _, id := range queue {
			if _, ok := seen[id]; ok {
				continue
			}
			if _, ok := currentSet[id]; ok {
				continue
			}
			currentSet[id] = struct{}{}
			current = append(current, id)
		}
		queue = nil
		if len(current) == 0 {
			continue
		}
		for _, id := range current {
			seen[id] = struct{}{}
		}
		callers, err := s.impactNeighbors(ctx, repoID, current, true)
		if err != nil {
			return nil, err
		}
		for _, sym := range callers {
			affected[sym.ID] = sym
			if _, ok := seen[sym.ID]; !ok {
				queue = append(queue, sym.ID)
			}
		}
		callees, err := s.impactNeighbors(ctx, repoID, current, false)
		if err != nil {
			return nil, err
		}
		for _, sym := range callees {
			affected[sym.ID] = sym
			if _, ok := seen[sym.ID]; !ok {
				queue = append(queue, sym.ID)
			}
		}
	}
	filesSet := make(map[string]struct{}, len(affected))
	fileList := make([]string, 0, len(affected))
	symbolList := make([]graph.Symbol, 0, len(affected))
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

func (s *Store) impactNeighbors(ctx context.Context, repoID int64, frontier []int64, callers bool) ([]graph.Symbol, error) {
	if len(frontier) == 0 {
		return nil, nil
	}
	const chunkSize = 250
	var out []graph.Symbol
	for start := 0; start < len(frontier); start += chunkSize {
		end := min(start+chunkSize, len(frontier))
		chunk := frontier[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM edges e
			JOIN symbols s ON s.id = e.src_symbol_id
			JOIN files f ON f.id = s.file_id
			WHERE e.repo_id = ? AND e.dst_symbol_id IN (` + placeholders + `)
		`
		if !callers {
			query = `
				SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
				       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
				FROM edges e
				JOIN symbols s ON s.id = e.dst_symbol_id
				JOIN files f ON f.id = s.file_id
				WHERE e.repo_id = ? AND e.src_symbol_id IN (` + placeholders + `) AND e.dst_symbol_id IS NOT NULL
			`
		}
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		items, err := scanSymbols(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

func (s *Store) RelatedTests(ctx context.Context, repoID int64, symbol, file string, limit, offset int) ([]RelatedTest, error) {
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
			OFFSET ?
		`, repoID, repoID, safeLimit(limit), safeOffset(offset))
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
			OFFSET ?
		`, repoID, targetID, safeLimit(limit), safeOffset(offset))
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

func (s *Store) SemanticSearch(ctx context.Context, repoID int64, query string, limit, offset int) ([]map[string]any, error) {
	tokens := texttoken.WeightsString(query)
	if len(tokens) == 0 {
		return nil, nil
	}
	limitVal := safeLimit(limit)
	offsetVal := safeOffset(offset)
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
		OFFSET ?
	`
	args := make([]any, 0, len(tokenList)+2)
	args = append(args, repoID)
	for _, token := range tokenList {
		args = append(args, token)
	}
	args = append(args, limitVal)
	args = append(args, offsetVal)
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
	out := make([]map[string]any, 0, limitVal)
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

func (s *Store) GraphSnapshot(ctx context.Context, repoID int64, focusSymbol string, depth int) ([]graph.Symbol, []ExportEdge, error) {
	if strings.TrimSpace(focusSymbol) == "" {
		symbols, err := s.loadSymbolsForExport(ctx, repoID, nil)
		if err != nil {
			return nil, nil, err
		}
		edges, err := s.loadEdgesForExport(ctx, repoID, nil)
		if err != nil {
			return nil, nil, err
		}
		return symbols, edges, nil
	}

	impact, err := s.ImpactRadius(ctx, repoID, []string{focusSymbol}, nil, depth)
	if err != nil {
		return nil, nil, err
	}
	impactSymbols, _ := impact["symbols"].([]graph.Symbol)
	if len(impactSymbols) == 0 {
		return nil, nil, nil
	}
	idSet := map[int64]struct{}{}
	ids := make([]int64, 0, len(impactSymbols))
	for _, sym := range impactSymbols {
		if _, ok := idSet[sym.ID]; ok {
			continue
		}
		idSet[sym.ID] = struct{}{}
		ids = append(ids, sym.ID)
	}
	edges, err := s.loadEdgesForExport(ctx, repoID, ids)
	if err != nil {
		return nil, nil, err
	}
	return impactSymbols, edges, nil
}

func (s *Store) ExportSymbolsPage(ctx context.Context, repoID int64, limit, offset int) ([]graph.Symbol, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ?
		ORDER BY s.id ASC
		LIMIT ?
		OFFSET ?
	`, repoID, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) ExportEdgesPage(ctx context.Context, repoID int64, limit, offset int) ([]ExportEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.src_symbol_id, COALESCE(src.qualified_name, ''), e.dst_symbol_id, COALESCE(dst.qualified_name, ''), e.dst_name, e.edge_kind, COALESCE(f.path, ''), e.line
		FROM edges e
		LEFT JOIN symbols src ON src.id = e.src_symbol_id
		LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id
		LEFT JOIN files f ON f.id = e.file_id
		WHERE e.repo_id = ?
		ORDER BY e.id ASC
		LIMIT ?
		OFFSET ?
	`, repoID, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanExportEdges(rows)
}

func (s *Store) loadSymbolsForExport(ctx context.Context, repoID int64, symbolIDs []int64) ([]graph.Symbol, error) {
	if len(symbolIDs) == 0 {
		rows, err := s.db.QueryContext(ctx, `
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ?
		`, repoID)
		if err != nil {
			return nil, err
		}
		return scanSymbols(rows)
	}
	out := make([]graph.Symbol, 0, len(symbolIDs))
	for _, chunk := range chunkInt64s(symbolIDs, 250) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND s.id IN (` + placeholders + `)
		`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		items, err := scanSymbols(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

func (s *Store) loadEdgesForExport(ctx context.Context, repoID int64, symbolIDs []int64) ([]ExportEdge, error) {
	if len(symbolIDs) == 0 {
		rows, err := s.db.QueryContext(ctx, `
			SELECT e.id, e.src_symbol_id, COALESCE(src.qualified_name, ''), e.dst_symbol_id, COALESCE(dst.qualified_name, ''), e.dst_name, e.edge_kind, COALESCE(f.path, ''), e.line
			FROM edges e
			LEFT JOIN symbols src ON src.id = e.src_symbol_id
			LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id
			LEFT JOIN files f ON f.id = e.file_id
			WHERE e.repo_id = ?
		`, repoID)
		if err != nil {
			return nil, err
		}
		return scanExportEdges(rows)
	}
	var out []ExportEdge
	for _, chunk := range chunkInt64s(symbolIDs, 250) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			SELECT e.id, e.src_symbol_id, COALESCE(src.qualified_name, ''), e.dst_symbol_id, COALESCE(dst.qualified_name, ''), e.dst_name, e.edge_kind, COALESCE(f.path, ''), e.line
			FROM edges e
			LEFT JOIN symbols src ON src.id = e.src_symbol_id
			LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id
			LEFT JOIN files f ON f.id = e.file_id
			WHERE e.repo_id = ? AND (e.src_symbol_id IN (` + placeholders + `) OR e.dst_symbol_id IN (` + placeholders + `))
		`
		args := make([]any, 0, (len(chunk)*2)+1)
		args = append(args, repoID)
		for _, id := range chunk {
			args = append(args, id)
		}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		items, err := scanExportEdges(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	byID := map[int64]ExportEdge{}
	for _, edge := range out {
		if _, ok := byID[edge.ID]; ok {
			continue
		}
		byID[edge.ID] = edge
	}
	unique := make([]ExportEdge, 0, len(byID))
	for _, edge := range byID {
		unique = append(unique, edge)
	}
	return unique, nil
}

func chunkInt64s(values []int64, chunkSize int) [][]int64 {
	if len(values) == 0 || chunkSize <= 0 {
		return nil
	}
	out := make([][]int64, 0, (len(values)+chunkSize-1)/chunkSize)
	for start := 0; start < len(values); start += chunkSize {
		end := min(start+chunkSize, len(values))
		out = append(out, values[start:end])
	}
	return out
}

func scanExportEdges(rows *sql.Rows) ([]ExportEdge, error) {
	defer rows.Close()
	var out []ExportEdge
	for rows.Next() {
		var edge ExportEdge
		var dstID sql.NullInt64
		if err := rows.Scan(
			&edge.ID,
			&edge.SrcSymbolID,
			&edge.SrcQualifiedName,
			&dstID,
			&edge.DstQualifiedName,
			&edge.DstName,
			&edge.Kind,
			&edge.FilePath,
			&edge.Line,
		); err != nil {
			return nil, err
		}
		if dstID.Valid {
			value := dstID.Int64
			edge.DstSymbolID = &value
		}
		out = append(out, edge)
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

// TraceDependencies performs a BFS traversal of the dependency graph starting
// from the given symbol, returning the full chain up to maxDepth levels.
func (s *Store) TraceDependencies(ctx context.Context, repoID int64, symbol string, direction string, maxDepth int) ([]map[string]any, error) {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	maxDepth = min(maxDepth, 10)
	if direction == "" {
		direction = "downstream"
	}

	// Find starting symbols by name match.
	pattern := "%" + symbol + "%"
	seedRows, err := s.db.QueryContext(ctx,
		`SELECT id, qualified_name, kind, name FROM symbols WHERE repo_id = ? AND (qualified_name LIKE ? OR name = ?)`,
		repoID, pattern, symbol)
	if err != nil {
		return nil, fmt.Errorf("trace_dependencies seed query: %w", err)
	}
	type symInfo struct {
		id            int64
		qualifiedName string
		kind          string
		name          string
	}
	var seeds []symInfo
	for seedRows.Next() {
		var si symInfo
		if err := seedRows.Scan(&si.id, &si.qualifiedName, &si.kind, &si.name); err != nil {
			seedRows.Close()
			return nil, err
		}
		seeds = append(seeds, si)
	}
	seedRows.Close()
	if err := seedRows.Err(); err != nil {
		return nil, err
	}

	type bfsEntry struct {
		id            int64
		qualifiedName string
		kind          string
		name          string
		file          string
		depth         int
		dir           string
	}

	visited := map[int64]bool{}
	var results []bfsEntry

	bfs := func(startSeeds []symInfo, dir string) error {
		queue := make([]bfsEntry, 0, len(startSeeds))
		for _, seed := range startSeeds {
			if visited[seed.id] {
				continue
			}
			visited[seed.id] = true
			queue = append(queue, bfsEntry{
				id: seed.id, qualifiedName: seed.qualifiedName,
				kind: seed.kind, name: seed.name, depth: 0, dir: dir,
			})
			results = append(results, bfsEntry{
				id: seed.id, qualifiedName: seed.qualifiedName,
				kind: seed.kind, name: seed.name, depth: 0, dir: dir,
			})
		}

		var query string
		if dir == "downstream" {
			query = `SELECT DISTINCT s.id, s.qualified_name, s.kind, s.name, f.path
				FROM edges e JOIN symbols s ON s.id = e.dst_symbol_id JOIN files f ON f.id = s.file_id
				WHERE e.src_symbol_id = ? AND e.dst_symbol_id IS NOT NULL`
		} else {
			query = `SELECT DISTINCT s.id, s.qualified_name, s.kind, s.name, f.path
				FROM edges e JOIN symbols s ON s.id = e.src_symbol_id JOIN files f ON f.id = s.file_id
				WHERE e.dst_symbol_id = ?`
		}

		for i := 0; i < len(queue); i++ {
			entry := queue[i]
			if entry.depth >= maxDepth {
				continue
			}
			rows, err := s.db.QueryContext(ctx, query, entry.id)
			if err != nil {
				return fmt.Errorf("trace_dependencies bfs query: %w", err)
			}
			for rows.Next() {
				var si bfsEntry
				if err := rows.Scan(&si.id, &si.qualifiedName, &si.kind, &si.name, &si.file); err != nil {
					rows.Close()
					return err
				}
				if visited[si.id] {
					continue
				}
				visited[si.id] = true
				si.depth = entry.depth + 1
				si.dir = dir
				queue = append(queue, si)
				results = append(results, si)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		return nil
	}

	if direction == "downstream" || direction == "both" {
		if err := bfs(seeds, "downstream"); err != nil {
			return nil, err
		}
	}
	if direction == "upstream" || direction == "both" {
		// Reset visited for upstream pass when doing both, but keep seed visited
		if direction == "both" {
			visited = map[int64]bool{}
			for _, s := range seeds {
				visited[s.id] = true
			}
		}
		if err := bfs(seeds, "upstream"); err != nil {
			return nil, err
		}
	}

	// Sort by depth ascending, then by symbol name.
	sort.Slice(results, func(i, j int) bool {
		if results[i].depth != results[j].depth {
			return results[i].depth < results[j].depth
		}
		return results[i].qualifiedName < results[j].qualifiedName
	})

	out := make([]map[string]any, len(results))
	for i, r := range results {
		out[i] = map[string]any{
			"symbol":    r.qualifiedName,
			"kind":      r.kind,
			"name":      r.name,
			"file":      r.file,
			"depth":     r.depth,
			"direction": r.dir,
		}
	}
	return out, nil
}

func scanSymbols(rows *sql.Rows) ([]graph.Symbol, error) {
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

// PageRank computes a simplified PageRank over the symbol dependency graph and
// returns the top-N symbols sorted by rank descending.
func (s *Store) PageRank(ctx context.Context, repoID int64, limit int) ([]map[string]any, error) {
	limit = safeLimit(limit)

	// Step 1: load all resolved edges.
	rows2, err := s.db.QueryContext(ctx,
		`SELECT src_symbol_id, dst_symbol_id FROM edges WHERE repo_id = ? AND dst_symbol_id IS NOT NULL`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()

	outLinks := map[int64][]int64{} // src -> list of dst
	allNodes := map[int64]struct{}{}
	for rows2.Next() {
		var src, dst int64
		if err := rows2.Scan(&src, &dst); err != nil {
			return nil, err
		}
		outLinks[src] = append(outLinks[src], dst)
		allNodes[src] = struct{}{}
		allNodes[dst] = struct{}{}
	}
	if err := rows2.Err(); err != nil {
		return nil, err
	}

	n := len(allNodes)
	if n == 0 {
		return []map[string]any{}, nil
	}

	// Assign indices.
	nodeIndex := make(map[int64]int, n)
	indexNode := make([]int64, 0, n)
	for id := range allNodes {
		nodeIndex[id] = len(indexNode)
		indexNode = append(indexNode, id)
	}

	// Step 2: run PageRank.
	const damping = 0.85
	const iterations = 20
	rank := make([]float64, n)
	newRank := make([]float64, n)
	initial := 1.0 / float64(n)
	for i := range rank {
		rank[i] = initial
	}

	for range iterations {
		base := (1.0 - damping) / float64(n)
		for i := range newRank {
			newRank[i] = base
		}
		for src, dsts := range outLinks {
			si := nodeIndex[src]
			share := damping * rank[si] / float64(len(dsts))
			for _, dst := range dsts {
				newRank[nodeIndex[dst]] += share
			}
		}
		rank, newRank = newRank, rank
	}

	// Step 3: sort by rank descending and pick top N.
	type ranked struct {
		id    int64
		score float64
	}
	results := make([]ranked, n)
	for i, id := range indexNode {
		results[i] = ranked{id: id, score: rank[i]}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})
	results = results[:min(len(results), limit)]

	// Step 4: load symbol info.
	prOut := make([]map[string]any, 0, len(results))
	for _, r := range results {
		var name, kind, path string
		err := s.db.QueryRowContext(ctx,
			`SELECT s.qualified_name, s.kind, COALESCE(f.path, '') FROM symbols s LEFT JOIN files f ON f.id = s.file_id WHERE s.id = ?`, r.id).
			Scan(&name, &kind, &path)
		if err != nil {
			continue
		}
		prOut = append(prOut, map[string]any{
			"symbol": name,
			"kind":   kind,
			"file":   path,
			"rank":   math.Round(r.score*1e6) / 1e6,
		})
	}
	return prOut, nil
}

// CouplingMetrics computes file-level coupling based on cross-file edge counts.
func (s *Store) CouplingMetrics(ctx context.Context, repoID int64, limit int) ([]map[string]any, error) {
	limit = safeLimit(limit)

	cRows, err := s.db.QueryContext(ctx, `
		SELECT f1.path as file_a, f2.path as file_b, COUNT(*) as edge_count
		FROM edges e
		JOIN symbols s1 ON s1.id = e.src_symbol_id
		JOIN symbols s2 ON s2.id = e.dst_symbol_id
		JOIN files f1 ON f1.id = s1.file_id
		JOIN files f2 ON f2.id = s2.file_id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NOT NULL AND f1.id != f2.id
		GROUP BY f1.path, f2.path
		ORDER BY edge_count DESC
		LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer cRows.Close()

	cOut := make([]map[string]any, 0)
	for cRows.Next() {
		var fileA, fileB string
		var edgeCount int
		if err := cRows.Scan(&fileA, &fileB, &edgeCount); err != nil {
			return nil, err
		}
		coupling := "low"
		if edgeCount >= 10 {
			coupling = "high"
		} else if edgeCount >= 5 {
			coupling = "medium"
		}
		cOut = append(cOut, map[string]any{
			"file_a":     fileA,
			"file_b":     fileB,
			"edge_count": edgeCount,
			"coupling":   coupling,
		})
	}
	return cOut, cRows.Err()
}

// DetectCycles finds circular dependencies at the file level using DFS with
// white/gray/black coloring.
func (s *Store) DetectCycles(ctx context.Context, repoID int64, limit int) ([]map[string]any, error) {
	limit = safeLimit(limit)

	// Build file-level dependency graph.
	dRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT f1.path, f2.path
		FROM edges e
		JOIN symbols s1 ON s1.id = e.src_symbol_id
		JOIN symbols s2 ON s2.id = e.dst_symbol_id
		JOIN files f1 ON f1.id = s1.file_id
		JOIN files f2 ON f2.id = s2.file_id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NOT NULL AND f1.id != f2.id`, repoID)
	if err != nil {
		return nil, err
	}
	defer dRows.Close()

	fileGraph := map[string][]string{}
	allFiles := map[string]struct{}{}
	for dRows.Next() {
		var src, dst string
		if err := dRows.Scan(&src, &dst); err != nil {
			return nil, err
		}
		fileGraph[src] = append(fileGraph[src], dst)
		allFiles[src] = struct{}{}
		allFiles[dst] = struct{}{}
	}
	if err := dRows.Err(); err != nil {
		return nil, err
	}

	// DFS cycle detection with coloring.
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	for f := range allFiles {
		color[f] = white
	}

	var cycles [][]string
	parent := map[string]string{}

	var dfs func(node string)
	dfs = func(node string) {
		if len(cycles) >= limit {
			return
		}
		color[node] = gray
		for _, next := range fileGraph[node] {
			if len(cycles) >= limit {
				return
			}
			switch color[next] {
			case gray:
				// Back edge found — extract cycle.
				cycle := []string{next}
				cur := node
				for cur != next {
					cycle = append(cycle, cur)
					cur = parent[cur]
				}
				// Reverse to get correct order.
				for i, j := 0, len(cycle)-1; i < j; i, j = i+1, j-1 {
					cycle[i], cycle[j] = cycle[j], cycle[i]
				}
				cycle = append(cycle, next) // close the cycle
				cycles = append(cycles, cycle)
			case white:
				parent[next] = node
				dfs(next)
			}
		}
		color[node] = black
	}

	// Sort files for deterministic output.
	sortedFiles := make([]string, 0, len(allFiles))
	for f := range allFiles {
		sortedFiles = append(sortedFiles, f)
	}
	sort.Strings(sortedFiles)

	for _, f := range sortedFiles {
		if color[f] == white && len(cycles) < limit {
			dfs(f)
		}
	}

	dOut := make([]map[string]any, 0, len(cycles))
	for _, c := range cycles {
		dOut = append(dOut, map[string]any{
			"cycle":  c,
			"length": len(c) - 1, // subtract the closing node
		})
	}
	return dOut, nil
}

func safeLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	return limit
}

func safeOffset(offset int) int {
	return max(offset, 0)
}

func quoteFTS(query string) string {
	tokens := strings.Fields(query)
	for i, token := range tokens {
		tokens[i] = fmt.Sprintf(`"%s"*`, strings.ReplaceAll(token, `"`, ""))
	}
	return strings.Join(tokens, " ")
}

// FileIDByPath returns the file ID for a given repo and relative path.
func (s *Store) FileIDByPath(ctx context.Context, repoID int64, path string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM files WHERE repo_id = ? AND path = ?`, repoID, path).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// ListFiles returns indexed files for a repository, optionally filtered by path prefix.
func (s *Store) ListFiles(ctx context.Context, repoID int64, pathFilter string, limit, offset int) ([]map[string]any, error) {
	query := `SELECT path, language, size_bytes FROM files WHERE repo_id = ? AND is_deleted = 0`
	args := []any{repoID}
	if pathFilter != "" {
		query += ` AND path LIKE ?`
		args = append(args, pathFilter+"%")
	}
	query += ` ORDER BY path ASC LIMIT ? OFFSET ?`
	args = append(args, safeLimit(limit), safeOffset(offset))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var path, language string
		var sizeBytes int64
		if err := rows.Scan(&path, &language, &sizeBytes); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"path":       path,
			"language":   language,
			"size_bytes": sizeBytes,
		})
	}
	return out, rows.Err()
}

// FindDeadCode returns symbols with no incoming edges and no references — likely dead code.
func (s *Store) FindDeadCode(ctx context.Context, repoID int64, limit, offset int) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.qualified_name, s.kind, s.name, f.path, f.language,
		       s.start_line, s.end_line
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ?
		  AND s.kind IN ('function', 'method', 'type', 'class', 'struct', 'interface')
		  AND s.id NOT IN (
		      SELECT DISTINCT dst_symbol_id FROM edges WHERE repo_id = ? AND dst_symbol_id IS NOT NULL
		  )
		  AND s.id NOT IN (
		      SELECT DISTINCT symbol_id FROM references_tbl WHERE repo_id = ? AND symbol_id IS NOT NULL
		  )
		  AND s.name NOT IN ('main', 'init', 'Main', 'Init')
		  AND s.name NOT LIKE 'Test%'
		  AND s.name NOT LIKE 'Benchmark%'
		  AND s.name NOT LIKE 'Example%'
		ORDER BY f.path, s.start_line
		LIMIT ? OFFSET ?
	`, repoID, repoID, repoID, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id int64
		var qualifiedName, kind, name, path, language string
		var startLine, endLine int
		if err := rows.Scan(&id, &qualifiedName, &kind, &name, &path, &language, &startLine, &endLine); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"symbol":     qualifiedName,
			"kind":       kind,
			"name":       name,
			"file":       path,
			"language":   language,
			"start_line": startLine,
			"end_line":   endLine,
		})
	}
	return out, rows.Err()
}

// --- Embedding methods ---

// UpsertSymbolEmbeddings stores vector embeddings for symbols in a file.
// symbolMap maps stable_key -> embedding vector.
func (s *Store) UpsertSymbolEmbeddings(ctx context.Context, repoID, fileID int64, modelName string, symbolMap map[string][]float32) error {
	if len(symbolMap) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO symbol_embeddings(symbol_id, file_id, repo_id, embedding, dimensions, model_name, updated_at)
		SELECT s.id, ?, ?, ?, ?, ?, ?
		FROM symbols s
		WHERE s.file_id = ? AND s.stable_key = ?
		ON CONFLICT(symbol_id) DO UPDATE SET
			embedding = excluded.embedding,
			dimensions = excluded.dimensions,
			model_name = excluded.model_name,
			updated_at = excluded.updated_at
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	for stableKey, vec := range symbolMap {
		blob := float32ToBytes(vec)
		if _, err := stmt.ExecContext(ctx, fileID, repoID, blob, len(vec), modelName, now, fileID, stableKey); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

// VectorSearch performs cosine similarity search over symbol embeddings.
// For repos with fewer than maxVectorScanSymbols embeddings, it uses a
// brute-force scan. For larger repos it pre-filters via FTS to keep memory
// bounded. Consider replacing with an HNSW index (e.g. sqlite-vss) for
// very large codebases.
const maxVectorScanSymbols = 50_000

func (s *Store) VectorSearch(ctx context.Context, repoID int64, queryVec []float32, limit, offset int) ([]map[string]any, error) {
	limitVal := safeLimit(limit)
	offsetVal := safeOffset(offset)

	// Guard against loading too many embeddings into memory.
	var embCount int64
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbol_embeddings WHERE repo_id = ?`, repoID).Scan(&embCount)
	if embCount > maxVectorScanSymbols {
		rows, err := s.db.QueryContext(ctx, `
			SELECT se.symbol_id, se.embedding, se.dimensions,
				   s.qualified_name, s.kind, s.signature, s.doc_summary,
				   f.path
			FROM symbol_embeddings se
			JOIN symbols s ON s.id = se.symbol_id
			JOIN files f ON f.id = s.file_id
			WHERE se.repo_id = ?
			ORDER BY se.updated_at DESC
			LIMIT ?
		`, repoID, maxVectorScanSymbols)
		if err != nil {
			return nil, err
		}
		return s.scanAndRankVectors(rows, queryVec, limitVal, offsetVal)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT se.symbol_id, se.embedding, se.dimensions,
			   s.qualified_name, s.kind, s.signature, s.doc_summary,
			   f.path
		FROM symbol_embeddings se
		JOIN symbols s ON s.id = se.symbol_id
		JOIN files f ON f.id = s.file_id
		WHERE se.repo_id = ?
	`, repoID)
	if err != nil {
		return nil, err
	}
	return s.scanAndRankVectors(rows, queryVec, limitVal, offsetVal)
}

func (s *Store) scanAndRankVectors(rows *sql.Rows, queryVec []float32, limit, offset int) ([]map[string]any, error) {
	defer rows.Close()

	type scored struct {
		file   string
		symbol string
		kind   string
		score  float64
	}

	var candidates []scored
	for rows.Next() {
		var symbolID int64
		var blob []byte
		var dims int
		var qualName, kind, sig, doc, filePath string
		if err := rows.Scan(&symbolID, &blob, &dims, &qualName, &kind, &sig, &doc, &filePath); err != nil {
			return nil, err
		}
		vec := bytesToFloat32(blob)
		sim := cosineSimilarity(queryVec, vec)
		if sim > 0 {
			candidates = append(candidates, scored{file: filePath, symbol: qualName, kind: kind, score: sim})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	end := min(offset+limit, len(candidates))
	if offset >= len(candidates) {
		return nil, nil
	}
	candidates = candidates[offset:end]

	out := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, map[string]any{
			"file":   c.file,
			"symbol": c.symbol,
			"kind":   c.kind,
			"score":  c.score,
			"why":    []string{"vector_similarity"},
		})
	}
	return out, nil
}

// HybridSearch combines FTS5 and vector search using Reciprocal Rank Fusion.
func (s *Store) HybridSearch(ctx context.Context, repoID int64, query string, queryVec []float32, limit, offset int) ([]map[string]any, error) {
	// Run both searches with a larger window for fusion.
	fusionK := 60
	fetchLimit := max(safeLimit(limit)*3, 50)

	ftsResults, err := s.SearchSymbols(ctx, repoID, query, fetchLimit, 0)
	if err != nil {
		return nil, err
	}

	vecResults, err := s.VectorSearch(ctx, repoID, queryVec, fetchLimit, 0)
	if err != nil {
		return nil, err
	}

	// Build RRF scores keyed by "file::symbol"
	type entry struct {
		file   string
		symbol string
		kind   string
		score  float64
		why    []string
	}
	merged := map[string]*entry{}

	for rank, sym := range ftsResults {
		key := sym.FilePath + "::" + sym.QualifiedName
		e, ok := merged[key]
		if !ok {
			e = &entry{file: sym.FilePath, symbol: sym.QualifiedName, kind: sym.Kind}
			merged[key] = e
		}
		e.score += 1.0 / float64(fusionK+rank+1)
		e.why = appendUnique(e.why, "fts")
	}

	for rank, vm := range vecResults {
		key := vm["file"].(string) + "::" + vm["symbol"].(string)
		e, ok := merged[key]
		if !ok {
			e = &entry{
				file:   vm["file"].(string),
				symbol: vm["symbol"].(string),
				kind:   vm["kind"].(string),
			}
			merged[key] = e
		}
		e.score += 1.0 / float64(fusionK+rank+1)
		e.why = appendUnique(e.why, "vector_similarity")
	}

	sorted := make([]*entry, 0, len(merged))
	for _, e := range merged {
		sorted = append(sorted, e)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].score > sorted[j].score
	})

	limitVal := safeLimit(limit)
	offsetVal := safeOffset(offset)
	end := min(offsetVal+limitVal, len(sorted))
	if offsetVal >= len(sorted) {
		return nil, nil
	}
	sorted = sorted[offsetVal:end]

	out := make([]map[string]any, 0, len(sorted))
	for _, e := range sorted {
		out = append(out, map[string]any{
			"file":   e.file,
			"symbol": e.symbol,
			"kind":   e.kind,
			"score":  e.score,
			"why":    e.why,
		})
	}
	return out, nil
}

// HasEmbeddings checks whether the repo has any stored embeddings.
func (s *Store) HasEmbeddings(ctx context.Context, repoID int64) (bool, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbol_embeddings WHERE repo_id = ?`, repoID).Scan(&count)
	if err != nil {
		// Table may not exist yet in older databases.
		return false, nil
	}
	return count > 0, nil
}

// --- Embedding helpers ---

func float32ToBytes(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func bytesToFloat32(buf []byte) []float32 {
	vec := make([]float32, len(buf)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return vec
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func appendUnique(slice []string, val string) []string {
	if slices.Contains(slice, val) {
		return slice
	}
	return append(slice, val)
}

// ArchitectureOverview returns a high-level overview of the repository
// including language breakdown, top-level directories, symbol/edge kind
// breakdowns, key entry points, and hub symbols.
func (s *Store) ArchitectureOverview(ctx context.Context, repoID int64) (map[string]any, error) {
	stats, err := s.Stats(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("architecture overview: stats: %w", err)
	}

	// Language breakdown
	languages := []map[string]any{}
	{
		rows, err := s.db.QueryContext(ctx,
			`SELECT language, COUNT(*) as file_count FROM files WHERE repo_id = ? AND is_deleted = 0 GROUP BY language ORDER BY file_count DESC`,
			repoID)
		if err != nil {
			return nil, fmt.Errorf("architecture overview: languages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var lang string
			var count int
			if err := rows.Scan(&lang, &count); err != nil {
				return nil, err
			}
			languages = append(languages, map[string]any{"language": lang, "file_count": count})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Top-level directories
	topDirs := []map[string]any{}
	{
		rows, err := s.db.QueryContext(ctx,
			`SELECT SUBSTR(path, 1, INSTR(path||'/', '/') - 1) AS dir, COUNT(*) as count FROM files WHERE repo_id = ? AND is_deleted = 0 GROUP BY dir ORDER BY count DESC LIMIT 20`,
			repoID)
		if err != nil {
			return nil, fmt.Errorf("architecture overview: directories: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var dir string
			var count int
			if err := rows.Scan(&dir, &count); err != nil {
				return nil, err
			}
			topDirs = append(topDirs, map[string]any{"directory": dir, "file_count": count})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Symbol kind breakdown
	symbolKinds := []map[string]any{}
	{
		rows, err := s.db.QueryContext(ctx,
			`SELECT kind, COUNT(*) as count FROM symbols WHERE repo_id = ? GROUP BY kind ORDER BY count DESC`,
			repoID)
		if err != nil {
			return nil, fmt.Errorf("architecture overview: symbol kinds: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var kind string
			var count int
			if err := rows.Scan(&kind, &count); err != nil {
				return nil, err
			}
			symbolKinds = append(symbolKinds, map[string]any{"kind": kind, "count": count})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Edge kind breakdown
	edgeKinds := []map[string]any{}
	{
		rows, err := s.db.QueryContext(ctx,
			`SELECT edge_kind, COUNT(*) as count FROM edges WHERE repo_id = ? GROUP BY edge_kind ORDER BY count DESC`,
			repoID)
		if err != nil {
			return nil, fmt.Errorf("architecture overview: edge kinds: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var kind string
			var count int
			if err := rows.Scan(&kind, &count); err != nil {
				return nil, err
			}
			edgeKinds = append(edgeKinds, map[string]any{"kind": kind, "count": count})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Key entry points (most incoming edges)
	entryPoints := []map[string]any{}
	{
		rows, err := s.db.QueryContext(ctx,
			`SELECT s.qualified_name, s.kind, f.path, COUNT(e.id) as caller_count FROM symbols s JOIN files f ON f.id = s.file_id LEFT JOIN edges e ON e.dst_symbol_id = s.id WHERE s.repo_id = ? GROUP BY s.id ORDER BY caller_count DESC LIMIT 15`,
			repoID)
		if err != nil {
			return nil, fmt.Errorf("architecture overview: entry points: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var qname, kind, path string
			var count int
			if err := rows.Scan(&qname, &kind, &path, &count); err != nil {
				return nil, err
			}
			entryPoints = append(entryPoints, map[string]any{
				"qualified_name": qname, "kind": kind, "file": path, "caller_count": count,
			})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Hub symbols (most outgoing edges)
	hubSymbols := []map[string]any{}
	{
		rows, err := s.db.QueryContext(ctx,
			`SELECT s.qualified_name, s.kind, f.path, COUNT(e.id) as callee_count FROM symbols s JOIN files f ON f.id = s.file_id LEFT JOIN edges e ON e.src_symbol_id = s.id WHERE s.repo_id = ? GROUP BY s.id ORDER BY callee_count DESC LIMIT 15`,
			repoID)
		if err != nil {
			return nil, fmt.Errorf("architecture overview: hub symbols: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var qname, kind, path string
			var count int
			if err := rows.Scan(&qname, &kind, &path, &count); err != nil {
				return nil, err
			}
			hubSymbols = append(hubSymbols, map[string]any{
				"qualified_name": qname, "kind": kind, "file": path, "callee_count": count,
			})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return map[string]any{
		"languages":       languages,
		"top_directories": topDirs,
		"symbol_kinds":    symbolKinds,
		"edge_kinds":      edgeKinds,
		"entry_points":    entryPoints,
		"hub_symbols":     hubSymbols,
		"totals": map[string]any{
			"files":      stats.Files,
			"symbols":    stats.Symbols,
			"edges":      stats.Edges,
			"references": stats.References,
		},
	}, nil
}

// AllImports returns a map of file path to list of import paths for the given repo.
func (s *Store) AllImports(ctx context.Context, repoID int64) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.path, fi.import_path
		FROM file_imports fi
		JOIN files f ON f.id = fi.file_id
		WHERE f.repo_id = ? AND f.is_deleted = 0
		ORDER BY f.path`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]string)
	for rows.Next() {
		var path, importPath string
		if err := rows.Scan(&path, &importPath); err != nil {
			return nil, err
		}
		result[path] = append(result[path], importPath)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// AllFilePaths returns all non-deleted file paths for the given repo.
func (s *Store) AllFilePaths(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path FROM files
		WHERE repo_id = ? AND is_deleted = 0
		ORDER BY path`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

// BenchmarkTokens estimates the token savings from using codegraph context
// vs reading all raw files in the repository.
func (s *Store) BenchmarkTokens(ctx context.Context, repoID int64, task string) (map[string]any, error) {
	// Step 1: total repo file stats.
	var fileCount int64
	var totalBytes int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) AS file_count, COALESCE(SUM(size_bytes),0) AS total_bytes FROM files WHERE repo_id = ? AND is_deleted = 0`,
		repoID,
	).Scan(&fileCount, &totalBytes)
	if err != nil {
		return nil, fmt.Errorf("benchmark repo totals: %w", err)
	}

	// Step 2: estimate context cost.
	var contextFileCount int64
	var contextBytes int64

	if task != "" {
		// Run a semantic search to find relevant files, similar to context_for_task.
		results, err := s.SemanticSearch(ctx, repoID, task, 30, 0)
		if err != nil {
			return nil, fmt.Errorf("benchmark semantic search: %w", err)
		}
		// Collect unique file paths from results.
		filePaths := map[string]bool{}
		for _, r := range results {
			if p, ok := r["file"].(string); ok && p != "" {
				filePaths[p] = true
			}
		}
		// Cap at 10 files to mirror context_for_task defaults.
		paths := make([]string, 0, len(filePaths))
		for p := range filePaths {
			if len(paths) >= 10 {
				break
			}
			paths = append(paths, p)
		}
		if len(paths) > 0 {
			placeholders := strings.TrimRight(strings.Repeat("?,", len(paths)), ",")
			args := make([]any, 0, len(paths)+1)
			args = append(args, repoID)
			for _, p := range paths {
				args = append(args, p)
			}
			err = s.db.QueryRowContext(ctx,
				`SELECT COUNT(*), COALESCE(SUM(size_bytes),0) FROM files WHERE repo_id = ? AND is_deleted = 0 AND path IN (`+placeholders+`)`,
				args...,
			).Scan(&contextFileCount, &contextBytes)
			if err != nil {
				return nil, fmt.Errorf("benchmark context bytes: %w", err)
			}
		}
	} else {
		// No task provided: estimate based on average file size * 10 files.
		var avgSize float64
		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(AVG(size_bytes),0) FROM files WHERE repo_id = ? AND is_deleted = 0`,
			repoID,
		).Scan(&avgSize)
		if err != nil {
			return nil, fmt.Errorf("benchmark avg size: %w", err)
		}
		contextFileCount = min(10, fileCount)
		contextBytes = int64(avgSize) * contextFileCount
	}

	// Step 3: build comparison result.
	totalTokens := totalBytes / 4
	contextTokens := contextBytes / 4
	var savingsPct float64
	if totalTokens > 0 {
		savingsPct = float64(totalTokens-contextTokens) / float64(totalTokens) * 100.0
	}

	return map[string]any{
		"repo_total_files":  fileCount,
		"repo_total_bytes":  totalBytes,
		"repo_total_tokens": totalTokens,
		"context_files":     contextFileCount,
		"context_bytes":     contextBytes,
		"context_tokens":    contextTokens,
		"token_savings_pct": savingsPct,
		"estimated_cost_without": map[string]any{
			"claude_sonnet_input": float64(totalTokens) * 3.0 / 1_000_000,
		},
		"estimated_cost_with": map[string]any{
			"claude_sonnet_input": float64(contextTokens) * 3.0 / 1_000_000,
		},
	}, nil
}

// ResolveCrossLanguageLinks creates edges between symbols in different languages
// that reference each other. It returns the total number of new edges created.
func (s *Store) ResolveCrossLanguageLinks(ctx context.Context, repoID int64) (int, error) {
	totalCreated := 0

	// Strategy 1: Shared name matching across languages.
	// Find symbols with identical names in different languages and create
	// cross_language_ref edges, filtering out short/common names.
	rows, err := s.db.QueryContext(ctx, `
		SELECT s1.id, s2.id, s1.name, s1.language, s2.language, s1.file_id, s2.file_id
		FROM symbols s1
		JOIN symbols s2 ON s1.name = s2.name AND s1.language != s2.language AND s1.repo_id = s2.repo_id
		WHERE s1.repo_id = ?
		AND s1.kind IN ('function', 'method', 'class', 'type', 'struct', 'interface')
		AND s2.kind IN ('function', 'method', 'class', 'type', 'struct', 'interface')
		AND length(s1.name) > 3
		AND s1.name NOT IN ('main', 'init', 'new', 'get', 'set', 'run', 'start', 'stop', 'open', 'close', 'read', 'write', 'delete', 'update', 'create', 'test', 'setup', 'handle', 'process')
		AND s1.id < s2.id
	`, repoID)
	if err != nil {
		return 0, fmt.Errorf("cross-language shared name query: %w", err)
	}
	defer rows.Close()

	type crossLink struct {
		srcID   int64
		dstID   int64
		name    string
		srcLang string
		dstLang string
		srcFile int64
		dstFile int64
	}
	var links []crossLink
	for rows.Next() {
		var l crossLink
		if err := rows.Scan(&l.srcID, &l.dstID, &l.name, &l.srcLang, &l.dstLang, &l.srcFile, &l.dstFile); err != nil {
			return 0, fmt.Errorf("cross-language scan: %w", err)
		}
		links = append(links, l)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("cross-language rows: %w", err)
	}

	for _, l := range links {
		evidence := "shared_name:" + l.srcLang + "→" + l.dstLang
		// Check if this edge already exists to avoid duplicates.
		var exists int
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM edges
			WHERE repo_id = ? AND src_symbol_id = ? AND dst_symbol_id = ? AND edge_kind = 'cross_language_ref'
		`, repoID, l.srcID, l.dstID).Scan(&exists)
		if err != nil {
			return totalCreated, fmt.Errorf("cross-language check existing: %w", err)
		}
		if exists > 0 {
			continue
		}
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, ?, 'cross_language_ref', ?, ?, 0)
		`, repoID, l.srcID, l.dstID, l.name, evidence, l.srcFile)
		if err != nil {
			return totalCreated, fmt.Errorf("cross-language insert: %w", err)
		}
		totalCreated++
	}

	// Strategy 2: Import-path based linking.
	// Find file_imports that reference paths matching files in other languages,
	// then link exported symbols between those files.
	importRows, err := s.db.QueryContext(ctx, `
		SELECT fi.file_id, fi.import_path, f.language
		FROM file_imports fi
		JOIN files f ON f.id = fi.file_id
		WHERE f.repo_id = ? AND f.is_deleted = 0
	`, repoID)
	if err != nil {
		return totalCreated, fmt.Errorf("cross-language imports query: %w", err)
	}
	defer importRows.Close()

	type importInfo struct {
		fileID     int64
		importPath string
		language   string
	}
	var imports []importInfo
	for importRows.Next() {
		var info importInfo
		if err := importRows.Scan(&info.fileID, &info.importPath, &info.language); err != nil {
			return totalCreated, fmt.Errorf("cross-language import scan: %w", err)
		}
		imports = append(imports, info)
	}
	if err := importRows.Err(); err != nil {
		return totalCreated, fmt.Errorf("cross-language import rows: %w", err)
	}

	// Build a map of file paths (without extension) to file IDs and languages.
	fileRows, err := s.db.QueryContext(ctx, `
		SELECT id, path, language FROM files WHERE repo_id = ? AND is_deleted = 0
	`, repoID)
	if err != nil {
		return totalCreated, fmt.Errorf("cross-language files query: %w", err)
	}
	defer fileRows.Close()

	type fileInfo struct {
		id       int64
		language string
	}
	filesByBase := map[string][]fileInfo{}
	for fileRows.Next() {
		var id int64
		var path, lang string
		if err := fileRows.Scan(&id, &path, &lang); err != nil {
			return totalCreated, fmt.Errorf("cross-language file scan: %w", err)
		}
		// Strip extension to get the base path for matching.
		base := strings.TrimSuffix(path, filepath.Ext(path))
		filesByBase[base] = append(filesByBase[base], fileInfo{id: id, language: lang})
	}
	if err := fileRows.Err(); err != nil {
		return totalCreated, fmt.Errorf("cross-language file rows: %w", err)
	}

	for _, imp := range imports {
		// Normalize import path: strip leading ./ or ../ prefixes and extensions.
		normalized := imp.importPath
		normalized = strings.TrimPrefix(normalized, "./")
		normalized = strings.TrimPrefix(normalized, "../")
		normalized = strings.TrimSuffix(normalized, filepath.Ext(normalized))

		matches, ok := filesByBase[normalized]
		if !ok {
			continue
		}
		for _, match := range matches {
			if match.language == imp.language {
				continue // only cross-language links
			}
			// Link exported symbols from the importing file to the target file's symbols.
			linkRows, err := s.db.QueryContext(ctx, `
				SELECT src.id, dst.id, dst.name, src.file_id
				FROM symbols src
				JOIN symbols dst ON dst.file_id = ? AND src.repo_id = dst.repo_id
				WHERE src.file_id = ? AND src.repo_id = ?
				AND src.kind IN ('function', 'method', 'class', 'type', 'struct', 'interface')
				AND dst.kind IN ('function', 'method', 'class', 'type', 'struct', 'interface')
				AND NOT EXISTS (
					SELECT 1 FROM edges
					WHERE repo_id = ? AND src_symbol_id = src.id AND dst_symbol_id = dst.id AND edge_kind = 'cross_language_ref'
				)
				LIMIT 50
			`, match.id, imp.fileID, repoID, repoID)
			if err != nil {
				continue
			}
			for linkRows.Next() {
				var srcID, dstID, srcFileID int64
				var dstName string
				if err := linkRows.Scan(&srcID, &dstID, &dstName, &srcFileID); err != nil {
					continue
				}
				evidence := "import_path:" + imp.language + "→" + match.language
				_, err = s.db.ExecContext(ctx, `
					INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
					VALUES(?, ?, ?, ?, 'cross_language_ref', ?, ?, 0)
				`, repoID, srcID, dstID, dstName, evidence, srcFileID)
				if err == nil {
					totalCreated++
				}
			}
			linkRows.Close()
		}
	}

	return totalCreated, nil
}

// --- Session Memory ---

func (s *Store) SessionLogEvent(ctx context.Context, repoID int64, sessionID, eventType, key, value, metadata string) error {
	if metadata == "" {
		metadata = "{}"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_events (repo_id, session_id, event_type, key, value, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, repoID, sessionID, eventType, key, value, metadata, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) SessionGetHistory(ctx context.Context, repoID int64, sessionID string, eventType string, limit, offset int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, session_id, event_type, key, value, metadata, created_at FROM session_events WHERE repo_id = ?`
	args := []any{repoID}
	if sessionID != "" {
		query += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	if eventType != "" {
		query += ` AND event_type = ?`
		args = append(args, eventType)
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []map[string]any
	for rows.Next() {
		var id int64
		var sid, etype, k, v, meta, createdAt string
		if err := rows.Scan(&id, &sid, &etype, &k, &v, &meta, &createdAt); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"id":         id,
			"session_id": sid,
			"event_type": etype,
			"key":        k,
			"value":      v,
			"metadata":   meta,
			"created_at": createdAt,
		})
	}
	return results, rows.Err()
}

func (s *Store) SessionGetHotFiles(ctx context.Context, repoID int64, sessionID string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT key AS file, COUNT(*) AS access_count, MAX(created_at) AS last_accessed
		FROM session_events
		WHERE repo_id = ? AND event_type IN ('read', 'edit')
		AND (? = '' OR session_id = ?)
		GROUP BY key
		ORDER BY access_count DESC
		LIMIT ?
	`, repoID, sessionID, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []map[string]any
	for rows.Next() {
		var file, lastAccessed string
		var count int64
		if err := rows.Scan(&file, &count, &lastAccessed); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"file":          file,
			"access_count":  count,
			"last_accessed": lastAccessed,
		})
	}
	return results, rows.Err()
}

func (s *Store) SessionGetContext(ctx context.Context, repoID int64, sessionID string) (map[string]any, error) {
	decisions, err := s.SessionGetHistory(ctx, repoID, sessionID, "decision", 10, 0)
	if err != nil {
		return nil, err
	}
	facts, err := s.SessionGetHistory(ctx, repoID, sessionID, "fact", 10, 0)
	if err != nil {
		return nil, err
	}
	tasks, err := s.SessionGetHistory(ctx, repoID, sessionID, "task", 10, 0)
	if err != nil {
		return nil, err
	}
	hotFiles, err := s.SessionGetHotFiles(ctx, repoID, sessionID, 10)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"decisions": decisions,
		"facts":     facts,
		"tasks":     tasks,
		"hot_files": hotFiles,
	}, nil
}
