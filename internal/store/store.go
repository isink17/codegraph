package store

import (
	"cmp"
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
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/limits"
	"github.com/isink17/codegraph/internal/texttoken"
)

//go:embed schema/*.sql
var migrationFS embed.FS

const (
	// sqliteDefaultMaxVariables is SQLite's commonly configured parameter limit (often 999).
	// Keep batch sizes below this to avoid "too many SQL variables" errors.
	sqliteDefaultMaxVariables = 999

	// sqliteInClauseBatchSize is a conservative IN-clause chunk size used for set-based deletes/updates.
	// It stays under sqliteDefaultMaxVariables with room for any additional parameters.
	sqliteInClauseBatchSize = 900

	// sqliteTokenValuesBatchRows controls multi-row inserts into token tables where each row uses 3 parameters.
	// 300*3=900 variables, staying under sqliteDefaultMaxVariables.
	sqliteTokenValuesBatchRows = 300

	// sqliteEmbeddingValuesBatchRows controls multi-row upserts into symbol_embeddings where each row uses 7 parameters.
	// 100*7=700 variables, staying under sqliteDefaultMaxVariables.
	sqliteEmbeddingValuesBatchRows = 100

	// sqliteReferenceValuesBatchRows controls multi-row inserts into references_tbl where each row uses 11 parameters.
	// 90*11=990 variables, staying under sqliteDefaultMaxVariables.
	sqliteReferenceValuesBatchRows = 90

	// sqliteEdgeValuesBatchRows controls multi-row inserts into edges where each row uses 7 parameters.
	// 140*7=980 variables, staying under sqliteDefaultMaxVariables.
	sqliteEdgeValuesBatchRows = 140

	// sqliteImportValuesBatchRows controls multi-row inserts into file_imports where each row uses 3 parameters.
	// 300*3=900 variables, staying under sqliteDefaultMaxVariables.
	sqliteImportValuesBatchRows = 300
	// sqliteTestLinkValuesBatchRows controls multi-row inserts into test_links where each row uses 8 parameters
	// (target_stable_key was added by migration 020).
	// 124*8=992 variables, staying under sqliteDefaultMaxVariables.
	sqliteTestLinkValuesBatchRows = 124

	// sqliteSymbolValuesBatchRows controls multi-row inserts into symbols where each row uses 19 parameters.
	// 52*19=988 variables, staying under sqliteDefaultMaxVariables.
	sqliteSymbolValuesBatchRows = 52
	// sqliteSymbolFTSValuesBatchRows controls multi-row inserts into symbol_fts where each row uses 6 parameters.
	// 150*6=900 variables, staying under sqliteDefaultMaxVariables.
	sqliteSymbolFTSValuesBatchRows = 150
)

type Store struct {
	db *sql.DB
	// neighborStmts counts the statements the batched context-neighbour
	// pipeline issues. The number is the load-bearing property of P19 -- it must
	// not grow with the seed count -- and wall-clock cannot prove that, so the
	// regression test reads this instead. One atomic add per database round
	// trip.
	//
	// It is not scoped to that pipeline alone: the batch shares the name cascade
	// (symbolIDsByColumn, symbolIDsBySuffixScan) with the public FindCallees, so
	// a FindCallees call adds to it too. A test that reads the counter must
	// reset it and then call only what it means to measure.
	neighborStmts atomic.Int64
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
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
	RepoID                  int64                      `json:"repo_id"`
	ScanID                  int64                      `json:"scan_id"`
	Rebuild                 bool                       `json:"rebuild,omitempty"`
	RemovedDBFiles          []string                   `json:"removed_db_files,omitempty"`
	FilesSeen               int                        `json:"files_seen"`
	FilesIndexed            int                        `json:"files_indexed"`
	FilesSkipped            int                        `json:"files_skipped"`
	FilesChanged            int                        `json:"files_changed"`
	FilesDeleted            int                        `json:"files_deleted"`
	FilesTotal              int                        `json:"files_total,omitempty"`
	FilesDeletedPct         float64                    `json:"files_deleted_pct,omitempty"`
	ParseErrors             int                        `json:"parse_errors,omitempty"`
	ParseSamples            []string                   `json:"parse_samples,omitempty"`
	LanguageCoverage        map[string]LanguageCounts  `json:"language_coverage,omitempty"`
	PhaseTimings            []ScanPhaseTiming          `json:"phase_timings,omitempty"`
	ExistingLoadMS          int64                      `json:"existing_load_ms,omitempty"`
	WalkMS                  int64                      `json:"walk_ms,omitempty"`
	ProcessWallMS           int64                      `json:"process_wall_ms,omitempty"`
	TaskMS                  int64                      `json:"task_ms,omitempty"`
	TaskOtherMS             int64                      `json:"task_other_ms,omitempty"`
	ParseMS                 int64                      `json:"parse_ms,omitempty"`
	ReadMS                  int64                      `json:"read_ms,omitempty"`
	HashMS                  int64                      `json:"hash_ms,omitempty"`
	WriteMS                 int64                      `json:"write_ms,omitempty"`
	WriteMetadataMS         int64                      `json:"write_metadata_ms,omitempty"`
	WriteReplaceMS          int64                      `json:"write_replace_ms,omitempty"`
	WriteMarkSeenFlushes    int                        `json:"write_mark_seen_flushes,omitempty"`
	WriteMarkSeenSkipped    int                        `json:"write_mark_seen_skipped,omitempty"`
	WriteTouchFlushes       int                        `json:"write_touch_flushes,omitempty"`
	WriteParseFailedFlushes int                        `json:"write_parse_failed_flushes,omitempty"`
	WriteReplaceFlushes     int                        `json:"write_replace_flushes,omitempty"`
	WriteStats              *WriteStats                `json:"write_stats,omitempty"`
	EmbedMS                 int64                      `json:"embed_ms,omitempty"`
	MarkMissingMS           int64                      `json:"mark_missing_ms,omitempty"`
	ResolveMS               int64                      `json:"resolve_ms,omitempty"`
	ResolveMode             string                     `json:"resolve_mode,omitempty"`
	ResolveCrossFileMS      int64                      `json:"resolve_cross_file_ms,omitempty"`
	ResolveCrossFileTargets int                        `json:"resolve_cross_file_targets,omitempty"`
	ResolveCrossFile        *ResolveEdgesForNamesStats `json:"resolve_cross_file,omitempty"`
	// ResolveTestLinksMS and ResolveTestLinksBound report the Pass-2 test-link
	// resolution separately from edge resolution, so a drop in binding rate is
	// visible in `codegraph index --json` instead of hiding inside ResolveMS.
	// ResolveTestLinksMS is also counted in ResolveMS, which remains the total
	// for the whole resolution pass.
	ResolveTestLinksMS    int64 `json:"resolve_test_links_ms,omitempty"`
	ResolveTestLinksBound int   `json:"resolve_test_links_bound,omitempty"`
	// ResolveTestLinksFileBound counts rows whose file-level target was set from
	// the test file's conventional production sibling (P22.2) -- rows related to
	// a file with the exact symbol unknown.
	ResolveTestLinksFileBound int   `json:"resolve_test_links_file_bound,omitempty"`
	DurationMS                int64 `json:"duration_ms"`
}

type ResolveEdgesForNamesStats struct {
	NamesInput        int `json:"names_input,omitempty"`
	NamesUnique       int `json:"names_unique,omitempty"`
	ExactQueryBatches int `json:"exact_query_batches,omitempty"`
	ExactHits         int `json:"exact_hits,omitempty"`
	QualifiedScanned  int `json:"qualified_scanned,omitempty"`
	SuffixHits        int `json:"suffix_hits,omitempty"`
	TargetsSelected   int `json:"targets_selected,omitempty"`
	// TargetsResolved counts edges actually bound to a destination symbol,
	// not merely selected as candidates.
	TargetsResolved int `json:"targets_resolved,omitempty"`
	// TargetsUnresolved counts selected edges with a known source language that
	// found no compatible destination. TargetsResolved + TargetsUnresolved +
	// UnknownSrcLanguage always equals TargetsSelected.
	TargetsUnresolved int `json:"targets_unresolved,omitempty"`
	// LanguageBlocked is the subset of TargetsUnresolved whose name was seen
	// only under a different language. It is a lower bound, not an exact count:
	// it is observable only for the names the candidate lookups queried.
	LanguageBlocked int `json:"language_blocked,omitempty"`
	// AmbiguityBlocked is the subset of TargetsUnresolved that had several
	// same-language candidates and no evidence naming one of them. It is
	// disjoint from LanguageBlocked: an edge is reported as blocked by the
	// ambiguity it actually hit, never as both.
	AmbiguityBlocked int `json:"ambiguity_blocked,omitempty"`
	// TestShadowBlocked is the subset of TargetsUnresolved whose calling file is
	// production code while every same-language candidate was declared in a test
	// file. Disjoint from both counters above: the name matched, in the right
	// language, unambiguously -- and production code is still not wired into a
	// test definition. See resolver_testfile.go.
	TestShadowBlocked int `json:"test_shadow_blocked,omitempty"`
	// UnknownSrcLanguage counts selected edges whose source file has no
	// persisted language. Implicit resolution fails closed for them regardless
	// of whether a destination existed.
	UnknownSrcLanguage int `json:"unknown_src_language,omitempty"`
	// InvalidateMS is the time invalidateNameEvidenceBindings spent clearing
	// bindings this batch may have made ambiguous, and InvalidatedBindings how
	// many it cleared. Without them the phase is invisible and the other timers
	// under-report the update.
	InvalidateMS             int64 `json:"invalidate_ms,omitempty"`
	InvalidatedBindings      int   `json:"invalidated_bindings,omitempty"`
	ExactSelectMS            int64 `json:"exact_select_ms,omitempty"`
	SuffixSelectMS           int64 `json:"suffix_select_ms,omitempty"`
	ResolveTargetsMS         int64 `json:"resolve_targets_ms,omitempty"`
	RustAffectedCrates       int   `json:"rust_affected_crates,omitempty"`
	RustAffectedModules      int   `json:"rust_affected_modules,omitempty"`
	RustAffectedEdges        int   `json:"rust_affected_edges,omitempty"`
	RustCandidateRows        int   `json:"rust_candidate_rows,omitempty"`
	RustReExportNodesVisited int   `json:"rust_reexport_nodes_visited,omitempty"`
	RustBatchInvalidationOps int   `json:"rust_batch_invalidation_ops,omitempty"`
	RustBatchApplyOps        int   `json:"rust_batch_apply_ops,omitempty"`
}

type ScanPhaseTiming struct {
	Phase string `json:"phase"`
	MS    int64  `json:"ms"`
}

type WriteStats struct {
	TxCount int `json:"tx_count,omitempty"`

	FileUpsertStatements int `json:"file_upsert_statements,omitempty"`

	FileGraphDeleteChunks              int `json:"file_graph_delete_chunks,omitempty"`
	FileGraphDeleteStatements          int `json:"file_graph_delete_statements,omitempty"`
	FileGraphDeleteTempIDInsertBatches int `json:"file_graph_delete_temp_id_insert_batches,omitempty"`
	FileGraphDeleteTempIDInsertRows    int `json:"file_graph_delete_temp_id_insert_rows,omitempty"`

	SymbolInserts    int `json:"symbol_inserts,omitempty"`
	SymbolFTSInserts int `json:"symbol_fts_inserts,omitempty"`

	SymbolInsertBatches    int `json:"symbol_insert_batches,omitempty"`
	SymbolInsertRows       int `json:"symbol_insert_rows,omitempty"`
	SymbolFTSInsertBatches int `json:"symbol_fts_insert_batches,omitempty"`
	SymbolFTSInsertRows    int `json:"symbol_fts_insert_rows,omitempty"`

	SymbolTokenInsertBatches int   `json:"symbol_token_insert_batches,omitempty"`
	SymbolTokenInsertRows    int   `json:"symbol_token_insert_rows,omitempty"`
	SymbolTokenizeNS         int64 `json:"symbol_tokenize_ns,omitempty"`
	SymbolTokenizeCalls      int   `json:"symbol_tokenize_calls,omitempty"`

	FileTokenInsertBatches int `json:"file_token_insert_batches,omitempty"`
	FileTokenInsertRows    int `json:"file_token_insert_rows,omitempty"`

	ReferenceInsertBatches int `json:"reference_insert_batches,omitempty"`
	ReferenceInsertRows    int `json:"reference_insert_rows,omitempty"`

	EdgeInsertBatches               int `json:"edge_insert_batches,omitempty"`
	EdgeInsertRows                  int `json:"edge_insert_rows,omitempty"`
	EdgeSourceDroppedUnattributable int `json:"edge_source_dropped_unattributable,omitempty"`
	EdgeSourceFallbackAttributed    int `json:"edge_source_fallback_attributed,omitempty"`

	ImportInsertBatches int `json:"import_insert_batches,omitempty"`
	ImportInsertRows    int `json:"import_insert_rows,omitempty"`

	TestLinkInsertBatches int `json:"test_link_insert_batches,omitempty"`
	TestLinkInsertRows    int `json:"test_link_insert_rows,omitempty"`

	TotalExecStatements int `json:"total_exec_statements,omitempty"`
}

type LanguageCounts struct {
	Seen        int `json:"seen"`
	Indexed     int `json:"indexed"`
	Skipped     int `json:"skipped"`
	ParseFailed int `json:"parse_failed"`
	// Extensions is populated for live scan/index summaries only.
	// It is not persisted in scan_language_coverage.
	// Historical scan summaries keep aggregate unknown counts only.
	Extensions map[string]LanguageCounts `json:"extensions,omitempty"`
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

type SymbolSearchResult struct {
	Matched bool           `json:"matched"`
	Matches []graph.Symbol `json:"matches"`
}

type NeighborResult struct {
	TargetFound     bool           `json:"target_found"`
	Callers         []graph.Symbol `json:"callers"`
	Callees         []graph.Symbol `json:"callees"`
	UnresolvedHints []graph.Symbol `json:"unresolved_hints,omitempty"`
}

type RelatedTestsResult struct {
	TargetFound bool          `json:"target_found"`
	Tests       []RelatedTest `json:"tests"`
}

type RelatedTestsForFilesResult struct {
	Requested int           `json:"requested"`
	Found     int           `json:"found"`
	Missing   []string      `json:"missing"`
	Tests     []RelatedTest `json:"tests"`
}

type TraceResult struct {
	TargetFound  bool             `json:"target_found"`
	Dependencies []map[string]any `json:"dependencies"`
	Total        int              `json:"total"`
	Offset       int              `json:"offset"`
	Truncated    bool             `json:"truncated"`
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
	// ResolutionStrategy and ResolutionConfidence explain a bound destination:
	// which resolver strategy selected it and how strong that evidence was.
	// Both are empty -- and so omitted -- for unresolved edges and for edges
	// bound before migration 019, which carry no recoverable provenance.
	// See edge_resolution.go for the value sets and the confidence mapping.
	ResolutionStrategy   string `json:"resolution_strategy,omitempty"`
	ResolutionConfidence string `json:"resolution_confidence,omitempty"`
	// TargetClassification says what this edge points at: `project` when a
	// destination is bound, and otherwise what the unresolved `dst_name` can be
	// proven to be -- `builtin`, `stdlib`, `external`, or `unknown`. It is
	// derived on read from language, imports, and name evidence, never stored,
	// so it cannot go stale against the binding in the same row. One short enum,
	// no per-edge explanation blob.
	//
	// See internal/store/target_classification.go for the evidence loader and
	// internal/classify for the rules and the deliberate per-language gaps.
	TargetClassification string `json:"target_classification,omitempty"`
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
	isNewDB := false
	if st, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			isNewDB = true
		} else {
			return nil, err
		}
	} else if st.Size() == 0 {
		isNewDB = true
	}
	dsn, err := BuildSQLiteDSN(path, opts, isNewDB, false)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		return nil, err
	}
	if err := withSQLiteBusyRetry(6*time.Second, 50*time.Millisecond, func() error {
		return applyPragmas(db, isNewDB, opts.PerformanceProfile)
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.Migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func BuildSQLiteDSN(path string, opts OpenOptions, isNewDB bool, readOnly bool) (string, error) {
	pragmas := buildPragmas(isNewDB, opts.PerformanceProfile)
	values := url.Values{}
	if readOnly {
		values.Set("mode", "ro")
	}
	for _, pragma := range pragmas {
		// journal_mode is a persistent, written setting, so applying it over a
		// mode=ro handle fails -- and it fails on the first statement rather
		// than at open, surfacing as an unattributable "attempt to write a
		// readonly database". A reader has no business changing the journal
		// mode anyway: it must read the database in whatever mode it already
		// uses, which is exactly what omitting the pragma does.
		if readOnly && strings.HasPrefix(pragma, "journal_mode(") {
			continue
		}
		values.Add("_pragma", pragma)
	}
	p := filepath.ToSlash(path)
	u := url.URL{Scheme: "file"}
	if strings.HasPrefix(p, "//") {
		rest := strings.TrimPrefix(p, "//")
		host, tail, _ := strings.Cut(rest, "/")
		u.Host = host
		u.Path = "/" + tail
	} else {
		if len(p) >= 2 && p[1] == ':' {
			p = "/" + p
		}
		u.Path = p
	}
	u.RawQuery = values.Encode()
	return u.String(), nil
}

func withSQLiteBusyRetry(timeout, interval time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := fn(); err != nil {
			if isSQLiteBusy(err) && time.Now().Before(deadline) {
				time.Sleep(interval)
				continue
			}
			return err
		}
		return nil
	}
}

func buildPragmas(isNewDB bool, profile string) []string {
	base := []string{
		`journal_mode(WAL)`,
		`busy_timeout(5000)`,
		`foreign_keys(ON)`,
		`cache_size(-65536)`,
	}
	if isNewDB {
		// auto_vacuum is only reliably applied for a brand-new DB before any tables are created.
		// For existing DBs, switching auto_vacuum requires VACUUM; we avoid doing that implicitly.
		base = append(base, `auto_vacuum(INCREMENTAL)`)

		// page_size is only reliably applied for a brand-new DB before any tables are created.
		// For existing DBs, changing page_size requires VACUUM; we avoid doing that implicitly.
		base = append(base, `page_size(8192)`)
	}
	perf := strings.ToLower(strings.TrimSpace(profile))
	switch perf {
	case "", "balanced":
		base = append(base, `synchronous(NORMAL)`, `temp_store(MEMORY)`)
	case "durable":
		base = append(base, `synchronous(FULL)`)
	case "fast":
		base = append(base, `synchronous(OFF)`, `temp_store(MEMORY)`)
	default:
		base = append(base, `synchronous(NORMAL)`, `temp_store(MEMORY)`)
	}
	return base
}

func applyPragmas(db *sql.DB, isNewDB bool, profile string) error {
	base := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA busy_timeout = 5000;`,
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA cache_size = -65536;`,
	}
	if isNewDB {
		// auto_vacuum is only reliably applied for a brand-new DB before any tables are created.
		// For existing DBs, switching auto_vacuum requires VACUUM; we avoid doing that implicitly.
		base = append(base, `PRAGMA auto_vacuum = INCREMENTAL;`)

		// page_size is only reliably applied for a brand-new DB before any tables are created.
		// For existing DBs, changing page_size requires VACUUM; we avoid doing that implicitly.
		base = append(base, `PRAGMA page_size = 8192;`)
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
	ctx := context.Background()
	entries, err := fs.ReadDir(migrationFS, "schema")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, entry := range entries {
		name := entry.Name()
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			continue
		}
		if err := withSQLiteBusyRetry(6*time.Second, 50*time.Millisecond, func() error {
			_, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`)
			return err
		}); err != nil {
			return err
		}

		exists, err := hasMigrationConn(ctx, conn, version)
		if err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			return err
		}
		if exists {
			if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
				return err
			}
			continue
		}
		sqlBytes, err := migrationFS.ReadFile(filepath.ToSlash(filepath.Join("schema", name)))
		if err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			return err
		}
		if _, err := conn.ExecContext(ctx, string(sqlBytes)); err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			return err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
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

func hasMigrationConn(ctx context.Context, conn *sql.Conn, version int) (bool, error) {
	var exists int
	err := conn.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, version).Scan(&exists)
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

func (s *Store) OptimizeFTS(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO symbol_fts(symbol_fts) VALUES('optimize')`)
	return time.Since(start), err
}

type WalCheckpointResult struct {
	Busy       int64  `json:"busy"`
	LogFrames  int64  `json:"log_frames"`
	CkptFrames int64  `json:"checkpointed_frames"`
	Mode       string `json:"mode"`
	DurationMS int64  `json:"duration_ms"`
}

func (s *Store) Analyze(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `ANALYZE`)
	return time.Since(start), err
}

func (s *Store) WalCheckpointTruncate(ctx context.Context) (WalCheckpointResult, error) {
	start := time.Now()
	var busy, logFrames, ckptFrames int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &ckptFrames); err != nil {
		return WalCheckpointResult{}, err
	}
	return WalCheckpointResult{
		Busy:       busy,
		LogFrames:  logFrames,
		CkptFrames: ckptFrames,
		Mode:       "TRUNCATE",
		DurationMS: time.Since(start).Milliseconds(),
	}, nil
}

