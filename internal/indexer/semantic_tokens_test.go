package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/texttoken"
)

const semanticTokenFixture = `package main

func RealConnectionPool() {}

func parseShortArg() {}

func main() {
	RealConnectionPool()
	parseShortArg()
}
`

func indexSemanticTokenFixture(t *testing.T, s *store.Store, repoRoot string, opts Options) store.ScanSummary {
	t.Helper()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	opts.RepoRoot = repoRoot
	summary, err := idx.Index(context.Background(), opts)
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	return summary
}

// openIndexedDatabase opens the sqlite file the indexer just wrote, directly.
// The tests below assert on `symbol_tokens`, which is deliberately not public
// store API, and reaching it through database/sql keeps it that way.
//
// The DSN comes from store.BuildSQLiteDSN rather than from concatenating a
// `file:` prefix onto the path: a Windows path is `C:\dir\graph.sqlite`, which
// is not a URI, and the builder is the thing that knows about drive letters,
// UNC paths and escaping. The driver name differs between the cgo and the
// modernc build, so it is resolved from the ones actually registered.
func openIndexedDatabase(t *testing.T, dbPath string, readOnly bool) *sql.DB {
	t.Helper()
	drivers := sql.Drivers()
	name := ""
	for _, candidate := range []string{"sqlite", "sqlite3"} {
		if slices.Contains(drivers, candidate) {
			name = candidate
			break
		}
	}
	if name == "" {
		t.Fatalf("no sqlite driver registered, have %v", drivers)
	}
	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, readOnly)
	if err != nil {
		t.Fatalf("build dsn for %s: %v", dbPath, err)
	}
	db, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func semanticTokenDump(t *testing.T, dbPath string) (string, int) {
	t.Helper()
	ctx := context.Background()
	db := openIndexedDatabase(t, dbPath, true)
	rows, err := db.QueryContext(ctx, `
		SELECT sym.qualified_name, st.token, st.weight
		FROM symbol_tokens st JOIN symbols sym ON sym.id = st.symbol_id
		JOIN files f ON f.id = sym.file_id AND f.is_deleted = 0
		ORDER BY sym.qualified_name, st.token`)
	if err != nil {
		t.Fatalf("dump tokens: %v", err)
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		var qualified, token string
		var weight float64
		if err := rows.Scan(&qualified, &token, &weight); err != nil {
			t.Fatal(err)
		}
		out += fmt.Sprintf("%s|%s|%g\n", qualified, token, weight)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT semantic_token_version FROM repos LIMIT 1`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return out, version
}

// makeSemanticTokensLegacy puts the database back into the state an older
// binary leaves it in: whole-token rows and no recorded version.
func makeSemanticTokensLegacy(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	db := openIndexedDatabase(t, dbPath, false)
	rows, err := db.QueryContext(ctx, `SELECT id, name, COALESCE(qualified_name,''), COALESCE(signature,''), COALESCE(doc_summary,'') FROM symbols`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id                                    int64
		name, qualified, signature, docSumary string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.qualified, &r.signature, &r.docSumary); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		all = append(all, r)
	}
	rows.Close()
	if _, err := db.ExecContext(ctx, `DELETE FROM symbol_tokens`); err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		weights := map[string]float64{}
		texttoken.WeightsStringsInto(weights, r.name, r.qualified, r.signature, r.docSumary)
		for token, weight := range weights {
			if _, err := db.ExecContext(ctx,
				`INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?,?,?)`, r.id, token, weight); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE repos SET semantic_token_version = 0`); err != nil {
		t.Fatal(err)
	}
}

