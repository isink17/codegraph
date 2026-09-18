package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestFirstUniqueCanonicalPathsPreservesRank(t *testing.T) {
	results := make([]map[string]any, 0, 13)
	results = append(results, map[string]any{"file": "rank/00.go"}, map[string]any{"file": "rank/00.go"})
	for i := 1; i < 12; i++ {
		results = append(results, map[string]any{"file": fmt.Sprintf("rank/%02d.go", i)})
	}
	want := []string{"rank/00.go", "rank/01.go", "rank/02.go", "rank/03.go", "rank/04.go", "rank/05.go", "rank/06.go", "rank/07.go", "rank/08.go", "rank/09.go"}
	if got := firstUniqueCanonicalPaths(results, 10); !reflect.DeepEqual(got, want) {
		t.Fatalf("firstUniqueCanonicalPaths() = %v, want %v", got, want)
	}
}

func TestSessionOrderingIsPublicAndPaginated(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	for _, event := range []struct {
		id       int64
		typ, key string
	}{{3, "decision", "new"}, {1, "decision", "old"}, {2, "decision", "middle"}, {4, "fact", "fact"}, {5, "task", "task"}} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO session_events(id, repo_id, session_id, event_type, key, value, metadata, created_at) VALUES (?, ?, 's', ?, ?, '', '{}', '2026-01-01T00:00:00Z')`, event.id, repoID, event.typ, event.key); err != nil {
			t.Fatal(err)
		}
	}
	for offset, want := range []int64{3, 2, 1} {
		got, err := s.SessionGetHistory(ctx, repoID, "s", "decision", 1, offset)
		if err != nil || len(got) != 1 || got[0]["id"] != want {
			t.Fatalf("history page %d = %v, %v; want id %d", offset, got, err, want)
		}
	}
	contextOut, err := s.SessionGetContext(ctx, repoID, "s")
	if err != nil || contextOut["decisions"].([]map[string]any)[0]["id"] != int64(3) || contextOut["facts"].([]map[string]any)[0]["id"] != int64(4) || contextOut["tasks"].([]map[string]any)[0]["id"] != int64(5) {
		t.Fatalf("SessionGetContext() = %v, %v", contextOut, err)
	}
	first, _ := json.Marshal(contextOut)
	again, _ := s.SessionGetContext(ctx, repoID, "s")
	second, _ := json.Marshal(again)
	if string(first) != string(second) {
		t.Fatalf("context JSON changed: %s != %s", first, second)
	}
}

func TestSessionHotFilesTiesUseCanonicalThenRawPath(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	for _, key := range []string{"z.go", `a\same.go`, "a/same.go", "b.go"} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO session_events(repo_id, session_id, event_type, key, value, metadata, created_at) VALUES (?, 's', 'read', ?, '', '{}', '2026-01-01T00:00:00Z')`, repoID, key); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SessionGetHotFiles(ctx, repoID, "s", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a/same.go", `a\same.go`, "b.go"}
	for i := range want {
		if got[i]["file"] != want[i] {
			t.Fatalf("hot files = %v, want %v", got, want)
		}
	}
}

