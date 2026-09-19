package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// pathFormatFixture indexes one tiny repository for real and hands back the
// raw database so a test can degrade it into a pre-P23 shape.
type pathFormatFixture struct {
	svc    *Service
	repoID int64
	raw    *sql.DB
}

func newPathFormatFixture(t *testing.T) *pathFormatFixture {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFixtureFile(t, repoRoot, "src/pkg/file.go", "package pkg\n\nfunc Thing() {}\n\nfunc Caller() { Thing() }\n")
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := indexer.New(s, parser.NewRegistry(goparser.New()), nil).Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &pathFormatFixture{svc: New(s, nil), repoID: repo.ID, raw: raw}
}

func TestServiceFreshEmptyUnmarkedRepositoryReads(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(s, nil)
	if got, err := svc.Stats(ctx, repo.ID); err != nil || got.Files != 0 || got.Symbols != 0 || got.Edges != 0 {
		t.Fatalf("Stats on fresh empty repo = %+v, %v", got, err)
	}
	if got, err := svc.FindSymbolExactResult(ctx, repo.ID, "Thing", 10, 0); err != nil || len(got.Matches) != 0 {
		t.Fatalf("FindSymbolExactResult on fresh empty repo = %+v, %v", got, err)
	}
	if got, err := svc.ListFiles(ctx, repo.ID, "", 10, 0); err != nil || len(got) != 0 {
		t.Fatalf("ListFiles on fresh empty repo = %v, %v", got, err)
	}
}

// degradeToLegacy removes the format marker and rewrites files.path to the
// native spelling a pre-P23 Windows index could have stored.
func (f *pathFormatFixture) degradeToLegacy(t *testing.T) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM settings WHERE value = 'logical-slash-v1'`,
		`UPDATE files SET path = replace(path, '/', '\')`,
	} {
		if _, err := f.raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := f.raw.Exec(`INSERT INTO dirty_files(repo_id, path, reason, queued_at) VALUES(?, ?, 'legacy', '2026-01-01T00:00:00Z')`, f.repoID, `src\pkg\file.go`); err != nil {
		t.Fatal(err)
	}
}

