package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

// ---------------------------------------------------------------------------
// Deterministic lifecycle adapter.
//
// The transitions under test (oversize, best-effort parse failure, recovery)
// must not depend on a real language parser happening to reject a byte
// sequence: a parser improvement would silently turn the fixture into a
// tautology. This adapter parses an explicit directive language and fails on
// demand, so "success -> fail -> success over identical bytes" is expressible.
// ---------------------------------------------------------------------------

type lifecycleAdapter struct {
	mu       sync.Mutex
	failing  map[string]bool
	parses   map[string]int
	profile  parser.Profile
	language string
	ext      string
}

func newLifecycleAdapter() *lifecycleAdapter {
	return &lifecycleAdapter{
		failing:  map[string]bool{},
		parses:   map[string]int{},
		language: "lifecycle",
		ext:      ".lc",
		profile:  parser.Profile{ID: "lifecycle:v1", EmitsCallEdges: true},
	}
}

func (a *lifecycleAdapter) Language() string     { return a.language }
func (a *lifecycleAdapter) Extensions() []string { return []string{a.ext} }
func (a *lifecycleAdapter) Profile() parser.Profile {
	return a.profile
}

func (a *lifecycleAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), a.ext)
}

func (a *lifecycleAdapter) setFailing(base string, failing bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failing[base] = failing
}

func (a *lifecycleAdapter) parseCount(base string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.parses[base]
}

func (a *lifecycleAdapter) Parse(_ context.Context, path string, content []byte) (graph.ParsedFile, error) {
	base := filepath.Base(path)
	a.mu.Lock()
	a.parses[base]++
	failing := a.failing[base]
	a.mu.Unlock()
	if failing {
		return graph.ParsedFile{}, errors.New("synthetic parse failure")
	}
	parsed := graph.ParsedFile{Language: a.language}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		verb, arg, _ := strings.Cut(line, " ")
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		switch verb {
		case "sym":
			parsed.Symbols = append(parsed.Symbols, graph.Symbol{
				Language:      a.language,
				Kind:          "function",
				Name:          arg,
				QualifiedName: arg,
				StableKey:     a.language + ":" + arg,
				// One declaration spanning the whole file, so every `call`
				// directive is attributed to it by line.
				Range: graph.Position{StartLine: 1, EndLine: 10000},
			})
		case "call":
			parsed.Edges = append(parsed.Edges, graph.Edge{DstName: arg, Kind: "calls", Line: 2})
		case "ref":
			parsed.References = append(parsed.References, graph.Reference{
				Kind:  "identifier",
				Name:  arg,
				Range: graph.Position{StartLine: 3, EndLine: 3},
			})
		case "testlink":
			if len(parsed.Symbols) == 0 {
				continue
			}
			idx := len(parsed.Symbols) - 1
			owner := parsed.Symbols[idx]
			parsed.TestLinks = append(parsed.TestLinks, graph.TestLink{
				TestName:        owner.Name,
				TargetName:      arg,
				Reason:          "naming",
				Score:           1,
				TestSymbolKey:   owner.StableKey,
				TargetStableKey: a.language + ":" + arg,
				TestSymbolIndex: &idx,
			})
		case "import":
			parsed.Imports = append(parsed.Imports, arg)
			parsed.Scope.Imports = append(parsed.Scope.Imports, graph.ScopeImport{
				SourceSpecifier: arg,
				Kind:            graph.ScopeImportSideEffect,
			})
		}
	}
	return parsed, nil
}

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

type lifecycleFixture struct {
	t       *testing.T
	root    string
	dbPath  string
	store   *store.Store
	adapter *lifecycleAdapter
	idx     *Indexer
	repoID  int64
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	adapter := newLifecycleAdapter()
	f := &lifecycleFixture{
		t:       t,
		root:    root,
		dbPath:  dbPath,
		store:   s,
		adapter: adapter,
		idx:     New(s, parser.NewRegistry(adapter), nil),
	}
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	f.repoID = repo.ID
	return f
}

// repoConfig writes .codegraph/config.json for the fixture repository.
func (f *lifecycleFixture) repoConfig(maxFileSize int64, parseErrorPolicy string) {
	f.t.Helper()
	dir := filepath.Join(f.root, config.RepoArtifactsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatalf("MkdirAll() error = %v", err)
	}
	body := fmt.Sprintf(`{"max_file_size_bytes": %d, "parse_error_policy": %q}`, maxFileSize, parseErrorPolicy)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		f.t.Fatalf("WriteFile(config.json) error = %v", err)
	}
}

func (f *lifecycleFixture) write(rel, content string) {
	f.t.Helper()
	writeFile(f.t, filepath.Join(f.root, rel), content)
}