// A fresh index writes decomposed tokens and records the version, without
// going anywhere near the refresh path.
func TestIndexWritesCurrentSemanticTokenVersion(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "main.go"), semanticTokenFixture)
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	indexSemanticTokenFixture(t, s, repoRoot, Options{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	dump, version := semanticTokenDump(t, dbPath)
	if version != texttoken.SemanticTokenVersion {
		t.Errorf("semantic_token_version = %d, want %d", version, texttoken.SemanticTokenVersion)
	}
	for _, want := range []string{"|connection|", "|pool|", "|real|", "|parse|", "|short|", "|arg|"} {
		if !strings.Contains(dump, want) {
			t.Errorf("fresh index missing component token %q", want)
		}
	}
}

// The migration path: an index whose rows were written under the old rules is
// brought forward by an ordinary update, and lands on exactly what a fresh
// index would have written. No file is re-read to do it.
func TestUpdateRefreshesLegacySemanticTokens(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "main.go"), semanticTokenFixture)

	freshDB := filepath.Join(t.TempDir(), "fresh.sqlite")
	fresh, err := store.Open(freshDB)
	if err != nil {
		t.Fatal(err)
	}
	indexSemanticTokenFixture(t, fresh, repoRoot, Options{})
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	wantDump, _ := semanticTokenDump(t, freshDB)

	legacyDB := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := store.Open(legacyDB)
	if err != nil {
		t.Fatal(err)
	}
	indexSemanticTokenFixture(t, legacy, repoRoot, Options{})
	repo, err := legacy.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	makeSemanticTokensLegacy(t, legacyDB)
	if legacy, err = store.Open(legacyDB); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.SemanticSearch(ctx, repo.ID, "connection pool", 10, 0); err == nil {
		t.Fatal("SemanticSearch served a legacy token index")
	}
	stale, err := legacy.SemanticTokensStale(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("legacy index not reported stale")
	}

	// An ordinary update with nothing changed on disk is the whole migration.
	summary := indexSemanticTokenFixture(t, legacy, repoRoot, Options{})
	if summary.FilesChanged != 0 {
		t.Errorf("FilesChanged = %d, want 0: the refresh must not reparse", summary.FilesChanged)
	}
	if _, err := legacy.SemanticSearch(ctx, repo.ID, "connection pool", 10, 0); err != nil {
		t.Errorf("SemanticSearch after refresh: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	gotDump, version := semanticTokenDump(t, legacyDB)
	if version != texttoken.SemanticTokenVersion {
		t.Errorf("semantic_token_version = %d, want %d", version, texttoken.SemanticTokenVersion)
	}
	if gotDump != wantDump {
		t.Errorf("refreshed index differs from a fresh one:\n got %s\nwant %s", gotDump, wantDump)
	}
}

// An incremental update that adds and removes a file must leave the same
// tokens a full rebuild would, with no row left behind for the deleted one.
func TestIncrementalUpdateSemanticTokenParity(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "main.go"), semanticTokenFixture)

	incrementalDB := filepath.Join(t.TempDir(), "incremental.sqlite")
	incremental, err := store.Open(incrementalDB)
	if err != nil {
		t.Fatal(err)
	}
	indexSemanticTokenFixture(t, incremental, repoRoot, Options{})

	writeFile(t, filepath.Join(repoRoot, "added.go"), "package main\n\nfunc DiskLruCacheEntry() {}\n")
	if err := os.Remove(filepath.Join(repoRoot, "main.go")); err != nil {
		t.Fatal(err)
	}
	indexSemanticTokenFixture(t, incremental, repoRoot, Options{})
	if err := incremental.Close(); err != nil {
		t.Fatal(err)
	}
	gotDump, _ := semanticTokenDump(t, incrementalDB)

	rebuiltDB := filepath.Join(t.TempDir(), "rebuilt.sqlite")
	rebuilt, err := store.Open(rebuiltDB)
	if err != nil {
		t.Fatal(err)
	}
	indexSemanticTokenFixture(t, rebuilt, repoRoot, Options{})
	if err := rebuilt.Close(); err != nil {
		t.Fatal(err)
	}
	wantDump, _ := semanticTokenDump(t, rebuiltDB)

	if gotDump != wantDump {
		t.Errorf("incremental tokens differ from a rebuild:\n got %s\nwant %s", gotDump, wantDump)
	}
	if strings.Contains(gotDump, "|pool|") {
		t.Error("tokens of the deleted file survived the update")
	}
	if !strings.Contains(gotDump, "|lru|") {
		t.Error("tokens of the added file are missing")
	}
}

// A scan that returns an error does not advance the stored version, and the
// index stays refused until an ordinary scan migrates it.
//
// The cancellation lands early -- `UpsertRepo` is the first context-carrying
// statement in `run`, well before the staleness probe -- so what this pins is
// the error path, not an interrupted rewrite. The rewrite's own atomicity is
// `RefreshSemanticTokens`'s claim and is tested where it lives, in
// store.TestRefreshSemanticTokensLeavesTheOldIndexIntactOnFailure.
func TestFailedScanLeavesSemanticTokensStale(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "main.go"), semanticTokenFixture)

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	indexSemanticTokenFixture(t, s, repoRoot, Options{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	makeSemanticTokensLegacy(t, dbPath)

	if s, err = store.Open(dbPath); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Cancelled before the first read, so the scan cannot reach its success
	// path whichever statement it dies on.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(cancelled, Options{RepoRoot: repoRoot}); err == nil {
		t.Fatal("Index with a cancelled context returned no error")
	}

	stale, err := s.SemanticTokensStale(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Error("a scan that returned an error advanced the semantic token version")
	}
	if _, err := s.SemanticSearch(ctx, repo.ID, "connection pool", 10, 0); !errors.Is(err, store.ErrSemanticTokensStale) {
		t.Errorf("SemanticSearch after a failed scan: err = %v, want ErrSemanticTokensStale", err)
	}

	// And the next ordinary run still migrates it.
	indexSemanticTokenFixture(t, s, repoRoot, Options{})
	if _, err := s.SemanticSearch(ctx, repo.ID, "connection pool", 10, 0); err != nil {
		t.Errorf("SemanticSearch after the recovering scan: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// A legacy repository row that holds no symbols reads as current rather than
// stale -- there is nothing written under any rules to refuse -- so the
// refresh branch never runs for it. MarkSemanticTokensCurrent at the end of a
// successful scan exists for exactly this case: without it the row would keep
// its stored 0 the moment a later scan gave it symbols, and the read paths
// would refuse an index that is in fact correct.
func TestLegacyEmptyRepositoryBecomesCurrentWhenItGainsSymbols(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	indexSemanticTokenFixture(t, s, repoRoot, Options{})
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// An empty repository has no tokens to rewrite, so this is the whole of
	// what a legacy row looks like here.
	makeSemanticTokensLegacy(t, dbPath)

	if s, err = store.Open(dbPath); err != nil {
		t.Fatal(err)
	}
	stale, err := s.SemanticTokensStale(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("a repository with no symbols reported a stale token index")
	}

	writeFile(t, filepath.Join(repoRoot, "main.go"), semanticTokenFixture)
	indexSemanticTokenFixture(t, s, repoRoot, Options{})
	if _, err := s.SemanticSearch(ctx, repo.ID, "connection pool", 10, 0); err != nil {
		t.Errorf("SemanticSearch after the repository gained symbols: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, version := semanticTokenDump(t, dbPath); version != texttoken.SemanticTokenVersion {
		t.Errorf("semantic_token_version = %d, want %d", version, texttoken.SemanticTokenVersion)
	}
}
