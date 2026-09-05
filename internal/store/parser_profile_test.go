package store

import (
	"testing"
)

// Migration 036 adds provenance columns and nothing else. In particular it must
// NOT clear content hashes: doing so would make the first run of a
// lower-capability binary reparse -- and flatten -- a graph produced by a
// call-capable one, which is the outcome the whole slice exists to prevent.
func TestMigration036EstablishesUnknownProvenanceWithoutClearingHashes(t *testing.T) {
	f := newGateFixture(t)

	javaFile := f.file(t, "src/A.java", "java")
	goFile := f.file(t, "internal/app/main.go", "go")
	f.symbol(t, javaFile, "A", "A", "java")
	f.symbol(t, goFile, "main", "main.main", "go")
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE files SET content_sha256 = 'stale', parse_state = 'indexed'`); err != nil {
		t.Fatalf("seed parse state: %v", err)
	}

	// Rewind to a pre-036 schema: the columns are gone and the migration is
	// unapplied, exactly as in a database written by an earlier release.
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_files_repo_language_profile`,
		`ALTER TABLE files DROP COLUMN parser_profile`,
		`ALTER TABLE files DROP COLUMN parser_call_edges`,
		`DELETE FROM schema_migrations WHERE version = 36`,
	} {
		if _, err := f.store.db.ExecContext(f.ctx, stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	if err := f.store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var hashes int
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM files WHERE content_sha256 = 'stale'`).Scan(&hashes); err != nil {
		t.Fatalf("count hashes: %v", err)
	}
	if hashes != 2 {
		t.Fatalf("content hashes surviving migration = %d, want 2", hashes)
	}

	groups, err := f.store.FileParserProfileGroups(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("FileParserProfileGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %#v, want one per language", groups)
	}
	for _, group := range groups {
		if group.Profile != "" || group.CallEdges {
			t.Fatalf("group %#v, want unknown provenance and no capability claim", group)
		}
	}
}

// Provenance follows persisted graph evidence, not the bookkeeping state.
//
// The distinction is load-bearing: TouchFilesMetadataBatch marks a file
// 'skipped' when its mtime moved but its content did not -- after a checkout or
// a rebuild -- while leaving all of its symbols and edges in place. A
// parse_state filter therefore made a whole repository look provenance-free and
// let a destructive downgrade through. A file that declares nothing (over the
// size cap, or behind a languages allowlist) holds no evidence and must not pin
// its language to a stale profile.
func TestFileParserProfileGroupsFollowGraphEvidence(t *testing.T) {
	f := newGateFixture(t)
	indexed := f.file(t, "src/A.java", "java")
	touched := f.file(t, "src/B.java", "java")
	overCap := f.file(t, "src/C.java", "java")
	f.symbol(t, indexed, "A", "A", "java")
	f.symbol(t, touched, "B", "B", "java")

	if _, err := f.store.db.ExecContext(f.ctx, `
		UPDATE files SET parse_state = 'indexed', parser_profile = 'treesitter:java:v1', parser_call_edges = 1
		WHERE path = 'src/A.java'`); err != nil {
		t.Fatalf("seed indexed: %v", err)
	}
	// The mtime-only touch path: still tree-sitter evidence, different state.
	if _, err := f.store.db.ExecContext(f.ctx, `
		UPDATE files SET parse_state = 'skipped', parser_profile = 'treesitter:java:v1', parser_call_edges = 1
		WHERE path = 'src/B.java'`); err != nil {
		t.Fatalf("seed touched: %v", err)
	}
	// Never parsed, declares nothing.
	if _, err := f.store.db.ExecContext(f.ctx, `
		UPDATE files SET parse_state = 'skipped' WHERE path = 'src/C.java'`); err != nil {
		t.Fatalf("seed over-cap: %v", err)
	}
	_ = overCap

	groups, err := f.store.FileParserProfileGroups(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("FileParserProfileGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].Profile != "treesitter:java:v1" || groups[0].Files != 2 || !groups[0].CallEdges {
		t.Fatalf("groups = %#v, want both evidence-bearing files under the tree-sitter profile", groups)
	}
}

// Provenance is a property of the adapter, not of the path separator: a
// Windows-style repo-relative path must not change the recorded profile.
func TestFileParserProfileGroupsIgnorePathShape(t *testing.T) {
	f := newGateFixture(t)
	win := f.file(t, `src\\win\\A.java`, "java")
	posix := f.file(t, "src/posix/B.java", "java")
	f.symbol(t, win, "A", "A", "java")
	f.symbol(t, posix, "B", "B", "java")
	if _, err := f.store.db.ExecContext(f.ctx, `
		UPDATE files SET parse_state = 'indexed', parser_profile = 'treesitter:java:v1', parser_call_edges = 1`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	groups, err := f.store.FileParserProfileGroups(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("FileParserProfileGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].Files != 2 {
		t.Fatalf("groups = %#v, want both paths under one profile", groups)
	}
}

func TestMigrationCeilingAndDatabaseIdentity(t *testing.T) {
	ceiling, err := MigrationCeiling()
	if err != nil {
		t.Fatalf("MigrationCeiling() error = %v", err)
	}
	if ceiling != 36 {
		t.Fatalf("MigrationCeiling() = %d, want 36", ceiling)
	}
	if DatabaseFormatUserVersion != 2 {
		t.Fatalf("DatabaseFormatUserVersion = %d, want 2", DatabaseFormatUserVersion)
	}
}