// rewrite changes a file's bytes and forces a distinct mtime, so change
// detection sees a modification rather than a coincidental stat match.
func (f *lifecycleFixture) rewrite(rel, content string) {
	f.t.Helper()
	time.Sleep(2 * time.Millisecond)
	f.write(rel, content)
}

func (f *lifecycleFixture) stat(rel string) os.FileInfo {
	f.t.Helper()
	info, err := os.Stat(filepath.Join(f.root, rel))
	if err != nil {
		f.t.Fatalf("Stat(%s) error = %v", rel, err)
	}
	return info
}

func (f *lifecycleFixture) index(opts Options) store.ScanSummary {
	f.t.Helper()
	opts.RepoRoot = f.root
	summary, err := f.idx.Index(context.Background(), opts)
	if err != nil {
		f.t.Fatalf("Index() error = %v", err)
	}
	return summary
}

func (f *lifecycleFixture) update(opts Options) store.ScanSummary {
	f.t.Helper()
	opts.RepoRoot = f.root
	summary, err := f.idx.Update(context.Background(), opts)
	if err != nil {
		f.t.Fatalf("Update() error = %v", err)
	}
	return summary
}

func (f *lifecycleFixture) raw() *sql.DB {
	f.t.Helper()
	dsn, err := store.BuildSQLiteDSN(f.dbPath, store.OpenOptions{}, false, false)
	if err != nil {
		f.t.Fatalf("BuildSQLiteDSN() error = %v", err)
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		f.t.Fatalf("sql.Open() error = %v", err)
	}
	f.t.Cleanup(func() { db.Close() })
	return db
}

func (f *lifecycleFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.raw().QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count query error = %v (%s)", err, query)
	}
	return n
}

type fileRow struct {
	Language      string
	SizeBytes     int64
	MtimeUnixNS   int64
	ContentSHA    string
	ParseState    string
	IsDeleted     int
	ParserProfile string
}

func (f *lifecycleFixture) fileRow(rel string) fileRow {
	f.t.Helper()
	var row fileRow
	err := f.raw().QueryRow(`
		SELECT language, size_bytes, mtime_unix_ns, content_sha256, parse_state, is_deleted, COALESCE(parser_profile, '')
		FROM files WHERE repo_id = ? AND path = ?
	`, f.repoID, rel).Scan(&row.Language, &row.SizeBytes, &row.MtimeUnixNS, &row.ContentSHA, &row.ParseState, &row.IsDeleted, &row.ParserProfile)
	if err != nil {
		f.t.Fatalf("fileRow(%s) error = %v", rel, err)
	}
	return row
}

// parserOwnedRows counts every parser-owned fact still attributed to a path.
// It is deliberately wider than `symbols`: the defect this phase fixes leaves
// references, imports, edges and test links behind just as readily.
func (f *lifecycleFixture) parserOwnedRows(rel string) map[string]int {
	f.t.Helper()
	out := map[string]int{}
	for name, query := range map[string]string{
		"symbols":               `SELECT COUNT(*) FROM symbols s JOIN files f ON f.id = s.file_id WHERE f.repo_id = ? AND f.path = ?`,
		"symbol_fts":            `SELECT COUNT(*) FROM symbol_fts t JOIN symbols s ON s.id = t.symbol_id JOIN files f ON f.id = s.file_id WHERE f.repo_id = ? AND f.path = ?`,
		"symbol_tokens":         `SELECT COUNT(*) FROM symbol_tokens t JOIN symbols s ON s.id = t.symbol_id JOIN files f ON f.id = s.file_id WHERE f.repo_id = ? AND f.path = ?`,
		"symbol_embeddings":     `SELECT COUNT(*) FROM symbol_embeddings t JOIN files f ON f.id = t.file_id WHERE f.repo_id = ? AND f.path = ?`,
		"references_tbl":        `SELECT COUNT(*) FROM references_tbl t JOIN files f ON f.id = t.file_id WHERE f.repo_id = ? AND f.path = ?`,
		"edges":                 `SELECT COUNT(*) FROM edges t JOIN files f ON f.id = t.file_id WHERE f.repo_id = ? AND f.path = ?`,
		"file_imports":          `SELECT COUNT(*) FROM file_imports t JOIN files f ON f.id = t.file_id WHERE f.repo_id = ? AND f.path = ?`,
		"scope_import_evidence": `SELECT COUNT(*) FROM scope_import_evidence t JOIN files f ON f.id = t.file_id WHERE f.repo_id = ? AND f.path = ?`,
		"test_links":            `SELECT COUNT(*) FROM test_links t JOIN files f ON f.id = t.test_file_id WHERE f.repo_id = ? AND f.path = ?`,
	} {
		out[name] = f.count(query, f.repoID, rel)
	}
	return out
}

