package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

func preferredSymbolFixture(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	s, repoID := newQueryTestStore(t)
	addPreferredSymbol(t, s, repoID, "a_wrapper.go", "LookupSymbolShortName", "store.LookupSymbolShortName")
	id := addPreferredSymbol(t, s, repoID, "z_impl.go", "lookupSymbolShortName", "store.lookupSymbolShortName")
	addPreferredSymbol(t, s, repoID, "other.go", "lookupSymbolShortName", "other.lookupSymbolShortName")
	return s, repoID, id
}

func addPreferredSymbol(t *testing.T, s *Store, repoID int64, file, name, qualified string) int64 {
	t.Helper()
	ctx := context.Background()
	fileID, err := insertTestFile(ctx, s, repoID, file)
	if err != nil {
		t.Fatal(err)
	}
	id, err := insertTestSymbol(ctx, s, repoID, fileID, name, qualified)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO symbol_fts(repo_id, symbol_id, name, qualified_name) VALUES(?, ?, ?, ?)`, repoID, id, name, qualified); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFindSymbolPreferredQualifiedIdentity(t *testing.T) {
	s, repoID, id := preferredSymbolFixture(t)
	ctx := context.Background()
	const name = "store.lookupSymbolShortName"
	want, err := s.FindSymbolExactResult(ctx, repoID, name, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.FindSymbolPreferredResult(ctx, repoID, name, 1, 0)
	if err != nil || !got.Matched || len(got.Matches) != 1 || got.Matches[0].ID != id || !reflect.DeepEqual(got.Matches, want.Matches) {
		t.Fatalf("preferred identity/metadata = %+v, %v; want %+v", got, err, want)
	}
	for _, offset := range []int{1, 100} {
		got, err := s.FindSymbolPreferredResult(ctx, repoID, name, 1, offset)
		if err != nil || !got.Matched || len(got.Matches) != 0 {
			t.Fatalf("exhausted exact page at %d = %+v, %v", offset, got, err)
		}
	}
}

func TestFindSymbolPreferredPreservesSearchFallback(t *testing.T) {
	s, repoID, _ := preferredSymbolFixture(t)
	ctx := context.Background()
	for _, query := range []string{"lookupSymbolShortName", "lookupSym", "other.lookupSym", "STORE.LOOKUPSYMBOLSHORTNAME", "NoSuchSymbolAnywhere"} {
		t.Run(query, func(t *testing.T) {
			want, err := s.SearchSymbolsResult(ctx, repoID, query, 20, 0)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.FindSymbolPreferredResult(ctx, repoID, query, 20, 0)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("fallback = %+v, %v; want legacy %+v", got, err, want)
			}
			if query != "NoSuchSymbolAnywhere" && len(got.Matches) == 0 {
				t.Fatal("fixture did not exercise a nonempty fallback")
			}
		})
	}
}

func TestFindSymbolPreferredKeepsExactAmbiguity(t *testing.T) {
	s, repoID, id := preferredSymbolFixture(t)
	ctx := context.Background()
	const name = "store.lookupSymbolShortName"
	duplicate := addPreferredSymbol(t, s, repoID, "y_impl.go", "lookupSymbolShortName", name)
	for offset, wantID := range []int64{duplicate, id} {
		got, err := s.FindSymbolPreferredResult(ctx, repoID, name, 1, offset)
		if err != nil || !got.Matched || len(got.Matches) != 1 || got.Matches[0].ID != wantID {
			t.Fatalf("exact candidate page %d = %+v, %v; want %d", offset, got, err, wantID)
		}
	}
	if _, err := s.FindCalleesResult(ctx, repoID, name, 0, 10, 0); !errors.Is(err, ErrSymbolAmbiguous) {
		t.Fatalf("singular relationship identity must remain ambiguous: %v", err)
	}
	got, err := s.FindCalleesResult(ctx, repoID, "ignored", id, 10, 0)
	if err != nil || !got.TargetFound {
		t.Fatalf("explicit identity = %+v, %v", got, err)
	}
}

func TestFindSymbolPreferredExcludesDeletedAndForeignExact(t *testing.T) {
	s, repoID, id := preferredSymbolFixture(t)
	ctx := context.Background()
	const name = "store.lookupSymbolShortName"
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET is_deleted = 1 WHERE id = (SELECT file_id FROM symbols WHERE id = ?)`, id); err != nil {
		t.Fatal(err)
	}
	foreign, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	addPreferredSymbol(t, s, foreign.ID, "foreign.go", "lookupSymbolShortName", name)
	want, err := s.SearchSymbolsResult(ctx, repoID, name, 20, 0)
	if err != nil || len(want.Matches) == 0 {
		t.Fatalf("live fallback control = %+v, %v", want, err)
	}
	got, err := s.FindSymbolPreferredResult(ctx, repoID, name, 20, 0)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("deleted/foreign identity masked fallback: %+v, %v; want %+v", got, err, want)
	}
}

