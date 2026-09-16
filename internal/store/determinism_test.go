package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