func TestArchitectureAndDeadCodeUseSemanticPageOrder(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	var source int64
	for i := 20; i >= 0; i-- {
		fileID, err := insertTestFile(ctx, s, repoID, fmt.Sprintf("dir%02d/file.go", i))
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("Target%02d", i)
		id, err := insertTestSymbol(ctx, s, repoID, fileID, name, "pkg."+name)
		if err != nil {
			t.Fatal(err)
		}
		if i == 20 {
			source = id
		}
		if i < 16 {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line) VALUES (?, ?, ?, '', 'call', '', ?, 1)`, repoID, source, id, fileID); err != nil {
				t.Fatal(err)
			}
		}
	}
	overview, err := s.ArchitectureOverview(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	dirs := overview["top_directories"].([]map[string]any)
	if len(dirs) != 20 || dirs[0]["directory"] != "dir00" || dirs[19]["directory"] != "dir19" {
		t.Fatalf("top_directories = %v", dirs)
	}
	entry := overview["entry_points"].([]map[string]any)
	if len(entry) != 15 || entry[0]["qualified_name"] != "pkg.Target00" || entry[14]["qualified_name"] != "pkg.Target14" {
		t.Fatalf("entry_points = %v", entry)
	}

	deadStore, deadRepoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, deadStore, deadRepoID, "same.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name string
		col  int
	}{{"Third", 3}, {"First", 1}, {"Second", 2}} {
		id, err := insertTestSymbol(ctx, deadStore, deadRepoID, fileID, row.name, "pkg."+row.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := deadStore.db.ExecContext(ctx, `UPDATE symbols SET start_line = 10, start_col = ?, end_line = 10, end_col = ? WHERE id = ?`, row.col, row.col+1, id); err != nil {
			t.Fatal(err)
		}
	}
	var pages []string
	for offset := 0; offset < 3; offset++ {
		rows, err := deadStore.FindDeadCode(ctx, deadRepoID, 1, offset)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, rows[0]["symbol"].(string))
	}
	if want := []string{"pkg.First", "pkg.Second", "pkg.Third"}; !reflect.DeepEqual(pages, want) {
		t.Fatalf("dead-code pages = %v, want %v", pages, want)
	}
}

func TestArchitectureHubTiesUseSemanticIdentity(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "hub.go")
	if err != nil {
		t.Fatal(err)
	}
	target, err := insertTestSymbol(ctx, s, repoID, fileID, "Target", "pkg.Target")
	if err != nil {
		t.Fatal(err)
	}
	for i := architectureTopN; i >= 0; i-- {
		name := fmt.Sprintf("Hub%02d", i)
		id, err := insertTestSymbol(ctx, s, repoID, fileID, name, "pkg."+name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line) VALUES (?, ?, ?, '', 'call', '', ?, 1)`, repoID, id, target, fileID); err != nil {
			t.Fatal(err)
		}
	}
	overview, err := s.ArchitectureOverview(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	hubs := overview["hub_symbols"].([]map[string]any)
	if len(hubs) != architectureTopN {
		t.Fatalf("hub count = %d", len(hubs))
	}
	for i := 0; i < architectureTopN; i++ {
		if want := fmt.Sprintf("pkg.Hub%02d", i); hubs[i]["qualified_name"] != want {
			t.Fatalf("hub_symbols[%d] = %v, want %s", i, hubs[i], want)
		}
	}
}

func TestArchitectureZeroDegreeFillUsesSemanticIdentity(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "zero.go")
	if err != nil {
		t.Fatal(err)
	}
	source, err := insertTestSymbol(ctx, s, repoID, fileID, "zzSource", "pkg.zzSource")
	if err != nil {
		t.Fatal(err)
	}
	target, err := insertTestSymbol(ctx, s, repoID, fileID, "Positive", "pkg.Positive")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line) VALUES (?, ?, ?, '', 'call', '', ?, 1)`, repoID, source, target, fileID); err != nil {
		t.Fatal(err)
	}
	for i := 20; i >= 0; i-- {
		name := fmt.Sprintf("Zero%02d", i)
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, name, "pkg."+name); err != nil {
			t.Fatal(err)
		}
	}
	overview, err := s.ArchitectureOverview(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	entry := overview["entry_points"].([]map[string]any)
	if entry[0]["qualified_name"] != "pkg.Positive" {
		t.Fatalf("entry_points[0] = %v", entry[0])
	}
	for i := 0; i < architectureTopN-1; i++ {
		if want := fmt.Sprintf("pkg.Zero%02d", i); entry[i+1]["qualified_name"] != want {
			t.Fatalf("entry_points[%d] = %v, want %s", i+1, entry[i+1], want)
		}
	}
}

func TestVectorSearchLargePrefilterUsesSemanticIdentity(t *testing.T) {
	ctx := context.Background()
	for _, order := range [][]string{{"c.go", "b.go", "a.go"}, {"a.go", "b.go", "c.go"}} {
		s, repoID := newQueryTestStore(t)
		for _, path := range order {
			fileID, err := insertTestFile(ctx, s, repoID, path)
			if err != nil {
				t.Fatal(err)
			}
			name := "pkg." + path[:1]
			symbolID, err := insertTestSymbol(ctx, s, repoID, fileID, path[:1], name)
			if err != nil {
				t.Fatal(err)
			}
			vector := []float32{0, 1}
			if path == "a.go" {
				vector = []float32{1, 0}
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO symbol_embeddings(symbol_id, file_id, repo_id, embedding, dimensions, model_name, updated_at) VALUES (?, ?, ?, ?, 2, 'test', '2026-01-01T00:00:00Z')`, symbolID, fileID, repoID, float32ToBytes(vector)); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := s.vectorSearch(ctx, repoID, []float32{1, 0}, 1, 0, 2)
		if err != nil || len(rows) != 1 || rows[0]["symbol"] != "pkg.a" {
			t.Fatalf("vectorSearch = %v, %v", rows, err)
		}
	}
}