func (s *Store) IncrementalVacuumAll(ctx context.Context) (beforeFreelist, afterFreelist int64, dur time.Duration, err error) {
	before, err := s.DBPragmas(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	// SQLite auto_vacuum modes:
	//   0 = NONE
	//   1 = FULL
	//   2 = INCREMENTAL (required for PRAGMA incremental_vacuum to reclaim pages)
	switch before.AutoVacuum {
	case 2:
		// ok
	case 0:
		return 0, 0, 0, fmt.Errorf("incremental vacuum requires PRAGMA auto_vacuum=INCREMENTAL (2); database is auto_vacuum=NONE (0)")
	case 1:
		return 0, 0, 0, fmt.Errorf("incremental vacuum requires PRAGMA auto_vacuum=INCREMENTAL (2); database is auto_vacuum=FULL (1)")
	default:
		return 0, 0, 0, fmt.Errorf("incremental vacuum requires PRAGMA auto_vacuum=INCREMENTAL (2); got auto_vacuum=%d", before.AutoVacuum)
	}

	start := time.Now()
	// PRAGMA incremental_vacuum without an argument attempts to remove all pages from the freelist.
	if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		return 0, 0, 0, err
	}
	after, err := s.DBPragmas(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	return before.FreelistCount, after.FreelistCount, time.Since(start), nil
}

type DBPragmas struct {
	SQLiteVersion     string `json:"sqlite_version"`
	JournalMode       string `json:"journal_mode"`
	Synchronous       string `json:"synchronous"`
	TempStore         string `json:"temp_store"`
	AutoVacuum        int64  `json:"auto_vacuum"`
	PageSize          int64  `json:"page_size"`
	PageCount         int64  `json:"page_count"`
	FreelistCount     int64  `json:"freelist_count"`
	BusyTimeoutMS     int64  `json:"busy_timeout_ms"`
	ForeignKeys       bool   `json:"foreign_keys"`
	WalAutocheckpoint int64  `json:"wal_autocheckpoint"`
	UserVersion       int64  `json:"user_version"`
	SymbolFTSPresent  bool   `json:"symbol_fts_present"`
}

func (s *Store) DBPragmas(ctx context.Context) (DBPragmas, error) {
	return QueryDBPragmas(ctx, s.db)
}

func QueryDBPragmas(ctx context.Context, db *sql.DB) (DBPragmas, error) {
	var out DBPragmas

	if err := db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&out.SQLiteVersion); err != nil {
		return DBPragmas{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&out.JournalMode); err != nil {
		return DBPragmas{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&out.Synchronous); err != nil {
		return DBPragmas{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&out.TempStore); err != nil {
		return DBPragmas{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&out.AutoVacuum); err != nil {
		return DBPragmas{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&out.PageSize); err != nil {
		return DBPragmas{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&out.PageCount); err != nil {
		return DBPragmas{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&out.FreelistCount); err != nil {
		return DBPragmas{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&out.BusyTimeoutMS); err != nil {
		return DBPragmas{}, err
	}
	var foreignKeys int64
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return DBPragmas{}, err
	}
	out.ForeignKeys = foreignKeys != 0
	if err := db.QueryRowContext(ctx, `PRAGMA wal_autocheckpoint`).Scan(&out.WalAutocheckpoint); err != nil {
		return DBPragmas{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&out.UserVersion); err != nil {
		return DBPragmas{}, err
	}

	var symbolFTSName string
	err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='symbol_fts'`).Scan(&symbolFTSName)
	if err == nil && symbolFTSName == "symbol_fts" {
		out.SymbolFTSPresent = true
	} else if errors.Is(err, sql.ErrNoRows) {
		out.SymbolFTSPresent = false
	} else if err != nil {
		return DBPragmas{}, err
	}
	return out, nil
}

// ExistingFileMeta is the slim per-row value the change-detection path
// actually consumes upfront. It carries only the (size, mtime) pair —
// the fields the indexer's fast path checks first. `content_sha256` is
// deliberately NOT in this struct: it's only consulted in the slow
// branch (size/mtime mismatch) and is fetched lazily via
// `LookupFileContentHash` so a repo-wide no-op load doesn't allocate
// one hex string per file.
type ExistingFileMeta struct {
	SizeBytes   int64
	MtimeUnixNS int64
}

// ExistingFiles returns active (non-deleted) file records for the repo,
// keyed by path with a slim `ExistingFileMeta` value. The projection is
// the fast-path minimum (`size_bytes`, `mtime_unix_ns`); `is_deleted = 0`
// is applied server-side so tombstone rows never reach the Go map. The
// content hash is fetched on demand via `LookupFileContentHash` only
// when (size, mtime) differs from disk.
func (s *Store) ExistingFiles(ctx context.Context, repoID int64) (map[string]ExistingFileMeta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, size_bytes, mtime_unix_ns
		FROM files
		WHERE repo_id = ? AND is_deleted = 0
	`, repoID)
	if err != nil {
		return nil, err
	}
	out := map[string]ExistingFileMeta{}
	if err := scanExistingFileMetasInto(rows, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ExistingFilesForPaths is the path-scoped sibling of ExistingFiles; same
// projection and same `is_deleted = 0` filter apply.
func (s *Store) ExistingFilesForPaths(ctx context.Context, repoID int64, paths []string) (map[string]ExistingFileMeta, error) {
	out := make(map[string]ExistingFileMeta, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	const chunkSize = 400
	for start := 0; start < len(paths); start += chunkSize {
		end := min(start+chunkSize, len(paths))
		chunk := paths[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			SELECT path, size_bytes, mtime_unix_ns
			FROM files
			WHERE repo_id = ? AND is_deleted = 0 AND path IN (` + placeholders + `)
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
		if err := scanExistingFileMetasInto(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LookupFileContentHash returns the stored `content_sha256` for the given
// active file path, or ("", false, nil) if no live row exists. Used by the
// indexer's slow-path "touch vs. replace" decision after a (size, mtime)
// mismatch — folding this into a per-file query keeps the upfront
// `ExistingFiles` map free of per-row hex strings.
func (s *Store) LookupFileContentHash(ctx context.Context, repoID int64, path string) (string, bool, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `
		SELECT content_sha256
		FROM files
		WHERE repo_id = ? AND is_deleted = 0 AND path = ?
	`, repoID, path).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return hash, true, nil
}

// scanExistingFileMetasInto scans rows directly into `dst` so callers can
// reuse a pre-allocated destination map across chunked queries instead of
// paying for a per-chunk map alloc + merge-loop.
func scanExistingFileMetasInto(rows *sql.Rows, dst map[string]ExistingFileMeta) error {
	defer rows.Close()
	for rows.Next() {
		var path string
		var meta ExistingFileMeta
		if err := rows.Scan(&path, &meta.SizeBytes, &meta.MtimeUnixNS); err != nil {
			return err
		}
		dst[path] = meta
	}
	return rows.Err()
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
	const chunkSize = 400
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for start := 0; start < len(paths); start += chunkSize {
		end := min(start+chunkSize, len(paths))
		chunk := paths[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			UPDATE files
			SET last_scan_id = ?, is_deleted = 0
			WHERE repo_id = ? AND path IN (` + placeholders + `)
		`
		args := make([]any, 0, len(chunk)+2)
		args = append(args, scanID, repoID)
		for _, path := range chunk {
			args = append(args, path)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
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
	result, err := s.replaceFileGraphsBatchWithStats(ctx, repoID, scanID, inputs, nil)
	return result.FileIDs, err
}

func (s *Store) ReplaceFileGraphsBatchWithStats(ctx context.Context, repoID, scanID int64, inputs []ReplaceFileGraphInput, stats *WriteStats) ([]int64, error) {
	result, err := s.replaceFileGraphsBatchWithStats(ctx, repoID, scanID, inputs, stats)
	return result.FileIDs, err
}

type ReplaceFileGraphResult struct {
	FileIDs   []int64
	SymbolIDs [][]int64
}

func (s *Store) ReplaceFileGraphsBatchWithSymbolIDs(ctx context.Context, repoID, scanID int64, inputs []ReplaceFileGraphInput, stats *WriteStats) (ReplaceFileGraphResult, error) {
	return s.replaceFileGraphsBatchWithStats(ctx, repoID, scanID, inputs, stats)
}

func (s *Store) replaceFileGraphsBatchWithStats(ctx context.Context, repoID, scanID int64, inputs []ReplaceFileGraphInput, stats *WriteStats) (ReplaceFileGraphResult, error) {
	var result ReplaceFileGraphResult
	if len(inputs) == 0 {
		return result, nil
	}
	if stats != nil {
		stats.TxCount++
		stats.FileUpsertStatements += len(inputs)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
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
		return result, err
	}
	defer upsertFileStmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	fileIDs := make([]int64, 0, len(inputs))
	for _, input := range inputs {
		var fileID int64
		if err := upsertFileStmt.QueryRowContext(ctx, repoID, input.Path, input.Language, input.SizeBytes, input.MtimeUnixNS, input.ContentHash, scanID, now).Scan(&fileID); err != nil {
			_ = tx.Rollback()
			return result, err
		}
		fileIDs = append(fileIDs, fileID)
	}

	if err := deleteFileGraphsBatch(ctx, tx, repoID, fileIDs, stats); err != nil {
		_ = tx.Rollback()
		return result, err
	}

	symbolIDs := make([][]int64, len(inputs))
	for idx, input := range inputs {
		fileID := fileIDs[idx]
		ids, err := insertParsedFileGraph(
			ctx,
			tx,
			repoID,
			fileID,
			input.Path,
			input.Parsed,
			stats,
		)
		if err != nil {
			_ = tx.Rollback()
			return result, err
		}
		symbolIDs[idx] = ids
	}

	if err := tx.Commit(); err != nil {
		return result, err
	}
	return ReplaceFileGraphResult{FileIDs: fileIDs, SymbolIDs: symbolIDs}, nil
}

// deleteFileGraphsBatch drops every row owned by the given files, including the
// symbols they define. Symbol ids are never reused (AUTOINCREMENT), so any edge
// in another file still bound to one of those symbols would become a dangling
// reference; the unbind steps below clear those inbound bindings (edges, with
// their P4 resolution metadata, and test_links.target_symbol_id) inside the
// same transaction, before the symbols go away.
func deleteFileGraphsBatch(ctx context.Context, tx *sql.Tx, repoID int64, fileIDs []int64, stats *WriteStats) error {
	if len(fileIDs) == 0 {
		return nil
	}

	// For large batches, use a temp table to avoid repeating large IN clauses across each dependent-table delete.
	// This reduces statement pressure from ~O(numTables * chunks) down to O(chunks + numTables).
	if len(fileIDs) > sqliteInClauseBatchSize {
		if err := prepareTmpDeleteFileIDs(ctx, tx, fileIDs, stats); err != nil {
			return err
		}
		return deleteFileGraphsBatchFromTemp(ctx, tx, repoID, stats)
	}

	execInChunks := func(sqlPrefix, sqlSuffix string, ids []int64, leadingArgs ...any) error {
		for start := 0; start < len(ids); start += sqliteInClauseBatchSize {
			end := start + sqliteInClauseBatchSize
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			query := sqlPrefix + sqlitePlaceholders(len(chunk)) + sqlSuffix
			args := make([]any, 0, len(chunk)+len(leadingArgs))
			args = append(args, leadingArgs...)
			for _, id := range chunk {
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
			if stats != nil {
				stats.FileGraphDeleteChunks++
				stats.FileGraphDeleteStatements++
				stats.TotalExecStatements++
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
	if err := execInChunks(`DELETE FROM scope_import_evidence WHERE file_id IN (`, `)`, fileIDs); err != nil {
		return err
	}
	if err := execInChunks(`DELETE FROM rust_module_evidence WHERE file_id IN (`, `)`, fileIDs); err != nil {
		return err
	}
	if err := execInChunks(`DELETE FROM file_scope_evidence WHERE file_id IN (`, `)`, fileIDs); err != nil {
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

	// Cross-language links are the exception to the unbind below: no call site
	// asserted them, so an unbound one states nothing a later pass could use,
	// and the implicit resolver -- which does not filter by edge_kind -- would
	// rebind it to a same-language symbol, producing exactly the cross-language
	// row with an implicit strategy the audit reports as a defect. Delete them
	// instead; ResolveCrossLanguageLinks recreates the ones the import evidence
	// still supports.
	if err := execInChunks(
		`DELETE FROM edges
		WHERE repo_id = ? AND edge_kind = '`+EdgeKindCrossLanguageRef+`'
		AND dst_symbol_id IN (SELECT id FROM symbols WHERE file_id IN (`,
		`))`,
		fileIDs,
		repoID,
	); err != nil {
		return err
	}

	// Unbind inbound edges from other files before their destination symbols
	// disappear, clearing the P4 provenance/confidence that described the now
	// dead binding. The edge row itself (dst_name, kind, evidence) survives so a
	// later resolve pass can re-bind it if a replacement symbol shows up.
	if err := execInChunks(
		`UPDATE edges SET `+resolverClearResolutionSQL+`
		WHERE repo_id = ? AND dst_symbol_id IN (SELECT id FROM symbols WHERE file_id IN (`,
		`))`,
		fileIDs,
		repoID,
	); err != nil {
		return err
	}

	// Same problem for test_links owned by other (test) files: their
	// target_symbol_id points at a symbol that is about to disappear. Unbind the
	// symbol-level target only — the row, its reason/score, and its
	// target_file_id survive so file-level RelatedTests keeps working while the
	// target file itself is still indexed. Rows whose target *file* is being
	// deleted are handled by the purge path (nullifyDeletedSymbolReferences).
	if err := execInChunks(
		`UPDATE test_links SET target_symbol_id = NULL
		WHERE repo_id = ? AND target_symbol_id IN (SELECT id FROM symbols WHERE file_id IN (`,
		`))`,
		fileIDs,
		repoID,
	); err != nil {
		return err
	}

	return execInChunks(`DELETE FROM symbols WHERE file_id IN (`, `)`, fileIDs)
}

func prepareTmpDeleteFileIDs(ctx context.Context, tx *sql.Tx, fileIDs []int64, stats *WriteStats) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_delete_file_ids(id INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	if stats != nil {
		stats.TotalExecStatements++
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_delete_file_ids`); err != nil {
		return err
	}
	if stats != nil {
		stats.TotalExecStatements++
	}

	for start := 0; start < len(fileIDs); start += sqliteInClauseBatchSize {
		end := start + sqliteInClauseBatchSize
		if end > len(fileIDs) {
			end = len(fileIDs)
		}
		chunk := fileIDs[start:end]
		placeholders := strings.Repeat("(?),", len(chunk))
		placeholders = strings.TrimSuffix(placeholders, ",")
		query := `INSERT INTO tmp_delete_file_ids(id) VALUES ` + placeholders
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
		if stats != nil {
			stats.FileGraphDeleteTempIDInsertBatches++
			stats.FileGraphDeleteTempIDInsertRows += len(chunk)
			stats.TotalExecStatements++
		}
	}

	return nil
}

func deleteFileGraphsBatchFromTemp(ctx context.Context, tx *sql.Tx, repoID int64, stats *WriteStats) error {
	exec := func(query string, args ...any) error {
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
		if stats != nil {
			stats.FileGraphDeleteStatements++
			stats.TotalExecStatements++
		}
		return nil
	}

	// Dependent tables that reference symbols must be deleted before deleting symbols.
	if err := exec(`DELETE FROM symbol_tokens WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id IN (SELECT id FROM tmp_delete_file_ids))`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM symbol_fts WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id IN (SELECT id FROM tmp_delete_file_ids))`); err != nil {
		return err
	}

	if err := exec(`DELETE FROM edges WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM references_tbl WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM file_imports WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM scope_import_evidence WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM rust_module_evidence WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM file_scope_evidence WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM file_tokens WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM test_links WHERE test_file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM symbol_embeddings WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	// See deleteFileGraphsBatch: cross-language links are deleted rather than
	// unbound, because an unbound one states nothing and would be rebound
	// same-language by the implicit resolver.
	if err := exec(`DELETE FROM edges
		WHERE repo_id = ? AND edge_kind = '`+EdgeKindCrossLanguageRef+`'
		AND dst_symbol_id IN (
			SELECT id FROM symbols WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)
		)`, repoID); err != nil {
		return err
	}
	// See deleteFileGraphsBatch: unbind inbound edges (and clear their stale
	// resolution metadata) while the destination symbols still exist.
	if err := exec(`UPDATE edges SET `+resolverClearResolutionSQL+`
		WHERE repo_id = ? AND dst_symbol_id IN (
			SELECT id FROM symbols WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)
		)`, repoID); err != nil {
		return err
	}
	// See deleteFileGraphsBatch: unbind test_links.target_symbol_id for links
	// owned by other files, keeping the row, its target_file_id and its
	// reason/score.
	if err := exec(`UPDATE test_links SET target_symbol_id = NULL
		WHERE repo_id = ? AND target_symbol_id IN (
			SELECT id FROM symbols WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)
		)`, repoID); err != nil {
		return err
	}
	if err := exec(`DELETE FROM symbols WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)`); err != nil {
		return err
	}
	return nil
}

func insertParsedFileGraph(
	ctx context.Context,
	tx *sql.Tx,
	repoID int64,
	fileID int64,
	filePath string,
	parsed graph.ParsedFile,
	stats *WriteStats,
) ([]int64, error) {
	stableToID := make(map[string]int64, len(parsed.Symbols))
	// symbolIDs[i] is the persisted id of parsed.Symbols[i]. Unlike stableToID
	// it survives colliding stable keys, so edge attribution can name the exact
	// declaration a call sits in.
	symbolIDs := make([]int64, len(parsed.Symbols))
	symbolTokenArgs := make([]any, 0, sqliteTokenValuesBatchRows*3)
	symbolTokenWeights := make(map[string]float64, 64)
	symbolFTSArgs := make([]any, 0, min(len(parsed.Symbols), sqliteSymbolFTSValuesBatchRows)*6)
	for start := 0; start < len(parsed.Symbols); start += sqliteSymbolValuesBatchRows {
		end := start + sqliteSymbolValuesBatchRows
		if end > len(parsed.Symbols) {
			end = len(parsed.Symbols)
		}
		batch := parsed.Symbols[start:end]
		batchStableToID, err := insertSymbolsBatchReturning(ctx, tx, repoID, fileID, batch, stats)
		if err != nil {
			return nil, err
		}
		for i, sym := range batch {
			symbolID, ok := batchStableToID[symbolKeyOf(sym)]
			if !ok || symbolID == 0 {
				return nil, fmt.Errorf("missing inserted id for stable_key=%q at %d:%d", sym.StableKey, sym.Range.StartLine, sym.Range.StartCol)
			}
			stableToID[sym.StableKey] = symbolID
			symbolIDs[start+i] = symbolID
			symbolFTSArgs = append(symbolFTSArgs, repoID, symbolID, sym.Name, sym.QualifiedName, sym.Signature, sym.DocSummary)
			if len(symbolFTSArgs) >= sqliteSymbolFTSValuesBatchRows*6 {
				if err := execSymbolFTSInsert(ctx, tx, symbolFTSArgs, stats); err != nil {
					return nil, err
				}
				symbolFTSArgs = symbolFTSArgs[:0]
			}
			clear(symbolTokenWeights)
			var tokenStart time.Time
			if stats != nil {
				tokenStart = time.Now()
				stats.SymbolTokenizeCalls++
			}
			texttoken.WeightsStringsInto(symbolTokenWeights, sym.Name, sym.QualifiedName, sym.Signature, sym.DocSummary)
			if stats != nil {
				stats.SymbolTokenizeNS += time.Since(tokenStart).Nanoseconds()
			}
			for token, weight := range symbolTokenWeights {
				symbolTokenArgs = append(symbolTokenArgs, symbolID, token, weight)
				if len(symbolTokenArgs) >= sqliteTokenValuesBatchRows*3 {
					if err := execTokenTriplesInsert(ctx, tx, "symbol_tokens", "symbol_id", symbolTokenArgs, stats); err != nil {
						return nil, err
					}
					symbolTokenArgs = symbolTokenArgs[:0]
				}
			}
		}
	}
	if len(symbolFTSArgs) > 0 {
		if err := execSymbolFTSInsert(ctx, tx, symbolFTSArgs, stats); err != nil {
			return nil, err
		}
	}
	if len(symbolTokenArgs) > 0 {
		if err := execTokenTriplesInsert(ctx, tx, "symbol_tokens", "symbol_id", symbolTokenArgs, stats); err != nil {
			return nil, err
		}
	}
	if len(parsed.References) > 0 {
		referenceArgs := make([]any, 0, min(len(parsed.References), sqliteReferenceValuesBatchRows)*11)
		for _, ref := range parsed.References {
			var symbolID any
			if ref.SymbolID != nil {
				symbolID = *ref.SymbolID
			}
			referenceArgs = append(
				referenceArgs,
				repoID,
				fileID,
				symbolID,
				ref.Kind,
				ref.Name,
				ref.QualifiedName,
				ref.Range.StartLine,
				ref.Range.StartCol,
				ref.Range.EndLine,
				ref.Range.EndCol,
				nil,
			)
			if len(referenceArgs) >= sqliteReferenceValuesBatchRows*11 {
				if err := execReferencesInsert(ctx, tx, referenceArgs, stats); err != nil {
					return nil, err
				}
				referenceArgs = referenceArgs[:0]
			}
		}
		if len(referenceArgs) > 0 {
			if err := execReferencesInsert(ctx, tx, referenceArgs, stats); err != nil {
				return nil, err
			}
		}
	}

	if len(parsed.Edges) > 0 {
		srcChooser := newSrcSymbolChooser(symbolIDs, parsed.Symbols)
		edgeArgs := make([]any, 0, min(len(parsed.Edges), sqliteEdgeValuesBatchRows)*7)
		for _, edge := range parsed.Edges {
			attribution := srcChooser.attribute(edge.Line)
			srcID := attribution.id
			if srcID == 0 {
				if stats != nil {
					stats.EdgeSourceDroppedUnattributable++
				}
				continue
			}
			edgeArgs = append(edgeArgs, repoID, srcID, edge.DstName, edge.Kind, edge.Evidence, fileID, edge.Line)
			if len(edgeArgs) >= sqliteEdgeValuesBatchRows*7 {
				if err := execUnresolvedEdgesInsert(ctx, tx, edgeArgs, stats); err != nil {
					return nil, err
				}
				edgeArgs = edgeArgs[:0]
			}
		}
		if len(edgeArgs) > 0 {
			if err := execUnresolvedEdgesInsert(ctx, tx, edgeArgs, stats); err != nil {
				return nil, err
			}
		}
	}

	if len(parsed.Imports) > 0 {
		importArgs := make([]any, 0, min(len(parsed.Imports), sqliteImportValuesBatchRows)*3)
		for _, imp := range parsed.Imports {
			importArgs = append(importArgs, repoID, fileID, imp)
			if len(importArgs) >= sqliteImportValuesBatchRows*3 {
				if err := execImportsInsert(ctx, tx, importArgs, stats); err != nil {
					return nil, err
				}
				importArgs = importArgs[:0]
			}
		}
		if len(importArgs) > 0 {
			if err := execImportsInsert(ctx, tx, importArgs, stats); err != nil {
				return nil, err
			}
		}
	}
	if parsed.Language == "java" || parsed.Language == "kotlin" || parsed.Scope.Package != "" || parsed.Scope.ModulePath != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO file_scope_evidence(repo_id, file_id, language, package_name, module_path) VALUES(?, ?, ?, ?, ?)`, repoID, fileID, parsed.Language, parsed.Scope.Package, parsed.Scope.ModulePath); err != nil {
			return nil, err
		}
	}
	if len(parsed.Scope.Modules) > 0 {
		args := make([]any, 0, len(parsed.Scope.Modules)*7)
		for _, module := range parsed.Scope.Modules {
			args = append(args, repoID, fileID, module.OwnerModule, module.Name, module.ExternalPath, boolInt(module.Inline), module.Visibility)
		}
		if err := execBatchInsert(ctx, tx, "rust_module_evidence", "repo_id, file_id, owner_module, module_name, external_path, is_inline, visibility", 7, args, stats); err != nil {
			return nil, err
		}
	}
	if len(parsed.Scope.Imports) > 0 {
		args := make([]any, 0, min(len(parsed.Scope.Imports), sqliteImportValuesBatchRows)*13)
		for _, evidence := range parsed.Scope.Imports {
			args = append(args, repoID, fileID, parsed.Language, evidence.SourceSpecifier, evidence.ImportedName, evidence.LocalName, evidence.Kind, boolInt(evidence.Wildcard), boolInt(evidence.Static), boolInt(evidence.ReExport), boolInt(evidence.NamespaceExport), boolInt(evidence.TypeOnly), evidence.OwnerModule)
			if len(args) >= sqliteImportValuesBatchRows*13 {
				if err := execBatchInsert(ctx, tx, "scope_import_evidence", "repo_id, file_id, language, source_specifier, imported_name, local_name, import_kind, wildcard, is_static, is_reexport, is_namespace_export, is_type_only, owner_module", 13, args, stats); err != nil {
					return nil, err
				}
				args = args[:0]
			}
		}
		if len(args) > 0 {
			if err := execBatchInsert(ctx, tx, "scope_import_evidence", "repo_id, file_id, language, source_specifier, imported_name, local_name, import_kind, wildcard, is_static, is_reexport, is_namespace_export, is_type_only, owner_module", 13, args, stats); err != nil {
				return nil, err
			}
		}
	}
	if len(parsed.FileTokens) > 0 {
		fileTokenArgs := make([]any, 0, min(len(parsed.FileTokens), sqliteTokenValuesBatchRows)*3)
		for token, weight := range parsed.FileTokens {
			fileTokenArgs = append(fileTokenArgs, fileID, token, weight)
			if len(fileTokenArgs) >= sqliteTokenValuesBatchRows*3 {
				if err := execTokenTriplesInsert(ctx, tx, "file_tokens", "file_id", fileTokenArgs, stats); err != nil {
					return nil, err
				}
				fileTokenArgs = fileTokenArgs[:0]
			}
		}
		if len(fileTokenArgs) > 0 {
			if err := execTokenTriplesInsert(ctx, tx, "file_tokens", "file_id", fileTokenArgs, stats); err != nil {
				return nil, err
			}
		}
	}

	// P10 Pass 1: persist the test-link *fact* only. The target is recorded as
	// the parser's stable key and left unbound; binding it against the symbol
	// table is a Pass-2 operation (ResolveTestLinks, resolve_test_links.go).
	//
	// Before P10 this loop looked each key up in `symbols` inside the write
	// transaction, which made the result depend on whether the target file
	// happened to be persisted earlier in the same indexing batch. `test_symbol_id`
	// stays here because it names a symbol of *this* file, inserted a few lines
	// above: it is intra-file and cannot be affected by batch order.
	//
	// P22.2: only a test file may declare test links. The producers key on the
	// function-name prefix alone, so a production file exporting
	// `TestConnection()` would otherwise mint a link that Pass 2 could bind to a
	// real production symbol -- a wrong test edge. IsTestFilePath is the single
	// shared test-file policy (P7); the parser stays free of it because path
	// classification is a store concern.
	if len(parsed.TestLinks) > 0 && IsTestFilePath(filePath) {
		testLinkArgs := make([]any, 0, min(len(parsed.TestLinks), sqliteTestLinkValuesBatchRows)*testLinkInsertCols)
		for _, link := range parsed.TestLinks {
			var testSymbolID any
			if link.TestSymbolIndex != nil {
				index := *link.TestSymbolIndex
				if index >= 0 && index < len(symbolIDs) && symbolIDs[index] != 0 &&
					parsed.Symbols[index].Kind == "function" &&
					parsed.Symbols[index].StableKey == link.TestSymbolKey {
					testSymbolID = symbolIDs[index]
				}
			}
			testLinkArgs = append(testLinkArgs, repoID, fileID, testSymbolID, nil, nil, link.Reason, link.Score, link.TargetStableKey)
			if len(testLinkArgs) >= sqliteTestLinkValuesBatchRows*testLinkInsertCols {
				if err := execTestLinksInsert(ctx, tx, testLinkArgs, stats); err != nil {
					return nil, err
				}
				testLinkArgs = testLinkArgs[:0]
			}
		}
		if len(testLinkArgs) > 0 {
			if err := execTestLinksInsert(ctx, tx, testLinkArgs, stats); err != nil {
				return nil, err
			}
		}
	}
	return symbolIDs, nil
}

// symbolRowKey identifies one inserted symbol row.
//
// stable_key alone is not enough: several adapters scope it to the module and
// omit the signature, so `func:java:Calc:add` names every overload of `add` and
// `func:typescript:http:request` names both a top-level `request` and a
// `Agent.request` method. Those are separate rows in `symbols` — there is no
// unique constraint on stable_key — and keying only by stable_key would discard
// all but one of their ids. Adding the declaration's start position separates
// them, so every symbol keeps the id of its own row.
type symbolRowKey struct {
	stableKey string
	startLine int
	startCol  int
}

func symbolKeyOf(sym graph.Symbol) symbolRowKey {
	return symbolRowKey{stableKey: sym.StableKey, startLine: sym.Range.StartLine, startCol: sym.Range.StartCol}
}

func insertSymbolsBatchReturning(ctx context.Context, tx *sql.Tx, repoID, fileID int64, symbols []graph.Symbol, stats *WriteStats) (map[symbolRowKey]int64, error) {
	if len(symbols) == 0 {
		return map[symbolRowKey]int64{}, nil
	}

	args := make([]any, len(symbols)*19)
	argIdx := 0
	for _, sym := range symbols {
		args[argIdx+0] = repoID
		args[argIdx+1] = fileID
		args[argIdx+2] = sym.Language
		args[argIdx+3] = sym.Kind
		args[argIdx+4] = sym.Name
		args[argIdx+5] = sym.QualifiedName
		args[argIdx+6] = sym.ContainerName
		args[argIdx+7] = sym.Signature
		args[argIdx+8] = sym.Visibility
		if sym.Static != nil {
			args[argIdx+9] = boolInt(*sym.Static)
		}
		args[argIdx+10] = sym.Range.StartLine
		args[argIdx+11] = sym.Range.StartCol
		args[argIdx+12] = sym.Range.EndLine
		args[argIdx+13] = sym.Range.EndCol
		args[argIdx+14] = sym.DocSummary
		args[argIdx+15] = sym.StableKey
		args[argIdx+16] = qualifiedSuffix(sym.QualifiedName)
		args[argIdx+17] = dotTail2(sym.QualifiedName)
		args[argIdx+18] = dotTail3(sym.QualifiedName)
		argIdx += 19
	}

	rows, err := tx.QueryContext(ctx, symbolInsertSQL(len(symbols)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[symbolRowKey]int64, len(symbols))
	rowCount := 0
	for rows.Next() {
		var id int64
		var key symbolRowKey
		if err := rows.Scan(&id, &key.stableKey, &key.startLine, &key.startCol); err != nil {
			return nil, err
		}
		out[key] = id
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if rowCount != len(symbols) {
		return nil, fmt.Errorf("symbol insert returned %d rows (expected %d)", rowCount, len(symbols))
	}
	if stats != nil {
		stats.SymbolInsertBatches++
		stats.SymbolInsertRows += rowCount
		stats.SymbolInserts += rowCount
		stats.TotalExecStatements++
	}
	return out, nil
}

var symbolInsertSQLCache sync.Map // map[int]string

func symbolInsertSQL(n int) string {
	if n <= 0 {
		return ""
	}
	if v, ok := symbolInsertSQLCache.Load(n); ok {
		return v.(string)
	}
	const prefix = "INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name, signature, visibility, is_static, start_line, start_col, end_line, end_col, doc_summary, stable_key, qualified_suffix, dot_tail2, dot_tail3) VALUES "
	const row = "(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	const suffix = " RETURNING id, stable_key, start_line, start_col"

	var b strings.Builder
	b.Grow(len(prefix) + n*(len(row)+1) + len(suffix))
	b.WriteString(prefix)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(row)
	}
	b.WriteString(suffix)
	s := b.String()
	actual, _ := symbolInsertSQLCache.LoadOrStore(n, s)
	return actual.(string)
}

func execSymbolFTSInsert(ctx context.Context, tx *sql.Tx, args []any, stats *WriteStats) error {
	if len(args) == 0 {
		return nil
	}
	if len(args)%6 != 0 {
		return fmt.Errorf("invalid symbol_fts insert args len=%d", len(args))
	}
	rows := len(args) / 6
	if err := execBatchInsert(ctx, tx, "symbol_fts", "repo_id, symbol_id, name, qualified_name, signature, doc_summary", 6, args, stats); err != nil {
		return err
	}
	if stats != nil {
		stats.SymbolFTSInsertBatches++
		stats.SymbolFTSInsertRows += rows
		stats.SymbolFTSInserts += rows
	}
	return nil
}

// Pre-baked column lists for the only two (table, idColumn) call sites
// (`symbol_tokens`/`symbol_id` and `file_tokens`/`file_id`). Avoids
// reallocating the column-list string per token-insert call.
const (
	symbolTokensInsertColumns = "symbol_id, token, weight"
	fileTokensInsertColumns   = "file_id, token, weight"
)

func execTokenTriplesInsert(ctx context.Context, tx *sql.Tx, table, idColumn string, args []any, stats *WriteStats) error {
	if len(args) == 0 {
		return nil
	}
	if len(args)%3 != 0 {
		return fmt.Errorf("invalid token insert args len=%d", len(args))
	}
	var columns string
	switch idColumn {
	case "symbol_id":
		columns = symbolTokensInsertColumns
	case "file_id":
		columns = fileTokensInsertColumns
	default:
		columns = idColumn + ", token, weight"
	}
	// Each row uses 3 params; batch sizes are controlled by sqliteTokenValuesBatchRows
	// to stay under sqliteDefaultMaxVariables. Stats counters (Symbol/FileToken*,
	// TotalExecStatements) are bumped exclusively in execBatchInsert to avoid
	// double-counting from this wrapper.
	return execBatchInsert(ctx, tx, table, columns, 3, args, stats)
}

type insertSQLKey struct {
	table   string
	columns string
	width   int
	rows    int
}

var insertSQLCache sync.Map // map[insertSQLKey]string

var sqlitePlaceholdersCache sync.Map // map[int]string

func sqlitePlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	if v, ok := sqlitePlaceholdersCache.Load(n); ok {
		return v.(string)
	}
	var b strings.Builder
	b.Grow(n*2 - 1)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
	}
	s := b.String()
	actual, _ := sqlitePlaceholdersCache.LoadOrStore(n, s)
	return actual.(string)
}

func execBatchInsert(ctx context.Context, tx *sql.Tx, table, columns string, rowsPerBatch int, args []any, stats *WriteStats) error {
	if len(args) == 0 {
		return nil
	}
	if len(args)%rowsPerBatch != 0 {
		return fmt.Errorf("invalid %s insert args len=%d (expected multiple of %d)", table, len(args), rowsPerBatch)
	}
	rowCount := len(args) / rowsPerBatch
	key := insertSQLKey{table: table, columns: columns, width: rowsPerBatch, rows: rowCount}
	queryAny, ok := insertSQLCache.Load(key)
	if !ok {
		// Build the row tuple once via the cached placeholder string, then
		// concatenate it rowCount times. Avoids the inner per-row loops and
		// the fmt.Fprintf allocation in the previous implementation.
		tuple := "(" + sqlitePlaceholders(rowsPerBatch) + ")"
		const prefix = "INSERT INTO "
		const sep = "("
		const valuesKw = ") VALUES "
		var b strings.Builder
		b.Grow(len(prefix) + len(table) + len(sep) + len(columns) + len(valuesKw) + rowCount*(len(tuple)+1))
		b.WriteString(prefix)
		b.WriteString(table)
		b.WriteString(sep)
		b.WriteString(columns)
		b.WriteString(valuesKw)
		for i := 0; i < rowCount; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(tuple)
		}
		queryAny, _ = insertSQLCache.LoadOrStore(key, b.String())
	}
	_, err := tx.ExecContext(ctx, queryAny.(string), args...)
	if err != nil {
		return err
	}
	if stats != nil {
		switch table {
		case "references_tbl":
			stats.ReferenceInsertBatches++
			stats.ReferenceInsertRows += rowCount
		case "edges":
			stats.EdgeInsertBatches++
			stats.EdgeInsertRows += rowCount
		case "test_links":
			stats.TestLinkInsertBatches++
			stats.TestLinkInsertRows += rowCount
		case "file_imports":
			stats.ImportInsertBatches++
			stats.ImportInsertRows += rowCount
		case "symbol_tokens":
			stats.SymbolTokenInsertBatches++
			stats.SymbolTokenInsertRows += rowCount
		case "file_tokens":
			stats.FileTokenInsertBatches++
			stats.FileTokenInsertRows += rowCount
		}
		stats.TotalExecStatements++
	}
	return nil
}

func execReferencesInsert(ctx context.Context, tx *sql.Tx, args []any, stats *WriteStats) error {
	return execBatchInsert(ctx, tx, "references_tbl", "repo_id, file_id, symbol_id, ref_kind, name, qualified_name, start_line, start_col, end_line, end_col, context_symbol_id", 11, args, stats)
}

func execUnresolvedEdgesInsert(ctx context.Context, tx *sql.Tx, args []any, stats *WriteStats) error {
	return execBatchInsert(ctx, tx, "edges", "repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line", 7, args, stats)
}

// testLinkInsertCols is the arity of the test_links insert tuple. It includes
// target_stable_key (migration 020), which Pass 2 resolves against.
const testLinkInsertCols = 8

func execTestLinksInsert(ctx context.Context, tx *sql.Tx, args []any, stats *WriteStats) error {
	return execBatchInsert(ctx, tx, "test_links", "repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score, target_stable_key", testLinkInsertCols, args, stats)
}

func execImportsInsert(ctx context.Context, tx *sql.Tx, args []any, stats *WriteStats) error {
	return execBatchInsert(ctx, tx, "file_imports", "repo_id, file_id, import_path", 3, args, stats)
}

// ownsSourceEdges reports whether a symbol kind is a code-bearing declaration
// whose body can contain the calls and references recorded as edges, and which
// may therefore be an edge's source symbol.
//
// Only "function" and "method" qualify. Every adapter emits one of those two
// for free functions, methods, constructors and accessors: most languages fold
// methods into "function" (both Go adapters, java, kotlin, csharp, ruby, php,
// cpp, rust, swift, and the non-cgo heuristic fallback), while the TypeScript
// and tree-sitter Python adapters emit "method". Every other kind the parsers
// can produce ("class", "type", "struct", "interface", "enum", "trait",
// "protocol", "actor", "object", "value") is a container or a declaration with
// no body of its own, and must never absorb a call made inside a member.
//
// This is deliberately narrower than a general "is callable" test: it answers
// only "can a reference at this line belong to this symbol's body?".
func ownsSourceEdges(kind string) bool {
	return kind == "function" || kind == "method" || kind == "constructor"
}

type funcSpan struct {
	start int
	end   int
	id    int64
}

type srcSymbolChooser struct {
	// spans holds every source-edge-owning symbol in the file, ordered by
	// start line ascending and, within one start line, by end line
	// descending. Choose relies on that order.
	spans []funcSpan
}

// newSrcSymbolChooser indexes a file's body-owning symbols by position.
// symbolIDs[i] must be the persisted id of symbols[i].
func newSrcSymbolChooser(symbolIDs []int64, symbols []graph.Symbol) srcSymbolChooser {
	// Pre-size to the upper bound of body-owning symbols (capped to keep the
	// allocation small for files with many container symbols). Avoids
	// the 16->32->64 doubling sequence for files with >16 functions.
	capHint := len(symbols)
	if capHint > 64 {
		capHint = 64
	}
	if capHint < 8 {
		capHint = 8
	}
	out := srcSymbolChooser{
		spans: make([]funcSpan, 0, capHint),
	}
	sorted := true
	for i, sym := range symbols {
		if !ownsSourceEdges(sym.Kind) {
			continue
		}
		if i >= len(symbolIDs) {
			break
		}
		id := symbolIDs[i]
		if id == 0 {
			continue
		}
		span := funcSpan{start: sym.Range.StartLine, end: sym.Range.EndLine, id: id}
		if n := len(out.spans); sorted && n > 0 && spanLess(span, out.spans[n-1]) {
			sorted = false
		}
		out.spans = append(out.spans, span)
	}
	// Adapters emit symbols in AST-walk order, which is not always sorted by
	// position (the Go adapters emit methods after top-level declarations).
	// Sorting here makes Choose independent of emission order, so the same
	// semantic file always yields the same source attribution.
	if !sorted {
		slices.SortFunc(out.spans, compareSpans)
	}
	return out
}

// spanLess orders spans by start line ascending, then by end line descending,
// so that within one start line the innermost (shortest) span sorts last.
func spanLess(a, b funcSpan) bool {
	if a.start != b.start {
		return a.start < b.start
	}
	return a.end > b.end
}

// compareSpans is spanLess as a three-way comparison, for slices.SortFunc.
// It compares rather than subtracts so the result cannot depend on the range
// of the line numbers involved.
func compareSpans(a, b funcSpan) int {
	if a.start != b.start {
		return cmp.Compare(a.start, b.start)
	}
	return cmp.Compare(b.end, a.end)
}

// Choose returns the id of the innermost body-owning symbol containing line.
//
// "Innermost" means the greatest start line that still contains the line and,
// among spans sharing that start line, the smallest end line.
//
// If two different symbols claim exactly the same span there is nothing left to
// separate them, because edges carry a line but no column. Choose fails closed
// rather than naming an ambiguous or unrelated symbol.
//
// When no span contains the line — a top-level or file-scope reference — it
// returns no owner rather than fabricating a caller.
func (c srcSymbolChooser) Choose(line int) int64 {
	return c.attribute(line).id
}

type sourceAttributionKind uint8

const (
	sourceAttributionExact sourceAttributionKind = iota + 1
	sourceAttributionAmbiguous
	sourceAttributionOutsideSpan
	sourceAttributionNoOwner
)

type sourceAttribution struct {
	kind sourceAttributionKind
	id   int64
}

func (c srcSymbolChooser) attribute(line int) sourceAttribution {
	if len(c.spans) == 0 {
		return sourceAttribution{kind: sourceAttributionNoOwner}
	}
	i := sort.Search(len(c.spans), func(i int) bool { return c.spans[i].start > line }) - 1
	// Spans are ordered by start line but can be nested or overlapping. Scan
	// backward from the last start<=line: the first containing span found has
	// the greatest start line and, within that start line, the smallest end.
	for j := i; j >= 0; j-- {
		span := c.spans[j]
		if line < span.start || line > span.end {
			continue
		}
		// Spans covering exactly these lines are contiguous under spanLess.
		// Take the whole group rather than just the neighbour, so the answer
		// cannot depend on the order equal spans happen to sit in. One
		// declaration emitted twice shares an id and is not an ambiguity; two
		// declarations on one span are.
		lo, hi := j, j
		for lo > 0 && c.spans[lo-1].start == span.start && c.spans[lo-1].end == span.end {
			lo--
		}
		for hi+1 < len(c.spans) && c.spans[hi+1].start == span.start && c.spans[hi+1].end == span.end {
			hi++
		}
		tied := false
		for k := lo; k <= hi; k++ {
			if c.spans[k].id != span.id {
				tied = true
				break
			}
		}
		if tied {
			return sourceAttribution{kind: sourceAttributionAmbiguous}
		}
		return sourceAttribution{kind: sourceAttributionExact, id: span.id}
	}
	return sourceAttribution{kind: sourceAttributionOutsideSpan}
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

// RepoHasExistingGraph reports whether this repository already holds
// relationships from an earlier scan.
//
// It is the "was this database written by an earlier run?" question the
// one-time resolver repairs need (resolver_ambiguity.go,
// resolver_type_scope.go): a repository being indexed for the very first time
// cannot hold bindings an older release made, and running a repo-wide repair
// resolve against it would only duplicate the index run's own resolve pass. The
// indexer therefore asks BEFORE pass 1 writes anything, when an empty answer
// still means "no prior graph".
func (s *Store) RepoHasExistingGraph(ctx context.Context, repoID int64) (bool, error) {
	var present int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM edges WHERE repo_id = ? LIMIT 1`, repoID).Scan(&present)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// PreviousSymbolNamesForPaths returns the distinct symbol names the given files
// declare RIGHT NOW, before a write replaces or removes them (P22.12).
//
// Uniqueness is a repository-wide property, so a name a file stops declaring
// changes the answer for edges in files the batch never touches. Those edges are
// reachable only by name, and after the write the name is gone -- so it has to
// be read while it still exists. See ResolveEdgesForPathsAndNames for what the
// names are then used for: they are trigger keys for re-decision, never
// permission to widen what a candidate may be.
//
// One indexed query per chunk of paths, matching `files.path` exactly the way
// MarkFilesDeletedBatch does. Symbols in already-deleted files are skipped: they
// are not part of the current facts and their graph rows are purged.
func (s *Store) PreviousSymbolNamesForPaths(ctx context.Context, repoID int64, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	names := map[string]struct{}{}
	for start := 0; start < len(paths); start += sqliteInClauseBatchSize {
		end := min(start+sqliteInClauseBatchSize, len(paths))
		chunk := paths[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, path := range chunk {
			args = append(args, path)
		}
		args = append(args, repoID)
		for _, path := range chunk {
			args = append(args, path)
		}
		// Driven from `files`, not from `symbols`: a `symbols.repo_id` predicate
		// makes SQLite scan the repository's whole symbol table before joining,
		// which turns a one-file save into a full-table read. The subquery form
		// takes idx_files_repo_path and then idx_symbols_file_start.
		rows, err := s.db.QueryContext(ctx, `
			SELECT name FROM (
			SELECT DISTINCT s.name AS name
			FROM symbols s
			WHERE s.name != '' AND s.file_id IN (
				SELECT id FROM files
				WHERE repo_id = ? AND is_deleted = 0
				  AND path IN (`+sqlitePlaceholders(len(chunk))+`)
			)
			UNION
			SELECT DISTINCT s.qualified_name AS name
			FROM symbols s
			WHERE s.qualified_name != '' AND s.file_id IN (
				SELECT id FROM files
				WHERE repo_id = ? AND is_deleted = 0
				  AND path IN (`+sqlitePlaceholders(len(chunk))+`)
			)
			)
		`, args...)
		if err != nil {
			return nil, err
		}
		if err := scanNamesInto(rows, names); err != nil {
			return nil, err
		}
	}
	return setToSlice(names), nil
}

// PreviousSymbolNamesForDeletedInScan is the same fact for files this scan just
// marked deleted. Their rows survive until PurgeDeletedFileGraphsForScan runs,
// which is the window this reads in -- MarkMissingDeleted never tells its caller
// which paths went away, so asking by scan is the only cheap way to see them.
func (s *Store) PreviousSymbolNamesForDeletedInScan(ctx context.Context, repoID, scanID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name FROM (
		SELECT DISTINCT s.name AS name
		FROM symbols s
		WHERE s.name != '' AND s.file_id IN (
			SELECT id FROM files
			WHERE repo_id = ? AND is_deleted = 1 AND last_scan_id = ?
		)
		UNION
		SELECT DISTINCT s.qualified_name AS name
		FROM symbols s
		WHERE s.qualified_name != '' AND s.file_id IN (
			SELECT id FROM files
			WHERE repo_id = ? AND is_deleted = 1 AND last_scan_id = ?
		)
		)
	`, repoID, scanID, repoID, scanID)
	if err != nil {
		return nil, err
	}
	names := map[string]struct{}{}
	if err := scanNamesInto(rows, names); err != nil {
		return nil, err
	}
	return setToSlice(names), nil
}

func scanNamesInto(rows *sql.Rows, out map[string]struct{}) error {
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		out[name] = struct{}{}
	}
	return rows.Err()
}

func (s *Store) MarkFilesDeletedBatch(ctx context.Context, repoID, scanID int64, paths []string) (int, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	const chunkSize = 400
	total := int64(0)
	for start := 0; start < len(paths); start += chunkSize {
		end := min(start+chunkSize, len(paths))
		chunk := paths[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			UPDATE files
			SET is_deleted = 1, parse_state = 'deleted', last_scan_id = ?
			WHERE repo_id = ? AND is_deleted = 0 AND path IN (` + placeholders + `)
		`
		args := make([]any, 0, len(chunk)+2)
		args = append(args, scanID, repoID)
		for _, path := range chunk {
			args = append(args, path)
		}
		res, err := s.db.ExecContext(ctx, query, args...)
		if err != nil {
			return int(total), err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return int(total), err
		}
		total += affected
	}
	return int(total), nil
}

// PurgeDeletedFileGraphsForScan removes dependent graph rows for files that were
// marked deleted during the given scan. This keeps stats/search/export from
// surfacing stale symbols/edges/references after file deletions while allowing
// future restores to re-index cleanly.
//
// Note: this also nulls out cross-file references to symbols defined in deleted
// files (for example edges.dst_symbol_id), so that future resolve passes can
// re-resolve them if the file returns.
func (s *Store) PurgeDeletedFileGraphsForScan(ctx context.Context, repoID, scanID int64) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id
		FROM files
		WHERE repo_id = ? AND is_deleted = 1 AND last_scan_id = ?
	`, repoID, scanID)
	if err != nil {
		return 0, err
	}
	var fileIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		fileIDs = append(fileIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(fileIDs) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		_ = tx.Rollback()
	}()

	if len(fileIDs) > sqliteInClauseBatchSize {
		if err := prepareTmpDeleteFileIDs(ctx, tx, fileIDs, nil); err != nil {
			return 0, err
		}
		if err := nullifyDeletedSymbolReferencesFromTemp(ctx, tx, repoID); err != nil {
			return 0, err
		}
		if err := deleteFileGraphsBatchFromTemp(ctx, tx, repoID, nil); err != nil {
			return 0, err
		}
	} else {
		if err := nullifyDeletedSymbolReferences(ctx, tx, repoID, fileIDs); err != nil {
			return 0, err
		}
		if err := deleteFileGraphsBatch(ctx, tx, repoID, fileIDs, nil); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true
	return len(fileIDs), nil
}

func nullifyDeletedSymbolReferences(ctx context.Context, tx *sql.Tx, repoID int64, fileIDs []int64) error {
	if len(fileIDs) == 0 {
		return nil
	}

	if len(fileIDs) > sqliteInClauseBatchSize {
		if err := prepareTmpDeleteFileIDs(ctx, tx, fileIDs, nil); err != nil {
			return err
		}
		return nullifyDeletedSymbolReferencesFromTemp(ctx, tx, repoID)
	}

	placeholders := strings.Repeat("?,", len(fileIDs))
	placeholders = strings.TrimSuffix(placeholders, ",")
	args := make([]any, 0, len(fileIDs)+1)
	args = append(args, repoID)
	for _, id := range fileIDs {
		args = append(args, id)
	}
	symbolIDs := `SELECT id FROM symbols WHERE file_id IN (` + placeholders + `)`

	// test_links pointing AT one of the deleted target files must lose that
	// target: target_file_id has no nullification path of its own, so without
	// this the row would survive pointing at a purged file and surface in
	// RelatedTests(file=...).
	//
	// P10 unbinds rather than deleting. The row is a fact *declared by the test
	// file*, which still exists and still declares it; only the target went
	// away. Deleting it destroyed target_stable_key too, so a target that merely
	// moved to another file could never rebind -- exactly the "permanently miss a
	// valid target" failure P10 exists to remove. Unbinding leaves the same
	// observable state for RelatedTests (no target file, no target symbol) while
	// letting Pass 2 rebind if the definition reappears anywhere.
	if _, err := tx.ExecContext(ctx, `
		UPDATE test_links
		SET target_file_id = NULL, target_symbol_id = NULL
		WHERE repo_id = ? AND target_file_id IN (`+placeholders+`)
	`, args...); err != nil {
		return err
	}

	// edges.dst_symbol_id and test_links.target_symbol_id are unbound by
	// deleteFileGraphsBatch itself (every symbol-removal path needs that, not
	// just this one), so they are not repeated here.
	if _, err := tx.ExecContext(ctx, `
		UPDATE references_tbl
		SET symbol_id = NULL
		WHERE repo_id = ? AND symbol_id IN (`+symbolIDs+`)
	`, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE references_tbl
		SET context_symbol_id = NULL
		WHERE repo_id = ? AND context_symbol_id IN (`+symbolIDs+`)
	`, args...); err != nil {
		return err
	}
	return nil
}

func nullifyDeletedSymbolReferencesFromTemp(ctx context.Context, tx *sql.Tx, repoID int64) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_delete_symbol_ids(id INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_delete_symbol_ids`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tmp_delete_symbol_ids(id)
		SELECT id
		FROM symbols
		WHERE file_id IN (SELECT id FROM tmp_delete_file_ids)
	`); err != nil {
		return err
	}

	// See nullifyDeletedSymbolReferences: rows with target_file_id pointing at
	// any deleted file become orphans even after target_symbol_id is nulled,
	// which would surface in RelatedTests(file=...). Unbind them up front,
	// keeping the row (and its target_stable_key) rebindable by Pass 2.
	if _, err := tx.ExecContext(ctx, `
		UPDATE test_links
		SET target_file_id = NULL, target_symbol_id = NULL
		WHERE repo_id = ? AND target_file_id IN (SELECT id FROM tmp_delete_file_ids)
	`, repoID); err != nil {
		return err
	}

	// See nullifyDeletedSymbolReferences: edges.dst_symbol_id and
	// test_links.target_symbol_id are unbound by deleteFileGraphsBatchFromTemp,
	// not here.
	if _, err := tx.ExecContext(ctx, `
		UPDATE references_tbl
		SET symbol_id = NULL
		WHERE repo_id = ? AND symbol_id IN (SELECT id FROM tmp_delete_symbol_ids)
	`, repoID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE references_tbl
		SET context_symbol_id = NULL
		WHERE repo_id = ? AND context_symbol_id IN (SELECT id FROM tmp_delete_symbol_ids)
	`, repoID); err != nil {
		return err
	}
	return nil
}

type edgeTarget struct {
	edgeID int64
	// srcLanguage is the persisted language of the edge's own source file. It
	// gates which candidate symbols may bind; an empty value fails closed.
	srcLanguage string
	// srcFileID identifies the edge's own source file. resolveEdgeTargets looks
	// it up in the repo's test-file set to decide whether this call site may bind
	// test definitions at all -- see resolver_testfile.go. Carrying the id rather
	// than a classified bool keeps IsTestFilePath called once per file per
	// resolve instead of once per edge.
	srcFileID int64
	dstName   string
	evidence  string
}

// ensureResolverAmbiguousNamesTable makes the veto table exist for the current
// transaction. Every strategy's UPDATE reads it, and the suffix strategies are
// also callable on their own (benchmarks, future callers), so each of them
// ensures it rather than assuming ResolveEdges ran first. An empty table vetoes
// nothing, which is the correct reading for a strategy invoked in isolation.
func ensureResolverAmbiguousNamesTable(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_java_scope_veto(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_kotlin_scope_veto(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+resolverCppNamespaceScopesTable+`(symbol_id INTEGER PRIMARY KEY, scope TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+resolverAmbiguousNamesTable+
		`(dst_name TEXT NOT NULL, dst_language TEXT NOT NULL, caller_is_test INTEGER NOT NULL,
			PRIMARY KEY(dst_name, dst_language, caller_is_test)) WITHOUT ROWID`); err != nil {
		return err
	}
	// The veto predicate reads the test-file set to decide which caller kind a
	// row applies to, so a strategy that ensures one table needs the other too --
	// and the bind gate reads the import scope (P22.9) for the same reason.
	if err := ensureResolverTestFilesTable(ctx, tx); err != nil {
		return err
	}
	return ensureResolverImportScopeTable(ctx, tx)
}

// prepareResolverTables builds the two per-resolve temp relations every
// repo-wide strategy reads: the test-file set (P7) and the cross-strategy
// ambiguity veto (P3). The veto aggregates classify candidates as production or
// test, so the order matters -- the test-file set is populated first.
//
// Re-indexing an already-resolved repository leaves nothing to decide, so a
// single EXISTS check skips both populations rather than scanning for candidates
// no edge is waiting on. The tables still exist (empty), which every strategy
// reads as "no test files, no vetoed names".
func (s *Store) prepareResolverTables(ctx context.Context, tx *sql.Tx, repoID int64) error {
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_java_scope_veto`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_kotlin_scope_veto`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+tsScopeVeto); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverCppNamespaceScopesTable); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE `+resolverCppNamespaceScopesTable+`(symbol_id INTEGER PRIMARY KEY, scope TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+resolverCppNamespaceScopesTable+`(symbol_id, scope)
		SELECT s.id, `+sqlCppNamespaceScope("s")+` FROM symbols s
		WHERE s.repo_id = ? AND s.language IN `+bareNameScopeAllKindsSQL, repoID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverAmbiguousNamesTable); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverTestFilesTable); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverImportScopeTable); err != nil {
		return err
	}
	// All three tables exist even on the early return below, so a strategy that
	// runs anyway reads them as "no test files, no vetoed names, no imports"
	// rather than failing.
	if err := ensureResolverAmbiguousNamesTable(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE `+tsScopeVeto+`(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return err
	}

	var hasUnresolved int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM edges
			WHERE repo_id = ? AND dst_symbol_id IS NULL AND dst_name != ''
		)
	`, repoID).Scan(&hasUnresolved); err != nil {
		return err
	}
	if hasUnresolved == 0 {
		return nil
	}
	if err := s.recordResolverTestFiles(ctx, tx, repoID); err != nil {
		return err
	}
	if err := s.recordResolverImportScope(ctx, tx, repoID); err != nil {
		return err
	}
	return s.recordAmbiguousResolverNames(ctx, tx, repoID)
}

// recordAmbiguousResolverNames fills the cross-strategy ambiguity veto table
// with the (dst_name, language, caller kind) triples that no single candidate
// serves, evaluated over the currently unresolved edge names at the two broadest
// evidence levels: exact qualified name and bare name.
//
// One row per caller kind that the level failed to decide for: a test caller is
// undecided when the group does not hold exactly one candidate, a production
// caller when it does not hold exactly one production candidate. The cross join
// against the two literal kinds keeps this at one aggregate per evidence level --
// the same two set-based scans over the same indexed columns the strategies
// themselves join on, with no per-edge or per-name query.
func (s *Store) recordAmbiguousResolverNames(ctx context.Context, tx *sql.Tx, repoID int64) error {
	// Both levels emit veto rows from the same shape: group candidate symbols by
	// (dst_name, language), count them and count the production ones, then emit
	// the caller kinds that count leaves undecided.
	vetoScopes := `
		JOIN (SELECT 0 AS caller_is_test UNION ALL SELECT 1) k
		WHERE (k.caller_is_test = 1 AND g.candidates != 1)
		   OR (k.caller_is_test = 0 AND g.production_candidates != 1)
	`
	candidateCounts := `COUNT(*) AS candidates,
			SUM(CASE WHEN tf.file_id IS NULL THEN 1 ELSE 0 END) AS production_candidates`
	ambiguityQueries := []string{
		resolverQualifiedLookupSQL + `
		INSERT OR IGNORE INTO ` + resolverAmbiguousNamesTable + `(dst_name, dst_language, caller_is_test)
		SELECT g.dst_name, g.dst_language, k.caller_is_test
		FROM (
			SELECT n.dst_name AS dst_name, s.language AS dst_language, ` + candidateCounts + `
			FROM qualified_names n
			CROSS JOIN symbols s
			  ON s.repo_id = ?
				 AND s.qualified_name = n.lookup_name
			` + resolverCandidateJoinSQL + `
			WHERE s.language != ''
			` + resolverQualifiedLookupFilter + `
			GROUP BY n.dst_name, s.language
		) g
		` + vetoScopes,
		`
		INSERT OR IGNORE INTO ` + resolverAmbiguousNamesTable + `(dst_name, dst_language, caller_is_test)
		SELECT g.dst_name, g.dst_language, k.caller_is_test
		FROM (
			SELECT n.dst_name AS dst_name, s.language AS dst_language, ` + candidateCounts + `
			FROM (
				SELECT DISTINCT dst_name
				FROM edges
				WHERE repo_id = ? AND dst_symbol_id IS NULL AND dst_name != ''
			) n
			JOIN symbols s
			  ON s.repo_id = ?
			 AND s.name = n.dst_name
			` + resolverCandidateJoinSQL + `
			WHERE ` + resolverBareNameLevelKindsSQL("s.") + `
			AND s.language != ''
			GROUP BY n.dst_name, s.language
		) g
		` + vetoScopes,
	}
	for _, query := range ambiguityQueries {
		if _, err := tx.ExecContext(ctx, query, repoID, repoID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ResolveEdges(ctx context.Context, repoID int64) (int, error) {
	// Pre-P22.9 bindings this rule refuses are cleared once per repository, so an
	// upgraded database converges instead of keeping relationships no strategy
	// here would reconsider. See resolver_type_scope.go.
	if err := s.repairTypeScopeBindingsOnce(ctx, repoID); err != nil {
		return 0, err
	}
	return s.resolveEdgesWithPreStep(ctx, repoID, nil)
}

// resolveEdgesWithPreStep is ResolveEdges with an optional statement run inside
// its transaction before any strategy does.
//
// It exists for the one-time repairs (resolver_ambiguity.go): a repair that
// clears bindings and then re-resolves must not be two commits, because a
// cancellation or an error between them would leave the repository with the
// bindings gone and nothing put back -- visibly under-resolved on every query
// until some later scan happens to succeed. Inside one transaction the pair is
// all-or-nothing.
func (s *Store) resolveEdgesWithPreStep(ctx context.Context, repoID int64, pre func(context.Context, *sql.Tx) error) (int, error) {
	totalResolved := 0
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if pre != nil {
		if err := pre(ctx, tx); err != nil {
			return 0, err
		}
	}

	// Record, once, which files are test files (P7) and which names are already
	// undecidable at the broadest evidence levels, so no later strategy can bind
	// one of them by matching a narrower slice of the same candidates. See
	// resolver_testfile.go and resolver_ambiguity.go.
	if err := s.prepareResolverTables(ctx, tx, repoID); err != nil {
		return 0, err
	}
	if n, err := resolveJavaScope(ctx, tx, repoID, nil); err != nil {
		return 0, err
	} else {
		totalResolved += n
	}
	if n, err := resolveKotlinScope(ctx, tx, repoID, nil); err != nil {
		return 0, err
	} else {
		totalResolved += n
	}
	if n, err := resolveTypeScriptScope(ctx, tx, repoID, nil); err != nil {
		return 0, err
	} else {
		totalResolved += n
	}
	if _, err := resolveRustModuleScope(ctx, tx, repoID, nil); err != nil {
		return 0, err
	}
	if targets, err := unresolvedCppEvidenceTargets(ctx, tx, repoID); err != nil {
		return 0, err
	} else if n, err := resolveCppEvidenceEdgesWith(ctx, tx, tx, repoID, targets); err != nil {
		return 0, err
	} else {
		totalResolved += n
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_own_module_veto(edge_id INTEGER PRIMARY KEY)`); err != nil {
		return 0, err
	}
	if n, _, err := s.resolveOwnModuleImports(ctx, tx, repoID, nil); err != nil {
		return 0, err
	} else {
		totalResolved += n
	}
	// Go bare calls are answered by their own package before any repo-wide
	// strategy runs, and resolverGoBareScopeSQL keeps those strategies off them
	// afterwards. See go_package_scope.go for why package scope cannot be
	// expressed as a filter on a repo-wide candidate group.
	if n, err := s.resolveGoPackageScopedBareNames(ctx, tx, repoID); err != nil {
		return 0, err
	} else {
		totalResolved += n
	}
	// Temp DDL is transactional in SQLite, so a rollback already discards these
	// tables. The explicit drop before the commit below is what keeps populated
	// tables off the pooled connection on the success path; this defer only
	// covers returns that never reach it.
	vetoDropped := false
	defer func() {
		if vetoDropped {
			return
		}
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverAmbiguousNamesTable)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverTestFilesTable)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverImportScopeTable)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_java_scope_veto`)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_kotlin_scope_veto`)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+tsScopeVeto)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_kotlin_scope_resolution`)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverCppNamespaceScopesTable)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_resolver_own_module_veto`)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_resolver_own_module_targets`)
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_resolver_own_module_resolution`)
		for _, table := range goBareScopeTables {
			_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+table)
		}
	}()

	// Strategy 1: Exact qualified name match.
	// Candidates are grouped per (dst_name, language) and applied only to edges
	// whose own source file language matches -- see resolver_language.go. A group
	// with several qualified-name matches in one language is ambiguous at this
	// strategy's evidence level and binds nothing -- see resolver_ambiguity.go.
	//
	// This is the one strategy that carries the language gate without the
	// cross-strategy ambiguity veto. The veto exists to stop a *narrower*
	// strategy from resurrecting a name a broader one could not decide; exact
	// qualified match is the broadest evidence there is, so a bare name that
	// several definitions share must not veto the one definition that owns the
	// fully qualified name. The Go-side binder makes the same call: it consults
	// the qualified candidates first and only then the bare tail.
	res, err := tx.ExecContext(ctx, resolverQualifiedLookupSQL+`,
		resolutions AS (
			SELECT n.dst_name AS dst_name, s.language AS dst_language,
				`+resolverCandidateAggregatesSQL+`
			FROM qualified_names n
			CROSS JOIN symbols s
			  ON s.repo_id = ?
				 AND s.qualified_name = n.lookup_name
			`+resolverCandidateJoinSQL+`
			WHERE s.language != ''
			`+resolverQualifiedLookupFilter+`
			GROUP BY n.dst_name, s.language
			`+resolverCandidateHavingSQL+`
		)
		UPDATE edges
		`+resolverSetResolvedSQL(ResolutionStrategyExactQualified)+`
		FROM resolutions r, files f
			`+resolverCallerTestJoinSQL+`
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL AND edges.dst_name != ''
		AND r.dst_name = edges.dst_name
		AND `+resolverBindableCandidateSQL+`
	`, repoID, repoID, repoID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalResolved += int(n)
	}

	// Strategy 2: Name match (unqualified), language-gated. A bare name defined
	// more than once in the calling language carries no evidence about which
	// definition was meant, so it stays unresolved.
	res, err = tx.ExecContext(ctx, `
		WITH distinct_names AS (
			SELECT DISTINCT dst_name
			FROM edges
			WHERE repo_id = ? AND dst_symbol_id IS NULL AND dst_name != ''
		),
		resolutions AS (
			SELECT n.dst_name AS dst_name, s.language AS dst_language,
				`+resolverCandidateAggregatesSQL+`
			FROM distinct_names n
			JOIN symbols s
			  ON s.repo_id = ?
			 AND s.name = n.dst_name
			`+resolverCandidateJoinSQL+`
			WHERE s.kind IN `+resolverBareNameKindsSQL+`
			AND s.language != ''
			GROUP BY n.dst_name, s.language
			`+resolverCandidateHavingSQL+`
		)
		UPDATE edges
		`+resolverSetResolvedSQL(ResolutionStrategyExactName)+`
		FROM resolutions r, files f
			`+resolverCallerTestJoinSQL+`
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL AND edges.dst_name != ''
		AND r.dst_name = edges.dst_name
		AND `+resolverBindGateSQL+`
	`, repoID, repoID, repoID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalResolved += int(n)
	}

	// Strategy 3a: Suffix match for slash-qualified symbols (e.g., pkg.Func matches github.com/org/repo/pkg.Func).
	// Strategy 3b: Two-segment dot-tail match (e.g., pkg.Func matches x.y.pkg.Func and also x.pkg.Func matches x.y.x.pkg.Func).
	// Avoid per-edge LIKE scans by precomputing suffix maps once, then doing indexed equality updates.
	n, err := s.resolveEdgesBySlashSuffix(ctx, tx, repoID)
	if err != nil {
		return 0, err
	}
	totalResolved += n

	// Strategy 3c: Dot-suffix fallback (for qualified names without a slash separator).
	// Keep as a narrower fallback (multi-dot dst_name only) to preserve existing semantics without paying LIKE cost on common pkg.Func cases.
	n, err = s.resolveEdgesByDotSuffix(ctx, tx, repoID)
	if err != nil {
		return 0, err
	}
	totalResolved += n

	// Strategy 4: Method receiver match (e.g., DoSomething matches MyStruct.DoSomething),
	// language-gated. Several receivers declaring the same method name are
	// indistinguishable without receiver-type evidence, so they bind nothing.
	if false { // receiver_method is legacy-only; bare calls do not prove a receiver.
		res, err = tx.ExecContext(ctx, `
		WITH distinct_names AS (
			SELECT DISTINCT dst_name
			FROM edges
			WHERE repo_id = ? AND dst_symbol_id IS NULL AND dst_name != ''
		),
		resolutions AS (
			SELECT n.dst_name AS dst_name, s.language AS dst_language,
				`+resolverCandidateAggregatesSQL+`
			FROM distinct_names n
			JOIN symbols s
			  ON s.repo_id = ?
			 AND s.name = n.dst_name
			`+resolverCandidateJoinSQL+`
			WHERE s.container_name != ''
			AND s.language != ''
			GROUP BY n.dst_name, s.language
			`+resolverCandidateHavingSQL+`
		)
		UPDATE edges
		`+resolverSetResolvedSQL(ResolutionStrategyReceiverMethod)+`
		FROM resolutions r, files f
			`+resolverCallerTestJoinSQL+`
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL AND edges.dst_name != ''
		AND r.dst_name = edges.dst_name
		AND `+resolverBindGateSQL+`
	`, repoID, repoID, repoID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			totalResolved += int(n)
		}
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverAmbiguousNamesTable); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverTestFilesTable); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverImportScopeTable); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_java_scope_veto`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverCppNamespaceScopesTable); err != nil {
		return 0, err
	}
	_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_resolver_own_module_veto`)
	_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_resolver_own_module_targets`)
	_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_resolver_own_module_resolution`)
	vetoDropped = true

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return totalResolved, nil
}

