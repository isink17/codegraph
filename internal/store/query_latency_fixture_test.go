package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Deterministic ~100k-symbol query-latency fixture.
//
// P12 measures warm local graph queries against a graph the size of a real
// large repository. The CodeGraph repo itself is far too small to expose the
// shapes that matter -- a symbol with tens of thousands of callers behaves
// nothing like a symbol with three -- so the latency target is proven against
// a synthetic graph built here.
//
// Three properties make it usable as evidence rather than decoration:
//
//  1. It is written through the production write path
//     (ReplaceFileGraphsBatchWithStats) and bound by the production resolvers
//     (ResolveEdges, ResolveTestLinks), so every derived column, index, FTS
//     row and edge binding is exactly what `codegraph index` would produce.
//     Nothing is hand-poked into a table.
//  2. It is fully deterministic: no rand, no clock, no map iteration in the
//     generator. The same fixture is produced on every run and every platform,
//     so a latency delta is attributable to a code change.
//  3. Degree is deliberately non-uniform. A fixture where every symbol has the
//     same fan-in would make caller/impact queries look fast for reasons that
//     do not survive contact with a real repository. This one models low-degree
//     symbols, medium-degree symbols, a hot hub with tens of thousands of
//     callers, a high fan-out source, a deep-but-bounded chain, an ambiguous
//     common name, a missing name, and a file with large test-link fan-out.
//
// Build cost is paid once per test binary (sync.Once) and is excluded from
// every measured query.

const (
	bigPkgCount      = 160 // packages; p%10 selects the language
	bigFilesPerPkg   = 24
	bigSymsPerFile   = 25
	bigTestFileCount = 400
	bigSymsPerTest   = 10

	bigChainLen     = 300  // deep-but-bounded dependency chain
	bigHubInPerFile = 8    // hub in-edges contributed by each Go source file
	bigHubOutDegree = 4000 // out-edges from the high fan-out source
	// Out-edges that deliberately resolve to nothing, so the callee fallback
	// path (name lookup per unresolved destination) is actually measured.
	bigUnresolvedOutDegree = 300
	bigHotTestFiles        = 50 // test files that all point at the same hot file
	bigMediumEveryNth      = 4  // every Nth Go file adds an edge to the medium target

	bigSrcFileCount = bigPkgCount * bigFilesPerPkg
	bigSrcSymCount  = bigSrcFileCount * bigSymsPerFile

	// Files whose symbols are the target of the hot test-link fan-out.
	bigHotSymCount = 10

	// Vocabulary seeds for the non-source files, kept far from the source
	// symbol index space so their generated docs differ from any real symbol's.
	bigHubG  = 1_000_000
	bigHotG  = 2_000_000
	bigTestG = 3_000_000
)

// bigCommonNames are deliberately colliding short names, so name-based lookup
// has an ambiguous case to resolve like a real repository does.
var bigCommonNames = []string{"Handle", "New", "Run", "Get", "Close"}

// bigGraphFixture is a built fixture plus the representative query targets
// selected from it. Benchmarks address targets by these fields rather than by
// literal names so the shape and the queries cannot drift apart.
type bigGraphFixture struct {
	store  *Store
	repoID int64
	dbPath string

	Files      int64
	Symbols    int64
	Edges      int64
	Resolved   int64
	Unresolved int64
	TestLinks  int64

	// Representative targets.
	HubTarget        string // very high fan-in ("hub.HubTarget")
	MediumTarget     string // medium fan-in
	LowTarget        string // low fan-in
	HubSource        string // very high fan-out
	UnresolvedSource string // high fan-out of unresolvable destinations
	ChainHead        string // head of the deep chain
	PlainSymbol      string // ordinary low-degree symbol
	PlainFile        string // ordinary source file
	HotFile          string // file with large test-link fan-out
	HotSymbol        string // symbol in HotFile
	CommonName       string // ambiguous short name (many matches)
	MissingName      string // name that exists nowhere
	FTSQuery         string // search term with many FTS matches
}

