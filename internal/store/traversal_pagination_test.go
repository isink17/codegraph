package store

import (
	"context"
	"reflect"
	"testing"
)

func buildTraceCollisionStore(t *testing.T, reverse bool) (*Store, int64) {
	t.Helper()
	s, repoID := newQueryTestStore(t)
	ctx := context.Background()
	paths := []string{"root.go", "a.go", "b.go"}
	if reverse {
		paths = []string{"root.go", "b.go", "a.go"}
	}
	files := make(map[string]int64, len(paths))
	for _, path := range paths {
		id, err := insertTestFile(ctx, s, repoID, path)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = id
	}
	root, err := insertTestSymbol(ctx, s, repoID, files["root.go"], "Root", "pkg.Root")
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"a.go", "b.go"}
	if reverse {
		order = []string{"b.go", "a.go"}
	}
	for _, path := range order {
		name := "SameA"
		kind := "function"
		if path == "b.go" {
			name, kind = "SameB", "method"
		}
		target, err := insertTestSymbolKind(ctx, s, repoID, files[path], name, "pkg.Same", kind, "", "go")
		if err != nil {
			t.Fatal(err)
		}
		edge, err := insertTestEdge(ctx, s, repoID, files["root.go"], root, "pkg.Same")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, target, edge); err != nil {
			t.Fatal(err)
		}
	}
	return s, repoID
}

func traceRows(t *testing.T, s *Store, repoID int64, limit, offset int) TraceResult {
	t.Helper()
	result, err := s.TraceDependenciesResult(context.Background(), repoID, "pkg.Root", "downstream", 1, limit, offset)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTracePaginationHasCanonicalOrderAndMetadata(t *testing.T) {
	first, firstRepo := buildTraceCollisionStore(t, false)
	second, secondRepo := buildTraceCollisionStore(t, true)

	all := traceRows(t, first, firstRepo, 10, 0)
	reversed := traceRows(t, second, secondRepo, 10, 0)
	if !reflect.DeepEqual(all, reversed) {
		t.Fatalf("reversed insertion changed trace result:\nfirst=%+v\nsecond=%+v", all, reversed)
	}
	if all.Total != 3 || len(all.Dependencies) != 3 || all.Offset != 0 || all.Truncated {
		t.Fatalf("full trace metadata = %+v", all)
	}
	if all.Dependencies[1]["file"] != "a.go" || all.Dependencies[2]["file"] != "b.go" {
		t.Fatalf("collision rows are not in semantic file order: %+v", all.Dependencies)
	}

	for _, limit := range []int{1, 2, 3} {
		var walked []map[string]any
		for offset := 0; offset < all.Total; offset += limit {
			page := traceRows(t, first, firstRepo, limit, offset)
			if page.Total != all.Total || page.Offset != offset || page.Truncated != (offset+len(page.Dependencies) < all.Total) {
				t.Fatalf("limit=%d offset=%d metadata = %+v", limit, offset, page)
			}
			walked = append(walked, page.Dependencies...)
		}
		if !reflect.DeepEqual(walked, all.Dependencies) {
			t.Fatalf("limit=%d page walk differs:\nwalked=%+v\nfull=%+v", limit, walked, all.Dependencies)
		}
	}

	end := traceRows(t, first, firstRepo, 2, all.Total)
	past := traceRows(t, first, firstRepo, 2, all.Total+100)
	for name, result := range map[string]TraceResult{"end": end, "past": past} {
		if len(result.Dependencies) != 0 || result.Offset != all.Total || result.Truncated || !result.TargetFound {
			t.Fatalf("%s trace boundary = %+v", name, result)
		}
	}

	missing := func() TraceResult {
		result, err := first.TraceDependenciesResult(context.Background(), firstRepo, "missing", "downstream", 1, 2, 100)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}()
	if missing.TargetFound || missing.Total != 0 || missing.Offset != 0 || missing.Truncated || len(missing.Dependencies) != 0 {
		t.Fatalf("missing trace = %+v", missing)
	}
}

func TestTraceBothDirectionIsRepeatable(t *testing.T) {
	s, repoID := buildTraceCollisionStore(t, false)
	ctx := context.Background()
	var root, rootFile int64
	if err := s.db.QueryRowContext(ctx, `SELECT id, file_id FROM symbols WHERE repo_id = ? AND qualified_name = 'pkg.Root'`, repoID).Scan(&root, &rootFile); err != nil {
		t.Fatal(err)
	}
	up, err := insertTestSymbol(ctx, s, repoID, rootFile, "Up", "pkg.Up")
	if err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repoID, rootFile, up, "pkg.Root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, root, edge); err != nil {
		t.Fatal(err)
	}
	first, err := s.TraceDependenciesResult(ctx, repoID, "pkg.Root", "both", 1, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.TraceDependenciesResult(ctx, repoID, "pkg.Root", "both", 1, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("both trace is not repeatable: first=%+v second=%+v", first, second)
	}
}

// TestTracePreservesLiteralBackslashFileIdentity pins the trace `file` column
// to the stored `files.path` bytes. `pkg/x\y.go` and `pkg/x/y.go` are two
// stored files under P23 logical identity. The seed lives in the backslash
// file and reaches one symbol in each sibling; both share a qualified name so
// depth and symbol tie and the stored path is the public ordering
// discriminator ('/' sorts before '\'). traceDependencies' former
// filepath.ToSlash on the seed row and on each BFS row folded the backslash
// spelling onto the sibling on Windows, aliasing the two rows and flipping
// their order. On POSIX ToSlash is the identity, so the old code passes there.
func TestTracePreservesLiteralBackslashFileIdentity(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	backslashFile, err := insertTestFile(ctx, s, repoID, `pkg/x\y.go`)
	if err != nil {
		t.Fatal(err)
	}
	slashFile, err := insertTestFile(ctx, s, repoID, "pkg/x/y.go")
	if err != nil {
		t.Fatal(err)
	}
	root, err := insertTestSymbol(ctx, s, repoID, backslashFile, "Root", "pkg.Root")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		file int64
		name string
	}{{slashFile, "SameSlash"}, {backslashFile, "SameBackslash"}} {
		id, err := insertTestSymbol(ctx, s, repoID, target.file, target.name, "pkg.Same")
		if err != nil {
			t.Fatal(err)
		}
		edge, err := insertTestEdge(ctx, s, repoID, backslashFile, root, "pkg.Same")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, id, edge); err != nil {
			t.Fatal(err)
		}
	}

	result, err := s.TraceDependenciesResult(ctx, repoID, "pkg.Root", "downstream", 1, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"symbol": "pkg.Root", "kind": "function", "name": "Root", "file": `pkg/x\y.go`, "depth": 0, "direction": "downstream"},
		{"symbol": "pkg.Same", "kind": "function", "name": "SameSlash", "file": "pkg/x/y.go", "depth": 1, "direction": "downstream"},
		{"symbol": "pkg.Same", "kind": "function", "name": "SameBackslash", "file": `pkg/x\y.go`, "depth": 1, "direction": "downstream"},
	}
	if result.Total != 3 || !result.TargetFound || !reflect.DeepEqual(result.Dependencies, want) {
		t.Fatalf("trace = %+v\nwant dependencies %v", result, want)
	}
	// Control: the two depth-1 rows are distinct stored identities and must not
	// alias in output; the name column proves which stored row each file came from.
	if result.Dependencies[1]["file"] == result.Dependencies[2]["file"] {
		t.Fatalf("sibling stored identities aliased: %+v", result.Dependencies[1:])
	}
}
