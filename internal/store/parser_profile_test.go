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
	if ceiling != 37 {
		t.Fatalf("MigrationCeiling() = %d, want 37", ceiling)
	}
	if DatabaseFormatUserVersion != 2 {
		t.Fatalf("DatabaseFormatUserVersion = %d, want 2", DatabaseFormatUserVersion)
	}
}

// Parser provenance protects every persisted semantic fact a parser produced,
// not only symbols. A source file can declare zero symbols and still own graph
// evidence a different adapter would write differently -- a TypeScript
// `export * from "./api"`, an `import "./bootstrap"`, a Python
// `from pkg import plugin` -- and reparsing such a file under another profile
// silently rewrites that evidence. Each case below is one parser-owned table
// standing alone as a zero-symbol file's only evidence.
func TestFileParserProfileGroupsCountEvidenceWithoutSymbols(t *testing.T) {
	// Each seed inserts exactly one row keyed to the evidence file, and nothing
	// else. `?1` is the repo, `?2` the evidence file, `?3` a symbol declared in
	// an unrelated Go file -- edges.src_symbol_id is NOT NULL, so the only way
	// to give a zero-symbol file an edge is to attribute it to a symbol that
	// lives elsewhere.
	cases := []struct {
		table string
		seed  string
	}{
		{"symbols", `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, stable_key, start_line, start_col, end_line, end_col)
			VALUES(?1, ?2, 'typescript', 'function', 'f', 'f', 'ts:f', 1, 0, 1, 0)`},
		{"references_tbl", `INSERT INTO references_tbl(repo_id, file_id, ref_kind, name, start_line, start_col, end_line, end_col)
			VALUES(?1, ?2, 'call', 'callee', 1, 0, 1, 6)`},
		{"edges", `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, file_id, line)
			VALUES(?1, ?3, 'callee', 'calls', ?2, 1)`},
		{"file_imports", `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES(?1, ?2, './bootstrap')`},
		{"file_scope_evidence", `INSERT INTO file_scope_evidence(repo_id, file_id, language, package_name, module_path)
			VALUES(?1, ?2, 'typescript', '', '')`},
		{"scope_import_evidence", `INSERT INTO scope_import_evidence(repo_id, file_id, language, source_specifier, import_kind, is_reexport)
			VALUES(?1, ?2, 'typescript', './api', 'named', 1)`},
		{"scope_module_candidate_evidence", `INSERT INTO scope_module_candidate_evidence(repo_id, source_file_id, source_specifier, candidate_path)
			VALUES(?1, ?2, './api', 'src/api.ts')`},
		{"rust_module_evidence", `INSERT INTO rust_module_evidence(repo_id, file_id, owner_module, module_name)
			VALUES(?1, ?2, 'crate', 'sub')`},
		{"go_local_binding_evidence", `INSERT INTO go_local_binding_evidence(repo_id, file_id, name, scope_start_line, scope_end_line)
			VALUES(?1, ?2, 'x', 1, 2)`},
		{"test_links", `INSERT INTO test_links(repo_id, test_file_id, reason, score) VALUES(?1, ?2, 'name', 1.0)`},
	}
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			f := newGateFixture(t)
			evidence := f.file(t, "src/evidence.ts", "typescript")
			donorFile := f.file(t, "internal/app/donor.go", "go")
			donor := f.symbol(t, donorFile, "Donor", "app.Donor", "go")
			if _, err := f.store.db.ExecContext(f.ctx, `
				UPDATE files SET parser_profile = 'treesitter:typescript:v1', parser_call_edges = 1
				WHERE id = ?`, evidence); err != nil {
				t.Fatalf("seed provenance: %v", err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, tc.seed, f.repoID, evidence, donor); err != nil {
				t.Fatalf("seed %s: %v", tc.table, err)
			}

			var symbols int
			if err := f.store.db.QueryRowContext(f.ctx,
				`SELECT COUNT(*) FROM symbols WHERE file_id = ?`, evidence).Scan(&symbols); err != nil {
				t.Fatalf("count symbols: %v", err)
			}
			if tc.table != "symbols" && symbols != 0 {
				t.Fatalf("fixture declares %d symbols, want a zero-symbol file", symbols)
			}

			groups, err := f.store.FileParserProfileGroups(f.ctx, f.repoID)
			if err != nil {
				t.Fatalf("FileParserProfileGroups: %v", err)
			}
			var found bool
			for _, group := range groups {
				if group.Language != "typescript" {
					continue
				}
				found = true
				if group.Profile != "treesitter:typescript:v1" || group.Files != 1 || !group.CallEdges {
					t.Fatalf("typescript group = %#v, want the %s-only file under its stamped profile", group, tc.table)
				}
			}
			if !found {
				t.Fatalf("groups = %#v, want the zero-symbol %s file to pin its profile", groups, tc.table)
			}
		})
	}
}

// The other half of the contract: do not overcorrect. A genuinely empty or
// comments-only source file owns no parser evidence, and the derived caches --
// tokenizations of symbols and of file text, plus the embedding pass output --
// are not evidence either. Neither may pin a language to a stale profile
// forever, because no reparse could change a fact that was never persisted.
func TestFileParserProfileGroupsIgnoreEvidencelessAndDerivedRows(t *testing.T) {
	f := newGateFixture(t)
	f.file(t, "src/empty.ts", "typescript")
	derived := f.file(t, "src/comments.ts", "typescript")
	// symbol_embeddings rows are keyed to a real symbol; this one belongs to an
	// unrelated Go file, so the TypeScript file still declares nothing itself.
	donorFile := f.file(t, "internal/app/donor.go", "go")
	donor := f.symbol(t, donorFile, "Donor", "app.Donor", "go")
	if _, err := f.store.db.ExecContext(f.ctx, `
		UPDATE files SET parse_state = 'indexed', parser_profile = 'treesitter:typescript:v1', parser_call_edges = 1
		WHERE language = 'typescript'`); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO file_tokens(file_id, token, weight) VALUES(?1, 'comment', 1.0)`,
		`INSERT INTO symbol_embeddings(symbol_id, file_id, repo_id, embedding, dimensions, updated_at)
			VALUES(?3, ?1, ?2, x'00', 1, '')`,
	} {
		if _, err := f.store.db.ExecContext(f.ctx, stmt, derived, f.repoID, donor); err != nil {
			t.Fatalf("seed derived (%s): %v", stmt, err)
		}
	}

	groups, err := f.store.FileParserProfileGroups(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("FileParserProfileGroups: %v", err)
	}
	for _, group := range groups {
		if group.Language == "typescript" {
			t.Fatalf("groups = %#v, want no provenance from evidenceless or derived-only files", groups)
		}
	}
}