var (
	bigGraphOnce sync.Once
	bigGraphVal  *bigGraphFixture
	bigGraphErr  error
	bigGraphDir  string
)

// releaseBigGraph closes and deletes the fixture. Called once from TestMain.
func releaseBigGraph() {
	if bigGraphVal != nil {
		_ = bigGraphVal.store.Close()
	}
	if bigGraphDir != "" {
		_ = os.RemoveAll(bigGraphDir)
	}
}

// bigGraph returns the shared fixture, building it on first use.
//
// The store stays open for the lifetime of the test binary: benchmarks measure
// warm queries, and reopening per benchmark would measure page-cache warmup
// instead. The database file lives in os.MkdirTemp rather than t.TempDir so it
// outlives the first caller's test.
func bigGraph(tb testing.TB) *bigGraphFixture {
	tb.Helper()
	bigGraphOnce.Do(func() {
		bigGraphVal, bigGraphErr = buildBigGraph()
	})
	if bigGraphErr != nil {
		tb.Fatalf("build big graph fixture: %v", bigGraphErr)
	}
	return bigGraphVal
}

// bigLangForPkg maps a package index to a language. p%10 < 6 is Go, < 8 is
// TypeScript, else Python -- so a language is preserved by stepping p by 10,
// which is how same-language edge targets are chosen.
func bigLangForPkg(p int) (lang, ext string) {
	switch {
	case p%10 < 6:
		return "go", ".go"
	case p%10 < 8:
		return "typescript", ".ts"
	default:
		return "python", ".py"
	}
}

// bigGoPkg returns the i-th Go package index (6 of every 10).
func bigGoPkg(i int) int {
	i %= 96
	return (i/6)*10 + i%6
}

func bigSrcPath(p, j int) string {
	_, ext := bigLangForPkg(p)
	return fmt.Sprintf("internal/pkg%03d/file_%02d%s", p, j, ext)
}

// bigSymbolName returns the source-level name of source symbol (p, j, m).
func bigSymbolName(p, j, m int) string {
	if m == bigSymsPerFile-1 {
		return bigCommonNames[(p+j)%len(bigCommonNames)]
	}
	g := (p*bigFilesPerPkg+j)*bigSymsPerFile + m
	return fmt.Sprintf("Fn%06d", g)
}

// bigSymbolKind spreads kinds so that only a subset of symbols is edge-source
// eligible (the write path attributes an edge to the enclosing *function*).
func bigSymbolKind(m int) string {
	if m == bigSymsPerFile-1 {
		return "function"
	}
	switch m % 5 {
	case 3:
		return "method"
	case 4:
		return "type"
	default:
		return "function"
	}
}

// bigSymbolQName mirrors how each language spells a qualified name, so the
// derived resolution columns (qualified_suffix, dot_tail2, dot_tail3) are
// populated the way a real mixed-language repository populates them.
func bigSymbolQName(p, j, m int) (qname, container string) {
	lang, _ := bigLangForPkg(p)
	name := bigSymbolName(p, j, m)
	switch lang {
	case "typescript":
		return fmt.Sprintf("internal/pkg%03d/file_%02d.%s", p, j, name), ""
	case "python":
		return fmt.Sprintf("pkg%03d.mod%02d.%s", p, j, name), fmt.Sprintf("mod%02d", j)
	default:
		if m%3 == 0 {
			return fmt.Sprintf("pkg%03d.%s", p, name), ""
		}
		return fmt.Sprintf("pkg%03d.Type%02d.%s", p, j, name), fmt.Sprintf("Type%02d", j)
	}
}

func bigStableKey(p, j, m int) string {
	lang, _ := bigLangForPkg(p)
	qname, _ := bigSymbolQName(p, j, m)
	g := (p*bigFilesPerPkg+j)*bigSymsPerFile + m
	return fmt.Sprintf("%s:%s:%d", lang, qname, g)
}