func (f *lifecycleFixture) assertNoParserOwnedRows(rel string) {
	f.t.Helper()
	for table, n := range f.parserOwnedRows(rel) {
		if n != 0 {
			f.t.Errorf("stale %s rows for retired %s = %d, want 0", table, rel, n)
		}
	}
}

func (f *lifecycleFixture) symbolNames() []string {
	f.t.Helper()
	rows, err := f.raw().Query(`SELECT name FROM symbols s JOIN files fl ON fl.id = s.file_id WHERE fl.repo_id = ? ORDER BY name`, f.repoID)
	if err != nil {
		f.t.Fatalf("symbolNames() error = %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			f.t.Fatalf("scan error = %v", err)
		}
		out = append(out, name)
	}
	return out
}

// findSymbolExact goes through a real public query surface, so the stale
// symbol is proven observable to callers and not merely present in a table.
func (f *lifecycleFixture) findSymbolExact(name string) []graph.Symbol {
	f.t.Helper()
	syms, err := f.store.FindSymbolExact(context.Background(), f.repoID, name, 50, 0)
	if err != nil {
		f.t.Fatalf("FindSymbolExact(%s) error = %v", name, err)
	}
	return syms
}

// dstSymbolIDForEdge returns the binding of the (file, dst_name) edge, and
// whether it is bound at all.
func (f *lifecycleFixture) dstSymbolIDForEdge(rel, dstName string) (int64, bool) {
	f.t.Helper()
	var id sql.NullInt64
	err := f.raw().QueryRow(`
		SELECT e.dst_symbol_id FROM edges e JOIN files f ON f.id = e.file_id
		WHERE f.repo_id = ? AND f.path = ? AND e.dst_name = ?
	`, f.repoID, rel, dstName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		f.t.Fatalf("no edge %s -> %s", rel, dstName)
	}
	if err != nil {
		f.t.Fatalf("dstSymbolIDForEdge error = %v", err)
	}
	return id.Int64, id.Valid
}

const oversizeFiller = "# " + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n"

