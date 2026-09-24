package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPHPComposerPSR4MappingsReplaceAndRead(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	repo, err := s.UpsertRepo(ctx, filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	old := []PHPComposerPSR4Mapping{{ManifestPath: "composer.json", MappingRole: "autoload", NamespacePrefix: "Old\\", RootPath: "old", RootOrdinal: 0}}
	if err := s.ReplacePHPComposerPSR4Mappings(ctx, repo.ID, old); err != nil {
		t.Fatal(err)
	}
	mappings := []PHPComposerPSR4Mapping{
		{ManifestPath: "composer.json", MappingRole: "autoload-dev", NamespacePrefix: "Dev\\", RootPath: "test", RootOrdinal: 0},
		{ManifestPath: "composer.json", MappingRole: "autoload", NamespacePrefix: "App\\", RootPath: "generated", RootOrdinal: 1},
		{ManifestPath: "composer.json", MappingRole: "autoload", NamespacePrefix: "App\\", RootPath: "src", RootOrdinal: 0},
		{ManifestPath: "composer.json", MappingRole: "autoload", NamespacePrefix: "Zoo\\", RootPath: "zoo", RootOrdinal: 0},
	}
	if err := s.ReplacePHPComposerPSR4Mappings(ctx, repo.ID, mappings); err != nil {
		t.Fatal(err)
	}
	got, err := s.PHPComposerPSR4Mappings(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []PHPComposerPSR4Mapping{mappings[2], mappings[1], mappings[3], mappings[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readback = %#v, want %#v", got, want)
	}
	// A duplicate violates the table uniqueness after DELETE. The whole
	// transaction must roll back, preserving the prior committed mapping set.
	if err := s.ReplacePHPComposerPSR4Mappings(ctx, repo.ID, []PHPComposerPSR4Mapping{mappings[2], mappings[2]}); err == nil {
		t.Fatal("duplicate replacement succeeded")
	}
	got, err = s.PHPComposerPSR4Mappings(ctx, repo.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("readback after failed replacement = %#v, %v; want %#v", got, err, want)
	}
	if err := s.ReplacePHPComposerPSR4Mappings(ctx, repo.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err = s.PHPComposerPSR4Mappings(ctx, repo.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty replacement = %#v, %v", got, err)
	}
}

func TestPHPComposerPSR4FingerprintLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	repo, err := s.UpsertRepo(ctx, filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if value, ok, err := s.PHPComposerPSR4Fingerprint(ctx, repo.ID); err != nil || ok || value != "" {
		t.Fatalf("missing marker = %q, %v, %v", value, ok, err)
	}
	if err := s.SetPHPComposerPSR4Fingerprint(ctx, repo.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPHPComposerPSR4Fingerprint(ctx, repo.ID, "second"); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := s.PHPComposerPSR4Fingerprint(ctx, repo.ID); err != nil || !ok || value != "second" {
		t.Fatalf("stored marker = %q, %v, %v", value, ok, err)
	}
}

func TestPHPComposerPSR4ReconcileBindingsAndReferencesAtomically(t *testing.T) {
	f := newPHPFixture(t)
	src := f.phpFile(t, "src/Service.php")
	legacy := f.phpFile(t, "legacy/Service.php")
	callerFile := f.phpFile(t, "src/Caller.php")
	f.typ(t, src, "App.Service")
	srcRun := f.method(t, src, "App.Service.run", "public", true)
	f.typ(t, legacy, "App.Service")
	legacyRun := f.method(t, legacy, "App.Service.run", "public", true)
	f.typ(t, callerFile, "App.Caller")
	caller := f.method(t, callerFile, "App.Caller.call", "public", false)
	edge := f.call(t, callerFile, srcOf(caller), "Service::run", 1)
	f.reference(t, callerFile, "Service::run", 1)
	srcMapping := PHPComposerPSR4Mapping{ManifestPath: "composer.json", MappingRole: "autoload", NamespacePrefix: "App\\", RootPath: "src", RootOrdinal: 0}
	legacyMapping := srcMapping
	legacyMapping.RootPath = "legacy"
	f.composer(t, srcMapping)
	f.resolveVia(t, "full", nil, nil)
	if err := f.store.ReconcileReferenceIdentities(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if got := f.binding(t, edge); got != "App.Service.run|php_composer_psr4|high" {
		t.Fatal(got)
	}
	f.assertReference(t, 1, srcOf(srcRun), srcOf(caller))

	if err := f.store.ReconcilePHPComposerPSR4(f.ctx, f.repoID, []PHPComposerPSR4Mapping{legacyMapping}); err != nil {
		t.Fatal(err)
	}
	if got := f.binding(t, edge); got != "App.Service.run|php_composer_psr4|high" {
		t.Fatal(got)
	}
	f.assertReference(t, 1, srcOf(legacyRun), srcOf(caller))

	if _, err := f.store.db.ExecContext(f.ctx, `CREATE TRIGGER fail_composer_reference BEFORE UPDATE ON references_tbl BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ReconcilePHPComposerPSR4(f.ctx, f.repoID, []PHPComposerPSR4Mapping{srcMapping}); err == nil {
		t.Fatal("reconcile unexpectedly succeeded")
	}
	if got := f.binding(t, edge); got != "App.Service.run|php_composer_psr4|high" {
		t.Fatalf("rollback binding = %s", got)
	}
	f.assertReference(t, 1, srcOf(legacyRun), srcOf(caller))
	mappings, err := f.store.PHPComposerPSR4Mappings(f.ctx, f.repoID)
	if err != nil || !reflect.DeepEqual(mappings, []PHPComposerPSR4Mapping{legacyMapping}) {
		t.Fatalf("rollback mappings = %#v, %v", mappings, err)
	}
}

func TestMigrationPHPComposerPSR4Mapping(t *testing.T) {
	const migrationVersion = 41
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	if versions := applyMigrationsBelow(t, ctx, dbPath, migrationVersion); len(versions) == 0 {
		t.Fatal("no prior migrations")
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='php_composer_psr4_mapping'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("mapping table = %d, %v", count, err)
	}
	for _, name := range []string{"idx_php_composer_psr4_mapping_repo_role_prefix", "idx_php_composer_psr4_mapping_repo_root"} {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("index %s = %d, %v", name, count, err)
		}
	}
	var userVersion int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil || userVersion != DatabaseFormatUserVersion {
		t.Fatalf("user_version = %d, %v", userVersion, err)
	}
}
