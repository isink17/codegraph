package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestV2DevelopmentBaselineRefusedWithoutMutation(t *testing.T) {
	for _, version := range []int{1, 44} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
			db, err := sql.Open(SQLiteDriverName(), path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
    CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
    PRAGMA user_version=2;`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO schema_migrations VALUES(?, '')`, version); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			v1 := filepath.Join(filepath.Dir(path), "codegraph.sqlite")
			if err := os.WriteFile(v1, []byte("unchanged legacy v1 cache"), 0600); err != nil {
				t.Fatal(err)
			}
			before := snapshotSQLiteArtifacts(t, path)
			for _, open := range []func(string) (*Store, error){Open, func(path string) (*Store, error) { return OpenReadOnly(path, OpenOptions{}) }} {
				s, err := open(path)
				if s != nil {
					s.Close()
					t.Fatal("development database opened")
				}
				if err == nil || !strings.Contains(err.Error(), "codegraph index <repo-path> --rebuild") {
					t.Fatalf("refusal = %v", err)
				}
				assertSQLiteArtifactsEqual(t, before, snapshotSQLiteArtifacts(t, path))
				data, err := os.ReadFile(v1)
				if err != nil || string(data) != "unchanged legacy v1 cache" {
					t.Fatalf("legacy v1 changed: %q, %v", data, err)
				}

			}
		})
	}
}

func TestV2BaselineIncludesCurrentParserAndScopeFacts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), RepoDatabaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, query := range []string{
		`SELECT parser_profile, parser_call_edges, parser_semantic_epoch, parse_state FROM files LIMIT 0`,
		`SELECT resolution_strategy, resolution_confidence FROM edges LIMIT 0`,
		`SELECT target_stable_key FROM test_links LIMIT 0`,
		`SELECT * FROM kotlin_jvm_callable_evidence LIMIT 0`,
		`SELECT * FROM kotlin_jvm_name_evidence LIMIT 0`,
		`SELECT * FROM swift_extension_memberships LIMIT 0`,
		`SELECT * FROM php_composer_psr4_mapping LIMIT 0`,
	} {
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("baseline schema: %s: %v", query, err)
		}
		rows.Close()
	}
	var markers int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key LIKE 'resolver.%repaired%' OR key LIKE 'parser.cpp_receiver_reparsed%'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Fatalf("baseline has %d retired upgrade markers", markers)
	}
}

func TestV2BaselineMarkerRefusalObservesWALWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key='format.schema_baseline'`); err != nil {
		t.Fatal(err)
	}
	before := snapshotSQLiteArtifacts(t, path)
	for _, open := range []func(string) (*Store, error){Open, func(path string) (*Store, error) { return OpenReadOnly(path, OpenOptions{}) }} {
		db, err := open(path)
		if db != nil {
			db.Close()
			t.Fatal("WAL development index opened")
		}
		if err == nil || !strings.Contains(err.Error(), "codegraph index <repo-path> --rebuild") {
			t.Fatalf("WAL refusal: %v", err)
		}
		assertSQLiteArtifactsEqual(t, before, snapshotSQLiteArtifacts(t, path))
	}
}