func (f *pathFormatFixture) snapshot(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, hex(path), is_deleted FROM files ORDER BY id`,
		`SELECT hex(path), reason, queued_at FROM dirty_files ORDER BY path`,
		`SELECT key, value FROM settings ORDER BY key`,
		`SELECT (SELECT COUNT(*) FROM files), (SELECT COUNT(*) FROM dirty_files), (SELECT COUNT(*) FROM scans), (SELECT COUNT(*) FROM symbols), (SELECT COUNT(*) FROM edges)`,
	} {
		rows, err := f.raw.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if bs, ok := v.([]byte); ok {
					b.Write(bs)
				} else {
					fmt.Fprint(&b, v)
				}
				b.WriteByte('|')
			}
			b.WriteByte('\n')
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

type serviceCall struct {
	method string // exported Service method the call exercises
	label  string
	call   func(ctx context.Context, svc *Service, repoID int64) (any, error)
}

// serviceRepositoryCalls lists one invocation per exported Service method,
// plus every branch whose gate could differ (RelatedTests by symbol vs file,
// RelatedTestsForFiles with no files). TestServiceMatrixCoversEveryExportedMethod
// keeps it complete.
func serviceRepositoryCalls() []serviceCall {
	const sym = "Thing"
	const file = "src/pkg/file.go"
	return []serviceCall{
		{"Stats", "Stats", func(ctx context.Context, s *Service, r int64) (any, error) { return s.Stats(ctx, r) }},
		{"ArchitectureOverview", "ArchitectureOverview", func(ctx context.Context, s *Service, r int64) (any, error) { return s.ArchitectureOverview(ctx, r) }},
		{"FindSymbol", "FindSymbol", func(ctx context.Context, s *Service, r int64) (any, error) { return s.FindSymbol(ctx, r, sym, 10, 0) }},
		{"FindSymbolExact", "FindSymbolExact", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.FindSymbolExact(ctx, r, sym, 10, 0)
		}},
		{"FindSymbolExactResult", "FindSymbolExactResult", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.FindSymbolExactResult(ctx, r, sym, 10, 0)
		}},
		{"SearchSymbols", "SearchSymbols", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.SearchSymbols(ctx, r, sym, 10, 0)
		}},
		{"SearchSymbolsResult", "SearchSymbolsResult", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.SearchSymbolsResult(ctx, r, sym, 10, 0)
		}},
		{"FindCallers", "FindCallers", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.FindCallers(ctx, r, sym, 0, 10, 0)
		}},
		{"FindCallersResult", "FindCallersResult", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.FindCallersResult(ctx, r, sym, 0, 10, 0)
		}},
		{"FindCallees", "FindCallees", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.FindCallees(ctx, r, "Caller", 0, 10, 0)
		}},
		{"FindCalleesResult", "FindCalleesResult", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.FindCalleesResult(ctx, r, "Caller", 0, 10, 0)
		}},
		{"ImpactRadius", "ImpactRadius", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.ImpactRadius(ctx, r, []string{sym}, nil, 1, 10, 0)
		}},
		{"RelatedTests", "RelatedTests(symbol)", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.RelatedTests(ctx, r, sym, "", 10, 0)
		}},
		{"RelatedTests", "RelatedTests(file)", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.RelatedTests(ctx, r, "", file, 10, 0)
		}},
		{"RelatedTestsResult", "RelatedTestsResult(symbol)", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.RelatedTestsResult(ctx, r, sym, "", 10, 0)
		}},
		{"RelatedTestsResult", "RelatedTestsResult(file)", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.RelatedTestsResult(ctx, r, "", file, 10, 0)
		}},
		{"RelatedTestsForFilesResult", "RelatedTestsForFilesResult", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.RelatedTestsForFilesResult(ctx, r, []string{file}, 10, 0)
		}},
		{"RelatedTestsForFiles", "RelatedTestsForFiles", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.RelatedTestsForFiles(ctx, r, []string{file}, 10, 0)
		}},
		{"RelatedTestsForFiles", "RelatedTestsForFiles(no files)", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.RelatedTestsForFiles(ctx, r, nil, 10, 0)
		}},
		{"SemanticSearch", "SemanticSearch", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.SemanticSearch(ctx, r, "thing", 10, 0)
		}},
		{"FindDeadCode", "FindDeadCode", func(ctx context.Context, s *Service, r int64) (any, error) { return s.FindDeadCode(ctx, r, 10, 0) }},
		{"ListFiles", "ListFiles", func(ctx context.Context, s *Service, r int64) (any, error) { return s.ListFiles(ctx, r, "", 10, 0) }},
		{"GraphSnapshot", "GraphSnapshot", func(ctx context.Context, s *Service, r int64) (any, error) {
			symbols, edges, err := s.GraphSnapshot(ctx, r, "", 1)
			if symbols == nil && edges == nil {
				return nil, err
			}
			return []any{symbols, edges}, err
		}},
		{"ExportSymbolsPage", "ExportSymbolsPage", func(ctx context.Context, s *Service, r int64) (any, error) { return s.ExportSymbolsPage(ctx, r, 10, 0) }},
		{"ExportEdgesPage", "ExportEdgesPage", func(ctx context.Context, s *Service, r int64) (any, error) { return s.ExportEdgesPage(ctx, r, 10, 0) }},
		{"ExportDOTNodeNamesPage", "ExportDOTNodeNamesPage", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.ExportDOTNodeNamesPage(ctx, r, 10, 0)
		}},
		{"TraceDependencies", "TraceDependencies", func(ctx context.Context, s *Service, r int64) (any, error) {
			items, total, err := s.TraceDependencies(ctx, r, sym, "upstream", 2, 10, 0)
			if items == nil && total == 0 {
				return nil, err
			}
			return []any{items, total}, err
		}},
		{"TraceDependenciesResult", "TraceDependenciesResult", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.TraceDependenciesResult(ctx, r, sym, "upstream", 2, 10, 0)
		}},
		{"BenchmarkTokens", "BenchmarkTokens", func(ctx context.Context, s *Service, r int64) (any, error) { return s.BenchmarkTokens(ctx, r, "thing") }},
		{"ResolveCrossLanguageLinks", "ResolveCrossLanguageLinks", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.ResolveCrossLanguageLinks(ctx, r)
		}},
		{"PageRank", "PageRank", func(ctx context.Context, s *Service, r int64) (any, error) { return s.PageRank(ctx, r, 10) }},
		{"CouplingMetrics", "CouplingMetrics", func(ctx context.Context, s *Service, r int64) (any, error) { return s.CouplingMetrics(ctx, r, 10) }},
		{"DetectCycles", "DetectCycles", func(ctx context.Context, s *Service, r int64) (any, error) { return s.DetectCycles(ctx, r, 10) }},
		{"AllImports", "AllImports", func(ctx context.Context, s *Service, r int64) (any, error) { return s.AllImports(ctx, r) }},
		{"AllFilePaths", "AllFilePaths", func(ctx context.Context, s *Service, r int64) (any, error) { return s.AllFilePaths(ctx, r) }},
		{"ContextForTask", "ContextForTask", func(ctx context.Context, s *Service, r int64) (any, error) {
			return s.ContextForTask(ctx, r, "thing", ContextForTaskOptions{})
		}},
	}
}

// TestServiceMatrixCoversEveryExportedMethod fails when a Service method is
// added without a row in serviceRepositoryCalls, so the legacy gate cannot
// silently split behaviour again.
func TestServiceMatrixCoversEveryExportedMethod(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range serviceRepositoryCalls() {
		covered[c.method] = true
	}
	typ := reflect.TypeOf(&Service{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !covered[name] {
			t.Errorf("exported Service method %s has no row in the legacy path-format matrix", name)
		}
	}
	for method := range covered {
		if _, ok := typ.MethodByName(method); !ok {
			t.Errorf("matrix names %s, which is not an exported Service method", method)
		}
	}
}

// TestServiceRepositoryReadsFailClosedOnLegacyPathFormat is the F4A4 gate
// proof: every exported Service method works on a current-format index and,
// once the same index is degraded to a pre-P23 shape, every one of them fails
// with ErrRepositoryPathFormatRebuild, returns nothing, and leaves the
// database byte-identical.
func TestServiceRepositoryReadsFailClosedOnLegacyPathFormat(t *testing.T) {
	ctx := context.Background()
	f := newPathFormatFixture(t)
	calls := serviceRepositoryCalls()

	// Positive control: current format, every method succeeds.
	for _, c := range calls {
		if _, err := c.call(ctx, f.svc, f.repoID); err != nil {
			t.Fatalf("%s on a current-format index: %v", c.label, err)
		}
	}
	if got, err := f.svc.FindSymbolExactResult(ctx, f.repoID, "Thing", 10, 0); err != nil || len(got.Matches) != 1 || got.Matches[0].FilePath != "src/pkg/file.go" {
		t.Fatalf("current-format FindSymbolExactResult = %+v, %v", got, err)
	}

	f.degradeToLegacy(t)
	before := f.snapshot(t)
	if !strings.Contains(before, "7372635C") { // hex(`src\`)
		t.Fatalf("fixture did not produce a native-spelled files.path:\n%s", before)
	}
	for _, c := range calls {
		got, err := c.call(ctx, f.svc, f.repoID)
		if !errors.Is(err, store.ErrRepositoryPathFormatRebuild) {
			t.Errorf("%s on legacy index: err = %v, want ErrRepositoryPathFormatRebuild", c.label, err)
			continue
		}
		if !isZeroResult(got) {
			t.Errorf("%s returned a result alongside the rebuild error: %#v", c.label, got)
		}
		if after := f.snapshot(t); after != before {
			t.Fatalf("%s mutated the legacy database:\nbefore:\n%s\nafter:\n%s", c.label, before, after)
		}
	}
}

// isZeroResult reports whether a rejected read handed back nothing: a nil
// map, slice or pointer, or a zero struct. Anything else is a partial result
// leaking past the gate.
func isZeroResult(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Pointer:
		return rv.IsNil()
	}
	return rv.IsZero()
}
