package store

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRustResolutionBatchStatsStaySetBased(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lib := rustAcceptanceFile(t, ctx, s, repo.ID, "lib.rs", "crate", "lib.rs")
	targetFile := rustAcceptanceFile(t, ctx, s, repo.ID, "a.rs", "crate::a", "lib.rs")
	rustAcceptanceFile(t, ctx, s, repo.ID, "other/main.rs", "crate", "other/main.rs")
	rustAcceptanceFile(t, ctx, s, repo.ID, "other/util.rs", "crate::util", "other/main.rs")
	target, err := insertTestSymbolLang(ctx, s, repo.ID, targetFile, "helper", "crate::a::helper", "rust")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE symbols SET visibility='public' WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO rust_module_evidence(repo_id,file_id,owner_module,module_name,external_path,visibility) VALUES(?,?,?,?,?,?)`, repo.ID, lib, "crate", "a", "a", "private"); err != nil {
		t.Fatal(err)
	}

	makeBatch := func(n int) map[int64]struct{} {
		ids := make(map[int64]struct{}, n)
		for i := 0; i < n; i++ {
			file := lib
			src, e := insertTestSymbolLang(ctx, s, repo.ID, file, "run"+strconv.Itoa(i), "crate::run"+strconv.Itoa(i), "rust")
			if e != nil {
				t.Fatal(e)
			}
			edge, e := insertTestEdge(ctx, s, repo.ID, file, src, "crate::a::helper")
			if e != nil {
				t.Fatal(e)
			}
			ids[edge] = struct{}{}
		}
		return ids
	}

	for _, n := range []int{1, 100} {
		ids := makeBatch(n)
		var stats RustResolutionStats
		if _, err := s.resolveRustModuleScopeStandaloneWithStats(ctx, repo.ID, ids, &stats); err != nil {
			t.Fatal(err)
		}
		if stats.AffectedCrates != 1 || stats.AffectedModules != 2 || stats.AffectedEdges != n || stats.BatchApplyOps != 1 || stats.ReExportEvidenceLoads != 1 {
			t.Fatalf("n=%d stats=%+v, want one crate, %d edges, one apply", n, stats, n)
		}
		if stats.BatchInvalidationOps != 0 {
			t.Fatalf("incremental stats=%+v, invalidation belongs to the central name pass", stats)
		}
	}
}

func TestRustResolutionUseScopeBindsExplicitImport(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lib := rustAcceptanceFile(t, ctx, s, repo.ID, "lib.rs", "crate", "lib.rs")
	a := rustAcceptanceFile(t, ctx, s, repo.ID, "a.rs", "crate::a", "lib.rs")
	caller := rustAcceptanceFile(t, ctx, s, repo.ID, "caller.rs", "crate::caller", "lib.rs")
	target, err := insertTestSymbolLang(ctx, s, repo.ID, a, "helper", "crate::a::helper", "rust")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE symbols SET visibility='public' WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO rust_module_evidence(repo_id,file_id,owner_module,module_name,external_path,visibility) VALUES(?,?,?,?,?,?)`, repo.ID, lib, "crate", "a", "a", "private"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO rust_module_evidence(repo_id,file_id,owner_module,module_name,external_path,visibility) VALUES(?,?,?,?,?,?)`, repo.ID, lib, "crate", "caller", "caller", "private"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,local_name,import_kind,wildcard,is_reexport,owner_module) VALUES(?,?,?,?,?,?,?,?,?)`, repo.ID, caller, "rust", "crate::a::helper", "helper", "use", 0, 0, "crate::caller"); err != nil {
		t.Fatal(err)
	}
	src, err := insertTestSymbolLang(ctx, s, repo.ID, caller, "run", "crate::caller::run", "rust")
	if err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, caller, src, "helper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveRustModuleScopeStandalone(ctx, repo.ID, map[int64]struct{}{edge: {}}); err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id=?`, edge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("target=%d, want %d", got, target)
	}
}

func rustAcceptanceFile(t *testing.T, ctx context.Context, s *Store, repoID int64, path, module, root string) int64 {
	t.Helper()
	id, err := insertTestFileLang(ctx, s, repoID, path, "rust")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,module_path,crate_root) VALUES(?,?,?,?,?)`, repoID, id, "rust", module, root); err != nil {
		t.Fatal(err)
	}
	return id
}