// qualifiedSuffix returns the substring of qname after the last '/' for
// slash-containing names, ” otherwise. It mirrors the resolver's `afterSlash`
// logic and is the value persisted in `symbols.qualified_suffix` (migration
// 016) so `resolveEdgesBySlashSuffix` can do an indexed equality JOIN against
// `idx_symbols_repo_qsuffix` instead of a repo-wide symbols scan + Go-side
// hash filter.
func qualifiedSuffix(qname string) string {
	if i := strings.LastIndexByte(qname, '/'); i >= 0 && i+1 < len(qname) {
		return qname[i+1:]
	}
	return ""
}

// dotTail3 returns the last three dot-separated segments of `qname`'s
// after-slash portion when that portion has at least three dots
// (i.e., at least four segments). Empty otherwise — including the case
// where `afterSlash` has exactly three segments, because such a qname
// can never match a `LIKE '%.' || dst_name` pattern (no preceding `.`
// before the leading segment). Persisted in `symbols.dot_tail3` so
// `resolveEdgesByDotSuffix`'s 2-dot dst_name path can do an indexed
// equality JOIN against `idx_symbols_repo_dot_tail3` instead of a
// repo-wide LIKE scan.
func dotTail3(qname string) string {
	afterSlash := qualifiedSuffix(qname)
	if afterSlash == "" {
		afterSlash = qname
	}
	dots := strings.Count(afterSlash, ".")
	if dots < 3 {
		return ""
	}
	rest := afterSlash
	// Strip leading "<segment>." (dots-2) times so `rest` becomes the last
	// three segments. e.g. "a.b.c.d.e" with dots=4 strips twice → "c.d.e".
	for i := 0; i < dots-2; i++ {
		idx := strings.IndexByte(rest, '.')
		if idx < 0 {
			return ""
		}
		rest = rest[idx+1:]
	}
	return rest
}