func bigSymStartLine(m int) int { return m*20 + 1 }
func bigSymEndLine(m int) int   { return m*20 + 18 }

func buildBigGraph() (*bigGraphFixture, error) {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "codegraph-bigfixture-")
	if err != nil {
		return nil, err
	}
	bigGraphDir = dir
	dbPath := filepath.Join(dir, "graph.sqlite")
	s, err := OpenWithOptions(dbPath, OpenOptions{PerformanceProfile: "fast"})
	if err != nil {
		return nil, err
	}
	repo, err := s.UpsertRepo(ctx, dir)
	if err != nil {
		return nil, err
	}
	f := &bigGraphFixture{
		store:  s,
		repoID: repo.ID,
		dbPath: dbPath,

		HubTarget:        "hub.HubTarget",
		MediumTarget:     "hub.MediumTarget",
		LowTarget:        "hub.LowTarget",
		HubSource:        "hub.HubSource",
		UnresolvedSource: "hub.HubUnresolvedSource",
		ChainHead:        "hub.Chain000",
		HotFile:          "internal/hot/hot.go",
		HotSymbol:        "hot.HotSym00",
		CommonName:       "Handle",
		MissingName:      "pkg999.ThisSymbolDoesNotExist",
		FTSQuery:         "semaphore",
	}
	f.PlainFile = bigSrcPath(0, 3)
	f.PlainSymbol, _ = bigSymbolQName(0, 3, 6)

	if err := writeBigGraph(ctx, s, repo.ID, f); err != nil {
		return nil, err
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		return nil, err
	}
	if _, err := s.ResolveTestLinks(ctx, repo.ID); err != nil {
		return nil, err
	}
	if _, err := s.Analyze(ctx); err != nil {
		return nil, err
	}
	// Fold the WAL back into the main database so DBSizeBytes reports the
	// steady-state size a user would see, not the write-amplified build state.
	if _, err := s.WalCheckpointTruncate(ctx); err != nil {
		return nil, err
	}
	if err := f.loadCounts(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *bigGraphFixture) loadCounts(ctx context.Context) error {
	q := func(sql string) (int64, error) {
		var n int64
		err := f.store.db.QueryRowContext(ctx, sql, f.repoID).Scan(&n)
		return n, err
	}
	var err error
	if f.Files, err = q(`SELECT COUNT(1) FROM files WHERE repo_id = ? AND is_deleted = 0`); err != nil {
		return err
	}
	if f.Symbols, err = q(`SELECT COUNT(1) FROM symbols WHERE repo_id = ?`); err != nil {
		return err
	}
	if f.Edges, err = q(`SELECT COUNT(1) FROM edges WHERE repo_id = ?`); err != nil {
		return err
	}
	if f.Resolved, err = q(`SELECT COUNT(1) FROM edges WHERE repo_id = ? AND dst_symbol_id IS NOT NULL`); err != nil {
		return err
	}
	if f.Unresolved, err = q(`SELECT COUNT(1) FROM edges WHERE repo_id = ? AND dst_symbol_id IS NULL`); err != nil {
		return err
	}
	if f.TestLinks, err = q(`SELECT COUNT(1) FROM test_links WHERE repo_id = ?`); err != nil {
		return err
	}
	return nil
}

// DBSizeBytes reports the on-disk size of the fixture database, including WAL.
func (f *bigGraphFixture) DBSizeBytes() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if st, err := os.Stat(f.dbPath + suffix); err == nil {
			total += st.Size()
		}
	}
	return total
}

func (f *bigGraphFixture) Describe() string {
	return fmt.Sprintf("files=%d symbols=%d edges=%d resolved=%d unresolved=%d test_links=%d db_bytes=%d",
		f.Files, f.Symbols, f.Edges, f.Resolved, f.Unresolved, f.TestLinks, f.DBSizeBytes())
}