func TestFindSymbolPreferredLikeFallback(t *testing.T) {
	s, repoID, id := preferredSymbolFixture(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `DROP TABLE symbol_fts`); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindSymbolPreferredResult(ctx, repoID, "store.lookupSymbolShortName", 1, 0)
	if err != nil || !got.Matched || len(got.Matches) != 1 || got.Matches[0].ID != id {
		t.Fatalf("exact identity on LIKE fallback = %+v, %v", got, err)
	}
	want, err := s.SearchSymbolsResult(ctx, repoID, "lookupSym", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.FindSymbolPreferredResult(ctx, repoID, "lookupSym", 20, 0)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("LIKE fallback changed: %+v, %v; want %+v", got, err, want)
	}
}

func TestFindSymbolPreferredUsesActiveSnapshot(t *testing.T) {
	s, repoID, id := preferredSymbolFixture(t)
	ctx := context.Background()
	s.db.SetMaxOpenConns(2)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbols`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET is_deleted = 1 WHERE id = (SELECT file_id FROM symbols WHERE id = ?)`, id); err != nil {
		t.Fatal(err)
	}
	readCtx := context.WithValue(ctx, parserSemanticGraphQueryerKey{}, parserSemanticGraphQueryer(tx))
	for _, offset := range []int{0, 1} {
		got, err := s.FindSymbolPreferredResult(readCtx, repoID, "store.lookupSymbolShortName", 1, offset)
		if err != nil || !got.Matched || (offset == 0 && (len(got.Matches) != 1 || got.Matches[0].ID != id)) || (offset == 1 && len(got.Matches) != 0) {
			t.Fatalf("pinned snapshot at offset %d = %+v, %v", offset, got, err)
		}
	}
	got, err := s.FindSymbolPreferredResult(ctx, repoID, "store.lookupSymbolShortName", 1, 0)
	if err != nil || len(got.Matches) != 1 || got.Matches[0].ID == id {
		t.Fatalf("fresh snapshot must use live fallback: %+v, %v", got, err)
	}
}

func TestFindSymbolPreferredDoesNotHideDatabaseErrors(t *testing.T) {
	s, repoID, _ := preferredSymbolFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindSymbolPreferredResult(context.Background(), repoID, "store.lookupSymbolShortName", 1, 0); err == nil {
		t.Fatal("closed store reported a successful lookup")
	}
}

func TestFindSymbolPreferredPreservesLiteralLanguageIdentities(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct{ qualified, language, kind string }{
		{"pkg.Widget", "java", "class"},
		{"app::work", "rust", "function"},
		{"cpp::operator[]", "cpp", "method"},
		{"mod.Widget.member", "py", "method"},
		{"pkg.Čtení", "go", "function"},
	} {
		t.Run(tc.qualified, func(t *testing.T) {
			id := addPreferredSymbol(t, s, repoID, tc.language+".src", "member", tc.qualified)
			if _, err := s.db.ExecContext(ctx, `UPDATE symbols SET language = ?, kind = ? WHERE id = ?`, tc.language, tc.kind, id); err != nil {
				t.Fatal(err)
			}
			got, err := s.FindSymbolPreferredResult(ctx, repoID, tc.qualified, 1, 0)
			if err != nil || len(got.Matches) != 1 || got.Matches[0].ID != id || got.Matches[0].QualifiedName != tc.qualified || got.Matches[0].Language != tc.language || got.Matches[0].Kind != tc.kind {
				t.Fatalf("literal identity metadata = %+v, %v", got, err)
			}
		})
	}
}