// dotTail2 returns the last two dot-separated segments of `qname`'s
// after-slash portion (matching the SQL CASE in migration 017's backfill).
// Empty when there's no dot in afterSlash. Persisted in
// `symbols.dot_tail2` so `resolveEdgesBySlashSuffix`'s dot-tail2 sub-branch
// can do an indexed equality JOIN against `idx_symbols_repo_dot_tail2`
// instead of a repo-wide symbols scan + Go-side string slicing.
func dotTail2(qname string) string {
	afterSlash := qualifiedSuffix(qname)
	if afterSlash == "" {
		afterSlash = qname
	}
	lastDot := strings.LastIndexByte(afterSlash, '.')
	if lastDot < 0 || lastDot+1 >= len(afterSlash) {
		return ""
	}
	start := 0
	if prevDot := strings.LastIndexByte(afterSlash[:lastDot], '.'); prevDot >= 0 {
		start = prevDot + 1
	}
	return afterSlash[start:]
}

func (s *Store) resolveEdgesByDotSuffix(ctx context.Context, tx *sql.Tx, repoID int64) (int, error) {
	totalResolved := 0
	if err := ensureResolverAmbiguousNamesTable(ctx, tx); err != nil {
		return 0, err
	}

	// Schema-backed prelude: 2-dot, no-slash dst_names match exactly the
	// last three segments of `afterSlash`, which is persisted in
	// `symbols.dot_tail3` (migration 018) + indexed by
	// `idx_symbols_repo_dot_tail3`. An indexed equality JOIN replaces the
	// per-distinct-dst-name LIKE scan for this dominant multi-dot case.
	// The LIKE fallback below selects dst_names with ≥2 dots, so 2-dot names
	// are offered to both passes; whatever this pass binds is simply no longer
	// unresolved when the fallback runs.
	{
		n, err := s.resolveEdgesByDotTail3(ctx, tx, repoID)
		if err != nil {
			return totalResolved, err
		}
		totalResolved += n
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_edge_dot_suffix`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_edge_dot_suffix(`+resolverCandidateColumnsDDL+`) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	dropped := false
	defer func() {
		if dropped {
			return
		}
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_edge_dot_suffix`)
	}()

	// De-correlate the expensive LIKE suffix match: do the work once per distinct
	// (dst_name, candidate language), then apply the result via an equality update
	// on edges.dst_name restricted to the edge's own source language.
	//
	// This scales with the number of unique names rather than total unresolved
	// edges. A dst_name whose LIKE pattern matches several same-language symbols
	// is ambiguous at suffix evidence level and is dropped, not tie-broken.
	if err := s.populateDotSuffixCandidates(ctx, tx, repoID); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE edges
		`+resolverSetResolvedSQL(ResolutionStrategyDotSuffix)+`
		FROM tmp_edge_dot_suffix r, files f
			`+resolverCallerTestJoinSQL+`
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL
		AND r.dst_name = edges.dst_name
		AND `+resolverBindGateSQL+`
	`, repoID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE temp.tmp_edge_dot_suffix`); err != nil {
		return 0, err
	}
	dropped = true
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	totalResolved += int(n)
	return totalResolved, nil
}

// resolveEdgesByDotTail3 handles the 2-dot, no-slash dst_name case via the
// schema-backed `symbols.dot_tail3` partial index (migration 018). The
// matching predicate stands in for the LIKE pattern
// `qualified_name LIKE '%.' || dst_name` on 2-dot dst_names, but is bound
// to an equality JOIN that scales with the unique-needed-name count
// instead of total symbols. It is not literally the same predicate: LIKE is
// ASCII case-insensitive and treats `_`/`%` inside dst_name as wildcards, so
// this pass is the stricter of the two. Reordering the passes would therefore
// change results, not just performance.
func (s *Store) resolveEdgesByDotTail3(ctx context.Context, tx *sql.Tx, repoID int64) (int, error) {
	if err := ensureResolverAmbiguousNamesTable(ctx, tx); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_needed_tail3(dst_name TEXT PRIMARY KEY)`); err != nil {
		return 0, err
	}
	droppedNeeded := false
	defer func() {
		if droppedNeeded {
			return
		}
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS tmp_resolver_needed_tail3`)
	}()
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolver_needed_tail3`); err != nil {
		return 0, err
	}

	// Seed with distinct unresolved dst_names that have exactly 2 dots and
	// no slash. (Length-of-string minus length-of-dotless-string == 2 dots.)
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO tmp_resolver_needed_tail3(dst_name)
		SELECT DISTINCT dst_name
		FROM edges
		WHERE repo_id = ? AND dst_symbol_id IS NULL
		AND instr(dst_name, '/') = 0
		AND length(dst_name) - length(replace(dst_name, '.', '')) = 2
	`, repoID); err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_symbol_dot_tail3`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_symbol_dot_tail3(`+resolverCandidateColumnsDDL+`) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	droppedDotTail3 := false
	defer func() {
		if droppedDotTail3 {
			return
		}
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_symbol_dot_tail3`)
	}()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tmp_symbol_dot_tail3(dst_name, dst_language, any_symbol_id, production_symbol_id)
		SELECT n.dst_name, s.language,
			`+resolverCandidateAggregatesSQL+`
		FROM tmp_resolver_needed_tail3 n
		JOIN symbols s
		  ON s.repo_id = ?
		 AND s.dot_tail3 = n.dst_name
		`+resolverCandidateJoinSQL+`
		WHERE s.dot_tail3 != '' AND s.language != ''
		GROUP BY n.dst_name, s.language
		`+resolverCandidateHavingSQL+`
	`, repoID); err != nil {
		return 0, err
	}

	updateRes, err := tx.ExecContext(ctx, `
		UPDATE edges
		`+resolverSetResolvedSQL(ResolutionStrategyDotTail3)+`
		FROM tmp_symbol_dot_tail3 r, files f
			`+resolverCallerTestJoinSQL+`
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL
		AND r.dst_name = edges.dst_name
		AND `+resolverBindGateSQL+`
	`, repoID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE temp.tmp_symbol_dot_tail3`); err != nil {
		return 0, err
	}
	droppedDotTail3 = true
	if _, err := tx.ExecContext(ctx, `DROP TABLE tmp_resolver_needed_tail3`); err != nil {
		return 0, err
	}
	droppedNeeded = true
	n, err := updateRes.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *Store) resolveEdgesBySlashSuffix(ctx context.Context, tx *sql.Tx, repoID int64) (int, error) {
	// Restrict suffix maps to names that can actually be consumed by the unresolved edge set.
	// This avoids building large Go maps for symbols that can't match any current unresolved edges.
	neededSuffix := map[string]struct{}{}
	neededTail2 := map[string]struct{}{}
	needRows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT dst_name
		FROM edges
		WHERE repo_id = ? AND dst_symbol_id IS NULL AND dst_name != '' AND instr(dst_name, '/') = 0
	`, repoID)
	if err != nil {
		return 0, err
	}
	for needRows.Next() {
		var dstName string
		if err := needRows.Scan(&dstName); err != nil {
			_ = needRows.Close()
			return 0, err
		}
		if dstName == "" {
			continue
		}
		neededSuffix[dstName] = struct{}{}
		firstDot := strings.IndexByte(dstName, '.')
		if firstDot >= 0 && firstDot == strings.LastIndexByte(dstName, '.') {
			// Dot-tail2 optimization only applies to dst_name values with exactly one dot.
			neededTail2[dstName] = struct{}{}
		}
	}
	if err := needRows.Err(); err != nil {
		_ = needRows.Close()
		return 0, err
	}
	if err := needRows.Close(); err != nil {
		return 0, err
	}
	// Skip the full symbols scan entirely when no current unresolved edge can
	// be satisfied by either suffix strategy. This is the common case after
	// upstream strategies have already absorbed the resolvable edges.
	if len(neededSuffix) == 0 {
		return 0, nil
	}
	// Ensured only past the early exit: a run with nothing to resolve should not
	// pay for DDL it will never read.
	if err := ensureResolverAmbiguousNamesTable(ctx, tx); err != nil {
		return 0, err
	}
	totalResolved := 0

	// Slash-suffix path: indexed equality JOIN against the persisted
	// `qualified_suffix` column (`idx_symbols_repo_qsuffix`, migration 016)
	// instead of a `SELECT id, qualified_name FROM symbols WHERE repo_id = ?`
	// scan + Go-side neededSuffix hash filter. Suffixes shared by several
	// same-language symbols bind nothing: the suffix is the whole evidence this
	// strategy has, and it does not distinguish them.
	{
		needNames := make([]string, 0, len(neededSuffix))
		for name := range neededSuffix {
			needNames = append(needNames, name)
		}

		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_needed_suffix(dst_name TEXT PRIMARY KEY)`); err != nil {
			return 0, err
		}
		droppedNeeded := false
		defer func() {
			if droppedNeeded {
				return
			}
			_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS tmp_resolver_needed_suffix`)
		}()
		if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolver_needed_suffix`); err != nil {
			return 0, err
		}

		// Keep well under SQLite's default variable limit (999).
		const maxPerInsert = 400
		for start := 0; start < len(needNames); start += maxPerInsert {
			end := min(start+maxPerInsert, len(needNames))
			chunk := needNames[start:end]
			var b strings.Builder
			b.WriteString(`INSERT OR IGNORE INTO tmp_resolver_needed_suffix(dst_name) VALUES `)
			args := make([]any, 0, len(chunk))
			for i, name := range chunk {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString("(?)")
				args = append(args, name)
			}
			if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
				return 0, err
			}
		}

		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_symbol_slash_suffix`); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_symbol_slash_suffix(`+resolverCandidateColumnsDDL+`) WITHOUT ROWID`); err != nil {
			return 0, err
		}
		droppedSlashSuffix := false
		defer func() {
			if droppedSlashSuffix {
				return
			}
			_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_symbol_slash_suffix`)
		}()

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tmp_symbol_slash_suffix(dst_name, dst_language, any_symbol_id, production_symbol_id)
			SELECT n.dst_name, s.language,
				`+resolverCandidateAggregatesSQL+`
			FROM tmp_resolver_needed_suffix n
			JOIN symbols s
			  ON s.repo_id = ?
			 AND s.qualified_suffix = n.dst_name
			`+resolverCandidateJoinSQL+`
			WHERE s.qualified_suffix != '' AND s.language != ''
			GROUP BY n.dst_name, s.language
			`+resolverCandidateHavingSQL+`
		`, repoID); err != nil {
			return 0, err
		}

		if false { // slash_suffix is legacy-only; current parsers cannot prove it.
			updateRes, err := tx.ExecContext(ctx, `
			UPDATE edges
			`+resolverSetResolvedSQL(ResolutionStrategySlashSuffix)+`
			FROM tmp_symbol_slash_suffix r, files f
			`+resolverCallerTestJoinSQL+`
			WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL
			AND r.dst_name = edges.dst_name
			AND `+resolverBindGateSQL+`
		`, repoID)
			if err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `DROP TABLE temp.tmp_symbol_slash_suffix`); err != nil {
				return 0, err
			}
			droppedSlashSuffix = true
			if _, err := tx.ExecContext(ctx, `DROP TABLE tmp_resolver_needed_suffix`); err != nil {
				return 0, err
			}
			droppedNeeded = true
			n, err := updateRes.RowsAffected()
			if err != nil {
				return 0, err
			}
			totalResolved += int(n)
		}
	}

	// Dot-tail2 path: this branch matches `last-2-dot-segments(afterSlash)`.
	// Schema-backed by `symbols.dot_tail2` (migration 017) + partial index
	// `idx_symbols_repo_dot_tail2`, so the same matching is now an indexed
	// equality JOIN instead of a Go-side full repo symbols scan + per-row
	// string slicing.
	if len(neededTail2) == 0 {
		return totalResolved, nil
	}
	{
		needNames := make([]string, 0, len(neededTail2))
		for name := range neededTail2 {
			needNames = append(needNames, name)
		}

		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_needed_tail2(dst_name TEXT PRIMARY KEY)`); err != nil {
			return 0, err
		}
		droppedNeededTail2 := false
		defer func() {
			if droppedNeededTail2 {
				return
			}
			_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS tmp_resolver_needed_tail2`)
		}()
		if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolver_needed_tail2`); err != nil {
			return 0, err
		}

		const maxPerInsert = 400
		for start := 0; start < len(needNames); start += maxPerInsert {
			end := min(start+maxPerInsert, len(needNames))
			chunk := needNames[start:end]
			var b strings.Builder
			b.WriteString(`INSERT OR IGNORE INTO tmp_resolver_needed_tail2(dst_name) VALUES `)
			args := make([]any, 0, len(chunk))
			for i, name := range chunk {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString("(?)")
				args = append(args, name)
			}
			if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
				return 0, err
			}
		}

		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_symbol_dot_tail2`); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_symbol_dot_tail2(`+resolverCandidateColumnsDDL+`) WITHOUT ROWID`); err != nil {
			return 0, err
		}
		droppedDotTail2 := false
		defer func() {
			if droppedDotTail2 {
				return
			}
			_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_symbol_dot_tail2`)
		}()

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tmp_symbol_dot_tail2(dst_name, dst_language, any_symbol_id, production_symbol_id)
			SELECT n.dst_name, s.language,
				`+resolverCandidateAggregatesSQL+`
			FROM tmp_resolver_needed_tail2 n
			JOIN symbols s
			  ON s.repo_id = ?
			 AND s.dot_tail2 = n.dst_name
			`+resolverCandidateJoinSQL+`
			WHERE s.dot_tail2 != '' AND s.language != ''
			GROUP BY n.dst_name, s.language
			`+resolverCandidateHavingSQL+`
		`, repoID); err != nil {
			return 0, err
		}

		updateRes, err := tx.ExecContext(ctx, `
			UPDATE edges
			`+resolverSetResolvedSQL(ResolutionStrategyDotTail2)+`
			FROM tmp_symbol_dot_tail2 r, files f
			`+resolverCallerTestJoinSQL+`
			WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL
			AND instr(edges.dst_name, '.') > 0 AND instr(edges.dst_name, '/') = 0
			AND instr(substr(edges.dst_name, instr(edges.dst_name, '.') + 1), '.') = 0
			AND r.dst_name = edges.dst_name
			AND `+resolverBindGateSQL+`
		`, repoID)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE temp.tmp_symbol_dot_tail2`); err != nil {
			return 0, err
		}
		droppedDotTail2 = true
		if _, err := tx.ExecContext(ctx, `DROP TABLE tmp_resolver_needed_tail2`); err != nil {
			return 0, err
		}
		droppedNeededTail2 = true
		n, err := updateRes.RowsAffected()
		if err != nil {
			return 0, err
		}
		totalResolved += int(n)
	}

	return totalResolved, nil
}

func (s *Store) ResolveEdgesForPaths(ctx context.Context, repoID int64, paths []string) error {
	return s.resolveEdgesForPaths(ctx, repoID, paths, nil, nil)
}

// ResolveEdgesForPathsAndNames shares one module discovery pass across the two
// incremental resolver scopes. Own-module edges are resolved repo-wide first;
// path/name scopes then handle remaining evidence without another WalkDir.
func (s *Store) ResolveEdgesForPathsAndNames(ctx context.Context, repoID int64, paths, names []string) (ResolveEdgesForNamesStats, error) {
	// Invalidate before anything re-binds: a binding this batch may have made
	// ambiguous has to be reconsidered, not merely left alone. It runs first so
	// the module pass below can immediately re-bind the own-module edges it
	// clears, instead of finding them vetoed and unresolved.
	// A changed file's IMPORT list is evidence too since P22.9, and it changes
	// the answer for files the batch does not otherwise mention -- so the names
	// whose visibility those edits could have flipped join the batch before
	// anything is invalidated. See typeScopeNamesForChangedPaths.
	if err := s.repairTypeScopeBindingsOnce(ctx, repoID); err != nil {
		return ResolveEdgesForNamesStats{}, err
	}
	scopes := newImportScopeCache(s, repoID)
	var err error
	scopes.rustRoots, err = s.rustRootsForPaths(ctx, repoID, paths)
	if err != nil {
		return ResolveEdgesForNamesStats{}, err
	}
	if err := s.invalidateRustBindingsForRoots(ctx, repoID, scopes.rustRoots); err != nil {
		return ResolveEdgesForNamesStats{}, err
	}
	rustNames, err := s.rustNamesForChangedPaths(ctx, repoID, scopes.rustRoots)
	if err != nil {
		return ResolveEdgesForNamesStats{}, err
	}
	names = mergeResolverNames(names, rustNames)
	tsNames, err := s.invalidateTypeScriptScopeBindings(ctx, repoID, paths)
	if err != nil {
		return ResolveEdgesForNamesStats{}, err
	}
	names = mergeResolverNames(names, tsNames)
	scopeNames, err := s.typeScopeNamesForChangedPaths(ctx, repoID, paths, scopes)
	if err != nil {
		return ResolveEdgesForNamesStats{}, err
	}
	names = mergeResolverNames(names, scopeNames)

	invalidateStarted := time.Now()
	invalidated, err := s.invalidateNameEvidenceBindings(ctx, repoID, names)
	if err != nil {
		return ResolveEdgesForNamesStats{}, err
	}
	invalidateMS := time.Since(invalidateStarted).Milliseconds()
	moduleVeto, err := s.resolveOwnModuleImportsStandalone(ctx, repoID, nil)
	if err != nil {
		return ResolveEdgesForNamesStats{}, err
	}
	if err := s.resolveEdgesForPaths(ctx, repoID, paths, moduleVeto, scopes); err != nil {
		return ResolveEdgesForNamesStats{}, err
	}
	stats, err := s.resolveEdgesForNamesWithStats(ctx, repoID, names, moduleVeto, scopes)
	if err == nil && (len(paths) > 0 || len(names) > 0) {
		var n int
		n, err = s.resolveDotSuffixIncrementally(ctx, repoID)
		stats.TargetsResolved += n
	}
	stats.InvalidateMS += invalidateMS
	stats.InvalidatedBindings += invalidated
	return stats, err
}

