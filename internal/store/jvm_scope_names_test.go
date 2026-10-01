package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestJVMScopeEdgeNamesSkipsNonJVMRepository(t *testing.T) {
	f := newGateFixture(t)
	f.file(t, "Caller.php", "php")
	// Removing the edge table proves the gate never prepares an edge query.
	if _, err := f.store.db.ExecContext(f.ctx, "DROP TABLE edges"); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.jvmScopeEdgeNames(f.ctx, f.repoID, []string{"Service"})
	if err != nil || len(got) != 0 {
		t.Fatalf("names=%v error=%v", got, err)
	}
}

func TestJVMScopeEdgeNamesMixedRepository(t *testing.T) {
	f := newGateFixture(t)
	for _, lang := range []string{"java", "kotlin", "php"} {
		file := f.file(t, "Caller."+lang, lang)
		src := f.symbol(t, file, "call", lang+".call", lang)
		f.edge(t, file, src, "Service.run")
		f.edge(t, file, src, "lib.Service.stop")
		f.edge(t, file, src, "Other.run")
		if lang == "php" {
			f.edge(t, file, src, "php.Service.only")
		}
	}
	got, err := f.store.jvmScopeEdgeNames(f.ctx, f.repoID, []string{"Service"})
	want := []string{"Service.run", "lib.Service.stop"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("names=%v error=%v want=%v", got, err, want)
	}
}

func BenchmarkJVMScopeEdgeNamesNonJVM(b *testing.B) {
	ctx := context.Background()
	s, err := Open(filepath.Join(b.TempDir(), "graph.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	file, err := insertTestFileLang(ctx, s, repo.ID, "Caller.php", "php")
	if err != nil {
		b.Fatal(err)
	}
	src, err := insertTestSymbolLang(ctx, s, repo.ID, file, "call", "call", "php")
	if err != nil {
		b.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		if _, err := tx.ExecContext(ctx, "INSERT INTO edges(repo_id,file_id,src_symbol_id,dst_name,edge_kind) VALUES(?,?,?,?,?)", repo.ID, file, src, fmt.Sprintf("Service%d.run", i), "calls"); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	names := make([]string, 60)
	for i := range names {
		names[i] = fmt.Sprintf("Service%d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.jvmScopeEdgeNames(ctx, repo.ID, names); err != nil {
			b.Fatal(err)
		}
	}
}
