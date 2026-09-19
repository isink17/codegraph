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

	"github.com/isink17/codegraph/internal/store"
)

// TestServiceGatedReadsFailClosedOnLegacyRepositoryPathFormat proves the
// query-layer read gate: every Service method that calls requirePaths returns
// ErrRepositoryPathFormatRebuild for a populated repository without the
// current marker, and returns no partial result.
func TestServiceGatedReadsFailClosedOnLegacyRepositoryPathFormat(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Populated, no marker: a pre-P23 repository as the query layer sees it.
	if _, err := raw.Exec(`INSERT INTO files(repo_id, path, language, indexed_at) VALUES(?, ?, 'go', '')`, repo.ID, `src\pkg\file.go`); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var b strings.Builder
		for _, q := range []string{
			`SELECT id, hex(path), is_deleted FROM files ORDER BY id`,
			`SELECT hex(path) FROM dirty_files ORDER BY path`,
			`SELECT key, value FROM settings ORDER BY key`,
		} {
			rows, err := raw.Query(q)
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
				fmt.Fprintln(&b, vals...)
			}
			rows.Close()
		}
		return b.String()
	}
	before := snapshot()
	svc := New(s, nil)
	reads := []struct {
		name string
		call func() (any, error)
	}{
		{"ArchitectureOverview", func() (any, error) { return svc.ArchitectureOverview(ctx, repo.ID) }},
		{"FindSymbol", func() (any, error) { return svc.FindSymbol(ctx, repo.ID, "Thing", 10, 0) }},
		{"FindSymbolExact", func() (any, error) { return svc.FindSymbolExact(ctx, repo.ID, "Thing", 10, 0) }},
		{"SearchSymbols", func() (any, error) { return svc.SearchSymbols(ctx, repo.ID, "Thing", 10, 0) }},
		{"FindCallers", func() (any, error) { return svc.FindCallers(ctx, repo.ID, "Thing", 0, 10, 0) }},
		{"FindCallees", func() (any, error) { return svc.FindCallees(ctx, repo.ID, "Thing", 0, 10, 0) }},
		{"ImpactRadius", func() (any, error) { return svc.ImpactRadius(ctx, repo.ID, []string{"Thing"}, nil, 1, 10, 0) }},
		{"RelatedTests", func() (any, error) { return svc.RelatedTests(ctx, repo.ID, "Thing", "", 10, 0) }},
		{"RelatedTestsForFiles", func() (any, error) { return svc.RelatedTestsForFiles(ctx, repo.ID, []string{"src/pkg/file.go"}, 10, 0) }},
		{"SemanticSearch", func() (any, error) { return svc.SemanticSearch(ctx, repo.ID, "thing", 10, 0) }},
		{"FindDeadCode", func() (any, error) { return svc.FindDeadCode(ctx, repo.ID, 10, 0) }},
		{"ListFiles", func() (any, error) { return svc.ListFiles(ctx, repo.ID, "", 10, 0) }},
		{"ContextForTask", func() (any, error) { return svc.ContextForTask(ctx, repo.ID, "Thing", ContextForTaskOptions{}) }},
	}
	for _, r := range reads {
		got, err := r.call()
		if !errors.Is(err, store.ErrRepositoryPathFormatRebuild) {
			t.Fatalf("%s on legacy repository: err = %v, want ErrRepositoryPathFormatRebuild", r.name, err)
		}
		if !isZeroResult(got) {
			t.Fatalf("%s returned a partial result alongside the rebuild error: %#v", r.name, got)
		}
		if after := snapshot(); after != before {
			t.Fatalf("%s mutated the legacy database:\nbefore:\n%s\nafter:\n%s", r.name, before, after)
		}
	}
	var marker int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM settings WHERE value = 'logical-slash-v1'`).Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("marker rows after rejected reads = %d, %v; want 0", marker, err)
	}
	var native int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM files WHERE path = ?`, `src\pkg\file.go`).Scan(&native); err != nil || native != 1 {
		t.Fatalf("legacy row rewritten: count = %d, %v", native, err)
	}
}

// isZeroResult reports whether a rejected read handed back nothing: a nil
// map, slice or pointer. Anything else is a partial result leaking past the
// gate.
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