// resolveDotSuffixIncrementally reruns only the active weak strategy after the
// ordinary incremental passes. Its candidate population is the repository's
// unresolved dotted edges; stronger strategies have already had first refusal.
// The transaction-local resolver tables keep the full and incremental SQL
// predicates identical, while no repo-wide resolver pass is repeated.
func (s *Store) resolveDotSuffixIncrementally(ctx context.Context, repoID int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.prepareResolverTables(ctx, tx, repoID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_own_module_veto(edge_id INTEGER PRIMARY KEY)`); err != nil {
		return 0, err
	}
	n, err := s.resolveEdgesByDotSuffix(ctx, tx, repoID)
	if err != nil {
		return 0, err
	}
	for _, table := range []string{
		resolverAmbiguousNamesTable, resolverTestFilesTable,
		resolverImportScopeTable, resolverCppNamespaceScopesTable,
		"tmp_resolver_own_module_veto", "tmp_resolver_own_module_targets",
		"tmp_resolver_own_module_resolution",
	} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+table); err != nil {
			return 0, err
		}
	}
	for _, table := range goBareScopeTables {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+table); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// invalidateNameEvidenceBindings unbinds implicit bindings whose uniqueness
// evidence this batch may have changed (P22.8).
//
// Every implicit strategy binds only when its candidate set holds exactly one
// eligible symbol. That is a property of the whole repository, not of one file,
// so a declaration arriving anywhere can invalidate a binding made anywhere
// else. The incremental resolver was additive: an edge lost its destination
// only when that destination disappeared (deleteFileGraphs), so a binding made
// while a name was unique survived the arrival of a second declaration and the
// graph ended up asserting a relationship a fresh index refuses. That is
// indexing history acting as semantic evidence, which is exactly what a
// content-addressed graph must never do.
//
// The scope is the batch's own names, not the repository: a declaration ARRIVING
// under a name puts that name in `names`, and a declaration disappearing already
// unbinds the edges that pointed at it (deleteFileGraphs). See
// ResolveEdgesForPathsAndNames for the ordering that lets the following passes
// re-decide every cleared edge.
//
// The opposite direction -- a declaration REMOVED -- is answered outside this
// function, and deliberately so (P22.12). The indexer now also collects the
// names a replaced or deleted file previously declared and passes OLD union NEW
// to ResolveEdgesForPathsAndNames, so a vanished name reaches the resolver. It
// does not reach THIS pass in any meaningful way, and must not: a name with a
// single declaration left cannot have made anything ambiguous, which is exactly
// what namesWithSeveralDeclarations filters out below. Removal is answered by
// re-deciding unresolved edges, not by clearing bound ones.
//
// Two exclusions, for opposite reasons:
//
//   - `cross_language_ref` edges are explicit P19b links, bound by their own
//     pass from import-bridge evidence. An implicit name pass neither owns nor
//     can restore them. The strategy predicate below already excludes their two
//     strategies, so the edge_kind guard is a second, independent line: this
//     pass must not become the one place that can silently delete a proven
//     cross-language relationship because someone widened a list.
//   - Strategies outside incrementallyRedecidableStrategies -- `receiver_method`,
//     `slash_suffix`, `dot_suffix` -- are produced by the repo-wide ResolveEdges
//     alone. Clearing one would delete a destination that no incremental pass
//     can rebuild, so the update would end up MISSING an edge the fresh index
//     keeps: a new divergence, in the more damaging direction. Their bindings
//     can therefore still go stale in the way described above; that is the
//     pre-existing behaviour, and narrowing it is a separate decision from
//     this one.
func (s *Store) invalidateNameEvidenceBindings(ctx context.Context, repoID int64, names []string) (int, error) {
	wanted := make(map[string]struct{}, len(names))
	unique := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := wanted[name]; ok {
			continue
		}
		wanted[name] = struct{}{}
		unique = append(unique, name)
	}
	if len(unique) == 0 {
		return 0, nil
	}

	// Only a name that is now declared MORE THAN ONCE can have invalidated
	// anything. Every implicit strategy binds on uniqueness, so a name with a
	// single declaration left cannot have made a binding ambiguous: either that
	// declaration is the binding's own destination, or the binding pointed at a
	// symbol that no longer exists -- and a vanished destination is unbound by
	// deleteFileGraphs, not here.
	//
	// This is a superset test (it ignores language and kind, and counts
	// qualified-name bindings under their bare tail), which is what it has to
	// be: over-approximating costs an extra re-decision that reaches the same
	// answer, while under-approximating would leave a stale binding. It is one
	// indexed query on idx_symbols_repo_name, and on the overwhelmingly common
	// single-file save it comes back empty and skips the work below entirely --
	// which took a one-file mitmproxy update from +25ms to nothing measurable.
	contested, err := s.namesWithSeveralDeclarations(ctx, repoID, unique)
	if err != nil {
		return 0, err
	}
	// C/C++ declaration evidence changes visibility even when the remaining
	// name is unique: a bound caller can lose its only header proof after a
	// declaration is removed or renamed. Reconsider such edges on every OLD ∪
	// NEW name, while retaining the contested-name fast path for other languages.
	cppArgs := make([]any, 1, len(unique)+1)
	cppArgs[0] = repoID
	for _, name := range unique {
		cppArgs = append(cppArgs, name)
	}
	cppRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT e.dst_name
		FROM edges e JOIN files f ON f.id = e.file_id
		WHERE e.repo_id = ? AND f.language = 'cpp' AND e.dst_name IN (`+strings.TrimRight(strings.Repeat("?,", len(unique)), ",")+`)`, cppArgs...)
	if err != nil {
		return 0, err
	}
	cppNames := map[string]struct{}{}
	for cppRows.Next() {
		var name string
		if err := cppRows.Scan(&name); err != nil {
			cppRows.Close()
			return 0, err
		}
		cppNames[name] = struct{}{}
	}
	if err := cppRows.Err(); err != nil {
		cppRows.Close()
		return 0, err
	}
	if err := cppRows.Close(); err != nil {
		return 0, err
	}
	legacyStale := map[int64]struct{}{}
	legacyArgs := make([]any, 0, len(unique)+1)
	legacyArgs = append(legacyArgs, repoID)
	for _, name := range unique {
		legacyArgs = append(legacyArgs, name)
	}
	legacyRows, err := s.db.QueryContext(ctx, `
		SELECT id FROM edges
		WHERE repo_id = ? AND dst_symbol_id IS NOT NULL
		AND resolution_strategy IN ('receiver_method', 'slash_suffix')
		  AND dst_name IN (`+strings.TrimRight(strings.Repeat("?,", len(unique)), ",")+
		`)`, legacyArgs...)
	if err != nil {
		return 0, err
	}
	if err := scanEdgeIDsInto(legacyRows, legacyStale); err != nil {
		return 0, err
	}
	allNames := append([]string(nil), unique...)
	allNames = append(allNames, contested...)
	seenNames := make(map[string]struct{}, len(allNames))
	for _, name := range allNames {
		seenNames[name] = struct{}{}
	}
	for name := range cppNames {
		if _, ok := seenNames[name]; !ok {
			allNames = append(allNames, name)
		}
	}
	unique = allNames
	wanted = make(map[string]struct{}, len(unique))
	for _, name := range unique {
		wanted[name] = struct{}{}
	}

	// Two selections, mirroring the name pass exactly so that everything
	// cleared is also reconsidered: indexed equality on the whole spelling,
	// then one pass over the qualified bound population for `.<name>` tails.
	// Migration 028 keeps the second one off a full table scan.
	stale := map[int64]struct{}{}
	for id := range legacyStale {
		stale[id] = struct{}{}
	}
	for start := 0; start < len(unique); start += sqliteInClauseBatchSize {
		end := min(start+sqliteInClauseBatchSize, len(unique))
		chunk := unique[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT id FROM edges
			WHERE repo_id = ? AND dst_symbol_id IS NOT NULL
			  AND edge_kind != '`+EdgeKindCrossLanguageRef+`'
			  AND resolution_strategy IN `+sqlQuotedList(incrementallyRedecidableStrategies)+`
			  AND dst_name IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
		`, args...)
		if err != nil {
			return 0, err
		}
		if err := scanEdgeIDsInto(rows, stale); err != nil {
			return 0, err
		}
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, dst_name FROM edges
		WHERE repo_id = ? AND dst_symbol_id IS NOT NULL
		  AND edge_kind != '`+EdgeKindCrossLanguageRef+`'
		  AND resolution_strategy IN `+sqlQuotedList(incrementallyRedecidableStrategies)+`
		  AND dst_name != '' AND instr(dst_name, '.') > 0
	`, repoID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id int64
		var dstName string
		if err := rows.Scan(&id, &dstName); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if dot := strings.LastIndexByte(dstName, '.'); dot >= 0 && dot+1 < len(dstName) {
			if _, ok := wanted[dstName[dot+1:]]; ok {
				stale[id] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}

	ids := make([]int64, 0, len(stale))
	for id := range stale {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	// One transaction for the whole unbind. The passes that rebind these edges
	// run afterwards in their own transactions, so a partially committed clear
	// would leave destinations dropped with nothing scheduled to restore them
	// until some later batch happened to mention the same names again.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, chunk := range chunkInt64s(ids, sqliteInClauseBatchSize) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		args = append(args, int64SliceToAny(chunk)...)
		if _, err := tx.ExecContext(ctx, `
			UPDATE edges SET `+resolverClearResolutionSQL+`
			WHERE repo_id = ? AND id IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
		`, args...); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// namesWithSeveralDeclarations returns those of `names` that more than one
// symbol in the repository declares. Order follows the input, so the caller's
// later statement text is a function of its own argument and not of row order.
func (s *Store) namesWithSeveralDeclarations(ctx context.Context, repoID int64, names []string) ([]string, error) {
	contested := make(map[string]struct{}, len(names))
	for start := 0; start < len(names); start += sqliteInClauseBatchSize {
		end := min(start+sqliteInClauseBatchSize, len(names))
		chunk := names[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT name FROM symbols
			WHERE repo_id = ? AND name IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
			GROUP BY name
			HAVING COUNT(*) > 1
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return nil, err
			}
			contested[name] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(contested))
	for _, name := range names {
		if _, ok := contested[name]; ok {
			out = append(out, name)
			delete(contested, name)
		}
	}
	return out, nil
}

// scanEdgeIDsInto collects edge ids from a query into set.
func scanEdgeIDsInto(rows *sql.Rows, set map[int64]struct{}) error {
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		set[id] = struct{}{}
	}
	return rows.Err()
}

func (s *Store) resolveEdgesForPaths(ctx context.Context, repoID int64, paths []string, moduleVeto map[int64]struct{}, scopes *importScopeCache) error {
	if len(paths) == 0 {
		return nil
	}
	uniquePaths := make([]string, 0, len(paths))
	seenPaths := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		scopePath := CanonicalRelPath(path)
		if scopePath == "" {
			continue
		}
		if _, ok := seenPaths[scopePath]; ok {
			continue
		}
		seenPaths[scopePath] = struct{}{}
		uniquePaths = append(uniquePaths, scopePath)
	}
	modulePaths := make(map[string]struct{}, len(uniquePaths))
	for _, path := range uniquePaths {
		modulePaths[path] = struct{}{}
	}
	var err error
	if moduleVeto == nil {
		moduleVeto, err = s.resolveOwnModuleImportsStandalone(ctx, repoID, &ownModuleScope{paths: modulePaths})
		if err != nil {
			return err
		}
	}

	const chunkSize = 400
	fileIDs := make([]int64, 0, len(uniquePaths))
	wantedPaths := make(map[string]struct{}, len(uniquePaths))
	storedPaths := make([]string, 0, len(uniquePaths)*3)
	for _, filePath := range uniquePaths {
		canonical := filePath
		wantedPaths[canonical] = struct{}{}
		storedPaths = append(storedPaths, storedPathVariants(canonical)...)
	}
	seenFileIDs := make(map[int64]struct{}, len(uniquePaths))
	// Source language per file is read once here (no per-edge lookup) and carried
	// into resolveEdgeTargets, which applies the shared language gate.
	languageByFileID := make(map[int64]string, len(uniquePaths))
	for start := 0; start < len(storedPaths); start += sqliteInClauseBatchSize {
		end := min(start+sqliteInClauseBatchSize, len(storedPaths))
		chunk := storedPaths[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `SELECT id, path, language FROM files WHERE repo_id = ? AND path IN (` + placeholders + `)`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, storedPath := range chunk {
			args = append(args, storedPath)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var storedPath string
			var language string
			if err := rows.Scan(&id, &storedPath, &language); err != nil {
				_ = rows.Close()
				return err
			}
			if _, ok := wantedPaths[canonicalStoredPath(storedPath)]; !ok {
				continue
			}
			if _, ok := seenFileIDs[id]; ok {
				continue
			}
			seenFileIDs[id] = struct{}{}
			fileIDs = append(fileIDs, id)
			languageByFileID[id] = language
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
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
		query := `SELECT id, dst_name, file_id, evidence FROM edges WHERE repo_id = ? AND dst_symbol_id IS NULL AND file_id IN (` + placeholders + `)`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		chunkTargets, err := scanEdgeTargets(rows, languageByFileID)
		if err != nil {
			return err
		}
		targets = append(targets, chunkTargets...)
	}
	_, err = s.resolveEdgeTargets(ctx, repoID, targets, moduleVeto, scopes)
	return err
}

// ResolveEdgesForNames attempts to resolve currently-unresolved edges across the
// repo whose dst_name matches (or ends with ".<name>") any of the provided
// names. This is used to keep incremental update runs correct when newly
// introduced symbols should resolve previously-unresolved edges in other files.
//
// It returns the number of candidate edges selected for resolution.
func (s *Store) ResolveEdgesForNames(ctx context.Context, repoID int64, names []string) (int, error) {
	stats, err := s.ResolveEdgesForNamesWithStats(ctx, repoID, names)
	if err != nil {
		return 0, err
	}
	return stats.TargetsSelected, nil
}

func (s *Store) ResolveEdgesForNamesWithStats(ctx context.Context, repoID int64, names []string) (ResolveEdgesForNamesStats, error) {
	return s.resolveEdgesForNamesWithStats(ctx, repoID, names, nil, nil)
}

func (s *Store) resolveEdgesForNamesWithStats(ctx context.Context, repoID int64, names []string, moduleVeto map[int64]struct{}, scopes *importScopeCache) (ResolveEdgesForNamesStats, error) {
	var stats ResolveEdgesForNamesStats
	if len(names) == 0 {
		return stats, nil
	}
	stats.NamesInput = len(names)
	seen := make(map[string]struct{}, len(names))
	unique := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		unique = append(unique, name)
	}
	if len(unique) == 0 {
		return stats, nil
	}
	stats.NamesUnique = len(unique)
	rustFilter := ""
	var rustFilterArgs []any
	if scopes != nil {
		if len(scopes.rustRoots) == 0 {
			rustFilter = " AND f.language <> 'rust'"
		} else {
			roots := make([]string, 0, len(scopes.rustRoots))
			for root := range scopes.rustRoots {
				roots = append(roots, root)
			}
			slices.Sort(roots)
			parts := make([]string, 0, len(roots)*2)
			for _, root := range roots {
				dir := root[:strings.LastIndex(root, "/")+1]
				parts = append(parts, "se.crate_root=? OR (se.crate_root='' AND (f.path=? OR f.path LIKE ?))")
				rustFilterArgs = append(rustFilterArgs, root, root, dir+"%")
			}
			rustFilter = " AND (f.language <> 'rust' OR (" + strings.Join(parts, " OR ") + "))"
		}
	}
	if moduleVeto == nil {
		// Standalone entry point: no caller ran the invalidation pass, so this
		// one owns it. It must precede the module pass below for the ordering
		// reason ResolveEdgesForPathsAndNames documents.
		invalidateStarted := time.Now()
		invalidated, err := s.invalidateNameEvidenceBindings(ctx, repoID, unique)
		if err != nil {
			return stats, err
		}
		stats.InvalidateMS = time.Since(invalidateStarted).Milliseconds()
		stats.InvalidatedBindings = invalidated
	}
	moduleNames := make(map[string]struct{}, len(unique))
	for _, name := range unique {
		moduleNames[name] = struct{}{}
		if scope := strings.LastIndex(name, "::"); scope >= 0 && scope+2 < len(name) {
			moduleNames[name[scope+2:]] = struct{}{}
			seen[name[scope+2:]] = struct{}{}
		}
		if dot := strings.LastIndexByte(name, '.'); dot >= 0 && dot+1 < len(name) {
			moduleNames[name[dot+1:]] = struct{}{}
		}
	}
	var err error
	if moduleVeto == nil {
		moduleVeto, err = s.resolveOwnModuleImportsStandalone(ctx, repoID, &ownModuleScope{names: moduleNames})
		if err != nil {
			return stats, err
		}
	}

	// Candidate selection:
	//
	// 1) Use indexed exact matches for dst_name = <name> (covers the common case
	//    where unresolved edges reference the simple symbol name directly).
	// 2) Only if needed, scan unresolved edges that contain a '.' and filter in Go
	//    for suffix matches (dst_name ends with ".<name>").
	//
	// This avoids scanning the full unresolved-edge set on large repos where many
	// unresolved edges have simple (non-qualified) dst_name values.
	targetByID := make(map[int64]edgeTarget, 64)

	exactStarted := time.Now()
	// Keep under sqliteDefaultMaxVariables (repoID + N names).
	for start := 0; start < len(unique); start += sqliteInClauseBatchSize {
		end := min(start+sqliteInClauseBatchSize, len(unique))
		chunk := unique[start:end]

		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		// The files join is one primary-key join per batch (not per edge) and
		// supplies the source language required by the shared resolver gate. The
		// caller's own file id travels with it so resolveEdgeTargets can classify
		// it against the repo's test-file set.
		query := `
			SELECT e.id, e.dst_name, f.language, e.file_id, e.evidence
			FROM edges e
			JOIN files f ON f.id = e.file_id
			LEFT JOIN file_scope_evidence se ON se.file_id=f.id AND se.repo_id=f.repo_id
			WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND e.dst_name IN (` + placeholders + `)` + rustFilter
		args := make([]any, 1+len(chunk))
		args[0] = repoID
		for i, name := range chunk {
			args[i+1] = name
		}
		args = append(args, rustFilterArgs...)

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return stats, err
		}
		for rows.Next() {
			var id int64
			var dstName string
			var srcLanguage string
			var srcFileID int64
			var evidence string
			if err := rows.Scan(&id, &dstName, &srcLanguage, &srcFileID, &evidence); err != nil {
				_ = rows.Close()
				return stats, err
			}
			targetByID[id] = edgeTarget{
				edgeID:      id,
				dstName:     dstName,
				srcLanguage: srcLanguage,
				srcFileID:   srcFileID,
				evidence:    evidence,
			}
			stats.ExactHits++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return stats, err
		}
		if err := rows.Close(); err != nil {
			return stats, err
		}
		stats.ExactQueryBatches++
	}
	stats.ExactSelectMS = time.Since(exactStarted).Milliseconds()

	// Suffix matching requires looking at qualified dst_name values. Keep this
	// as a single pass over the qualified unresolved set (no repeated LIKE
	// queries), but avoid scanning simple dst_name values entirely. C++ uses
	// `::` rather than `.`, so its final component must join the same update
	// batch when a declaration arrives after its caller.
	suffixStarted := time.Now()
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.dst_name, f.language, e.file_id, e.evidence
		FROM edges e
		JOIN files f ON f.id = e.file_id
		LEFT JOIN file_scope_evidence se ON se.file_id=f.id AND se.repo_id=f.repo_id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND e.dst_name != '' AND (instr(e.dst_name, '.') > 0 OR instr(e.dst_name, '::') > 0)`+rustFilter, append([]any{repoID}, rustFilterArgs...)...)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var id int64
		var dstName string
		var srcLanguage string
		var srcFileID int64
		var evidence string
		if err := rows.Scan(&id, &dstName, &srcLanguage, &srcFileID, &evidence); err != nil {
			_ = rows.Close()
			return stats, err
		}
		stats.QualifiedScanned++
		if _, ok := targetByID[id]; ok {
			continue
		}
		last := strings.LastIndexByte(dstName, '.')
		if scope := strings.LastIndex(dstName, "::"); scope > last {
			last = scope
		}
		if last >= 0 && last+1 < len(dstName) {
			if _, ok := seen[dstName[last+1:]]; ok {
				targetByID[id] = edgeTarget{
					edgeID:      id,
					dstName:     dstName,
					srcLanguage: srcLanguage,
					srcFileID:   srcFileID,
					evidence:    evidence,
				}
				stats.SuffixHits++
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return stats, err
	}
	if err := rows.Close(); err != nil {
		return stats, err
	}
	stats.SuffixSelectMS = time.Since(suffixStarted).Milliseconds()

	targets := make([]edgeTarget, 0, len(targetByID))
	for _, target := range targetByID {
		targets = append(targets, target)
	}
	stats.TargetsSelected = len(targets)
	resolveStarted := time.Now()
	outcome, err := s.resolveEdgeTargets(ctx, repoID, targets, moduleVeto, scopes)
	if err != nil {
		return stats, err
	}
	stats.TargetsResolved = outcome.resolved
	stats.TargetsUnresolved = outcome.unresolved
	stats.LanguageBlocked = outcome.languageBlocked
	stats.AmbiguityBlocked = outcome.ambiguityBlocked
	stats.TestShadowBlocked = outcome.testShadowBlocked
	stats.UnknownSrcLanguage = outcome.unknownSrcLanguage
	stats.RustAffectedCrates = outcome.rustStats.AffectedCrates
	stats.RustAffectedModules = outcome.rustStats.AffectedModules
	stats.RustAffectedEdges = outcome.rustStats.AffectedEdges
	stats.RustCandidateRows = outcome.rustStats.CandidateRows
	stats.RustReExportNodesVisited = outcome.rustStats.ReExportNodesVisited
	stats.RustBatchInvalidationOps = outcome.rustStats.BatchInvalidationOps
	stats.RustBatchApplyOps = outcome.rustStats.BatchApplyOps
	stats.ResolveTargetsMS = time.Since(resolveStarted).Milliseconds()
	return stats, nil
}

func scanEdgeTargets(rows *sql.Rows, languageByFileID map[int64]string) ([]edgeTarget, error) {
	defer rows.Close()
	var targets []edgeTarget
	for rows.Next() {
		var target edgeTarget
		if err := rows.Scan(&target.edgeID, &target.dstName, &target.srcFileID, &target.evidence); err != nil {
			return nil, err
		}
		target.srcLanguage = languageByFileID[target.srcFileID]
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return targets, nil
}

func (s *Store) CountUnresolvedEdgesByDstName(ctx context.Context, repoID int64, dstName string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM edges
		WHERE repo_id = ? AND dst_symbol_id IS NULL AND dst_name = ?
	`, repoID, dstName).Scan(&n)
	return n, err
}

// CountDanglingEdgeTargets reports how many edges in the repo still carry a
// dst_symbol_id whose symbol row no longer exists. The referential invariant is
// that dst_symbol_id either points at a live symbol or is NULL, so this must be
// zero after any indexing, re-indexing, or purge run.
func (s *Store) CountDanglingEdgeTargets(ctx context.Context, repoID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM edges e
		WHERE e.repo_id = ?
		  AND e.dst_symbol_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM symbols s WHERE s.id = e.dst_symbol_id)
	`, repoID).Scan(&n)
	return n, err
}

// resolveEdgeTargetsOutcome reports what the shared Go-side resolver actually
// did, so callers can distinguish real resolutions from candidates that were
// selected but rejected by the language gate.
type resolveEdgeTargetsOutcome struct {
	resolved int
	// unresolved counts targets with a known source language that found no
	// compatible candidate, for any reason.
	unresolved int
	// languageBlocked is the subset of unresolved whose name was seen only under
	// a different language. It is a lower bound: it is observable only for names
	// the lookup actually queried.
	languageBlocked int
	// ambiguityBlocked is the subset of unresolved that had several
	// same-language candidates and no evidence separating them. These two
	// counters are disjoint: a target blocked by ambiguity in its own language
	// is not reported as language-blocked.
	ambiguityBlocked int
	// testShadowBlocked is the subset of unresolved where the calling file is
	// production code and every same-language candidate was declared in a test
	// file. Disjoint from the two counters above: the name was found, in the
	// right language, and was not ambiguous -- it was simply not something
	// production code may be wired into.
	testShadowBlocked  int
	unknownSrcLanguage int
	rustStats          RustResolutionStats
}

// binderFallback decides which evidence level a dst_name may fall back to when
// its exact qualified lookup finds nothing, and returns the lookup value plus
// the symbols column it must be matched against ("" means no safe fallback --
// the edge stays unresolved).
//
// The classification is by the dst_name's own syntax (P22.1):
//
//   - Bare names (`Close`) use the bare-name lookup, restricted to the kinds a
//     call edge can denote -- the same candidate set and the same reported
//     strategy (exact_name) as the repo-wide bare-name pass, since P22.8.
//   - Import-path spellings (containing '/') abstain (P22.8). They are
//     package-qualified by construction, and the package they name is decided
//     by explicit evidence: resolveOwnModuleImports maps the import path
//     against the repository's own `go.mod` files and binds the exact package
//     target (module_import), vetoing everything weaker for that edge. An
//     import path that does NOT map into the module names something outside
//     the repository -- `database/sql.Open` is stdlib, `github.com/acme/lib.Open`
//     is a dependency -- and the project's own unique `Open` is simply not what
//     was called. Degrading to the tail was the pre-P22.5 stopgap this comment
//     used to defer; it survived into P22.8 as the single largest source of
//     full-index/incremental divergence, and it never produced a binding the
//     repo-wide resolver agreed with (measured: 0 on CodeGraph, pprof and
//     mitmproxy, against 18 wrong ones). Non-Go parsers also emit slash-bearing
//     spellings for ordinary expressions (`(dir / "x").open`), where the tail is
//     not an identifier the call names either.
//   - Member/scope-qualified spellings (a '.' or '::', no slash) carry their
//     qualifier as evidence. `rows.Close` does not mean a unique project
//     method named Close; it means a member of `rows`, which CodeGraph may
//     only bind when the destination's own identity confirms the qualifier.
//     One dot falls back to the persisted dot_tail2, two dots to dot_tail3 --
//     the same equality evidence the repo-wide strategies use. Deeper or
//     ::-scoped spellings have no schema-backed tail and abstain (repo-wide
//     keeps its low-confidence LIKE fallback for the multi-dot forms).
//
// One known, deliberate gap against the repo-wide strategies: the dot-tail
// fallbacks do not consult the bare-level ambiguity veto (reachable only when
// a symbol's bare `name` itself contains a dot -- the pre-existing strategy-set
// gap class documented in resolver_ambiguity.go). The slash-qualified identity
// case is closed: no adapter emits slash-qualified `qualified_name`s, and the
// slash-qualified *spelling* is owned by module_import (P22.5).
func binderFallback(dstName string) (lookup, column string) {
	name := strings.TrimSpace(dstName)
	if name == "" {
		return "", ""
	}
	if strings.ContainsRune(name, '/') {
		return "", ""
	}
	if strings.Contains(name, "::") {
		return "", ""
	}
	switch strings.Count(name, ".") {
	case 0:
		return name, "name"
	case 1:
		return name, "dot_tail2"
	case 2:
		return name, "dot_tail3"
	default:
		return "", ""
	}
}

// binderFallbackForTarget keeps exact C++ type-like spellings on the same
// name-evidence level as the repo-wide resolver. Their punctuation is part of
// the symbol name, not a dot-tail qualifier.
func binderFallbackForTarget(target edgeTarget) (lookup, column string) {
	if target.srcLanguage == "cpp" && cppEvidenceTarget(target) && !strings.Contains(target.dstName, "::") {
		return target.dstName, "name"
	}
	return binderFallback(target.dstName)
}

func setToSlice(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	return out
}

// resolveEdgeTargets is the single Go-side binder shared by the path-scoped and
// name-targeted resolvers. Candidate lookups are keyed by (name, language) and
// a target only binds when resolverLanguageCompatible allows it, which is the
// same rule the repo-wide SQL strategies apply.
func (s *Store) resolveEdgeTargets(ctx context.Context, repoID int64, targets []edgeTarget, moduleVeto map[int64]struct{}, scopes *importScopeCache) (resolveEdgeTargetsOutcome, error) {
	var outcome resolveEdgeTargetsOutcome
	if len(targets) == 0 {
		return outcome, nil
	}
	rustIDs := map[int64]struct{}{}
	for _, target := range targets {
		if target.srcLanguage == "rust" {
			rustIDs[target.edgeID] = struct{}{}
		}
	}
	if len(rustIDs) > 0 {
		// The changed-file path pass has already selected the crate root. Keep
		// that selection on the rewritten caller evidence before the standalone
		// Rust pass derives its bounded file set.
		if scopes != nil && len(scopes.rustRoots) == 1 {
			var root string
			for candidate := range scopes.rustRoots {
				root = candidate
			}
			for _, target := range targets {
				if target.srcLanguage != "rust" {
					continue
				}
				if _, err := s.db.ExecContext(ctx, `UPDATE file_scope_evidence SET crate_root=? WHERE repo_id=? AND file_id=? AND crate_root=''`, root, repoID, target.srcFileID); err != nil {
					return outcome, err
				}
			}
		}
		bound, err := s.resolveRustModuleScopeStandaloneWithStats(ctx, repoID, rustIDs, &outcome.rustStats)
		if err != nil {
			return outcome, err
		}
		outcome.resolved += len(bound)
		remaining := targets[:0]
		for _, target := range targets {
			if target.srcLanguage != "rust" {
				remaining = append(remaining, target)
			}
		}
		targets = remaining
	}
	if n, err := s.resolveCppEvidenceEdges(ctx, repoID, targets); err != nil {
		return outcome, err
	} else {
		outcome.resolved += n
	}
	remaining := targets[:0]
	cppIDs := make([]int64, 0, len(targets))
	for _, target := range targets {
		if cppEvidenceTarget(target) {
			cppIDs = append(cppIDs, target.edgeID)
			continue
		}
		remaining = append(remaining, target)
	}
	if len(cppIDs) > 0 {
		stillUnresolved := map[int64]struct{}{}
		for _, chunk := range chunkInt64s(cppIDs, sqliteInClauseBatchSize) {
			args := append([]any{repoID}, int64SliceToAny(chunk)...)
			rows, err := s.db.QueryContext(ctx, `SELECT id FROM edges WHERE repo_id = ? AND dst_symbol_id IS NULL AND id IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)`, args...)
			if err != nil {
				return outcome, err
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return outcome, err
				}
				stillUnresolved[id] = struct{}{}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return outcome, err
			}
			rows.Close()
		}
		for _, target := range targets {
			if _, ok := stillUnresolved[target.edgeID]; ok {
				remaining = append(remaining, target)
			}
		}
	}
	targets = remaining
	if len(targets) == 0 {
		return outcome, nil
	}
	javaIDs := make(map[int64]struct{})
	javaFiles := make(map[int64]struct{})
	for _, target := range targets {
		if target.srcLanguage != "java" {
			continue
		}
		javaFiles[target.srcFileID] = struct{}{}
	}
	if len(javaFiles) > 0 {
		ids := make([]string, 0, len(javaFiles))
		for id := range javaFiles {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		rows, err := s.db.QueryContext(ctx, `SELECT file_id FROM file_scope_evidence WHERE repo_id=? AND file_id IN (`+strings.Join(ids, ",")+`)`, repoID)
		if err != nil {
			return outcome, err
		}
		seenFiles := map[int64]struct{}{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return outcome, err
			}
			seenFiles[id] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return outcome, err
		}
		for _, target := range targets {
			if _, ok := seenFiles[target.srcFileID]; ok {
				javaIDs[target.edgeID] = struct{}{}
			}
		}
	}
	if len(javaIDs) > 0 {
		n, err := resolveJavaScope(ctx, s.db, repoID, javaIDs)
		if err != nil {
			return outcome, err
		}
		outcome.resolved += n
		remaining = targets[:0]
		for _, target := range targets {
			if target.srcLanguage != "java" {
				remaining = append(remaining, target)
				continue
			}
			if _, scoped := javaIDs[target.edgeID]; !scoped {
				remaining = append(remaining, target)
			}
		}
		targets = remaining
	}
	kotlinIDs := make(map[int64]struct{})
	kotlinFiles := make(map[int64]struct{})
	for _, target := range targets {
		if target.srcLanguage == "kotlin" {
			kotlinFiles[target.srcFileID] = struct{}{}
		}
	}
	if len(kotlinFiles) > 0 {
		ids := make([]string, 0, len(kotlinFiles))
		for id := range kotlinFiles {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		rows, err := s.db.QueryContext(ctx, `SELECT file_id FROM file_scope_evidence WHERE repo_id=? AND language='kotlin' AND file_id IN (`+strings.Join(ids, ",")+`)`, repoID)
		if err != nil {
			return outcome, err
		}
		seenKotlinFiles := map[int64]struct{}{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return outcome, err
			}
			seenKotlinFiles[id] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return outcome, err
		}
		for _, target := range targets {
			if _, ok := seenKotlinFiles[target.srcFileID]; ok {
				kotlinIDs[target.edgeID] = struct{}{}
			}
		}
	}
	if len(kotlinIDs) > 0 {
		n, err := resolveKotlinScope(ctx, s.db, repoID, kotlinIDs)
		if err != nil {
			return outcome, err
		}
		outcome.resolved += n
		remaining = targets[:0]
		for _, target := range targets {
			if target.srcLanguage != "kotlin" {
				remaining = append(remaining, target)
				continue
			}
			if _, scoped := kotlinIDs[target.edgeID]; !scoped {
				remaining = append(remaining, target)
			}
		}
		targets = remaining
	}
	tsIDs := make(map[int64]struct{})
	for _, target := range targets {
		if target.srcLanguage == "typescript" {
			tsIDs[target.edgeID] = struct{}{}
		}
	}
	if len(tsIDs) > 0 {
		n, err := resolveTypeScriptScope(ctx, s.db, repoID, tsIDs)
		if err != nil {
			return outcome, err
		}
		outcome.resolved += n
		remaining = targets[:0]
		for _, target := range targets {
			if target.srcLanguage != "typescript" {
				remaining = append(remaining, target)
			}
		}
		targets = remaining
	}
	if len(targets) == 0 {
		return outcome, nil
	}
	// P22.6: a bare spelling in a Go file is answered by the calling symbol's own
	// package and by nothing else, so those targets take a separate lookup and
	// never consult the repo-wide candidate maps below. This is the Go-side twin
	// of resolveGoPackageScopedBareNames; the two must agree edge for edge.
	goBare := make(map[int64]struct{}, len(targets))
	goBareNameSet := map[string]struct{}{}
	goBareEdgeIDs := make([]int64, 0, len(targets))
	for _, target := range targets {
		if _, blocked := moduleVeto[target.edgeID]; blocked {
			continue
		}
		if strings.HasPrefix(target.evidence, "macro_unexpanded:") {
			outcome.unresolved++
			continue
		}
		if target.srcLanguage != "go" || !goBareCallName(target.dstName) {
			continue
		}
		goBare[target.edgeID] = struct{}{}
		goBareEdgeIDs = append(goBareEdgeIDs, target.edgeID)
		goBareNameSet[target.dstName] = struct{}{}
	}

	qualifiedSet := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if _, blocked := moduleVeto[target.edgeID]; blocked {
			continue
		}
		if _, bare := goBare[target.edgeID]; bare {
			continue
		}
		if target.dstName != "" {
			qualifiedSet[target.dstName] = struct{}{}
		}
	}
	qualifiedNames := make([]string, 0, len(qualifiedSet))
	for name := range qualifiedSet {
		qualifiedNames = append(qualifiedNames, name)
	}
	// The repo's test-file set is read once per resolve and shared by both
	// candidate lookups, so no candidate needs its own path or classification.
	testFileIDs, err := testFileIDsForRepo(ctx, s.db, repoID)
	if err != nil {
		return outcome, err
	}
	byQualified, err := s.resolveSymbolsByQualifiedNames(ctx, repoID, qualifiedNames, testFileIDs)
	if err != nil {
		return outcome, err
	}

	goBareScopes, err := goSymbolScopesByEdgeSource(ctx, s.db, repoID, goBareEdgeIDs)
	if err != nil {
		return outcome, err
	}
	goBareScopeKeys := make(map[string]struct{}, len(goBareScopes))
	for _, scope := range goBareScopes {
		if scope.key != goPackageScopeUnknown {
			goBareScopeKeys[scope.key] = struct{}{}
		}
	}
	goBareGroups, err := goBareCandidateGroups(ctx, s.db, repoID, goBareScopeKeys, setToSlice(goBareNameSet), testFileIDs)
	if err != nil {
		return outcome, err
	}

	shortSet := make(map[string]struct{}, len(targets))
	tail2Set := map[string]struct{}{}
	tail3Set := map[string]struct{}{}
	for _, target := range targets {
		if _, bare := goBare[target.edgeID]; bare {
			continue
		}
		key := symbolLangKey{name: target.dstName, language: target.srcLanguage}
		if _, ok := byQualified.groups[key]; ok {
			// The qualified name matched something in the caller's language, so
			// no weaker evidence level of the same name is ever consulted:
			// either the qualified evidence identifies one definition this
			// caller may bind, or it identified candidates it may not, and
			// falling back to strictly weaker evidence could only pick among
			// the same set arbitrarily -- or, worse, retarget an explicit
			// reference at an unrelated symbol. Mirrors the qualified-level
			// veto rows in recordAmbiguousResolverNames.
			continue
		}
		fallbackName, column := binderFallbackForTarget(target)
		if fallbackName == "" {
			continue
		}
		switch column {
		case "name":
			shortSet[fallbackName] = struct{}{}
		case "dot_tail2":
			tail2Set[fallbackName] = struct{}{}
		case "dot_tail3":
			tail3Set[fallbackName] = struct{}{}
		}
	}
	byShort, err := s.resolveUniqueSymbolsByNames(ctx, repoID, setToSlice(shortSet), testFileIDs)
	if err != nil {
		return outcome, err
	}
	byTail2, err := s.resolveSymbolCandidates(ctx, repoID, "dot_tail2", setToSlice(tail2Set), testFileIDs)
	if err != nil {
		return outcome, err
	}
	byTail3, err := s.resolveSymbolCandidates(ctx, repoID, "dot_tail3", setToSlice(tail3Set), testFileIDs)
	if err != nil {
		return outcome, err
	}
	// The P22.9 import scope, built only when a type candidate is actually in
	// play. It costs one scan of `files` and one of `file_imports`, and the
	// overwhelming majority of incremental batches resolve nothing but callables,
	// so the guard keeps that off the common path entirely.
	var importScope map[int64]map[int64]struct{}
	if len(byQualified.typeSymbolFiles) > 0 || len(byShort.typeSymbolFiles) > 0 {
		importScope, err = scopes.get(ctx, s, repoID)
		if err != nil {
			return outcome, err
		}
	}

	// P22.15: a bare C/C++ spelling may not claim another class's member
	// (cpp_class_scope.go). Both halves are batched -- one indexed lookup per
	// distinct bare name, one per chunk of bare C/C++ edges -- and both are built
	// only when the batch actually holds such an edge, so a Go-only or
	// Python-only update pays nothing for the rule.
	var cppMemberTargets, cppCallerClasses map[int64]string
	var cppNamespaceTargets, cppCallerNamespaces map[int64]string
	cppBareEdgeIDs := make([]int64, 0, len(targets))
	cppBareNameSet := map[string]struct{}{}
	for _, target := range targets {
		if _, blocked := moduleVeto[target.edgeID]; blocked {
			continue
		}
		if !bareNameScopeAllKinds(target.srcLanguage) || !goBareCallName(target.dstName) {
			continue
		}
		cppBareEdgeIDs = append(cppBareEdgeIDs, target.edgeID)
		cppBareNameSet[target.dstName] = struct{}{}
	}
	if len(cppBareEdgeIDs) > 0 {
		cppMemberTargets, err = cppClassScopesByName(ctx, s.db, repoID, setToSlice(cppBareNameSet))
		if err != nil {
			return outcome, err
		}
		cppCallerClasses, err = cppCallerClassScopesByEdge(ctx, s.db, repoID, cppBareEdgeIDs)
		if err != nil {
			return outcome, err
		}
		cppNamespaceTargets, err = cppNamespaceScopesByName(ctx, s.db, repoID, setToSlice(cppBareNameSet))
		if err != nil {
			return outcome, err
		}
		cppCallerNamespaces, err = cppNamespaceScopesByEdge(ctx, s.db, repoID, cppBareEdgeIDs)
		if err != nil {
			return outcome, err
		}
	}

	// Names that exist somewhere in the repo, in any language and whether or not
	// they are ambiguous there. Used only to report honestly why a candidate
	// stayed unresolved.
	qualifiedNamesAnyLanguage := byQualified.names()
	shortNamesAnyLanguage := byShort.names()
	for name := range byTail2.names() {
		shortNamesAnyLanguage[name] = struct{}{}
	}
	for name := range byTail3.names() {
		shortNamesAnyLanguage[name] = struct{}{}
	}

	type edgeResolution struct {
		edgeID int64
		dstID  int64
		// strategy names the evidence level that actually bound this edge, so
		// the UPDATE below can persist it alongside dst_symbol_id. The binder
		// has five: the Go package-scoped bare pass (P22.6), the qualified-name
		// lookup, the dot_tail2/dot_tail3 member fallbacks, and the bare-name
		// lookup (binderFallback picks which one a dst_name may consult).
		// binderStrategies is the registered set.
		strategy string
	}
	resolutions := make([]edgeResolution, 0, len(targets))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outcome, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_kotlin_scope_veto(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return outcome, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_kotlin_scope_veto`); err != nil {
		return outcome, err
	}
	for _, target := range targets {
		if _, blocked := moduleVeto[target.edgeID]; blocked {
			continue
		}
		fallbackName, fallbackColumn := binderFallbackForTarget(target)
		if target.srcLanguage == "" {
			// Unknown source language: never guess a destination language.
			// Candidate maps only contain non-empty languages, so this is also
			// what resolverLanguageCompatible would decide.
			outcome.unknownSrcLanguage++
			continue
		}
		qualifiedKey := symbolLangKey{name: target.dstName, language: target.srcLanguage}
		fallbackKey := symbolLangKey{name: fallbackName, language: target.srcLanguage}
		// Which candidates this edge may bind depends on whether the *calling*
		// file is a test file, exactly as resolverChosenCandidateSQL decides it
		// for the repo-wide strategies. See resolver_testfile.go.
		_, callerIsTest := testFileIDs[target.srcFileID]
		if _, bare := goBare[target.edgeID]; bare {
			// One evidence level, one candidate set: the calling symbol's own Go
			// package. No fallback, because there is nothing weaker a bare Go
			// identifier is entitled to.
			var matchedGroup candidateGroup
			var matched bool
			if scope, known := goBareScopes[target.edgeID]; known && scope.key != goPackageScopeUnknown {
				matchedGroup, matched = goBareGroups[goScopeName{scope: scope.key, name: target.dstName}]
			}
			dstID, ok := int64(0), false
			if matched {
				dstID, ok = matchedGroup.chosen(callerIsTest)
			}
			if !ok || dstID == 0 {
				outcome.unresolved++
				switch {
				case matched && matchedGroup.ambiguousFor(callerIsTest):
					outcome.ambiguityBlocked++
				case matched:
					outcome.testShadowBlocked++
				}
				continue
			}
			resolutions = append(resolutions, edgeResolution{
				edgeID:   target.edgeID,
				dstID:    dstID,
				strategy: ResolutionStrategyGoPackageScope,
			})
			continue
		}
		strategy := ResolutionStrategyExactQualified
		matchedGroup, matched := byQualified.groups[qualifiedKey]
		if !matched {
			// Only when the qualified name matched nothing in this language may
			// the fallback evidence level be consulted -- and which level that
			// is depends on the dst_name's own syntax (binderFallback): bare
			// names use the kind-restricted name lookup, member spellings
			// must confirm their qualifier against the destination's dot-tail
			// identity, and spellings with no safe fallback abstain.
			switch fallbackColumn {
			case "name":
				matchedGroup, matched = byShort.groups[fallbackKey]
				strategy = ResolutionStrategyExactName
			case "dot_tail2":
				matchedGroup, matched = byTail2.groups[fallbackKey]
				strategy = ResolutionStrategyDotTail2
			case "dot_tail3":
				matchedGroup, matched = byTail3.groups[fallbackKey]
				strategy = ResolutionStrategyDotTail3
			}
		}
		dstID, ok := int64(0), false
		if matched {
			dstID, ok = matchedGroup.chosen(callerIsTest)
		}
		if ok && dstID != 0 && typeScopeGatedLanguage(target.srcLanguage) && goBareCallName(target.dstName) &&
			(byQualified.typeTargetOutOfScope(dstID, target.srcFileID, importScope) ||
				byShort.typeTargetOutOfScope(dstID, target.srcFileID, importScope)) {
			// A bare spelling naming a type the calling file neither declares nor
			// imports. Repository-global uniqueness is not scope evidence for a
			// class name (P22.9, resolver_type_scope.go), and there is nothing
			// weaker to fall back to, so the edge stays unresolved -- exactly what
			// resolverBareNameTypeScopeSQL does on the full path.
			//
			// Gated on the spelling rather than on the strategy so the two levels a
			// bare name can reach (a qualified_name that happens to be bare, and
			// the bare-name lookup) are both covered; goBareCallName is the Go twin
			// of the SQL guard's sqlNotBareName and is not Go-specific despite the
			// name.
			outcome.unresolved++
			continue
		}
		if ok && dstID != 0 && bareNameScopeAllKinds(target.srcLanguage) && goBareCallName(target.dstName) &&
			cppNamespaceTargetOutOfScope(target.edgeID, dstID, cppNamespaceTargets, cppCallerNamespaces) {
			outcome.unresolved++
			continue
		}
		if ok && dstID != 0 && bareNameScopeAllKinds(target.srcLanguage) && goBareCallName(target.dstName) &&
			cppMemberTargetOutOfClassScope(target.edgeID, dstID, cppMemberTargets, cppCallerClasses) {
			// A bare C/C++ spelling naming a member of a class the calling symbol
			// is not a member of. Same file is not class evidence and neither is an
			// include, so there is nothing weaker to fall back to and the edge stays
			// unresolved -- exactly what resolverCppBareMemberScopeSQL does on the
			// full path (P22.15, cpp_class_scope.go).
			outcome.unresolved++
			continue
		}
		if !ok || dstID == 0 {
			outcome.unresolved++
			switch {
			case matched && matchedGroup.ambiguousFor(callerIsTest):
				// Several definitions this caller may bind claimed this name.
				// Report it as ambiguity rather than as a language block, which
				// it is not.
				outcome.ambiguityBlocked++
			case matched && !matchedGroup.levelReachableFor(callerIsTest):
				// The name matched, and every match was a test definition this
				// production caller may not bind. Not ambiguity and not a
				// language block: a production call is never wired into a test.
				outcome.testShadowBlocked++
			case matched:
				// The level decided, but through a kind no strategy this binder
				// runs can bind -- a lone container-bearing enum, which only the
				// repo-wide `receiver_method` strategy owns. Neither ambiguity
				// nor a test shadow, and counting it as either would misreport
				// why the edge stayed unresolved.
			default:
				_, qualifiedElsewhere := qualifiedNamesAnyLanguage[target.dstName]
				_, shortElsewhere := shortNamesAnyLanguage[fallbackName]
				if qualifiedElsewhere || shortElsewhere {
					outcome.languageBlocked++
				}
			}
			continue
		}
		resolutions = append(resolutions, edgeResolution{edgeID: target.edgeID, dstID: dstID, strategy: strategy})
	}

	if len(resolutions) == 0 {
		_ = tx.Rollback()
		return outcome, nil
	}

	// `strategy_rank` is the row's index into binderStrategies. The strategy and
	// confidence strings stay out of the temp table entirely; the single UPDATE
	// below decodes the rank back into them with a CASE. That keeps the per-row
	// payload at the two bound ids it was before provenance existed, plus one
	// small integer written as statement text.
	//
	// Recreated rather than reused: the shape of this table changed once
	// already, and a survivor from an older definition would fail the INSERT on
	// a column count instead of being repaired. Matches how every other resolver
	// temp table in this file is set up.
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_edge_resolution`); err != nil {
		_ = tx.Rollback()
		return outcome, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_edge_resolution(edge_id INTEGER PRIMARY KEY, dst_symbol_id INTEGER NOT NULL, strategy_rank INTEGER NOT NULL)`); err != nil {
		_ = tx.Rollback()
		return outcome, err
	}

	// One pass over `resolutions`, chunked to stay under SQLite's default
	// variable limit (999). Each row still binds exactly two values -- its rank
	// is interpolated into the VALUES list rather than bound -- so the chunk
	// size is unchanged from before provenance existed.
	const maxPairsPerInsert = 400
	for start := 0; start < len(resolutions); start += maxPairsPerInsert {
		end := min(start+maxPairsPerInsert, len(resolutions))
		chunk := resolutions[start:end]
		var b strings.Builder
		b.WriteString(`INSERT INTO tmp_edge_resolution(edge_id, dst_symbol_id, strategy_rank) VALUES `)
		args := make([]any, 0, len(chunk)*2)
		for i, r := range chunk {
			if i > 0 {
				b.WriteString(",")
			}
			// The rank goes into the statement text, not the bound values: it is
			// a small compile-time-derived integer, and binding it would widen
			// every row's argument payload for no gain.
			b.WriteString("(?,?,")
			b.WriteString(binderStrategyRankLiteral(r.strategy))
			b.WriteString(")")
			args = append(args, r.edgeID, r.dstID)
		}
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			_ = tx.Rollback()
			return outcome, err
		}
	}

	// One statement writes the destination and the explanation together, so a
	// bound edge can never be left unexplained -- the strategy_rank is decoded
	// back into its strings by binderSetResolvedSQL.
	updateRes, err := tx.ExecContext(ctx, `
		UPDATE edges
		`+binderSetResolvedSQL()+`
		FROM tmp_edge_resolution t
		WHERE t.edge_id = edges.id
	`)
	if err != nil {
		_ = tx.Rollback()
		return outcome, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE tmp_edge_resolution`); err != nil {
		_ = tx.Rollback()
		return outcome, err
	}
	if err := tx.Commit(); err != nil {
		return outcome, err
	}
	if n, err := updateRes.RowsAffected(); err != nil {
		// Every resolution targets a distinct, still-unresolved edge id, so the
		// count is known exactly even when the driver cannot report it.
		outcome.resolved = len(resolutions)
	} else {
		outcome.resolved = int(n)
	}
	return outcome, nil
}

// resolveSymbolsByQualifiedNames returns qualified-name candidate groups keyed
// by (qualified_name, language), so a caller can only ever select a destination
// in its own language and never picks between several same-language definitions
// of the same qualified name. Symbols with no persisted language are excluded
// (fail closed).
//
// A group with several definitions the caller may bind stays undecided instead
// of being collapsed with a tie-break, which is the same decision the repo-wide
// exact-qualified strategy makes (resolver_ambiguity.go).
func (s *Store) resolveSymbolsByQualifiedNames(ctx context.Context, repoID int64, qualifiedNames []string, testFileIDs map[int64]struct{}) (symbolCandidates, error) {
	return s.resolveSymbolCandidates(ctx, repoID, "qualified_name", qualifiedNames, testFileIDs)
}

// resolveUniqueSymbolsByNames returns short-name candidate groups keyed by
// (name, language), including the groups several same-language definitions
// claim.
//
// Uniqueness is evaluated per language rather than repo-wide because the
// language gate must not let an unrelated foreign-language definition of the
// same name discard an otherwise valid same-language target. Symbols with no
// persisted language are excluded (fail closed).
// resolveUniqueSymbolsByNames groups bare-name candidates. It is the one column
// resolveSymbolCandidates treats as two populations: the level's candidates
// (resolverBareNameLevelKindsSQL, what makes the name ambiguous) and the
// narrower half this strategy may bind (resolverBareNameKindsSQL, the
// `bindable` flag). The qualified and dot-tail levels carry their qualifier as
// evidence instead, and every candidate they match is bindable.
func (s *Store) resolveUniqueSymbolsByNames(ctx context.Context, repoID int64, names []string, testFileIDs map[int64]struct{}) (symbolCandidates, error) {
	return s.resolveSymbolCandidates(ctx, repoID, "name", names, testFileIDs)
}

// candidateGroup is one (matched name, language) group of destination
// candidates, summarised the same way resolverCandidateAggregatesSQL summarises
// it for the repo-wide strategies.
//
// Each id is non-zero only while its count is exactly one: `add` clears it again
// the moment a second candidate of that kind arrives. That is deliberate --
// keeping the first-seen id past that point would store a value derived from SQL
// row order, which is precisely the tie-break P3 removed, one careless read away
// from returning.
//
// Two populations, not one (P22.12). What an evidence level's candidates ARE is
// a property of the level; what THIS strategy may bind out of them is narrower.
// They coincide everywhere except the bare-name level, where the repo-wide
// resolver reaches the same symbols through two strategies -- `exact_name`,
// restricted to resolverBareNameKindsSQL, and `receiver_method`, restricted to
// container-bearing symbols of any kind -- and recordAmbiguousResolverNames
// therefore counts their union. See resolver_ambiguity.go.
type candidateGroup struct {
	candidates           int
	productionCandidates int
	anySymbolID          int64
	productionSymbolID   int64

	// levelCandidates/levelProductionCandidates count every symbol the evidence
	// level reached, including kinds this strategy may not bind. They are the
	// Go-side twin of the veto rows recordAmbiguousResolverNames writes, and are
	// always >= their bindable counterparts above.
	levelCandidates           int
	levelProductionCandidates int
}

// add folds one candidate into the group. Order of calls does not affect the
// result: the counts are order-free, and an id survives only in the case where
// there is exactly one row it could have come from.
//
// `bindable` says whether this strategy may write the candidate as a
// destination. A candidate that is not bindable still counts toward the level,
// because another strategy at the same evidence level could own it and the
// repo-wide veto counts it for exactly that reason.
func (g candidateGroup) add(symbolID int64, isTest, bindable bool) candidateGroup {
	g.levelCandidates++
	if !isTest {
		g.levelProductionCandidates++
	}
	if !bindable {
		return g
	}
	g.candidates++
	switch g.candidates {
	case 1:
		g.anySymbolID = symbolID
	case 2:
		g.anySymbolID = 0
	}
	if isTest {
		return g
	}
	g.productionCandidates++
	switch g.productionCandidates {
	case 1:
		g.productionSymbolID = symbolID
	case 2:
		g.productionSymbolID = 0
	}
	return g
}

// chosen returns the destination a caller of the given kind may bind, mirroring
// resolverBindGateSQL exactly: the sole candidate this caller kind may bind
// (resolverChosenCandidateSQL), and no veto row for the level
// (resolverAmbiguousNamesSQL).
func (g candidateGroup) chosen(callerIsTest bool) (int64, bool) {
	if g.levelUndecidedFor(callerIsTest) {
		return 0, false
	}
	if callerIsTest {
		return g.anySymbolID, g.anySymbolID != 0
	}
	return g.productionSymbolID, g.productionSymbolID != 0
}

// levelUndecidedFor is the Go-side twin of one veto row: the level matched
// something, and this caller kind had no single candidate there. A test caller
// needs the level to hold exactly one candidate, a production caller exactly one
// production candidate -- the same two readings recordAmbiguousResolverNames
// emits rows for.
func (g candidateGroup) levelUndecidedFor(callerIsTest bool) bool {
	if callerIsTest {
		return g.levelCandidates != 1
	}
	return g.levelProductionCandidates != 1
}

// ambiguousFor reports whether the evidence level held several candidates this
// caller could not tell apart. It backs the honest "why did this stay
// unresolved" counter, so it names the level's population rather than the
// narrower bindable one.
func (g candidateGroup) ambiguousFor(callerIsTest bool) bool {
	if callerIsTest {
		return g.levelCandidates > 1
	}
	return g.levelProductionCandidates > 1
}

// levelReachableFor reports whether the level held any candidate this caller
// kind is allowed to consider at all, ignoring uniqueness. A production caller
// for which it is false faced nothing but test definitions -- the P7 test
// shadow -- which is a different unresolved reason from a level that decided
// itself through a kind no strategy here owns.
func (g candidateGroup) levelReachableFor(callerIsTest bool) bool {
	if callerIsTest {
		return g.levelCandidates > 0
	}
	return g.levelProductionCandidates > 0
}

// symbolCandidates is the outcome of one candidate lookup: every
// (name, language) group that matched, with enough detail for the binder to
// apply the same caller-kind rule the SQL strategies apply. Keeping the groups
// explicit rather than pre-collapsing them to "unique or not" is what lets the
// binder refuse instead of falling through to a weaker strategy that would pick
// arbitrarily.
type symbolCandidates struct {
	groups map[symbolLangKey]candidateGroup

	// typeSymbolFiles is the declaring file of every candidate whose kind is a
	// type (resolver_type_scope.go). It is what lets the binder ask the same
	// question the repo-wide resolverBareNameTypeScopeSQL asks -- without a
	// second query, since these rows are already being scanned.
	typeSymbolFiles map[int64]int64
}

// typeTargetOutOfScope reports whether binding `symbolID` from a caller in
// `callerFileID` would assert a bare-name relationship to a type the caller's
// file neither declares nor imports. It is the Go-side twin of
// resolverBareNameTypeScopeSQL and must answer identically;
// resolver_type_scope_test.go pins that against the SQL path on the same graph.
//
// Non-type candidates are never in the map, so the common case is a single map
// lookup on the already-chosen id.
func (c symbolCandidates) typeTargetOutOfScope(symbolID, callerFileID int64, importScope map[int64]map[int64]struct{}) bool {
	declaringFileID, isType := c.typeSymbolFiles[symbolID]
	if !isType || declaringFileID == callerFileID {
		return false
	}
	_, imported := importScope[callerFileID][declaringFileID]
	return !imported
}

// names reports every name that matched a symbol in any language, ambiguous or
// not. It backs the honest "why did this stay unresolved" counters.
func (c symbolCandidates) names() map[string]struct{} {
	out := make(map[string]struct{}, len(c.groups))
	for key := range c.groups {
		out[key.name] = struct{}{}
	}
	return out
}

// resolveSymbolCandidates loads the symbols matching `names` on `column` and
// groups them in Go by (matched name, language), recording how many there are
// and how many are declared outside a test file.
//
// The grouping is done here rather than in SQL because IsTestFilePath is the
// single definition of "test file" (resolver_testfile.go) and restating it as a
// SQL predicate is exactly the drift this phase forbids. `testFileIDs` is the
// repo's test-file set, built once per resolve by the caller, so a candidate's
// kind costs a map lookup on its `file_id` -- no join, no path string per
// candidate, and no per-candidate query.
//
// A symbol whose file id is absent from the set counts as production, so a ghost
// row still blocks rather than silently promoting another candidate.
//
// The Go-side counting matches the repo-wide COUNT(*)/SUM(...) exactly, so the
// two entrypoints agree on which names are undecidable; the recorded ids only
// ever name the sole member of a group of one. `column` is a package-internal
// literal, never caller input, and must be one of the four identity columns
// the binder matches on.
func (s *Store) resolveSymbolCandidates(ctx context.Context, repoID int64, column string, names []string, testFileIDs map[int64]struct{}) (symbolCandidates, error) {
	switch column {
	case "qualified_name", "name", "dot_tail2", "dot_tail3":
	default:
		panic("store: resolveSymbolCandidates called with unsupported column " + column)
	}
	out := symbolCandidates{
		groups:          map[symbolLangKey]candidateGroup{},
		typeSymbolFiles: map[int64]int64{},
	}
	if len(names) == 0 {
		return out, nil
	}
	const chunkSize = 400
	for start := 0; start < len(names); start += chunkSize {
		end := min(start+chunkSize, len(names))
		chunk := names[start:end]
		hasGlobal := false
		for _, name := range chunk {
			if strings.HasPrefix(name, "::") {
				hasGlobal = true
				break
			}
		}
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		// The literal `column != ''` term restates what non-empty lookup names
		// already imply; it is there so the partial dot-tail indexes
		// (idx_symbols_repo_dot_tail2/3, predicate `!= ''`) stay usable when
		// the names arrive as bound parameters.
		// The bare-name level is the one place where "what the level matched" and
		// "what this strategy may bind" differ, and both have to be loaded.
		//
		// The repo-wide resolver reaches the bare-name level through two
		// strategies: `exact_name`, restricted to resolverBareNameKindsSQL, and
		// `receiver_method`, restricted to container-bearing symbols of any kind.
		// recordAmbiguousResolverNames therefore counts their union when it
		// decides whether the level is undecidable, while `exact_name` binds only
		// the kind-restricted half. Loading only that half -- which is what the
		// binder did before P22.12 -- made a name declared once as a callable and
		// once as a container-bearing enum look unique here and ambiguous there,
		// so the same tree resolved differently after an update than after an
		// index. resolverBareNameLevelKindsSQL is the level; the `bindable` flag
		// below is the strategy. See resolver_ambiguity.go.
		//
		// The qualified and dot-tail levels carry their qualifier as evidence and
		// are not kind-restricted on either side, so every candidate they match
		// is bindable.
		kindFilter := ""
		if column == "name" {
			kindFilter = ` AND ` + resolverBareNameLevelKindsSQL("")
		}
		match := column + ` IN (` + placeholders + `)`
		if column == "qualified_name" {
			match = `(` + match + ` AND NOT (language = 'cpp' AND instr(qualified_name, '::') = 0 AND ` + column + ` NOT LIKE '::%')`
			if hasGlobal {
				match += ` OR (language = 'cpp' AND instr(qualified_name, '::') = 0 AND '::' || qualified_name IN (` + placeholders + `))`
			}
			match += `)`
		}
		query := `
			SELECT ` + column + `, language, id, file_id, kind
			FROM symbols
			WHERE repo_id = ? AND language != '' AND ` + column + ` != '' AND ` + match + kindFilter + `
		`
		args := make([]any, 0, len(chunk)*2+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		if column == "qualified_name" && hasGlobal {
			for _, name := range chunk {
				args = append(args, name)
			}
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return symbolCandidates{}, err
		}
		for rows.Next() {
			var name string
			var language string
			var id int64
			var fileID int64
			var kind string
			if err := rows.Scan(&name, &language, &id, &fileID, &kind); err != nil {
				_ = rows.Close()
				return symbolCandidates{}, err
			}
			key := symbolLangKey{name: name, language: language}
			_, isTest := testFileIDs[fileID]
			out.groups[key] = out.groups[key].add(id, isTest, column != "name" || resolverBareNameKindBindable(kind))
			// Recorded at every level, because whether the P22.9 scope rule applies
			// is decided by the edge's *spelling*, not by which level matched: a
			// bare dst_name can equal a symbol's qualified_name as easily as its
			// name. The binder's goBareCallName guard is what keeps qualifier-
			// bearing spellings out, mirroring sqlNotBareName on the SQL side.
			if bareNameScopeCoversKind(language, kind) {
				out.typeSymbolFiles[id] = fileID
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return symbolCandidates{}, err
		}
		if err := rows.Close(); err != nil {
			return symbolCandidates{}, err
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
		// No repo row means the repository is not indexed. Reporting that in
		// CodeGraph's own words keeps a driver string out of `graph_stats`.
		if errors.Is(err, sql.ErrNoRows) {
			return graph.Stats{}, fmt.Errorf("%w: repo %d", ErrRepoNotIndexed, repoID)
		}
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
	// The page is selected in a subquery that carries only the ordering keys.
	// A broad term can match tens of thousands of symbols, and sorting rows
	// that drag doc_summary and signature through the sorter costs far more
	// than sorting (qualified_name, id) pairs and then fetching twenty rows.
	rows, err := s.db.QueryContext(ctx, `
		WITH page AS (
			SELECT s.id AS id
			FROM symbol_fts fts
			JOIN symbols s ON s.id = fts.symbol_id
			WHERE s.repo_id = ? AND symbol_fts MATCH ?
			ORDER BY s.qualified_name ASC, s.kind ASC, s.container_name ASC, s.signature ASC, s.stable_key ASC,
			         s.start_line ASC, s.start_col ASC, s.end_line ASC, s.end_col ASC
			LIMIT ?
			OFFSET ?
		)
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		-- CROSS JOIN for the same reason the caller/callee page query uses one:
		-- the page is a twenty-row subset, and it must drive the join rather
		-- than be probed once per symbol in the repository.
		FROM page p
		CROSS JOIN symbols s ON s.id = p.id
		JOIN files f ON f.id = s.file_id
		ORDER BY s.qualified_name ASC, s.kind ASC, s.container_name ASC, s.signature ASC, s.stable_key ASC,
		         s.start_line ASC, s.start_col ASC, s.end_line ASC, s.end_col ASC
	`, repoID, quoteFTS(query), safeLimit(limit), safeOffset(offset))
	if err != nil {
		rows, err = s.db.QueryContext(ctx, `
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND (s.name LIKE ? OR s.qualified_name LIKE ?)
			ORDER BY s.qualified_name ASC, s.kind ASC, s.container_name ASC, s.signature ASC, s.stable_key ASC,
			         s.start_line ASC, s.start_col ASC, s.end_line ASC, s.end_col ASC
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
		ORDER BY s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
		LIMIT ?
		OFFSET ?
	`, repoID, query, query, safeLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// ImpactRadius returns one page of the change-impact closure around a set of
// seeds, with the full closure size reported alongside it.
//
// The page exists because the closure is bounded only by the graph: a deep
// traversal from a hub symbol reaches most of a repository. Callers that need
// the whole closure rather than a tool-sized answer use impactClosure directly.
func (s *Store) ImpactRadius(ctx context.Context, repoID int64, symbols []string, files []string, depth, limit, offset int) (map[string]any, error) {
	symbolList, fileList, presence, unresolvedEdges, unresolvedNames, err := s.impactClosureWithPresence(ctx, repoID, symbols, files, depth)
	if err != nil {
		return nil, err
	}

	// The traversal closure is bounded only by the graph, so the response is
	// paged like any other. `affected_symbols` keeps reporting the whole
	// closure and `truncated` says plainly when the page is not all of it: a
	// bounded page must never read as a complete impact set.
	totalSymbols := len(symbolList)
	pageSize := safeLimit(limit)
	start := min(safeOffset(offset), totalSymbols)
	end := min(start+pageSize, totalSymbols)
	symbolPage := symbolList[start:end]

	// Files follow the symbols actually returned, so the two halves of the
	// response describe the same set.
	pageFilesSet := make(map[string]struct{}, len(symbolPage))
	pageFiles := make([]string, 0, len(symbolPage))
	for _, sym := range symbolPage {
		if _, ok := pageFilesSet[sym.FilePath]; ok {
			continue
		}
		pageFilesSet[sym.FilePath] = struct{}{}
		pageFiles = append(pageFiles, sym.FilePath)
	}
	sort.Strings(pageFiles)

	return map[string]any{
		"symbols":       symbolPage,
		"files":         pageFiles,
		"seed_presence": presence,
		"summary": map[string]any{
			"affected_symbols": totalSymbols,
			"affected_files":   len(fileList),
			"returned_symbols": len(symbolPage),
			"returned_files":   len(pageFiles),
			"unresolved_edges": unresolvedEdges,
			"unresolved_names": unresolvedNames,
			"offset":           start,
			"truncated":        end < totalSymbols,
		},
	}, nil
}

// impactClosure computes the full change-impact closure: every symbol reachable
// from the seeds within depth hops, and every file those symbols live in, both
// in a stable order.
//
// It is deliberately unpaged. Bulk export asks for the whole subgraph and is
// allowed to; only the tool surface on top of it is bounded.
func (s *Store) impactClosure(ctx context.Context, repoID int64, symbols []string, files []string, depth int) ([]graph.Symbol, []string, error) {
	symbolsOut, filesOut, _, _, _, err := s.impactClosureWithPresence(ctx, repoID, symbols, files, depth)
	return symbolsOut, filesOut, err
}

func (s *Store) impactClosureWithPresence(ctx context.Context, repoID int64, symbols []string, files []string, depth int) ([]graph.Symbol, []string, ImpactSeedPresence, int, int, error) {
	affected := make(map[int64]graph.Symbol, len(symbols))
	queue := make([]int64, 0, len(symbols))
	presence := ImpactSeedPresence{Requested: len(symbols) + len(files)}
	ids, found, err := s.lookupImpactSymbolSeeds(ctx, repoID, symbols)
	if err != nil {
		return nil, nil, ImpactSeedPresence{}, 0, 0, err
	}
	for i, name := range symbols {
		if !found[i] {
			presence.Missing = append(presence.Missing, name)
			continue
		}
		presence.Found++
		queue = append(queue, ids[i])
	}
	seedIDs := map[int64]struct{}{}
	for i, id := range ids {
		if found[i] {
			seedIDs[id] = struct{}{}
		}
	}
	seedIDsList := make([]int64, 0, len(seedIDs))
	for id := range seedIDs {
		seedIDsList = append(seedIDsList, id)
	}
	for _, seedChunk := range chunkInt64s(seedIDsList, sqliteInClauseBatchSize) {
		seedSymbols, err := s.symbolsByIDs(ctx, repoID, seedChunk, len(seedChunk), 0)
		if err != nil {
			return nil, nil, ImpactSeedPresence{}, 0, 0, err
		}
		for _, seed := range seedSymbols {
			affected[seed.ID] = seed
		}
	}
	for _, file := range files {
		file = normalizeRepoRelPath(file)
		if file == "" {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM symbols s JOIN files f ON f.repo_id = s.repo_id AND f.id = s.file_id
			WHERE s.repo_id = ? AND f.path = ?
		`, repoID, file)
		if err != nil {
			return nil, nil, ImpactSeedPresence{}, 0, 0, err
		}
		fileFound := false
		for rows.Next() {
			sym, err := scanSymbol(rows)
			if err != nil {
				_ = rows.Close()
				return nil, nil, ImpactSeedPresence{}, 0, 0, err
			}
			fileFound = true
			affected[sym.ID] = sym
			queue = append(queue, sym.ID)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, ImpactSeedPresence{}, 0, 0, err
		}
		if fileFound {
			presence.Found++
		} else {
			presence.Missing = append(presence.Missing, file)
		}
	}
	if depth <= 0 {
		depth = 2
	}
	// Defensive backstop only: public callers are rejected above MaxDepth
	// before the traversal starts. Ten hops already reaches the connected
	// closure of any real repository.
	depth = min(depth, limits.MaxDepth)
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
			return nil, nil, ImpactSeedPresence{}, 0, 0, err
		}
		for _, sym := range callers {
			affected[sym.ID] = sym
			if _, ok := seen[sym.ID]; !ok {
				queue = append(queue, sym.ID)
			}
		}
		callees, err := s.impactNeighbors(ctx, repoID, current, false)
		if err != nil {
			return nil, nil, ImpactSeedPresence{}, 0, 0, err
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
	sort.Slice(symbolList, func(i, j int) bool {
		if symbolList[i].FilePath != symbolList[j].FilePath {
			return symbolList[i].FilePath < symbolList[j].FilePath
		}
		if symbolList[i].QualifiedName != symbolList[j].QualifiedName {
			return symbolList[i].QualifiedName < symbolList[j].QualifiedName
		}
		if symbolList[i].Range.StartLine != symbolList[j].Range.StartLine {
			return symbolList[i].Range.StartLine < symbolList[j].Range.StartLine
		}
		if symbolList[i].Range.StartCol != symbolList[j].Range.StartCol {
			return symbolList[i].Range.StartCol < symbolList[j].Range.StartCol
		}
		return symbolList[i].ID < symbolList[j].ID
	})
	sort.Strings(fileList)
	unresolvedEdges, unresolvedNames, err := s.impactUnresolvedEvidence(ctx, repoID, symbolList)
	if err != nil {
		return nil, nil, ImpactSeedPresence{}, 0, 0, err
	}
	return symbolList, fileList, presence, unresolvedEdges, unresolvedNames, nil
}

