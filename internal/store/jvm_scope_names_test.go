package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
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

func TestJVMScopeEdgeNamesReceiverHelperDependencies(t *testing.T) {
	f := newGateFixture(t)
	for _, lang := range []string{"kotlin", "java", "php"} {
		file := f.file(t, "Caller."+lang, lang)
		src := f.symbol(t, file, "run", lang+".run", lang)
		for _, bound := range []bool{false, true} {
			name := fmt.Sprintf("%s.this%d", lang, map[bool]int{false: 0, true: 1}[bound])
			id := f.edge(t, file, src, name)
			var target any
			if bound {
				target = src
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence=?,dst_symbol_id=? WHERE id=?`, graph.KotlinOrdinaryLambdaReceiverEvidence+"forEach", target, id); err != nil {
				t.Fatal(err)
			}
		}
		id := f.edge(t, file, src, lang+".otherHelper")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence=? WHERE id=?`, graph.KotlinOrdinaryLambdaReceiverEvidence+"map", id); err != nil {
			t.Fatal(err)
		}
	}
	other, err := f.store.UpsertRepo(f.ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := insertTestFileLang(f.ctx, f.store, other.ID, "Other.kt", "kotlin")
	if err != nil {
		t.Fatal(err)
	}
	src, err := insertTestSymbolLang(f.ctx, f.store, other.ID, file, "run", "other.run", "kotlin")
	if err != nil {
		t.Fatal(err)
	}
	id, err := insertTestEdge(f.ctx, f.store, other.ID, file, src, "other.this")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence=? WHERE id=?`, graph.KotlinOrdinaryLambdaReceiverEvidence+"forEach", id); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.jvmScopeEdgeNames(f.ctx, f.repoID, []string{"forEach"})
	want := []string{"kotlin.this0", "kotlin.this1"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("names=%v err=%v want=%v", got, err, want)
	}
	for _, name := range []string{"unrelated", "forEa", "forEachSuffix"} {
		if got, err := f.store.jvmScopeEdgeNames(f.ctx, f.repoID, []string{name}); err != nil || len(got) != 0 {
			t.Errorf("unrelated %q names=%v err=%v", name, got, err)
		}
	}
	// Cross multiple bind-budget batches without broadening the selected population.
	names := make([]string, 1100)
	for i := range names {
		names[i] = fmt.Sprintf("unrelated%d", i)
	}
	names[len(names)-1] = "forEach"
	if got, err := f.store.jvmScopeEdgeNames(f.ctx, f.repoID, names); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("batched names=%v err=%v want=%v", got, err, want)
	}
}