// bigContent returns directive content padded past `minBytes`.
func bigContent(directives string, minBytes int) string {
	var b strings.Builder
	b.WriteString(directives)
	for b.Len() < minBytes {
		b.WriteString(oversizeFiller)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 3. Oversize retirement
// ---------------------------------------------------------------------------

func TestOversizeFileRetiresStaleGraph(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(4096, "fail_fast")
	f.write("a.lc", "sym OldSymbol\ncall Helper\nref Helper\nimport ./dep\n")
	f.write("dep.lc", "sym Helper\n")

	f.index(Options{})
	if got := f.parserOwnedRows("a.lc")["symbols"]; got != 1 {
		t.Fatalf("initial symbols for a.lc = %d, want 1", got)
	}
	if len(f.findSymbolExact("OldSymbol")) != 1 {
		t.Fatalf("OldSymbol not indexed by the first run")
	}

	f.rewrite("a.lc", bigContent("sym NewSymbol\n", 8192))
	summary := f.update(Options{})

	// a.lc (retired) and dep.lc (unchanged) are both "not indexed by this run".
	if summary.FilesSkipped != 2 {
		t.Errorf("FilesSkipped = %d, want 2", summary.FilesSkipped)
	}
	row := f.fileRow("a.lc")
	if row.SizeBytes != f.stat("a.lc").Size() {
		t.Errorf("size_bytes = %d, want %d (metadata must describe current bytes)", row.SizeBytes, f.stat("a.lc").Size())
	}
	if row.IsDeleted != 0 {
		t.Errorf("is_deleted = %d, want 0 (an oversize file still exists)", row.IsDeleted)
	}
	if row.ParseState != store.ParseStateOversize {
		t.Errorf("parse_state = %q, want %q", row.ParseState, store.ParseStateOversize)
	}
	if row.ParserProfile != "" {
		t.Errorf("parser_profile = %q, want empty (nothing parsed those bytes)", row.ParserProfile)
	}
	if got := f.findSymbolExact("OldSymbol"); len(got) != 0 {
		t.Errorf("OldSymbol still queryable after successful oversize scan: %+v", got)
	}
	f.assertNoParserOwnedRows("a.lc")
	// The unrelated file keeps its graph.
	if got := f.parserOwnedRows("dep.lc")["symbols"]; got != 1 {
		t.Errorf("dep.lc symbols = %d, want 1 (unrelated files untouched)", got)
	}
}

// ---------------------------------------------------------------------------
// 4. best_effort parse failure retirement
// ---------------------------------------------------------------------------

func TestBestEffortParseFailureRetiresStaleGraph(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(1<<20, "best_effort")
	f.write("a.lc", "sym OldSymbol\ncall Helper\nref Helper\nimport ./dep\n")
	f.write("dep.lc", "sym Helper\n")

	f.index(Options{})
	if len(f.findSymbolExact("OldSymbol")) != 1 {
		t.Fatalf("OldSymbol not indexed by the first run")
	}

	f.adapter.setFailing("a.lc", true)
	f.rewrite("a.lc", "sym Whatever\n")
	summary := f.update(Options{})

	if summary.ParseErrors != 1 {
		t.Errorf("ParseErrors = %d, want 1", summary.ParseErrors)
	}
	row := f.fileRow("a.lc")
	if row.ParseState != store.ParseStateFailed {
		t.Errorf("parse_state = %q, want %q", row.ParseState, store.ParseStateFailed)
	}
	if row.ContentSHA == "" {
		t.Errorf("content_sha256 empty, want the failed content's hash")
	}
	if row.ParserProfile != "" {
		t.Errorf("parser_profile = %q, want empty (the parse did not converge)", row.ParserProfile)
	}
	if got := f.findSymbolExact("OldSymbol"); len(got) != 0 {
		t.Errorf("OldSymbol still queryable after a best-effort failure: %+v", got)
	}
	f.assertNoParserOwnedRows("a.lc")
	if got := f.parserOwnedRows("dep.lc")["symbols"]; got != 1 {
		t.Errorf("dep.lc symbols = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// 5/10. Re-entry: cap raised over an unchanged file
// ---------------------------------------------------------------------------

func TestOversizeCapRaiseReparsesUnchangedFile(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(4096, "fail_fast")
	f.write("a.lc", bigContent("sym Big\n", 8192))

	f.index(Options{})
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateOversize {
		t.Fatalf("parse_state after fresh oversize index = %q, want %q", got, store.ParseStateOversize)
	}
	f.assertNoParserOwnedRows("a.lc")

	before := f.stat("a.lc")
	f.repoConfig(1<<20, "fail_fast")
	f.update(Options{})

	after := f.stat("a.lc")
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("fixture changed the file; the retry must not depend on that")
	}
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateIndexed {
		t.Errorf("parse_state after cap raise = %q, want %q", got, store.ParseStateIndexed)
	}
	if len(f.findSymbolExact("Big")) != 1 {
		t.Errorf("cap raise did not reparse an unchanged file")
	}
}

// ---------------------------------------------------------------------------
// 12. best_effort success -> fail -> success over identical bytes
// ---------------------------------------------------------------------------

func TestBestEffortRecoveryWithoutForce(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(1<<20, "best_effort")
	f.write("a.lc", "sym Recovered\n")

	f.index(Options{})
	if len(f.findSymbolExact("Recovered")) != 1 {
		t.Fatalf("first index did not persist Recovered")
	}

	f.adapter.setFailing("a.lc", true)
	f.rewrite("a.lc", "sym Recovered\nref Recovered\n")
	if summary := f.update(Options{}); summary.ParseErrors != 1 {
		t.Fatalf("ParseErrors = %d, want 1", summary.ParseErrors)
	}
	f.assertNoParserOwnedRows("a.lc")

	before := f.stat("a.lc")
	f.adapter.setFailing("a.lc", false)
	f.update(Options{})
	after := f.stat("a.lc")
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("fixture changed the file; recovery must not depend on that")
	}

	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateIndexed {
		t.Errorf("parse_state after recovery = %q, want %q", got, store.ParseStateIndexed)
	}
	if len(f.findSymbolExact("Recovered")) != 1 {
		t.Errorf("best-effort recovery required --force or a file touch")
	}
	if got := f.fileRow("a.lc").ParserProfile; got != "lifecycle:v1" {
		t.Errorf("parser_profile after recovery = %q, want lifecycle:v1", got)
	}
}

// ---------------------------------------------------------------------------
// 8. Inbound bindings
// ---------------------------------------------------------------------------

func TestRetirementUnbindsInboundEdges(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(4096, "fail_fast")
	f.write("a.lc", "sym Foo\n")
	f.write("b.lc", "sym Caller\ncall Foo\n")
	f.write("c.lc", "sym Untouched\n")
	f.write("d.lc", "sym Other\ncall Untouched\n")

	f.index(Options{})
	if _, bound := f.dstSymbolIDForEdge("b.lc", "Foo"); !bound {
		t.Fatalf("b.lc -> Foo was never bound; fixture cannot prove unbinding")
	}
	untouchedID, bound := f.dstSymbolIDForEdge("d.lc", "Untouched")
	if !bound {
		t.Fatalf("d.lc -> Untouched was never bound")
	}

	f.rewrite("a.lc", bigContent("sym Foo\n", 8192))
	f.update(Options{})

	if id, bound := f.dstSymbolIDForEdge("b.lc", "Foo"); bound {
		t.Errorf("b.lc -> Foo still bound to retired symbol %d", id)
	}
	if id, bound := f.dstSymbolIDForEdge("d.lc", "Untouched"); !bound || id != untouchedID {
		t.Errorf("unrelated binding changed: got (%d, %v), want (%d, true)", id, bound, untouchedID)
	}
}

// TestRetirementResolvesSurvivingAmbiguousCandidate: two files declare Target,
// so an edge naming it is ambiguous and unbound. Retiring one declaration
// leaves a unique candidate, and the surviving one must be picked up by the
// same removed-name invalidation an ordinary deletion uses.
func TestRetirementResolvesSurvivingAmbiguousCandidate(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(4096, "fail_fast")
	f.write("one.lc", "sym Target\n")
	f.write("two.lc", "sym Target\n")
	f.write("caller.lc", "sym Caller\ncall Target\n")

	f.index(Options{})
	if _, bound := f.dstSymbolIDForEdge("caller.lc", "Target"); bound {
		t.Skipf("resolver bound an ambiguous name; fixture assumption no longer holds")
	}

	f.rewrite("one.lc", bigContent("sym Target\n", 8192))
	f.update(Options{})

	if _, bound := f.dstSymbolIDForEdge("caller.lc", "Target"); !bound {
		t.Errorf("surviving unique candidate was not bound after retirement")
	}
}

// ---------------------------------------------------------------------------
// 10. Max-file-size transition matrix
// ---------------------------------------------------------------------------

func TestMaxFileSizeTransitionMatrix(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(1<<20, "fail_fast")
	f.write("a.lc", "sym Small\n")

	// indexed -> still indexed
	f.index(Options{})
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateIndexed {
		t.Fatalf("fresh indexed parse_state = %q", got)
	}
	f.update(Options{})
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateIndexed {
		t.Errorf("indexed -> still indexed parse_state = %q, want indexed", got)
	}

	// indexed -> oversize (cap lowered, bytes unchanged)
	f.repoConfig(4, "fail_fast")
	f.update(Options{})
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateOversize {
		t.Errorf("indexed -> oversize parse_state = %q, want oversize", got)
	}
	f.assertNoParserOwnedRows("a.lc")

	// oversize -> still oversize: cheap no-op, no parser read.
	parsesBefore := f.adapter.parseCount("a.lc")
	readsBefore := readFileCallCount()
	f.update(Options{})
	if got := f.adapter.parseCount("a.lc"); got != parsesBefore {
		t.Errorf("still-oversize no-op parsed the file (%d -> %d)", parsesBefore, got)
	}
	if got := readFileCallCount(); got != readsBefore {
		t.Errorf("still-oversize no-op read the file (%d -> %d reads)", readsBefore, got)
	}
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateOversize {
		t.Errorf("oversize -> still oversize parse_state = %q", got)
	}

	// oversize -> indexed after cap increase, bytes unchanged.
	f.repoConfig(1<<20, "fail_fast")
	f.update(Options{})
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateIndexed {
		t.Errorf("oversize -> indexed parse_state = %q, want indexed", got)
	}
	if len(f.findSymbolExact("Small")) != 1 {
		t.Errorf("Small not restored after cap increase")
	}

	// fresh oversize
	f.write("fresh.lc", bigContent("sym Fresh\n", 8192))
	f.repoConfig(4096, "fail_fast")
	f.update(Options{})
	if got := f.fileRow("fresh.lc").ParseState; got != store.ParseStateOversize {
		t.Errorf("fresh oversize parse_state = %q, want oversize", got)
	}
	f.assertNoParserOwnedRows("fresh.lc")
	if got := f.fileRow("fresh.lc").IsDeleted; got != 0 {
		t.Errorf("fresh oversize is_deleted = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 15. Path-scoped (watcher) retirement
// ---------------------------------------------------------------------------

func TestPathScopedUpdateRetiresStaleGraph(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(4096, "best_effort")
	f.write("a.lc", "sym OldSymbol\nimport ./dep\n")
	f.write("b.lc", "sym Caller\ncall OldSymbol\n")
	f.index(Options{})
	if _, bound := f.dstSymbolIDForEdge("b.lc", "OldSymbol"); !bound {
		t.Fatalf("b.lc -> OldSymbol was never bound")
	}

	f.rewrite("a.lc", bigContent("sym NewSymbol\n", 8192))
	f.update(Options{Paths: []string{"a.lc"}, ScanKind: "watch"})

	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateOversize {
		t.Errorf("path-scoped parse_state = %q, want oversize", got)
	}
	f.assertNoParserOwnedRows("a.lc")
	if got := f.findSymbolExact("OldSymbol"); len(got) != 0 {
		t.Errorf("path-scoped update left OldSymbol queryable: %+v", got)
	}
	if id, bound := f.dstSymbolIDForEdge("b.lc", "OldSymbol"); bound {
		t.Errorf("path-scoped update left b.lc -> OldSymbol bound to %d", id)
	}
}

// ---------------------------------------------------------------------------
// 16. Deletion after retirement
// ---------------------------------------------------------------------------

func TestDeleteAfterRetirement(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(4096, "fail_fast")
	f.write("a.lc", "sym Gone\n")
	f.write("keep.lc", "sym Keep\n")
	f.index(Options{})

	f.rewrite("a.lc", bigContent("sym Gone\n", 8192))
	summary := f.update(Options{})
	if summary.FilesDeleted != 0 {
		t.Errorf("retirement counted as deletion: FilesDeleted = %d, want 0", summary.FilesDeleted)
	}

	if err := os.Remove(filepath.Join(f.root, "a.lc")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	summary = f.update(Options{})
	if summary.FilesDeleted != 1 {
		t.Errorf("FilesDeleted = %d, want 1", summary.FilesDeleted)
	}
	if got := f.count(`SELECT COUNT(*) FROM files WHERE repo_id = ? AND path = ? AND is_deleted = 0`, f.repoID, "a.lc"); got != 0 {
		t.Errorf("live rows for deleted a.lc = %d, want 0", got)
	}
	if len(f.findSymbolExact("Keep")) != 1 {
		t.Errorf("unrelated file lost its graph")
	}
}

// ---------------------------------------------------------------------------
// 8/12. Full vs incremental parity across the whole lifecycle
// ---------------------------------------------------------------------------

func TestRetirementFullIncrementalParity(t *testing.T) {
	build := func(f *lifecycleFixture) {
		f.repoConfig(4096, "best_effort")
		f.write("a.lc", "sym Foo\ncall Helper\nref Helper\nimport ./dep\n")
		f.write("b.lc", "sym Caller\ncall Foo\n")
		f.write("dep.lc", "sym Helper\n")
	}

	incremental := newLifecycleFixture(t)
	build(incremental)
	incremental.index(Options{})
	incremental.rewrite("a.lc", bigContent("sym Foo\n", 8192))
	incremental.update(Options{})

	full := newLifecycleFixture(t)
	build(full)
	full.write("a.lc", bigContent("sym Foo\n", 8192))
	full.index(Options{})

	if got, want := incremental.symbolNames(), full.symbolNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("symbol parity: incremental = %v, full = %v", got, want)
	}
	incID, incBound := incremental.dstSymbolIDForEdge("b.lc", "Foo")
	_, fullBound := full.dstSymbolIDForEdge("b.lc", "Foo")
	if incBound != fullBound {
		t.Errorf("binding parity: incremental bound = %v (%d), full bound = %v", incBound, incID, fullBound)
	}
}

// ---------------------------------------------------------------------------
// 21. parse_state contract
// ---------------------------------------------------------------------------

func TestParseStateLifecycleContract(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(1<<20, "best_effort")
	f.write("a.lc", "sym A\n")
	f.write("b.lc", "sym B\n")

	f.index(Options{})
	for _, rel := range []string{"a.lc", "b.lc"} {
		if got := f.fileRow(rel).ParseState; got != store.ParseStateIndexed {
			t.Fatalf("%s parse_state after index = %q, want indexed", rel, got)
		}
	}

	// mark_seen: unchanged size+mtime keeps the state it had.
	f.update(Options{})
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateIndexed {
		t.Errorf("mark_seen changed parse_state to %q, want indexed", got)
	}

	// touch: content hash unchanged, mtime moved -- the graph is still current.
	time.Sleep(2 * time.Millisecond)
	now := time.Now()
	if err := os.Chtimes(filepath.Join(f.root, "a.lc"), now, now); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	f.update(Options{})
	// 'skipped' is the pre-existing spelling of "this scan did not reparse the
	// file because its content hash was unchanged". It is a state in which the
	// persisted graph DOES describe the bytes on disk, which is what
	// ParseStateDescribesCurrentBytes turns on.
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateSkipped {
		t.Errorf("hash-equal touch parse_state = %q, want skipped (graph still describes these bytes)", got)
	}
	if got := f.parserOwnedRows("a.lc")["symbols"]; got != 1 {
		t.Errorf("hash-equal touch dropped the graph: symbols = %d, want 1", got)
	}

	// best-effort failure
	f.adapter.setFailing("b.lc", true)
	f.rewrite("b.lc", "sym B2\n")
	f.update(Options{})
	if got := f.fileRow("b.lc").ParseState; got != store.ParseStateFailed {
		t.Errorf("parse failure parse_state = %q, want failed", got)
	}

	// recovery
	f.adapter.setFailing("b.lc", false)
	f.update(Options{})
	if got := f.fileRow("b.lc").ParseState; got != store.ParseStateIndexed {
		t.Errorf("recovery parse_state = %q, want indexed", got)
	}

	// deletion
	if err := os.Remove(filepath.Join(f.root, "b.lc")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	f.update(Options{})
	if got := f.fileRow("b.lc").ParseState; got != store.ParseStateDeleted {
		t.Errorf("deletion parse_state = %q, want deleted", got)
	}
}

// ---------------------------------------------------------------------------
// 11. Language allowlist: pinned, deliberately NOT a retirement
// ---------------------------------------------------------------------------

func TestLanguageAllowlistDoesNotRetireGraph(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(1<<20, "fail_fast")
	f.write("a.lc", "sym Kept\n")
	f.index(Options{})

	f.update(Options{Languages: []string{"go"}})

	if got := f.parserOwnedRows("a.lc")["symbols"]; got != 1 {
		t.Errorf("a language-scoped run retired an unrelated language: symbols = %d, want 1", got)
	}
	if got := f.fileRow("a.lc").ParseState; got != store.ParseStateIndexed {
		t.Errorf("parse_state after an excluded-language run = %q, want indexed", got)
	}
}

// ---------------------------------------------------------------------------
// 6. fail_fast is unchanged: the scan fails and claims nothing
// ---------------------------------------------------------------------------

func TestFailFastParseErrorFailsScanAndKeepsGraph(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(1<<20, "fail_fast")
	f.write("a.lc", "sym Kept\n")
	f.index(Options{})

	f.adapter.setFailing("a.lc", true)
	f.rewrite("a.lc", "sym Kept\nref Kept\n")
	if _, err := f.idx.Update(context.Background(), Options{RepoRoot: f.root}); err == nil {
		t.Fatalf("Update() error = nil, want the fail_fast parse error")
	}
	if got := f.parserOwnedRows("a.lc")["symbols"]; got != 1 {
		t.Errorf("a failed scan mutated the graph: symbols = %d, want 1", got)
	}
}

// readFile is indirected in indexer.go; the test binary wraps it so a "cheap
// no-op" claim can be proven rather than asserted.
var readFileCalls atomic.Int64

func init() {
	inner := readFile
	readFile = func(name string) ([]byte, error) {
		readFileCalls.Add(1)
		return inner(name)
	}
}

func readFileCallCount() int { return int(readFileCalls.Load()) }

// ---------------------------------------------------------------------------
// 18. Embeddings follow the symbols they describe
// ---------------------------------------------------------------------------

// fixedEmbedder is deterministic and, unlike the noop embedder, actually
// persists rows -- which is the only way to prove an embedding ghost is gone.
type fixedEmbedder struct{}

func (fixedEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}

func (fixedEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{0.1, 0.2}
	}
	return out, nil
}

func (fixedEmbedder) Dimensions() int { return 2 }

func TestRetirementRemovesEmbeddingsAndRecoveryRestoresThem(t *testing.T) {
	f := newLifecycleFixture(t)
	f.idx = New(f.store, parser.NewRegistry(f.adapter), fixedEmbedder{})
	f.repoConfig(1<<20, "best_effort")
	f.write("a.lc", "sym Embedded\n")

	f.index(Options{})
	if got := f.parserOwnedRows("a.lc")["symbol_embeddings"]; got != 1 {
		t.Fatalf("symbol_embeddings after index = %d, want 1", got)
	}

	f.adapter.setFailing("a.lc", true)
	f.rewrite("a.lc", "sym Embedded\nref Embedded\n")
	f.update(Options{})
	if got := f.parserOwnedRows("a.lc")["symbol_embeddings"]; got != 0 {
		t.Errorf("embedding ghost survived retirement: %d rows, want 0", got)
	}

	f.adapter.setFailing("a.lc", false)
	f.update(Options{})
	if got := f.parserOwnedRows("a.lc")["symbol_embeddings"]; got != 1 {
		t.Errorf("symbol_embeddings after recovery = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// 19. Test links owned by a retired test file
// ---------------------------------------------------------------------------

func TestRetirementRemovesTestLinks(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(4096, "fail_fast")
	f.write("target.lc", "sym Widget\n")
	f.write("tests/widget.lc", "sym TestWidget\ntestlink Widget\n")
	f.index(Options{})
	if got := f.parserOwnedRows("tests/widget.lc")["test_links"]; got == 0 {
		t.Skipf("no test link derived for the fixture; nothing to prove")
	}

	f.rewrite("tests/widget.lc", bigContent("sym TestWidget\ntestlink Widget\n", 8192))
	f.update(Options{})
	if got := f.parserOwnedRows("tests/widget.lc")["test_links"]; got != 0 {
		t.Errorf("test_links owned by a retired file = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Retirement dispatch: exactly once, and only when something left the graph
// ---------------------------------------------------------------------------

func (f *lifecycleFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.raw().Exec(query, args...); err != nil {
		f.t.Fatalf("exec error = %v (%s)", err, query)
	}
}

// A repair migration marks a file `pending` WITHOUT touching the rows it
// declared (033/034/035 clear only content_sha256). Such a file still owns a
// full graph, so retiring it is a real transition and has to dispatch the same
// invalidation an indexed file would -- inferring "owns nothing" from the state
// name alone would silently skip it.
func TestRetirementOfPendingFileDispatchesInvalidation(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(4096, "fail_fast")
	f.write("a.lc", "sym Foo\n")
	f.write("b.lc", "sym Caller\ncall Foo\n")
	f.index(Options{})
	if _, bound := f.dstSymbolIDForEdge("b.lc", "Foo"); !bound {
		t.Fatalf("b.lc -> Foo was never bound")
	}

	f.exec(`UPDATE files SET parse_state = ? WHERE repo_id = ? AND path = ?`, store.ParseStatePending, f.repoID, "a.lc")
	f.rewrite("a.lc", bigContent("sym Foo\n", 8192))
	summary := f.update(Options{})

	if summary.ResolveMode == "none" {
		t.Errorf("ResolveMode = none; a pending file's retirement was not dispatched")
	}
	f.assertNoParserOwnedRows("a.lc")
	if id, bound := f.dstSymbolIDForEdge("b.lc", "Foo"); bound {
		t.Errorf("b.lc -> Foo still bound to retired symbol %d", id)
	}
}

// A file that fails to parse on every update is re-read and re-parsed every
// time -- that is the retry contract -- but after the first retirement it owns
// nothing, so no later update may dispatch a repo-wide resolve or test-link
// pass on its behalf.
func TestRepeatedParseFailureDoesNotChurnPassTwo(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(1<<20, "best_effort")
	f.write("a.lc", "sym Foo\n")
	f.write("b.lc", "sym Other\n")
	f.index(Options{})

	f.adapter.setFailing("a.lc", true)
	f.rewrite("a.lc", "sym Foo\nref Foo\n")
	if got := f.update(Options{}).ResolveMode; got == "none" {
		t.Fatalf("first failure ResolveMode = none, want the retirement dispatched")
	}

	summary := f.update(Options{})
	if summary.ParseErrors != 1 {
		t.Errorf("ParseErrors = %d, want 1 (the file is still retried)", summary.ParseErrors)
	}
	if summary.ResolveMode != "none" {
		t.Errorf("ResolveMode = %q, want none on a repeat failure that removed nothing", summary.ResolveMode)
	}
	if summary.ResolveTestLinksMS != 0 {
		t.Errorf("ResolveTestLinksMS = %d, want 0", summary.ResolveTestLinksMS)
	}
}

// Same rule for an oversize file whose mtime keeps moving (a log, a generated
// bundle): the metadata is refreshed, but nothing left the graph, so Pass 2
// must stay idle.
func TestRepeatedOversizeWriteDoesNotChurnPassTwo(t *testing.T) {
	f := newLifecycleFixture(t)
	f.repoConfig(4096, "fail_fast")
	f.write("a.lc", "sym Foo\n")
	f.index(Options{})

	f.rewrite("a.lc", bigContent("sym Foo\n", 8192))
	if got := f.update(Options{}).ResolveMode; got == "none" {
		t.Fatalf("first oversize ResolveMode = none, want the retirement dispatched")
	}

	f.rewrite("a.lc", bigContent("sym Foo\n", 9216))
	summary := f.update(Options{})
	if summary.ResolveMode != "none" {
		t.Errorf("ResolveMode = %q, want none when a still-oversize file only grew", summary.ResolveMode)
	}
	if got := f.fileRow("a.lc").SizeBytes; got != f.stat("a.lc").Size() {
		t.Errorf("size_bytes = %d, want %d (metadata still has to be truthful)", got, f.stat("a.lc").Size())
	}
}