func (s *Store) impactUnresolvedEvidence(ctx context.Context, repoID int64, symbols []graph.Symbol) (int, int, error) {
	if len(symbols) == 0 {
		return 0, 0, nil
	}
	ids := make([]int64, len(symbols))
	for i, sym := range symbols {
		ids[i] = sym.ID
	}
	unresolvedEdges := 0
	names := map[string]struct{}{}
	for _, chunk := range chunkInt64s(ids, sqliteInClauseBatchSize) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := append([]any{repoID}, int64SliceToAny(chunk)...)
		rows, err := s.db.QueryContext(ctx, `SELECT e.dst_name, COUNT(*)
			FROM edges e
			WHERE e.repo_id = ? AND e.src_symbol_id IN (`+placeholders+`)
			  AND e.dst_symbol_id IS NULL AND e.dst_name != ''
			GROUP BY e.dst_name`, args...)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var name string
			var edges int
			if err := rows.Scan(&name, &edges); err != nil {
				rows.Close()
				return 0, 0, err
			}
			names[name] = struct{}{}
			unresolvedEdges += edges
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, 0, err
		}
		rows.Close()
	}
	return unresolvedEdges, len(names), nil
}

// impactNeighbors returns the neighbours of a frontier chunk, one row per edge.
//
// Deduplicating in SQL was tried and reverted: adding DISTINCT cut allocations
// by roughly a quarter on a hot hub but made the query 8-24% slower, because
// SQLite has to dedupe sixteen wide columns while the caller is already folding
// the rows into a map keyed by symbol id. The duplicate rows are cheaper than
// the dedup.
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
			JOIN symbols s ON s.repo_id = e.repo_id AND s.id = e.src_symbol_id
			JOIN files f ON f.repo_id = e.repo_id AND f.id = s.file_id
			WHERE e.repo_id = ? AND e.dst_symbol_id IN (` + placeholders + `)
			ORDER BY s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
		`
		if !callers {
			query = `
				SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
				       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
				FROM edges e
				JOIN symbols s ON s.repo_id = e.repo_id AND s.id = e.dst_symbol_id
				JOIN files f ON f.repo_id = e.repo_id AND f.id = s.file_id
				WHERE e.repo_id = ? AND e.src_symbol_id IN (` + placeholders + `) AND e.dst_symbol_id IS NOT NULL
				ORDER BY s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
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
	return s.relatedTests(ctx, repoID, symbol, file, limit, offset, 0, false)
}