// insertVectorFixture inserts a file, a symbol and an embedding sharing one
// updated_at so scan-cap ordering depends solely on the path tie-break.
func insertVectorFixture(ctx context.Context, t *testing.T, s *Store, repoID int64, path string, vector []float32) {
	t.Helper()
	fileID, err := insertTestFile(ctx, s, repoID, path)
	if err != nil {
		t.Fatal(err)
	}
	symbolID, err := insertTestSymbol(ctx, s, repoID, fileID, path, "pkg."+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO symbol_embeddings(symbol_id, file_id, repo_id, embedding, dimensions, model_name, updated_at) VALUES (?, ?, ?, ?, 2, 'test', '2026-01-01T00:00:00Z')`, symbolID, fileID, repoID, float32ToBytes(vector)); err != nil {
		t.Fatal(err)
	}
}

// TestVectorSearchScanCapOrdersByRawPath pins the capped preselection to raw
// files.path ordering. `a\foo.go` is a legal logical identity on POSIX; the
// former REPLACE(f.path, char(92), '/') tie-break read it as `a/foo.go`, which
// sorts before `a0.go` and would take the single scan-cap slot.
func TestVectorSearchScanCapOrdersByRawPath(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	insertVectorFixture(ctx, t, s, repoID, `a\foo.go`, []float32{1, 0})
	insertVectorFixture(ctx, t, s, repoID, "a0.go", []float32{1, 0})

	rows, err := s.vectorSearch(ctx, repoID, []float32{1, 0}, 10, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["file"] != "a0.go" {
		t.Fatalf("vectorSearch scanCap=1 = %v, want single a0.go", rows)
	}
}

// TestVectorSearchPreservesExactPathIdentity proves the full-scan branch
// returns files.path byte-for-byte and paginates on that raw ordering.
func TestVectorSearchPreservesExactPathIdentity(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	insertVectorFixture(ctx, t, s, repoID, `a\foo.go`, []float32{1, 0})
	insertVectorFixture(ctx, t, s, repoID, "a0.go", []float32{1, 0})

	rows, err := s.vectorSearch(ctx, repoID, []float32{1, 0}, 10, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["file"] != "a0.go" || rows[1]["file"] != `a\foo.go` {
		t.Fatalf("vectorSearch = %v, want [a0.go a\\foo.go]", rows)
	}
	for i, want := range []string{"a0.go", `a\foo.go`} {
		page, err := s.vectorSearch(ctx, repoID, []float32{1, 0}, 1, i, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 || page[0]["file"] != want {
			t.Fatalf("vectorSearch(limit=1, offset=%d) = %v, want %s", i, page, want)
		}
	}
}

// TestAllFilePathsPreservesRawLogicalIdentityAndOrder pins AllFilePaths to raw
// files.path bytes and raw BINARY ordering. `a\foo.go` is a legal logical
// identity; the former REPLACE(path, char(92), '/') ordering read it as
// `a/foo.go`, sorting it ahead of `a0.go`, and filepath.ToSlash would rewrite
// its bytes on Windows.
func TestAllFilePathsPreservesRawLogicalIdentityAndOrder(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	for _, path := range []string{"z.go", `a\foo.go`, "a0.go"} {
		if _, err := insertTestFile(ctx, s, repoID, path); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.AllFilePaths(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a0.go", `a\foo.go`, "z.go"}
	if len(got) != len(want) {
		t.Fatalf("AllFilePaths() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllFilePaths()[%d] = %q, want %q (full %q)", i, got[i], want[i], got)
		}
	}
	if !strings.Contains(got[1], `\`) {
		t.Fatalf("AllFilePaths() lost literal backslash: %q", got[1])
	}
}

// TestAllImportsPreservesExactPathIdentity pins the AllImports map key to raw
// files.path bytes. `a\foo.go` and `a/foo.go` are distinct logical identities;
// the former filepath.ToSlash(path) collapsed them into one key on Windows and
// merged their import lists.
func TestAllImportsPreservesExactPathIdentity(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	seeded := map[string]string{
		`a\foo.go`: "example.com/backslash",
		"a/foo.go": "example.com/slash",
	}
	for path, importPath := range seeded {
		fileID, err := insertTestFile(ctx, s, repoID, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES(?, ?, ?)`, repoID, fileID, importPath); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.AllImports(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("AllImports() = %v, want 2 distinct keys", got)
	}
	for path, importPath := range seeded {
		imports, ok := got[path]
		if !ok {
			t.Fatalf("AllImports() missing key %q, got %v", path, got)
		}
		if len(imports) != 1 || imports[0] != importPath {
			t.Fatalf("AllImports()[%q] = %q, want [%q] (no merge)", path, imports, importPath)
		}
	}
}