func (s *Store) relatedTests(ctx context.Context, repoID int64, symbol, file string, limit, offset int, resolvedTargetID int64, targetResolved bool) ([]RelatedTest, error) {
	var rows *sql.Rows
	var err error
	if file != "" {
		file = normalizeRepoRelPath(file)
		canonical := CanonicalRelPath(file)
		if canonical == "" {
			return []RelatedTest{}, nil
		}
		variants := storedPathVariants(canonical)
		var targetFileID int64
		args := make([]any, 0, len(variants)+1)
		args = append(args, repoID)
		for _, variant := range variants {
			args = append(args, variant)
		}
		lookupRows, err := s.db.QueryContext(ctx, `SELECT id FROM files WHERE repo_id = ? AND path IN (`+sqlPlaceholders(len(variants))+`) ORDER BY id`, args...)
		if err != nil {
			return nil, err
		}
		var matches []int64
		for lookupRows.Next() {
			if err := lookupRows.Scan(&targetFileID); err != nil {
				lookupRows.Close()
				return nil, err
			}
			matches = append(matches, targetFileID)
		}
		if err := lookupRows.Err(); err != nil {
			lookupRows.Close()
			return nil, err
		}
		lookupRows.Close()
		if len(matches) == 0 || len(matches) > 1 {
			return []RelatedTest{}, nil
		}
		targetFileID = matches[0]
		// Two evidence classes, deduplicated per (test file, test symbol) with the
		// strongest surviving (P22.2):
		//
		//   - persisted test_links rows whose file-level target is this file
		//     (symbol-bound rows carry the symbol's file; sibling-bound rows carry
		//     the filename-convention sibling)
		//   - call evidence: a linked test function with a resolved call edge into
		//     a symbol this file defines. Derived from `edges` at query time so it
		//     follows edge lifecycle (P6) with no second materialised copy.
		//
		// The bare `reason`/`score` under MAX(pick) are SQLite's documented
		// bare-column semantics: they come from the row that supplied the max.
		// `pick` folds an explicit per-reason rank into the score so a score tie
		// across evidence classes (a sibling-bound row keeps its producer score,
		// so 'test_name_match'/0.8 and 'test_file_name_match'/0.8 can share one
		// group) still selects one deterministic winner; rows that tie on `pick`
		// share both score and reason, so the bare columns are identical either
		// way. The call-evidence arm excludes tests declared by the seed file
		// itself: a test file's own tests calling its own helpers are not
		// "related tests" of that file.
		rows, err = s.db.QueryContext(ctx, `
			SELECT path, symbol, reason, score FROM (
				SELECT path, symbol, reason, score,
					MAX(CAST(ROUND(score * 1000) AS INTEGER) * 4 + reason_rank) AS pick
				FROM (
					SELECT f.path AS path, COALESCE(s.qualified_name, '') AS symbol,
						t.reason AS reason, t.score AS score,
						CASE t.reason WHEN 'test_name_match' THEN 2
							WHEN '`+testLinkFileReason+`' THEN 1 ELSE 0 END AS reason_rank
					FROM test_links t
					JOIN files f ON f.id = t.test_file_id
					LEFT JOIN symbols s ON s.id = t.test_symbol_id
					WHERE t.repo_id = ? AND t.target_file_id = ?
					UNION ALL
					SELECT tf.path, ts.qualified_name, 'test_calls', 0.9, 3
					FROM edges e
					JOIN test_links tl ON tl.repo_id = e.repo_id AND tl.test_symbol_id = e.src_symbol_id
					JOIN symbols ts ON ts.id = e.src_symbol_id
					JOIN files tf ON tf.id = ts.file_id
					WHERE e.repo_id = ? AND e.edge_kind = 'calls'
					  AND ts.file_id != ?
					  AND e.dst_symbol_id IN (SELECT id FROM symbols WHERE repo_id = ? AND file_id = ?)
				)
				GROUP BY path, symbol
			)
			-- Canonical-form tie-break: which tests survive LIMIT must not depend on
			-- whether the index was written on Windows or on Linux.
			ORDER BY score DESC, REPLACE(path, '\', '/'), symbol
			LIMIT ?
			OFFSET ?
		`, repoID, targetFileID, repoID, targetFileID, repoID, targetFileID, safeLimit(limit), safeOffset(offset))
	} else {
		// targetID is declared separately so that the assignment below writes the
		// function-scoped err. With `targetID, err := ...` the query's error was
		// bound to a block-local err, leaving the outer one nil and rows nil --
		// a dropped error followed by a nil dereference in the deferred Close.
		var targetID int64
		if !targetResolved {
			targetID, err = s.lookupSymbolID(ctx, repoID, symbol, 0)
			if err != nil {
				return nil, err
			}
		} else {
			targetID = resolvedTargetID
		}
		// Same two evidence classes as the file branch, seeded by one symbol:
		// rows name-bound to it, plus linked test functions that call it. Same
		// deterministic `pick` rule as the file branch.
		rows, err = s.db.QueryContext(ctx, `
			SELECT path, symbol, reason, score FROM (
				SELECT path, symbol, reason, score,
					MAX(CAST(ROUND(score * 1000) AS INTEGER) * 4 + reason_rank) AS pick
				FROM (
					SELECT f.path AS path, COALESCE(s.qualified_name, '') AS symbol,
						t.reason AS reason, t.score AS score,
						CASE t.reason WHEN 'test_name_match' THEN 2
							WHEN '`+testLinkFileReason+`' THEN 1 ELSE 0 END AS reason_rank
					FROM test_links t
					JOIN files f ON f.id = t.test_file_id
					LEFT JOIN symbols s ON s.id = t.test_symbol_id
					WHERE t.repo_id = ? AND t.target_symbol_id = ?
					UNION ALL
					SELECT tf.path, ts.qualified_name, 'test_calls', 0.9, 3
					FROM edges e
					JOIN test_links tl ON tl.repo_id = e.repo_id AND tl.test_symbol_id = e.src_symbol_id
					JOIN symbols ts ON ts.id = e.src_symbol_id
					JOIN files tf ON tf.id = ts.file_id
					WHERE e.repo_id = ? AND e.edge_kind = 'calls' AND e.dst_symbol_id = ?
				)
				GROUP BY path, symbol
			)
			ORDER BY score DESC, REPLACE(path, '\', '/'), symbol
			LIMIT ?
			OFFSET ?
		`, repoID, targetID, repoID, targetID, safeLimit(limit), safeOffset(offset))
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
		item.File = canonicalStoredPath(item.File)
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
		SELECT s.id, f.path, COALESCE(s.qualified_name, ''), SUM(st.weight) AS score
		FROM symbol_tokens st
		JOIN symbols s ON s.id = st.symbol_id
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ? AND st.token IN (` + placeholders + `)
		GROUP BY s.id, f.path, s.qualified_name
		-- Score alone is not a total order: weights come from a small fixed set,
		-- so ties are the rule rather than the exception and a LIMIT/OFFSET page
		-- boundary would fall in an arbitrary place. The grouping keys are
		-- already computed, so using them as the tie-break is free.
		--
		-- The tie-break sorts the canonical form of the path, not the stored one.
		-- files.path is native, and backslash (0x5C) and slash (0x2F) sort either side of
		-- the digits and capitals, so ordering the raw column would put a different
		-- 30 rows through LIMIT on Windows than on Linux for the same repository --
		-- a different seed set, and so a different ranked context.
		ORDER BY score DESC, REPLACE(f.path, '\', '/') ASC, s.qualified_name ASC, s.kind ASC,
		         s.container_name ASC, s.signature ASC, s.stable_key ASC, s.start_line ASC, s.start_col ASC,
		         s.end_line ASC, s.end_col ASC
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
		id     int64
		file   string
		symbol string
		score  float64
	}
	out := make([]map[string]any, 0, limitVal)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.file, &item.symbol, &item.score); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"symbol_id": item.id,
			// Canonical (slash) form, like every other path this store hands out.
			// `files.path` is native, so on Windows the raw column value would make
			// this producer's `file` disagree with the `file` of every symbol-shaped
			// result -- and with the key any consumer joins them on.
			"file":   CanonicalRelPath(item.file),
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

	// A focused export wants the whole subgraph, not a tool-sized page of it, so
	// it takes the unpaged closure directly. Truncating here would silently
	// drop nodes from a graph the caller asked to export in full.
	impactSymbols, _, err := s.impactClosure(ctx, repoID, []string{focusSymbol}, nil, depth)
	if err != nil {
		return nil, nil, err
	}
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
	`, repoID, exportLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// ExportDOTNodeNamesPage yields the unique, non-empty qualified_name values
// for the repo in stable alphabetical order, paged. It exists so DOTStream
// can emit deduped + sorted DOT node lines without materialising the full
// `[]graph.Symbol` slice (peak memory on the no-focus DOT path was
// previously O(repo) in `Symbol`-sized rows; this trims it to O(pageSize)
// in plain strings).
func (s *Store) ExportDOTNodeNamesPage(ctx context.Context, repoID int64, limit, offset int) ([]string, error) {
	pageSize := exportLimit(limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT qualified_name
		FROM symbols
		WHERE repo_id = ? AND qualified_name <> ''
		ORDER BY qualified_name ASC
		LIMIT ?
		OFFSET ?
	`, repoID, pageSize, safeOffset(offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Size the buffer from the normalized page size, never from the raw
	// argument: a caller asking for a hundred million rows must not reserve a
	// hundred million slots before the first row is read.
	out := make([]string, 0, pageSize)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (s *Store) ExportEdgesPage(ctx context.Context, repoID int64, limit, offset int) ([]ExportEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+exportEdgeColumnsSQL+`
		FROM edges e
		LEFT JOIN symbols src ON src.id = e.src_symbol_id
		LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id
		LEFT JOIN files f ON f.id = e.file_id
		WHERE e.repo_id = ?
		ORDER BY e.id ASC
		LIMIT ?
		OFFSET ?
	`, repoID, exportLimit(limit), safeOffset(offset))
	if err != nil {
		return nil, err
	}
	edges, evidence, err := scanExportEdges(rows)
	if err != nil {
		return nil, err
	}
	if err := s.classifyExportEdges(ctx, repoID, edges, evidence); err != nil {
		return nil, err
	}
	return edges, nil
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
			SELECT `+exportEdgeColumnsSQL+`
			FROM edges e
			LEFT JOIN symbols src ON src.id = e.src_symbol_id
			LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id
			LEFT JOIN files f ON f.id = e.file_id
			WHERE e.repo_id = ?
		`, repoID)
		if err != nil {
			return nil, err
		}
		edges, evidence, err := scanExportEdges(rows)
		if err != nil {
			return nil, err
		}
		if err := s.classifyExportEdges(ctx, repoID, edges, evidence); err != nil {
			return nil, err
		}
		return edges, nil
	}
	var out []ExportEdge
	for _, chunk := range chunkInt64s(symbolIDs, 250) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		query := `
			SELECT ` + exportEdgeColumnsSQL + `
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
		items, evidence, err := scanExportEdges(rows)
		if err != nil {
			return nil, err
		}
		if err := s.classifyExportEdges(ctx, repoID, items, evidence); err != nil {
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

// scanExportEdges reads exportEdgeColumnsSQL. It returns the wire records and,
// positionally aligned with them, the classification evidence carried by the two
// trailing columns. TargetClassification is left empty here: it is filled by
// classifyExportEdges, which is the only place that decides it.
func scanExportEdges(rows *sql.Rows) ([]ExportEdge, []exportEdgeEvidence, error) {
	defer rows.Close()
	var out []ExportEdge
	var evidence []exportEdgeEvidence
	for rows.Next() {
		var edge ExportEdge
		var dstID sql.NullInt64
		var item exportEdgeEvidence
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
			&edge.ResolutionStrategy,
			&edge.ResolutionConfidence,
			&item.fileID,
			&item.srcLanguage,
			&item.callSite,
		); err != nil {
			return nil, nil, err
		}
		if dstID.Valid {
			value := dstID.Int64
			edge.DstSymbolID = &value
		}
		edge.FilePath = filepath.ToSlash(edge.FilePath)
		out = append(out, edge)
		evidence = append(evidence, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return out, evidence, nil
}

func (s *Store) QueueDirtyFile(ctx context.Context, repoID int64, path, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dirty_files(repo_id, path, reason, queued_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(repo_id, path) DO UPDATE SET reason=excluded.reason, queued_at=excluded.queued_at
	`, repoID, path, reason, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) QueueDirtyFiles(ctx context.Context, repoID int64, paths []string, reason string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		_ = tx.Rollback()
	}()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO dirty_files(repo_id, path, reason, queued_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(repo_id, path) DO UPDATE SET reason=excluded.reason, queued_at=excluded.queued_at
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, path := range paths {
		if _, err := stmt.ExecContext(ctx, repoID, path, reason, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Store) HasDirtyFiles(ctx context.Context, repoID int64) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM dirty_files WHERE repo_id = ? LIMIT 1`, repoID).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ClaimDirtyFiles snapshots all currently queued dirty paths for a repo and
// marks them as "inflight" by rewriting queued_at/reason in a single write
// transaction. This avoids the crash window inherent in destructive draining
// (delete-then-process) while still allowing successful callers to delete only
// the rows they claimed.
func (s *Store) ClaimDirtyFiles(ctx context.Context, repoID int64, claimAt, claimReason string) ([]string, error) {
	if claimAt == "" {
		return nil, fmt.Errorf("claimAt must be non-empty")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
	}()

	rows, err := conn.QueryContext(ctx, `SELECT path FROM dirty_files WHERE repo_id = ? ORDER BY queued_at`, repoID)
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

	for _, chunk := range chunkStrings(paths, 900) {
		qMarks := strings.Repeat("?,", len(chunk))
		qMarks = qMarks[:len(qMarks)-1]
		args := make([]any, 0, 3+len(chunk))
		args = append(args, claimAt, claimReason, repoID)
		for _, p := range chunk {
			args = append(args, p)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`
			UPDATE dirty_files
			SET queued_at = ?, reason = ?
			WHERE repo_id = ? AND path IN (%s)
		`, qMarks), args...); err != nil {
			return nil, err
		}
	}

	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, err
	}
	committed = true
	return paths, nil
}

// DeleteClaimedDirtyFiles removes previously-claimed rows, but only if their
// queued_at still matches the claim timestamp. If a path was re-queued after
// claim (queued_at changed), it is retained for the next flush.
func (s *Store) DeleteClaimedDirtyFiles(ctx context.Context, repoID int64, paths []string, claimedAt string) error {
	if len(paths) == 0 {
		return nil
	}
	if claimedAt == "" {
		return fmt.Errorf("claimedAt must be non-empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		_ = tx.Rollback()
	}()

	for _, chunk := range chunkStrings(paths, sqliteInClauseBatchSize) {
		qMarks := strings.Repeat("?,", len(chunk))
		qMarks = qMarks[:len(qMarks)-1]
		args := make([]any, 0, 3+len(chunk))
		args = append(args, repoID, claimedAt)
		for _, p := range chunk {
			args = append(args, p)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			DELETE FROM dirty_files
			WHERE repo_id = ? AND queued_at = ? AND path IN (%s)
		`, qMarks), args...); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Store) DrainDirtyFiles(ctx context.Context, repoID int64) ([]string, error) {
	// Take a write lock up-front so that events queued concurrently cannot be
	// inserted until after we delete the drained rows. Also ensures we can safely
	// rollback if scanning fails/cancels (avoids losing work on partial reads).
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
	}()

	rows, err := conn.QueryContext(ctx, `DELETE FROM dirty_files WHERE repo_id = ? RETURNING path, queued_at`, repoID)
	if err == nil {
		defer rows.Close()
		type drained struct {
			path     string
			queuedAt string
		}
		var drainedRows []drained
		for rows.Next() {
			var d drained
			if err := rows.Scan(&d.path, &d.queuedAt); err != nil {
				return nil, err
			}
			drainedRows = append(drainedRows, d)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		sort.Slice(drainedRows, func(i, j int) bool { return drainedRows[i].queuedAt < drainedRows[j].queuedAt })
		out := make([]string, len(drainedRows))
		for i := range drainedRows {
			out[i] = drainedRows[i].path
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return nil, err
		}
		committed = true
		return out, nil
	}

	// Fallback for older SQLite builds/drivers without `RETURNING`.
	if !isSQLiteReturningUnsupported(err) {
		return nil, err
	}

	selRows, err := conn.QueryContext(ctx, `SELECT path FROM dirty_files WHERE repo_id = ? ORDER BY queued_at`, repoID)
	if err != nil {
		return nil, err
	}
	defer selRows.Close()

	var out []string
	for selRows.Next() {
		var path string
		if err := selRows.Scan(&path); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	if err := selRows.Err(); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM dirty_files WHERE repo_id = ?`, repoID); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, err
	}
	committed = true
	return out, nil
}

func isSQLiteReturningUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "returning") && strings.Contains(msg, "syntax")
}

func chunkStrings(values []string, chunkSize int) [][]string {
	if len(values) == 0 || chunkSize <= 0 {
		return nil
	}
	out := make([][]string, 0, (len(values)+chunkSize-1)/chunkSize)
	for start := 0; start < len(values); start += chunkSize {
		end := min(start+chunkSize, len(values))
		out = append(out, values[start:end])
	}
	return out
}

// ErrSymbolNotFound reports that a name was looked up and no symbol in the
// index carries it. It is CodeGraph's own vocabulary on purpose: absence of a
// symbol is an ordinary answer to a lookup, and a caller -- human or agent --
// must be able to tell it apart from the database failing, which a raw
// `sql: no rows in result set` cannot express.
//
// Wrap it with the name that was requested so the message stays actionable:
//
//	fmt.Errorf("%w: %q", ErrSymbolNotFound, symbol)
var ErrSymbolNotFound = errors.New("symbol not found")

var ErrSymbolAmbiguous = errors.New("symbol is ambiguous")

// SymbolNotFoundError builds the wrapped not-found error for a requested name,
// so every surface reports absence the same way.
func SymbolNotFoundError(symbol string) error {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return ErrSymbolNotFound
	}
	return fmt.Errorf("%w: %q", ErrSymbolNotFound, symbol)
}

func SymbolAmbiguousError(symbol string, count int) error {
	symbol = strings.TrimSpace(symbol)
	if count > 0 {
		return fmt.Errorf("%w: %q (%d exact candidates)", ErrSymbolAmbiguous, symbol, count)
	}
	return fmt.Errorf("%w: %q", ErrSymbolAmbiguous, symbol)
}

func (s *Store) lookupSymbolID(ctx context.Context, repoID int64, symbol string, symbolID int64) (int64, error) {
	ids, err := s.lookupSymbolIDs(ctx, repoID, symbol, symbolID)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		// A name that matches several definitions is not reported here: the
		// first candidate wins, in the deterministic order lookupSymbolIDs
		// establishes. Only genuine absence is an error.
		return 0, SymbolNotFoundError(symbol)
	}
	return ids[0], nil
}

// lookupImpactSymbolSeeds applies the user-input cascade to every requested
// seed in one statement per bind-sized chunk. The row rank is the same
// qualified, short, suffix, short-name precedence as lookupSymbolIDs.
func (s *Store) lookupImpactSymbolSeeds(ctx context.Context, repoID int64, symbols []string) ([]int64, []bool, error) {
	ids := make([]int64, len(symbols))
	found := make([]bool, len(symbols))
	const chunkSize = 300
	for start := 0; start < len(symbols); start += chunkSize {
		end := min(start+chunkSize, len(symbols))
		var values strings.Builder
		args := make([]any, 0, (end-start)*2+1)
		for i := start; i < end; i++ {
			if values.Len() > 0 {
				values.WriteString(",")
			}
			values.WriteString("(?,?,?)")
			name := strings.TrimSpace(strings.TrimPrefix(symbols[i], "::"))
			args = append(args, i, name, lookupSymbolShortName(name))
		}
		args = append(args, repoID)
		rows, err := s.db.QueryContext(ctx, `WITH requested(ord, name, short) AS (VALUES `+values.String()+`), candidates AS (
			SELECT req.ord, s.id,
			       CASE
			         WHEN s.qualified_name = req.name THEN 0
			         WHEN s.name = req.name THEN 1
			         WHEN req.short <> '' AND (s.qualified_name LIKE '%::' || req.short OR s.qualified_name LIKE '%.' || req.short) THEN 2
			         WHEN req.short <> req.name AND req.short <> '' AND s.name = req.short THEN 3
			         ELSE 4
			       END AS rank,
			       s.qualified_name, f.path, s.start_line, s.start_col
			FROM requested req
			JOIN symbols s ON s.repo_id = ? AND (
				s.qualified_name = req.name OR s.name = req.name OR
				(req.short <> '' AND (s.qualified_name LIKE '%::' || req.short OR s.qualified_name LIKE '%.' || req.short)) OR
				(req.short <> req.name AND req.short <> '' AND s.name = req.short)
			)
			JOIN files f ON f.id = s.file_id
		)
		SELECT ord, id FROM candidates
		ORDER BY ord, rank, qualified_name, path, start_line, start_col, id`, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var ord int
			var id int64
			if err := rows.Scan(&ord, &id); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if !found[ord] {
				ids[ord], found[ord] = id, true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
	}
	return ids, found, nil
}

func (s *Store) lookupSymbolIDs(ctx context.Context, repoID int64, symbol string, symbolID int64) ([]int64, error) {
	if symbolID != 0 {
		identity, ok, err := s.lookupSymbolIdentity(ctx, repoID, symbolID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		return []int64{identity.ID}, nil
	}
	symbol = strings.TrimSpace(strings.TrimPrefix(symbol, "::"))
	if symbol == "" {
		return nil, nil
	}
	short := lookupSymbolShortName(symbol)
	queries := []struct {
		sql  string
		args []any
	}{
		{
			sql: `
				SELECT DISTINCT s.id
				FROM symbols s
				JOIN files f ON f.id = s.file_id
				WHERE s.repo_id = ? AND s.qualified_name = ?
				ORDER BY s.qualified_name ASC, f.path ASC, s.start_line ASC, s.start_col ASC, s.id ASC
			`,
			args: []any{repoID, symbol},
		},
		{
			sql: `
				SELECT DISTINCT s.id
				FROM symbols s
				JOIN files f ON f.id = s.file_id
				WHERE s.repo_id = ? AND s.name = ?
				ORDER BY s.qualified_name ASC, f.path ASC, s.start_line ASC, s.start_col ASC, s.id ASC
			`,
			args: []any{repoID, symbol},
		},
	}
	if short != "" {
		queries = append(queries, struct {
			sql  string
			args []any
		}{
			sql: `
				SELECT DISTINCT s.id
				FROM symbols s
				JOIN files f ON f.id = s.file_id
				WHERE s.repo_id = ? AND (s.qualified_name LIKE ? OR s.qualified_name LIKE ?)
				ORDER BY s.qualified_name ASC, f.path ASC, s.start_line ASC, s.start_col ASC, s.id ASC
			`,
			args: []any{repoID, "%::" + short, "%." + short},
		})
	}
	if short != "" && short != symbol {
		queries = append(queries, struct {
			sql  string
			args []any
		}{
			sql: `
				SELECT DISTINCT s.id
				FROM symbols s
				JOIN files f ON f.id = s.file_id
				WHERE s.repo_id = ? AND s.name = ?
				ORDER BY s.qualified_name ASC, f.path ASC, s.start_line ASC, s.start_col ASC, s.id ASC
			`,
			args: []any{repoID, short},
		})
	}
	for _, query := range queries {
		ids, err := s.lookupSymbolIDsByQuery(ctx, query.sql, query.args...)
		if err != nil {
			return nil, err
		}
		if len(ids) > 0 {
			return ids, nil
		}
	}
	return nil, nil
}

func (s *Store) lookupSymbolIDsByQuery(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, 4)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func lookupSymbolShortName(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return ""
	}
	if idx := strings.LastIndex(symbol, "::"); idx >= 0 && idx+2 < len(symbol) {
		return strings.TrimSpace(symbol[idx+2:])
	}
	if idx := strings.LastIndexByte(symbol, '.'); idx >= 0 && idx+1 < len(symbol) {
		return strings.TrimSpace(symbol[idx+1:])
	}
	return symbol
}

func int64SliceToAny(values []int64) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func (s *Store) symbolsByIDs(ctx context.Context, repoID int64, ids []int64, limit, offset int) ([]graph.Symbol, error) {
	if len(ids) == 0 {
		return []graph.Symbol{}, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		FROM symbols s
		JOIN files f ON f.repo_id = s.repo_id AND f.id = s.file_id
		WHERE s.repo_id = ? AND s.id IN (`+placeholders+`)
		ORDER BY s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
		LIMIT ?
		OFFSET ?
	`, append(append([]any{repoID}, int64SliceToAny(ids)...), safeLimit(limit), safeOffset(offset))...)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// queryUnresolvedDstNamesBySrcIDs returns the distinct unresolved destination
// names of a set of source symbols, both flattened in first-seen order and
// grouped by the source symbol that spelled them.
//
// The grouping is what lets a bare Go spelling be scoped to the package of the
// symbol that actually wrote it (P22.6): merging every source's names into one
// set first would leave nothing to scope against once several sources of an
// ambiguous name live in different packages.
//
// The id list is chunked: an ambiguous short name in a large repository can
// resolve to thousands of source symbols, and splicing all of them into one
// `IN (?, ?, ...)` would exceed SQLITE_MAX_VARIABLE_NUMBER on drivers still
// built with the historical 999-variable limit.
func (s *Store) queryUnresolvedDstNamesBySrcIDs(ctx context.Context, repoID int64, srcIDs []int64) ([]string, map[int64][]string, error) {
	if len(srcIDs) == 0 {
		return nil, nil, nil
	}
	seen := map[string]struct{}{}
	var names []string
	bySrc := map[int64][]string{}
	for _, chunk := range chunkInt64s(srcIDs, sqliteInClauseBatchSize) {
		ph := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		rows, err := s.db.QueryContext(ctx,
			`SELECT DISTINCT e.src_symbol_id, e.dst_name FROM edges e
			 WHERE e.repo_id = ? AND e.src_symbol_id IN (`+ph+`) AND e.dst_symbol_id IS NULL AND e.dst_name != ''
			 ORDER BY e.src_symbol_id ASC, e.dst_name ASC`,
			append([]any{repoID}, int64SliceToAny(chunk)...)...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var src int64
			var name string
			if err := rows.Scan(&src, &name); err != nil {
				rows.Close()
				return nil, nil, err
			}
			bySrc[src] = append(bySrc[src], name)
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return names, bySrc, nil
}

func scanSymbol(scanner interface{ Scan(dest ...any) error }) (graph.Symbol, error) {
	var sym graph.Symbol
	if err := scanner.Scan(
		&sym.ID, &sym.FileID, &sym.Language, &sym.Kind, &sym.Name, &sym.QualifiedName, &sym.ContainerName, &sym.Signature, &sym.Visibility,
		&sym.Range.StartLine, &sym.Range.StartCol, &sym.Range.EndLine, &sym.Range.EndCol, &sym.DocSummary, &sym.StableKey, &sym.FilePath,
	); err != nil {
		return graph.Symbol{}, err
	}
	// Normalize paths in outputs to be deterministic across platforms and call sites.
	sym.FilePath = filepath.ToSlash(sym.FilePath)
	return sym, nil
}

// TraceDependencies performs a BFS traversal of the dependency graph starting
// from the given symbol, returning the full chain up to maxDepth levels.
// It returns one page of the traversal plus the total number of nodes reached,
// so a caller can report a bounded page without implying it is the whole chain.
func (s *Store) TraceDependencies(ctx context.Context, repoID int64, symbol string, direction string, maxDepth, limit, offset int) ([]map[string]any, int, error) {
	result, err := s.traceDependencies(ctx, repoID, symbol, direction, maxDepth, limit, offset)
	return result.Dependencies, result.Total, err
}

func (s *Store) traceDependencies(ctx context.Context, repoID int64, symbol string, direction string, maxDepth, limit, offset int) (TraceResult, error) {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	// Defensive backstop only; public callers are rejected above MaxDepth.
	maxDepth = min(maxDepth, limits.MaxDepth)
	if direction == "" {
		direction = "downstream"
	}

	// Resolve one exact semantic identity: qualified name first, then short name.
	// A singular trace must not silently widen into several unrelated seeds.
	seedName := strings.TrimSpace(strings.TrimPrefix(symbol, "::"))
	seedRows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.qualified_name, s.kind, s.name, f.path
			FROM symbols s JOIN files f ON f.repo_id = s.repo_id AND f.id = s.file_id
			WHERE s.repo_id = ? AND s.qualified_name = ?
			UNION ALL
		 SELECT s.id, s.qualified_name, s.kind, s.name, f.path
			FROM symbols s JOIN files f ON f.repo_id = s.repo_id AND f.id = s.file_id
			WHERE s.repo_id = ? AND s.qualified_name <> ? AND s.name = ?`,
		repoID, seedName, repoID, seedName, seedName)
	if err != nil {
		return TraceResult{}, fmt.Errorf("trace_dependencies seed query: %w", err)
	}
	type symInfo struct {
		id            int64
		qualifiedName string
		kind          string
		name          string
		file          string
	}
	var seeds []symInfo
	for seedRows.Next() {
		var si symInfo
		if err := seedRows.Scan(&si.id, &si.qualifiedName, &si.kind, &si.name, &si.file); err != nil {
			seedRows.Close()
			return TraceResult{}, err
		}
		seeds = append(seeds, si)
	}
	seedRows.Close()
	if err := seedRows.Err(); err != nil {
		return TraceResult{}, err
	}
	if len(seeds) > 1 {
		return TraceResult{}, SymbolAmbiguousError(symbol, len(seeds))
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
				kind: seed.kind, name: seed.name, file: filepath.ToSlash(seed.file), depth: 0, dir: dir,
			})
		}

		var query string
		if dir == "downstream" {
			query = `SELECT DISTINCT s.id, s.qualified_name, s.kind, s.name, f.path
				FROM edges e JOIN symbols s ON s.repo_id = e.repo_id AND s.id = e.dst_symbol_id
				JOIN files f ON f.repo_id = e.repo_id AND f.id = s.file_id
				WHERE e.repo_id = ? AND e.src_symbol_id = ? AND e.dst_symbol_id IS NOT NULL`
		} else {
			query = `SELECT DISTINCT s.id, s.qualified_name, s.kind, s.name, f.path
				FROM edges e JOIN symbols s ON s.repo_id = e.repo_id AND s.id = e.src_symbol_id
				JOIN files f ON f.repo_id = e.repo_id AND f.id = s.file_id
				WHERE e.repo_id = ? AND e.dst_symbol_id = ?`
		}

		for i := 0; i < len(queue); i++ {
			entry := queue[i]
			if entry.depth >= maxDepth {
				continue
			}
			rows, err := s.db.QueryContext(ctx, query, repoID, entry.id)
			if err != nil {
				return fmt.Errorf("trace_dependencies bfs query: %w", err)
			}
			for rows.Next() {
				var si bfsEntry
				if err := rows.Scan(&si.id, &si.qualifiedName, &si.kind, &si.name, &si.file); err != nil {
					rows.Close()
					return err
				}
				si.file = filepath.ToSlash(si.file)
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
			return TraceResult{}, err
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
			return TraceResult{}, err
		}
	}

	// Sort by the complete public row, with an explicit direction rank.
	sort.Slice(results, func(i, j int) bool {
		if results[i].depth != results[j].depth {
			return results[i].depth < results[j].depth
		}
		if results[i].qualifiedName != results[j].qualifiedName {
			return results[i].qualifiedName < results[j].qualifiedName
		}
		if results[i].file != results[j].file {
			return results[i].file < results[j].file
		}
		if results[i].kind != results[j].kind {
			return results[i].kind < results[j].kind
		}
		if results[i].name != results[j].name {
			return results[i].name < results[j].name
		}
		return traceDirectionRank(results[i].dir) < traceDirectionRank(results[j].dir)
	})

	// The traversal has no natural size bound -- a hub symbol in a large
	// repository reaches thousands of nodes at the default depth -- so the
	// chain is paged after sorting, and the total travels with the page.
	total := len(results)
	pageStart := min(safeOffset(offset), total)
	pageEnd := min(pageStart+safeLimit(limit), total)
	page := results[pageStart:pageEnd]

	out := make([]map[string]any, len(page))
	for i, r := range page {
		out[i] = map[string]any{
			"symbol":    r.qualifiedName,
			"kind":      r.kind,
			"name":      r.name,
			"file":      r.file,
			"depth":     r.depth,
			"direction": r.dir,
		}
	}
	return TraceResult{
		TargetFound:  len(seeds) > 0,
		Dependencies: out,
		Total:        total,
		Offset:       pageStart,
		Truncated:    pageEnd < total,
	}, nil
}