// TestFindDeadCodePreservesLogicalPathOrderAndIdentity pins FindDeadCode to raw
// files.path ordering and raw returned identity. `a\foo.go` is a legal logical
// identity; the former REPLACE(f.path, char(92), '/') ordering read it as
// `a/foo.go`, which sorts before `a0.go` and changes LIMIT/OFFSET page
// membership. filepath.ToSlash is a no-op for literal backslashes on POSIX, so
// the ordering assertion is what reverse-fails locally; the output rewrite is
// Windows-only and proved by CI.
func TestFindDeadCodePreservesLogicalPathOrderAndIdentity(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	for _, seed := range []struct{ path, symbol string }{
		{`a\foo.go`, "DeadB"},
		{"a0.go", "DeadA"},
	} {
		fileID, err := insertTestFile(ctx, s, repoID, seed.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, seed.symbol, "pkg."+seed.symbol); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"a0.go", `a\foo.go`}
	rows, err := s.FindDeadCode(ctx, repoID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(want) {
		t.Fatalf("FindDeadCode() = %v, want %d rows", rows, len(want))
	}
	for i, path := range want {
		if rows[i]["file"] != path {
			t.Fatalf("FindDeadCode()[%d][file] = %q, want %q (full %v)", i, rows[i]["file"], path, rows)
		}
	}
	for i, path := range want {
		page, err := s.FindDeadCode(ctx, repoID, 1, i)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 || page[0]["file"] != path {
			t.Fatalf("FindDeadCode(limit=1, offset=%d) = %v, want %q", i, page, path)
		}
	}
}

// TestRelatedTestsPreservesLogicalPathOrderAndPagination pins both RelatedTests
// seed branches to raw files.path ordering. `a\foo_test.go` is a legal logical
// identity; the former REPLACE(path, '\', '/') tie-break read it as
// `a/foo_test.go`, and '/' sorts before '0', reversing the pair and changing
// which row survives LIMIT.
func TestRelatedTestsPreservesLogicalPathOrderAndPagination(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	targetFileID, err := insertTestFile(ctx, s, repoID, "target.go")
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := insertTestSymbol(ctx, s, repoID, targetFileID, "Target", "pkg.Target")
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct{ path, symbol string }{
		{`a\foo_test.go`, "TestB"},
		{"a0_test.go", "TestA"},
	} {
		testFileID, err := insertTestFile(ctx, s, repoID, seed.path)
		if err != nil {
			t.Fatal(err)
		}
		testSymbolID, err := insertTestSymbol(ctx, s, repoID, testFileID, seed.symbol, "pkg."+seed.symbol)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score, target_stable_key)
			VALUES(?, ?, ?, ?, ?, 'test_name_match', 0.8, '')
		`, repoID, testFileID, testSymbolID, targetFileID, targetID); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"a0_test.go", `a\foo_test.go`}
	for _, seed := range []struct{ name, symbol, file string }{
		{"file seed", "", "target.go"},
		{"symbol seed", "pkg.Target", ""},
	} {
		got, err := s.RelatedTests(ctx, repoID, seed.symbol, seed.file, 10, 0)
		if err != nil {
			t.Fatalf("%s: %v", seed.name, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: RelatedTests() = %+v, want %d rows", seed.name, got, len(want))
		}
		for i, path := range want {
			if got[i].File != path {
				t.Fatalf("%s: RelatedTests()[%d].File = %q, want %q (full %+v)", seed.name, i, got[i].File, path, got)
			}
		}
		for i, path := range want {
			page, err := s.RelatedTests(ctx, repoID, seed.symbol, seed.file, 1, i)
			if err != nil {
				t.Fatalf("%s: %v", seed.name, err)
			}
			if len(page) != 1 || page[0].File != path {
				t.Fatalf("%s: RelatedTests(limit=1, offset=%d) = %+v, want %q", seed.name, i, page, path)
			}
		}
	}
}

// TestArchitectureTopDirectoriesPreserveLogicalBackslashIdentity pins the
// top-level directory bucket to the raw files.path first logical segment.
// `a\literal` is a legal directory identity; the former
// REPLACE(path, char(92), '/') read its backslash as a separator and collapsed
// those files into the `a` bucket, changing grouping and counts.
func TestArchitectureTopDirectoriesPreserveLogicalBackslashIdentity(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	for _, path := range []string{"a/normal.go", `a\literal/one.go`, `a\literal/two.go`, "b/normal.go"} {
		if _, err := insertTestFile(ctx, s, repoID, path); err != nil {
			t.Fatal(err)
		}
	}

	overview, err := s.ArchitectureOverview(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	got := overview["top_directories"].([]map[string]any)
	want := []struct {
		dir   string
		count int
	}{{`a\literal`, 2}, {"a", 1}, {"b", 1}}
	if len(got) != len(want) {
		t.Fatalf("top_directories = %v, want %d buckets", got, len(want))
	}
	for i, w := range want {
		if got[i]["directory"] != w.dir || got[i]["file_count"] != w.count {
			t.Fatalf("top_directories[%d] = %v, want {%q %d} (full %v)", i, got[i], w.dir, w.count, got)
		}
	}
}

// TestFirstUniqueCanonicalPathsKeepsDistinctLogicalIdentities proves the helper
// dedupes exact repeats only. `a/literal.go` and `a\literal.go` are distinct
// logical identities; the former CanonicalRelPath call could fold them into one
// on Windows and drop a SemanticSearch hit before the context-size lookup.
func TestFirstUniqueCanonicalPathsKeepsDistinctLogicalIdentities(t *testing.T) {
	results := []map[string]any{
		{"file": "a/literal.go"},
		{"file": `a\literal.go`},
		{"file": "a/literal.go"},
		{"file": 42},
		{"file": ""},
	}
	want := []string{"a/literal.go", `a\literal.go`}
	if got := firstUniqueCanonicalPaths(results, 10); !reflect.DeepEqual(got, want) {
		t.Fatalf("firstUniqueCanonicalPaths() = %q, want %q", got, want)
	}
}

// TestBenchmarkTokensKeepsDistinctLogicalPathSizes proves the context-size map
// is keyed on raw files.path, so two distinct logical identities are charged
// separately rather than collapsed into one entry.
func TestBenchmarkTokensKeepsDistinctLogicalPathSizes(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	for _, seed := range []struct {
		path   string
		symbol string
		size   int64
	}{
		{"a/literal.go", "NeedleSlash", 100},
		{`a\literal.go`, "NeedleBackslash", 200},
	} {
		fileID, err := insertTestFile(ctx, s, repoID, seed.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE files SET size_bytes = ? WHERE id = ?`, seed.size, fileID); err != nil {
			t.Fatal(err)
		}
		sym, err := insertTestSymbol(ctx, s, repoID, fileID, seed.symbol, "pkg."+seed.symbol)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?, 'needle', 1.0)`, sym); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.BenchmarkTokens(ctx, repoID, "needle")
	if err != nil {
		t.Fatalf("BenchmarkTokens() error = %v", err)
	}
	if got["context_files"] != int64(2) || got["context_bytes"] != int64(300) {
		t.Fatalf("context = (%v files, %v bytes), want (2 files, 300 bytes)", got["context_files"], got["context_bytes"])
	}
}

// TestRubyPathsChangedKeepsDistinctLogicalIdentities pins the Ruby changed-path
// probe to raw logical identities. `a/literal.rb` and `a\literal.rb` are
// distinct; the former CanonicalRelPath call folded the second onto the first
// on Windows, so a changed non-Ruby file would have reported a Ruby change.
func TestRubyPathsChangedKeepsDistinctLogicalIdentities(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	if _, err := insertTestFileLang(ctx, s, repoID, "a/literal.rb", "ruby"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestFileLang(ctx, s, repoID, `a\literal.rb`, "go"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		paths []string
		want  bool
	}{
		{"ruby identity", []string{"a/literal.rb"}, true},
		{"backslash identity is not the ruby file", []string{`a\literal.rb`}, false},
		{"empty paths ignored", []string{"", `a\literal.rb`, ""}, false},
		{"exact duplicates deduped", []string{"a/literal.rb", "a/literal.rb"}, true},
		{"no paths", nil, false},
	} {
		got, err := s.rubyPathsChanged(ctx, repoID, tc.paths)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: rubyPathsChanged(%q) = %v, want %v", tc.name, tc.paths, got, tc.want)
		}
	}
}

// TestResolveEdgesForPathsKeepsDistinctLogicalIdentities pins the path-scoped
// resolver to raw logical identities. `b/x\y.go` and `b/x/y.go` are two
// distinct files. The former CanonicalRelPath call rewrote the first into the
// second on Windows, so scoping one reconsidered the other: the named file
// stayed unresolved and an unrelated file was resolved instead. On POSIX filepath.ToSlash leaves the backslash alone, so the old
// code also passes here; Windows CI is where it reverse-fails.
func TestResolveEdgesForPathsKeepsDistinctLogicalIdentities(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	declare := func(path, name, qualified string) int64 {
		t.Helper()
		fileID, err := insertTestFile(ctx, s, repoID, path)
		if err != nil {
			t.Fatal(err)
		}
		symbolID, err := insertTestSymbol(ctx, s, repoID, fileID, name, qualified)
		if err != nil {
			t.Fatal(err)
		}
		return symbolID
	}
	call := func(path, symbol, dstName string) int64 {
		t.Helper()
		fileID, err := insertTestFile(ctx, s, repoID, path)
		if err != nil {
			t.Fatal(err)
		}
		srcID, err := insertTestSymbol(ctx, s, repoID, fileID, symbol, "pkg."+symbol)
		if err != nil {
			t.Fatal(err)
		}
		edgeID, err := insertTestEdge(ctx, s, repoID, fileID, srcID, dstName)
		if err != nil {
			t.Fatal(err)
		}
		return edgeID
	}
	target := declare(`b/x\target.go`, "Target", "pkg.Target")
	declare("b/x/other.go", "Other", "pkg.Other")
	backslashEdge := call(`b/x\y.go`, "UseBackslash", "Target")
	slashEdge := call("b/x/y.go", "UseSlash", "Other")

	// Empty and duplicate entries must not change what the scope covers.
	if err := s.ResolveEdgesForPaths(ctx, repoID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveEdgesForPaths(ctx, repoID, []string{""}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveEdgesForPaths(ctx, repoID, []string{`b/x\y.go`, "", `b/x\y.go`}); err != nil {
		t.Fatal(err)
	}

	var got sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, backslashEdge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != target {
		t.Fatalf(`scoped b/x\y.go edge dst_symbol_id = (%v, %d), want (true, %d)`, got.Valid, got.Int64, target)
	}
	var other sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, slashEdge).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if other.Valid {
		t.Fatalf("b/x/y.go edge resolved to %d, want untouched by that scope", other.Int64)
	}
}