// traceDirectionRank is the public ordering contract: downstream precedes upstream.
func traceDirectionRank(direction string) int {
	switch direction {
	case "downstream":
		return 0
	case "upstream":
		return 1
	default:
		return 2
	}
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
//
// Determinism is a requirement here, not a nicety: a limited page that is not
// reproducible cannot be compared across two calls, across the direct and
// gateway MCP surfaces, or across two databases holding the same graph.
//
// The observed defect was selection and ordering. Equal scores are the common
// case in a symbol graph -- every leaf has the same rank -- and the page was
// cut before it was ordered, from a slice built by ranging a Go map, so both
// which symbols made the LIMIT and what order they came back in were arbitrary.
// Selection and ordering are now one sort with a semantic tie-break: file,
// qualified name, kind, then position. Never the row id, so two databases that
// hold the same graph inserted in different orders produce the same page.
//
// The summation order is fixed for a second, latent reason. Contributions were
// accumulated while ranging a map, and floating-point addition is not
// associative, so two nodes whose ranks are mathematically equal could differ
// in their last bits -- and then the tie-break above would never see a tie to
// break. It is now summed in symbol-id order, which is stable for a given
// database.
//
// Be precise about the limit of that. Id order is insertion order, so two
// databases holding the same graph could still sum in different orders. Making
// the arithmetic itself graph-ordered would mean loading identity for every
// node before ranking, which measured as more than double this function's
// runtime on a 100k-symbol graph -- and no fixture in the suite reproduces the
// defect it would close, including one built specifically to try. The
// insertion-order tests pass on the id-ordered sum.
func (s *Store) PageRank(ctx context.Context, repoID int64, limit int) ([]map[string]any, error) {
	limit = safeLimit(limit)

	// Step 1: load all resolved edges.
	//
	// No ORDER BY: the only order that matters is the order sources are summed
	// in, and sorting 100k source ids in Go is a few milliseconds where making
	// SQLite sort every edge row cost ~40ms on the same fixture. Per-source
	// destination order is irrelevant, because every destination of one source
	// receives the identical share.
	rows2, err := s.db.QueryContext(ctx,
		`SELECT src_symbol_id, dst_symbol_id FROM edges
		 WHERE repo_id = ? AND dst_symbol_id IS NOT NULL`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()

	outLinks := map[int64][]int64{} // src -> list of dst
	var srcOrder []int64            // sources in ascending id order
	allNodes := map[int64]struct{}{}
	for rows2.Next() {
		var src, dst int64
		if err := rows2.Scan(&src, &dst); err != nil {
			return nil, err
		}
		if len(outLinks[src]) == 0 {
			srcOrder = append(srcOrder, src)
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

	// Node indices in id order. Iterating the node map here would make the
	// slice layout -- and so the summation order below -- storage-dependent.
	indexNode := make([]int64, 0, n)
	for id := range allNodes {
		indexNode = append(indexNode, id)
	}
	slices.Sort(indexNode)
	slices.Sort(srcOrder)
	nodeIndex := make(map[int64]int, n)
	for i, id := range indexNode {
		nodeIndex[id] = i
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
		// Ranging srcOrder, not outLinks: float addition is not associative, so
		// map order here would perturb the scores themselves. srcOrder is sorted
		// by symbol id -- see the function comment for what that does and does
		// not guarantee.
		for _, src := range srcOrder {
			dsts := outLinks[src]
			si := nodeIndex[src]
			share := damping * rank[si] / float64(len(dsts))
			for _, dst := range dsts {
				newRank[nodeIndex[dst]] += share
			}
		}
		rank, newRank = newRank, rank
	}

	// Step 3: find the score cut, then order everything that reaches it by
	// identity and take the page.
	//
	// Selection and ordering have to be one decision. The pre-P19 shape cut the
	// page first and ordered it afterwards, which makes the *membership*
	// arbitrary among ties -- and ties are the common case, since every leaf
	// scores the same. No amount of ordering afterwards repairs that.
	//
	// Identity is what orders the ties, and it is loaded only for the rows that
	// reach the cut rather than for the whole graph. That matters: on a
	// 100k-symbol graph a whole-graph identity load more than doubled this
	// function's runtime. The candidate set is the page plus its ties -- a
	// handful of extra rows normally, and as large as the graph only when the
	// graph itself ties, which is exactly when the tie-break has to see all of
	// it.
	type ranked struct {
		id    int64
		score float64
	}
	results := make([]ranked, n)
	for i, id := range indexNode {
		results[i] = ranked{id: id, score: rank[i]}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	cut := results[min(len(results), limit)-1].score
	candidates := results
	for i, r := range results {
		if r.score < cut {
			candidates = results[:i]
			break
		}
	}

	ids := make([]int64, 0, len(candidates))
	for _, r := range candidates {
		ids = append(ids, r.id)
	}
	identity, err := s.symbolIdentities(ctx, ids)
	if err != nil {
		return nil, err
	}

	type rankedRow struct {
		score float64
		id    symbolIdentity
	}
	rowsOut := make([]rankedRow, 0, len(candidates))
	for _, r := range candidates {
		// A rank entry whose symbol row vanished mid-query is skipped, as it was
		// before P19.
		si, ok := identity[r.id]
		if !ok {
			continue
		}
		rowsOut = append(rowsOut, rankedRow{score: r.score, id: si})
	}
	sort.Slice(rowsOut, func(i, j int) bool {
		if rowsOut[i].score != rowsOut[j].score {
			return rowsOut[i].score > rowsOut[j].score
		}
		return lessSymbolIdentity(rowsOut[i].id, rowsOut[j].id)
	})
	rowsOut = rowsOut[:min(len(rowsOut), limit)]

	prOut := make([]map[string]any, 0, len(rowsOut))
	for _, r := range rowsOut {
		prOut = append(prOut, map[string]any{
			"symbol": r.id.QualifiedName,
			"kind":   r.id.Kind,
			"file":   r.id.Path,
			"rank":   math.Round(r.score*1e6) / 1e6,
		})
	}
	return prOut, nil
}

// symbolIdentity is the semantic identity of a symbol: everything needed to
// order two of them without consulting a row id.
type symbolIdentity struct {
	ID            int64
	RepoID        int64
	Name          string
	QualifiedName string
	Language      string
	Kind          string
	Path          string
	StartLine     int
	StartCol      int
}

// lookupSymbolIdentity validates an explicit symbol id against its repository
// and returns the persisted identity used by exact-id query semantics.
func (s *Store) lookupSymbolIdentity(ctx context.Context, repoID, symbolID int64) (symbolIdentity, bool, error) {
	var identity symbolIdentity
	err := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.repo_id, s.name, s.qualified_name, s.language,
		       s.kind, COALESCE(f.path, ''), s.start_line, s.start_col
		FROM symbols s
		LEFT JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ? AND s.id = ?
	`, repoID, symbolID).Scan(
		&identity.ID, &identity.RepoID, &identity.Name,
		&identity.QualifiedName, &identity.Language, &identity.Kind,
		&identity.Path, &identity.StartLine, &identity.StartCol,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return symbolIdentity{}, false, nil
	}
	if err != nil {
		return symbolIdentity{}, false, err
	}
	identity.Path = filepath.ToSlash(identity.Path)
	return identity, true, nil
}

// lessSymbolIdentity is the canonical total order over symbol identities.
//
// Path first, then qualified name, then kind, then position. Position is the
// backstop for two symbols that genuinely share a file, a name and a kind
// (overloads); it is canonical source data, unlike the row id, so it orders the
// same way in every database that holds the same graph.
func lessSymbolIdentity(a, b symbolIdentity) bool {
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.QualifiedName != b.QualifiedName {
		return a.QualifiedName < b.QualifiedName
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.StartLine != b.StartLine {
		return a.StartLine < b.StartLine
	}
	return a.StartCol < b.StartCol
}

// symbolIdentities loads the identity of every given symbol id, chunked.
//
// It replaces a per-id SELECT: PageRank ran one round trip for each row it
// returned, which is a page-sized N+1 on top of the real work.
func (s *Store) symbolIdentities(ctx context.Context, ids []int64) (map[int64]symbolIdentity, error) {
	out := make(map[int64]symbolIdentity, len(ids))
	for _, chunk := range chunkInt64s(ids, sqliteInClauseBatchSize) {
		// No repo_id predicate, deliberately. The per-id SELECT this replaces
		// keyed on `s.id` alone, and `symbols.id` is the primary key, so adding
		// the repository would only ever *remove* a row -- one reached through a
		// cross-repository dst_symbol_id, which the schema does not forbid. That
		// row used to appear in the page with its real name; dropping it here
		// would silently retitle the change as a bug fix.
		rows, err := s.db.QueryContext(ctx, `
			SELECT s.id, s.qualified_name, s.kind, COALESCE(f.path, ''), s.start_line, s.start_col
			FROM symbols s
			LEFT JOIN files f ON f.id = s.file_id
			WHERE s.id IN (`+placeholders(len(chunk))+`)
		`, int64SliceToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var si symbolIdentity
			if err := rows.Scan(&id, &si.QualifiedName, &si.Kind, &si.Path, &si.StartLine, &si.StartCol); err != nil {
				_ = rows.Close()
				return nil, err
			}
			si.Path = filepath.ToSlash(si.Path)
			out[id] = si
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
		-- The tie-break is the pair of grouping keys, which is unique per row, so
		-- the order is total. It sorts the *stored* form rather than the
		-- canonical one: REPLACE() here cost ~6% of this query on a 100k-symbol
		-- graph, and the two forms differ only on Windows, where files.path is
		-- native. P23 makes files.path canonical and removes the distinction
		-- globally; paying for it per row in the meantime is not worth it.
		-- Edge counts tie constantly -- most coupled pairs share one or two
		-- edges -- so score alone decides neither the order of the page nor its
		-- membership. The grouping keys are already computed and are a total
		-- order over the result, so they cost nothing as the tie-break.
		ORDER BY edge_count DESC, f1.path ASC, f2.path ASC
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
			// Slash form, like every other path this store hands out -- and like
			// PageRank's `file`, so the three analyses of one tool agree.
			"file_a":     filepath.ToSlash(fileA),
			"file_b":     filepath.ToSlash(fileB),
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
		src, dst = filepath.ToSlash(src), filepath.ToSlash(dst)
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
	// And sort each node's successors, which the DFS walks in order.
	//
	// Sorted roots were enough to make the *starting points* stable but not the
	// walk: successors were visited in whatever order the query yielded, and
	// that decides which back edge is found first and so which cycles a limited
	// page reports. Sorting here rather than with an ORDER BY on the query: the
	// paths are already canonical by this point, so this orders the same way on
	// every platform, and it measured ~28% cheaper than making SQLite do it.
	//
	// This is a guarantee rather than a demonstrated fix: removing it does not
	// make the determinism tests fail, because the DISTINCT above happens to
	// yield path order on the shapes tried. It costs one sort of a list that is
	// already built.
	for f := range fileGraph {
		sort.Strings(fileGraph[f])
	}

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

// safeLimit normalizes a page size for a query that answers a public tool. It
// is the store's defensive backstop, not the public policy: MCP and the CLI
// reject an out-of-range `limit` before a query runs, and this only bounds a
// value that reached the store without passing through them.
func safeLimit(limit int) int {
	return limits.PageRows(limit)
}

// exportLimit normalizes a page size for the bulk exporters. They page with a
// larger window than a query does, and `graph export --limit 0` means "stream
// everything", so they cannot share the query-page ceiling.
func exportLimit(limit int) int {
	return limits.ExportRows(limit)
}

func safeOffset(offset int) int {
	return limits.Offset(offset)
}

func normalizeRepoRelPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	// Normalize common caller variations: forward slashes, leading ./, etc.
	path = filepath.FromSlash(path)
	path = filepath.Clean(path)
	if path == "." {
		return ""
	}
	return path
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
		variants := storedPathVariants(CanonicalRelPath(pathFilter))
		query += ` AND (` + strings.TrimRight(strings.Repeat("path LIKE ? OR ", len(variants)), " OR ") + `)`
		for _, variant := range variants {
			args = append(args, variant+"%")
		}
	}
	query += ` ORDER BY REPLACE(path, char(92), '/') ASC LIMIT ? OFFSET ?`
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
			"path":       filepath.ToSlash(path),
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
		-- f.repo_id is implied by the join (a symbol's file is in the symbol's
		-- repo), but stating it lets SQLite seek idx_files_repo_path instead of
		-- scanning every repo's files, which is also what makes the (f.path,
		-- s.start_line) ordering come out of the indexes rather than a sort.
		WHERE s.repo_id = ? AND f.repo_id = ?
		  AND s.kind IN ('function', 'method', 'type', 'class', 'struct', 'interface')
		  AND NOT EXISTS (
		      SELECT 1 FROM edges e
		      WHERE e.repo_id = s.repo_id AND e.dst_symbol_id = s.id
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM references_tbl r
		      WHERE r.repo_id = s.repo_id AND r.symbol_id = s.id
		  )
		  AND s.name NOT IN ('main', 'init', 'Main', 'Init')
		  AND s.name NOT LIKE 'Test%'
		  AND s.name NOT LIKE 'Benchmark%'
		  AND s.name NOT LIKE 'Example%'
		ORDER BY REPLACE(f.path, char(92), '/'), s.start_line
		LIMIT ? OFFSET ?
	`, repoID, repoID, safeLimit(limit), safeOffset(offset))
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
			"file":       filepath.ToSlash(path),
			"language":   language,
			"start_line": startLine,
			"end_line":   endLine,
		})
	}
	return out, rows.Err()
}

// --- Embedding methods ---

// UpsertSymbolEmbeddings stores vectors against exact persisted symbol IDs.
func (s *Store) UpsertSymbolEmbeddings(ctx context.Context, repoID int64, modelName string, items []SymbolEmbeddingUpsert) error {
	return s.UpsertSymbolEmbeddingsBatch(ctx, repoID, modelName, items)
}

type SymbolEmbeddingUpsert struct {
	SymbolID int64
	FileID   int64
	Vector   []float32
}

func (s *Store) UpsertSymbolEmbeddingsBatch(ctx context.Context, repoID int64, modelName string, items []SymbolEmbeddingUpsert) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	valid := 0
	for _, item := range items {
		if item.SymbolID != 0 && item.FileID != 0 && len(item.Vector) > 0 {
			valid++
		}
	}
	if valid == 0 {
		_ = tx.Rollback()
		return nil
	}

	const embedCols = 7
	embedArgs := make([]any, 0, sqliteEmbeddingValuesBatchRows*embedCols)
	flush := func() error {
		if len(embedArgs) == 0 {
			return nil
		}
		rows := len(embedArgs) / embedCols
		var b strings.Builder
		b.WriteString(`INSERT INTO symbol_embeddings(symbol_id, file_id, repo_id, embedding, dimensions, model_name, updated_at) VALUES `)
		for i := 0; i < rows; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("(?,?,?,?,?,?,?)")
		}
		b.WriteString(`
			ON CONFLICT(symbol_id) DO UPDATE SET
				embedding = excluded.embedding,
				dimensions = excluded.dimensions,
				model_name = excluded.model_name,
				updated_at = excluded.updated_at
		`)
		if _, err := tx.ExecContext(ctx, b.String(), embedArgs...); err != nil {
			return err
		}
		embedArgs = embedArgs[:0]
		return nil
	}

	for _, item := range items {
		if item.SymbolID == 0 || item.FileID == 0 || len(item.Vector) == 0 {
			continue
		}
		embedArgs = append(embedArgs, item.SymbolID, item.FileID, repoID, float32ToBytes(item.Vector), len(item.Vector), modelName, now)
		if len(embedArgs) >= sqliteEmbeddingValuesBatchRows*embedCols {
			if err := flush(); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	if err := flush(); err != nil {
		_ = tx.Rollback()
		return err
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
		id        int64
		file      string
		symbol    string
		kind      string
		signature string
		score     float64
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
			// Canonical form: HybridSearch fuses these entries with SearchSymbols
			// results keyed on `file + "::" + qualified_name`, and SearchSymbols
			// reports the slash form. Left native, the two halves of the fusion
			// would never meet on Windows and every hit would score as if it had
			// been found by one searcher only.
			candidates = append(candidates, scored{id: symbolID, file: CanonicalRelPath(filePath), symbol: qualName, kind: kind, signature: sig, score: sim})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Equal cosine similarity is common for short, similar symbol texts; without
	// the key tie-break the page a symbol lands on would depend on scan order.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].file != candidates[j].file {
			return candidates[i].file < candidates[j].file
		}
		if candidates[i].symbol != candidates[j].symbol {
			return candidates[i].symbol < candidates[j].symbol
		}
		if candidates[i].kind != candidates[j].kind {
			return candidates[i].kind < candidates[j].kind
		}
		return candidates[i].signature < candidates[j].signature
	})

	end := min(offset+limit, len(candidates))
	if offset >= len(candidates) {
		return nil, nil
	}
	candidates = candidates[offset:end]

	out := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, map[string]any{
			"symbol_id": c.id,
			"file":      c.file,
			"symbol":    c.symbol,
			"kind":      c.kind,
			"signature": c.signature,
			"score":     c.score,
			"why":       []string{"vector_similarity"},
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
		id        int64
		file      string
		symbol    string
		kind      string
		signature string
		score     float64
		why       []string
	}
	merged := map[string]*entry{}

	for rank, sym := range ftsResults {
		key := strconv.FormatInt(sym.ID, 10)
		e, ok := merged[key]
		if !ok {
			e = &entry{id: sym.ID, file: sym.FilePath, symbol: sym.QualifiedName, kind: sym.Kind, signature: sym.Signature}
			merged[key] = e
		}
		e.score += 1.0 / float64(fusionK+rank+1)
		e.why = appendUnique(e.why, "fts")
	}

	for rank, vm := range vecResults {
		id, ok := vm["symbol_id"].(int64)
		if !ok || id == 0 {
			continue
		}
		key := strconv.FormatInt(id, 10)
		e, ok := merged[key]
		if !ok {
			signature, _ := vm["signature"].(string)
			e = &entry{
				id:        id,
				file:      vm["file"].(string),
				symbol:    vm["symbol"].(string),
				kind:      vm["kind"].(string),
				signature: signature,
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
	// RRF scores tie often (two entries found at the same rank by the same
	// searcher score identically), and the fused entries arrive from a map, so
	// score alone would make both the page boundary and the order within a page
	// depend on Go's map iteration. The grouping keys complete the order.
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].score != sorted[j].score {
			return sorted[i].score > sorted[j].score
		}
		if sorted[i].file != sorted[j].file {
			return sorted[i].file < sorted[j].file
		}
		if sorted[i].symbol != sorted[j].symbol {
			return sorted[i].symbol < sorted[j].symbol
		}
		if sorted[i].kind != sorted[j].kind {
			return sorted[i].kind < sorted[j].kind
		}
		return sorted[i].signature < sorted[j].signature
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
			"symbol_id": e.id,
			"file":      e.file,
			"symbol":    e.symbol,
			"kind":      e.kind,
			"signature": e.signature,
			"score":     e.score,
			"why":       e.why,
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
	// Deriving the totals from the breakdowns (below) removed the Stats call
	// that used to front this function, and with it the only thing that failed
	// for a repository id that does not exist. Without this probe an unknown id
	// would return a well-formed document full of zeroes instead of an error.
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM repos WHERE id = ?`, repoID).Scan(&exists); err != nil {
		// This error is returned straight to an MCP client, so an absent repo
		// row is reported in CodeGraph's vocabulary rather than the driver's.
		// A genuine database failure keeps its own wording.
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("architecture overview: %w: repo %d", ErrRepoNotIndexed, repoID)
		}
		return nil, fmt.Errorf("architecture overview: repo %d: %w", repoID, err)
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
			`SELECT SUBSTR(REPLACE(path, char(92), '/') , 1, INSTR(REPLACE(path, char(92), '/') || '/', '/') - 1) AS dir, COUNT(*) as count FROM files WHERE repo_id = ? AND is_deleted = 0 GROUP BY dir ORDER BY count DESC LIMIT 20`,
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
	entryPoints, err := s.topDegreeSymbols(ctx, repoID, "dst_symbol_id", "caller_count", architectureTopN)
	if err != nil {
		return nil, fmt.Errorf("architecture overview: entry points: %w", err)
	}

	// Hub symbols (most outgoing edges)
	hubSymbols, err := s.topDegreeSymbols(ctx, repoID, "src_symbol_id", "callee_count", architectureTopN)
	if err != nil {
		return nil, fmt.Errorf("architecture overview: hub symbols: %w", err)
	}

	// Totals. Three of the four are exactly the sums of breakdowns this
	// function has already computed, so re-deriving them costs nothing;
	// asking Stats for them meant a second full count of files, symbols and
	// edges (plus a repeated language GROUP BY) for numbers already in hand.
	// Only the reference count has no breakdown to sum.
	var references int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM references_tbl WHERE repo_id = ?`, repoID).Scan(&references); err != nil {
		return nil, fmt.Errorf("architecture overview: references: %w", err)
	}

	return map[string]any{
		"languages":       languages,
		"top_directories": topDirs,
		"symbol_kinds":    symbolKinds,
		"edge_kinds":      edgeKinds,
		"entry_points":    entryPoints,
		"hub_symbols":     hubSymbols,
		"totals": map[string]any{
			"files":      sumCountField(languages, "file_count"),
			"symbols":    sumCountField(symbolKinds, "count"),
			"edges":      sumCountField(edgeKinds, "count"),
			"references": references,
		},
	}, nil
}

// sumCountField totals an integer field across a breakdown list.
func sumCountField(rows []map[string]any, field string) int64 {
	var total int64
	for _, row := range rows {
		if v, ok := row[field].(int); ok {
			total += int64(v)
		}
	}
	return total
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
		path = filepath.ToSlash(path)
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
		ORDER BY REPLACE(path, char(92), '/')`, repoID)
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
		paths = append(paths, filepath.ToSlash(path))
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
			// SemanticSearch reports canonical (slash) paths; `files.path` holds the
			// indexing host's native form. Bind both, or this predicate matches
			// nothing on Windows and the benchmark reports a zero-byte context.
			bound := make([]string, 0, len(paths)*2)
			for _, p := range paths {
				bound = append(bound, storedPathVariants(p)...)
			}
			placeholders := strings.TrimRight(strings.Repeat("?,", len(bound)), ",")
			args := make([]any, 0, len(bound)+1)
			args = append(args, repoID)
			for _, p := range bound {
				args = append(args, p)
			}
			// Rows, not aggregates: a database that holds both forms of one path (a
			// graph.sqlite carried between hosts) would otherwise count that file
			// twice and overstate the context it charges for. Folding on the canonical
			// path counts each logical file once.
			rows, err := s.db.QueryContext(ctx,
				`SELECT path, size_bytes FROM files WHERE repo_id = ? AND is_deleted = 0 AND path IN (`+placeholders+`)`,
				args...,
			)
			if err != nil {
				return nil, fmt.Errorf("benchmark context bytes: %w", err)
			}
			sizes := map[string]int64{}
			for rows.Next() {
				var path string
				var size int64
				if err := rows.Scan(&path, &size); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("benchmark context bytes: %w", err)
				}
				sizes[CanonicalRelPath(path)] = size
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("benchmark context bytes: %w", err)
			}
			if err := rows.Close(); err != nil {
				return nil, fmt.Errorf("benchmark context bytes: %w", err)
			}
			for _, size := range sizes {
				contextFileCount++
				contextBytes += size
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
	// These two keep a larger default than the shared one, but they must still
	// pass through the store's ceiling: the backstop is only a backstop if it
	// has no exceptions.
	limit = min(limit, limits.StoreMaxRows)
	offset = safeOffset(offset)
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
	limit = min(limit, limits.StoreMaxRows)
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
