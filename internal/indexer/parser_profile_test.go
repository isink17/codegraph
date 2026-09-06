package indexer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

// profileAdapter is a deterministic stand-in for a production adapter: it
// declares a parser profile and either does or does not emit a call edge,
// which is the only capability difference the safety rules turn on.
type profileAdapter struct {
	language string
	ext      string
	profile  parser.Profile
	// noProfile drops the ProfileProvider implementation's answer, standing in
	// for an adapter that declares nothing.
	noProfile bool
	// emptyFn parses every file to an empty ParsedFile, standing in for a
	// source file that is blank or holds nothing but comments.
	emptyFn bool
}

func (a *profileAdapter) Language() string { return a.language }

func (a *profileAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), a.ext)
}

func (a *profileAdapter) Extensions() []string { return []string{a.ext} }

func (a *profileAdapter) Profile() parser.Profile {
	if a.noProfile {
		return parser.Profile{}
	}
	return a.profile
}

func (a *profileAdapter) Parse(_ context.Context, path string, content []byte) (graph.ParsedFile, error) {
	if strings.Contains(string(content), "PARSE_FAIL") {
		return graph.ParsedFile{}, errors.New("synthetic parse failure")
	}
	// The zero-symbol shapes real adapters produce. `import "./bootstrap"`
	// yields a raw import and a side-effect scope import; `export * from
	// "./api"` yields only a namespace re-export -- not even a raw import --
	// and neither declares a symbol. Verified against the production
	// TypeScript and Python adapters, which parse exactly this way.
	if strings.Contains(string(content), "IMPORT_ONLY") {
		return graph.ParsedFile{
			Language: a.language,
			Imports:  []string{"./bootstrap"},
			Scope: graph.ScopeEvidence{Imports: []graph.ScopeImport{{
				SourceSpecifier: "./bootstrap",
				Kind:            graph.ScopeImportSideEffect,
			}}},
		}, nil
	}
	if strings.Contains(string(content), "REEXPORT_ONLY") {
		return graph.ParsedFile{
			Language: a.language,
			Scope: graph.ScopeEvidence{Imports: []graph.ScopeImport{{
				SourceSpecifier: "./api",
				Kind:            graph.ScopeImportNamespace,
				Wildcard:        true,
				ReExport:        true,
				NamespaceExport: true,
			}}},
		}, nil
	}
	if a.emptyFn {
		return graph.ParsedFile{Language: a.language}, nil
	}
	base := strings.TrimSuffix(filepath.Base(path), a.ext)
	parsed := graph.ParsedFile{
		Language: a.language,
		Symbols: []graph.Symbol{{
			Language:      a.language,
			Kind:          "function",
			Name:          base,
			QualifiedName: base,
			StableKey:     a.language + ":" + base,
			// Edge sources are attributed by line, so the declaration has to
			// span the line the call sits on.
			Range: graph.Position{StartLine: 1, EndLine: 10},
		}},
	}
	if a.profile.EmitsCallEdges {
		parsed.Edges = []graph.Edge{{
			SrcSymbolID: 0,
			DstName:     "callee",
			Kind:        "calls",
			Line:        2,
		}}
	}
	return parsed, nil
}

func callCapable(language, ext, id string) *profileAdapter {
	return &profileAdapter{language: language, ext: ext, profile: parser.Profile{ID: id, EmitsCallEdges: true}}
}

func symbolsOnly(language, ext, id string) *profileAdapter {
	return &profileAdapter{language: language, ext: ext, profile: parser.Profile{ID: id, EmitsCallEdges: false}}
}

// profileStore keeps the raw database path next to the store so tests can read
// columns the Store API deliberately does not expose.
type profileStore struct {
	*store.Store
	path string
}

func newProfileStore(t *testing.T) *profileStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return &profileStore{Store: s, path: path}
}

// raw opens a second read-only handle on the same file. The scans under test
// have all committed by the time it is used.
func (p *profileStore) raw(t *testing.T) *sql.DB {
	t.Helper()
	dsn, err := store.BuildSQLiteDSN(p.path, store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatalf("BuildSQLiteDSN() error = %v", err)
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func writeProfileFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func profilesInDB(t *testing.T, s *profileStore, repoID int64) []store.FileParserProfileGroup {
	t.Helper()
	groups, err := s.FileParserProfileGroups(context.Background(), repoID)
	if err != nil {
		t.Fatalf("FileParserProfileGroups() error = %v", err)
	}
	return groups
}

func repoID(t *testing.T, s *profileStore, root string) int64 {
	t.Helper()
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	return repo.ID
}

func countCallEdges(t *testing.T, s *profileStore, root, language string) int {
	t.Helper()
	id := repoID(t, s, root)
	rows, err := s.raw(t).QueryContext(context.Background(), `
		SELECT COUNT(*) FROM edges e JOIN files f ON f.id = e.file_id
		WHERE e.repo_id = ? AND f.language = ? AND e.edge_kind = 'calls'
	`, id, language)
	if err != nil {
		t.Fatalf("query edges error = %v", err)
	}
	defer rows.Close()
	count := 0
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			t.Fatalf("scan error = %v", err)
		}
	}
	return count
}

// --- planParserProfiles unit cases -----------------------------------------

func TestPlanParserProfilesCases(t *testing.T) {
	tsJava := parser.Profile{ID: "treesitter:java:v1", EmitsCallEdges: true}
	heurJava := parser.Profile{ID: "heuristic:java:v1", EmitsCallEdges: false}
	all := func(string) bool { return true }

	t.Run("same profile is a no-op", func(t *testing.T) {
		plan, err := planParserProfiles(
			[]store.FileParserProfileGroup{{Language: "java", Profile: tsJava.ID, CallEdges: true, Files: 3}},
			map[string]parser.Profile{"java": tsJava}, all, false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(plan.reparseLanguages) != 0 {
			t.Fatalf("reparse = %v, want none", plan.languages())
		}
	})

	t.Run("upgrade to call-capable reparses", func(t *testing.T) {
		plan, err := planParserProfiles(
			[]store.FileParserProfileGroup{{Language: "java", Profile: heurJava.ID, Files: 3}},
			map[string]parser.Profile{"java": tsJava}, all, false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got := plan.languages(); len(got) != 1 || got[0] != "java" {
			t.Fatalf("reparse = %v, want [java]", got)
		}
	})

	t.Run("downgrade from call-capable refuses", func(t *testing.T) {
		_, err := planParserProfiles(
			[]store.FileParserProfileGroup{{Language: "java", Profile: tsJava.ID, CallEdges: true, Files: 3}},
			map[string]parser.Profile{"java": heurJava}, all, false)
		if !errors.Is(err, ErrParserDowngradeRefused) {
			t.Fatalf("err = %v, want ErrParserDowngradeRefused", err)
		}
		var typed *ParserProfileError
		if !errors.As(err, &typed) || typed.Language != "java" || typed.Stored != tsJava.ID {
			t.Fatalf("typed error = %#v", typed)
		}
	})

	t.Run("unknown legacy converges under call-capable parser", func(t *testing.T) {
		plan, err := planParserProfiles(
			[]store.FileParserProfileGroup{{Language: "java", Profile: "", Files: 3}},
			map[string]parser.Profile{"java": tsJava}, all, false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got := plan.languages(); len(got) != 1 || got[0] != "java" {
			t.Fatalf("reparse = %v, want [java]", got)
		}
	})

	t.Run("unknown legacy fails closed under call-less parser", func(t *testing.T) {
		_, err := planParserProfiles(
			[]store.FileParserProfileGroup{{Language: "java", Profile: "", Files: 3}},
			map[string]parser.Profile{"java": heurJava}, all, false)
		if !errors.Is(err, ErrParserDowngradeRefused) {
			t.Fatalf("err = %v, want ErrParserDowngradeRefused", err)
		}
		var typed *ParserProfileError
		if !errors.As(err, &typed) || typed.Stored != "" {
			t.Fatalf("typed error = %#v, want unknown stored profile", typed)
		}
	})

	t.Run("call-less to call-less is only an upgrade, not a downgrade", func(t *testing.T) {
		other := parser.Profile{ID: "heuristic:java:v2", EmitsCallEdges: false}
		plan, err := planParserProfiles(
			[]store.FileParserProfileGroup{{Language: "java", Profile: heurJava.ID, Files: 1}},
			map[string]parser.Profile{"java": other}, all, false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got := plan.languages(); len(got) != 1 || got[0] != "java" {
			t.Fatalf("reparse = %v, want [java]", got)
		}
	})

	t.Run("path-scoped transition refuses", func(t *testing.T) {
		_, err := planParserProfiles(
			[]store.FileParserProfileGroup{{Language: "java", Profile: heurJava.ID, Files: 1}},
			map[string]parser.Profile{"java": tsJava}, all, true)
		if !errors.Is(err, ErrParserProfileTransitionRequired) {
			t.Fatalf("err = %v, want ErrParserProfileTransitionRequired", err)
		}
	})

	t.Run("mixed same-language state is detected", func(t *testing.T) {
		groups := []store.FileParserProfileGroup{
			{Language: "java", Profile: tsJava.ID, CallEdges: true, Files: 2},
			{Language: "java", Profile: heurJava.ID, Files: 1},
		}
		_, err := planParserProfiles(groups, map[string]parser.Profile{"java": tsJava}, all, true)
		if !errors.Is(err, ErrParserProfileTransitionRequired) {
			t.Fatalf("path-scoped err = %v, want transition required", err)
		}
		plan, err := planParserProfiles(groups, map[string]parser.Profile{"java": tsJava}, all, false)
		if err != nil {
			t.Fatalf("full err = %v", err)
		}
		if got := plan.languages(); len(got) != 1 || got[0] != "java" {
			t.Fatalf("reparse = %v, want [java]", got)
		}
	})

	t.Run("mixed downgrade prefers the proven call-capable predecessor", func(t *testing.T) {
		groups := []store.FileParserProfileGroup{
			{Language: "java", Profile: "", Files: 1},
			{Language: "java", Profile: tsJava.ID, CallEdges: true, Files: 1},
		}
		_, err := planParserProfiles(groups, map[string]parser.Profile{"java": heurJava}, all, false)
		var typed *ParserProfileError
		if !errors.As(err, &typed) || typed.Stored != tsJava.ID {
			t.Fatalf("typed = %#v, want stored=%s", typed, tsJava.ID)
		}
	})

	t.Run("invalidation is language scoped", func(t *testing.T) {
		goProfile := parser.Profile{ID: "go-ast:go:v1", EmitsCallEdges: true}
		groups := []store.FileParserProfileGroup{
			{Language: "java", Profile: heurJava.ID, Files: 1},
			{Language: "go", Profile: goProfile.ID, CallEdges: true, Files: 5},
		}
		plan, err := planParserProfiles(groups,
			map[string]parser.Profile{"java": tsJava, "go": goProfile}, all, false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got := plan.languages(); len(got) != 1 || got[0] != "java" {
			t.Fatalf("reparse = %v, want [java] only", got)
		}
	})

	t.Run("unaffected language does not block a path-scoped scan", func(t *testing.T) {
		goProfile := parser.Profile{ID: "go-ast:go:v1", EmitsCallEdges: true}
		groups := []store.FileParserProfileGroup{
			{Language: "java", Profile: heurJava.ID, Files: 1},
			{Language: "go", Profile: goProfile.ID, CallEdges: true, Files: 5},
		}
		onlyGo := func(language string) bool { return language == "go" }
		if _, err := planParserProfiles(groups,
			map[string]parser.Profile{"java": tsJava, "go": goProfile}, onlyGo, true); err != nil {
			t.Fatalf("err = %v, want nil for a Go-only scan", err)
		}
	})

	t.Run("language without a current adapter is ignored", func(t *testing.T) {
		plan, err := planParserProfiles(
			[]store.FileParserProfileGroup{{Language: "java", Profile: tsJava.ID, CallEdges: true, Files: 2}},
			map[string]parser.Profile{}, all, false)
		if err != nil || len(plan.reparseLanguages) != 0 {
			t.Fatalf("err = %v, reparse = %v", err, plan.languages())
		}
	})
}

// --- end-to-end scan behaviour ---------------------------------------------

func TestParserProfileUpgradeReparsesUnchangedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "A.java"), "class A {}\n")
	writeProfileFile(t, filepath.Join(root, "B.java"), "class B {}\n")
	s := newProfileStore(t)

	degraded := New(s.Store, parser.NewRegistry(symbolsOnly("java", ".java", "heuristic:java:v1")), nil)
	if _, err := degraded.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if got := countCallEdges(t, s, root, "java"); got != 0 {
		t.Fatalf("degraded call edges = %d, want 0", got)
	}

	upgraded := New(s.Store, parser.NewRegistry(callCapable("java", ".java", "treesitter:java:v1")), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if summary.FilesChanged != 2 {
		t.Fatalf("FilesChanged = %d, want 2 (unchanged files must be reparsed)", summary.FilesChanged)
	}
	if got := strings.Join(summary.ParserProfileLanguages, ","); got != "java" {
		t.Fatalf("ParserProfileLanguages = %q, want \"java\"", got)
	}
	if got := countCallEdges(t, s, root, "java"); got != 2 {
		t.Fatalf("upgraded call edges = %d, want 2", got)
	}
	groups := profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != "treesitter:java:v1" || !groups[0].CallEdges {
		t.Fatalf("groups = %#v, want a single call-capable tree-sitter profile", groups)
	}

	// Second run with the same binary is a true no-op: no forced reparse.
	second, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("second Update() error = %v", err)
	}
	if second.FilesChanged != 0 || second.FilesIndexed != 0 {
		t.Fatalf("second update changed=%d indexed=%d, want 0/0", second.FilesChanged, second.FilesIndexed)
	}
	if len(second.ParserProfileLanguages) != 0 {
		t.Fatalf("second update reconverged %v, want none", second.ParserProfileLanguages)
	}
}

func TestParserProfileDowngradeRefusedWithZeroMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "A.java"), "class A {}\n")
	writeProfileFile(t, filepath.Join(root, "B.java"), "class B {}\n")
	s := newProfileStore(t)

	capable := New(s.Store, parser.NewRegistry(callCapable("java", ".java", "treesitter:java:v1")), nil)
	if _, err := capable.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	before := graphSnapshot(t, s)

	degraded := New(s.Store, parser.NewRegistry(symbolsOnly("java", ".java", "heuristic:java:v1")), nil)
	for _, run := range []struct {
		name string
		fn   func() (store.ScanSummary, error)
	}{
		{"update", func() (store.ScanSummary, error) { return degraded.Update(ctx, Options{RepoRoot: root}) }},
		{"index", func() (store.ScanSummary, error) { return degraded.Index(ctx, Options{RepoRoot: root}) }},
		{"forced", func() (store.ScanSummary, error) {
			return degraded.Update(ctx, Options{RepoRoot: root, Force: true})
		}},
		{"path-scoped", func() (store.ScanSummary, error) {
			return degraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"A.java"}})
		}},
	} {
		if _, err := run.fn(); !errors.Is(err, ErrParserDowngradeRefused) {
			t.Fatalf("%s: err = %v, want ErrParserDowngradeRefused", run.name, err)
		}
	}

	after := graphSnapshot(t, s)
	if before != after {
		t.Fatalf("graph mutated by a refused downgrade:\nbefore %#v\nafter  %#v", before, after)
	}
}

func TestParserProfilePathScopedTransitionRefused(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "A.java"), "class A {}\n")
	writeProfileFile(t, filepath.Join(root, "B.java"), "class B {}\n")
	writeProfileFile(t, filepath.Join(root, "main.go"), "package main\n")
	s := newProfileStore(t)

	goAdapter := callCapable("go", ".go", "go-ast:go:v1")
	old := New(s.Store, parser.NewRegistry(symbolsOnly("java", ".java", "heuristic:java:v1"), goAdapter), nil)
	if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	upgraded := New(s.Store, parser.NewRegistry(callCapable("java", ".java", "treesitter:java:v1"), goAdapter), nil)
	before := graphSnapshot(t, s)
	_, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"A.java"}})
	if !errors.Is(err, ErrParserProfileTransitionRequired) {
		t.Fatalf("err = %v, want ErrParserProfileTransitionRequired", err)
	}
	if after := graphSnapshot(t, s); before != after {
		t.Fatalf("path-scoped refusal mutated the graph:\nbefore %#v\nafter  %#v", before, after)
	}

	// A Go-only path-scoped scan is unaffected by the stale Java profile.
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"main.go"}}); err != nil {
		t.Fatalf("Go-only path scan err = %v, want nil", err)
	}

	// One full run converges the language, after which path scans resume.
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("full Update() error = %v", err)
	}
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"A.java"}}); err != nil {
		t.Fatalf("post-convergence path scan err = %v, want nil", err)
	}
}

func TestParserProfileLanguageScopedInvalidation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "A.java"), "class A {}\n")
	writeProfileFile(t, filepath.Join(root, "main.go"), "package main\n")
	s := newProfileStore(t)

	goAdapter := callCapable("go", ".go", "go-ast:go:v1")
	old := New(s.Store, parser.NewRegistry(symbolsOnly("java", ".java", "heuristic:java:v1"), goAdapter), nil)
	if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	upgraded := New(s.Store, parser.NewRegistry(callCapable("java", ".java", "treesitter:java:v1"), goAdapter), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if summary.FilesChanged != 1 {
		t.Fatalf("FilesChanged = %d, want 1 (only the Java file)", summary.FilesChanged)
	}
	if got := strings.Join(summary.ParserProfileLanguages, ","); got != "java" {
		t.Fatalf("ParserProfileLanguages = %q, want \"java\"", got)
	}
}

// A parse failure mid-convergence must leave provenance TRUTHFUL: the file that
// could not be reparsed still holds the old parser's symbols, so the language is
// genuinely mixed, and the next path-scoped update has to say so rather than
// convert one more file. See parser.Profile and store.FileParserProfileGroups.
func TestParserProfileParseFailureKeepsProvenanceTruthful(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "A.java"), "class A {}\n")
	writeProfileFile(t, filepath.Join(root, "B.java"), "class B {}\n")
	if err := os.MkdirAll(filepath.Join(root, ".codegraph"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeProfileFile(t, filepath.Join(root, ".codegraph", "config.json"), `{"parse_error_policy":"best_effort"}`)
	s := newProfileStore(t)

	old := New(s.Store, parser.NewRegistry(symbolsOnly("java", ".java", "heuristic:java:v1")), nil)
	if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	writeProfileFile(t, filepath.Join(root, "B.java"), "PARSE_FAIL\n")
	upgraded := New(s.Store, parser.NewRegistry(callCapable("java", ".java", "treesitter:java:v1")), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if summary.ParseErrors != 1 {
		t.Fatalf("ParseErrors = %d, want 1", summary.ParseErrors)
	}

	// The failed file was NOT stamped with the parser that never produced its
	// rows.
	var stamped string
	if err := s.raw(t).QueryRowContext(ctx,
		`SELECT parser_profile FROM files WHERE path = 'B.java'`).Scan(&stamped); err != nil {
		t.Fatalf("scan error = %v", err)
	}
	if stamped != "heuristic:java:v1" {
		t.Fatalf("failed file parser_profile = %q, want the profile that actually wrote its graph", stamped)
	}

	// So the language reads as mixed, and an incremental update refuses.
	groups := profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 2 {
		t.Fatalf("groups = %#v, want both profiles while convergence is incomplete", groups)
	}
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"A.java"}}); !errors.Is(err, ErrParserProfileTransitionRequired) {
		t.Fatalf("path-scoped err = %v, want ErrParserProfileTransitionRequired", err)
	}

	// Fixing the file lets a full update finish convergence.
	writeProfileFile(t, filepath.Join(root, "B.java"), "class B {}\n")
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("converging Update() error = %v", err)
	}
	groups = profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != "treesitter:java:v1" {
		t.Fatalf("groups = %#v, want a single converged profile", groups)
	}
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"A.java"}}); err != nil {
		t.Fatalf("post-convergence path scan err = %v, want nil", err)
	}
}

// The mtime-only touch path leaves a file's graph in place but rewrites its
// bookkeeping state. Provenance must survive that, or one `git checkout` would
// disarm the downgrade guard on a whole repository.
func TestParserProfileSurvivesMtimeOnlyTouch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	javaPath := filepath.Join(root, "A.java")
	writeProfileFile(t, javaPath, "class A {}\n")
	s := newProfileStore(t)

	capable := New(s.Store, parser.NewRegistry(callCapable("java", ".java", "treesitter:java:v1")), nil)
	if _, err := capable.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	// Same bytes, new mtime -- what a checkout or rebuild produces.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(javaPath, future, future); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	if _, err := capable.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("touch Update() error = %v", err)
	}
	groups := profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != "treesitter:java:v1" || !groups[0].CallEdges {
		t.Fatalf("groups after touch = %#v, want the tree-sitter profile intact", groups)
	}

	before := graphSnapshot(t, s)
	degraded := New(s.Store, parser.NewRegistry(symbolsOnly("java", ".java", "heuristic:java:v1")), nil)
	if _, err := degraded.Update(ctx, Options{RepoRoot: root}); !errors.Is(err, ErrParserDowngradeRefused) {
		t.Fatalf("err = %v, want ErrParserDowngradeRefused after a touch-only update", err)
	}
	if after := graphSnapshot(t, s); before != after {
		t.Fatalf("refusal after touch mutated the graph")
	}
}

func TestParserProfileUnknownLegacyProvenance(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T) (*profileStore, string) {
		t.Helper()
		root := t.TempDir()
		writeProfileFile(t, filepath.Join(root, "A.java"), "class A {}\n")
		s := newProfileStore(t)
		// A pre-036 database: rows exist with no recorded provenance.
		noProfile := &profileAdapter{
			language:  "java",
			ext:       ".java",
			profile:   parser.Profile{ID: "treesitter:java:v1", EmitsCallEdges: true},
			noProfile: true,
		}
		legacy := New(s.Store, parser.NewRegistry(noProfile), nil)
		if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
			t.Fatalf("legacy Index() error = %v", err)
		}
		groups := profilesInDB(t, s, repoID(t, s, root))
		if len(groups) != 1 || groups[0].Profile != "" {
			t.Fatalf("legacy groups = %#v, want unknown provenance", groups)
		}
		return s, root
	}

	t.Run("call-capable parser converges", func(t *testing.T) {
		s, root := setup(t)
		upgraded := New(s.Store, parser.NewRegistry(callCapable("java", ".java", "treesitter:java:v1")), nil)
		summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if summary.FilesChanged != 1 {
			t.Fatalf("FilesChanged = %d, want 1", summary.FilesChanged)
		}
		if got := countCallEdges(t, s, root, "java"); got != 1 {
			t.Fatalf("call edges = %d, want 1", got)
		}
	})

	t.Run("call-less parser refuses", func(t *testing.T) {
		s, root := setup(t)
		before := graphSnapshot(t, s)
		degraded := New(s.Store, parser.NewRegistry(symbolsOnly("java", ".java", "heuristic:java:v1")), nil)
		if _, err := degraded.Update(ctx, Options{RepoRoot: root}); !errors.Is(err, ErrParserDowngradeRefused) {
			t.Fatalf("err = %v, want ErrParserDowngradeRefused", err)
		}
		if after := graphSnapshot(t, s); before != after {
			t.Fatalf("refusal mutated the graph")
		}
	})

	t.Run("path-scoped run refuses while provenance is unknown", func(t *testing.T) {
		s, root := setup(t)
		upgraded := New(s.Store, parser.NewRegistry(callCapable("java", ".java", "treesitter:java:v1")), nil)
		if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"A.java"}}); !errors.Is(err, ErrParserProfileTransitionRequired) {
			t.Fatalf("err = %v, want ErrParserProfileTransitionRequired", err)
		}
	})
}

func TestParserProfileFreshDegradedIndexIsAllowedAndHonest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "A.java"), "class A {}\n")
	s := newProfileStore(t)

	degraded := New(s.Store, parser.NewRegistry(symbolsOnly("java", ".java", "heuristic:java:v1")), nil)
	if _, err := degraded.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("fresh degraded Index() error = %v", err)
	}
	groups := profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != "heuristic:java:v1" || groups[0].CallEdges {
		t.Fatalf("groups = %#v, want an honest symbols-only profile", groups)
	}
	langs := degraded.SupportedLanguages()
	if len(langs) != 1 || langs[0].ParserProfile != "heuristic:java:v1" || langs[0].CallEdges {
		t.Fatalf("SupportedLanguages() = %#v", langs)
	}
}

// graphSnapshot is the byte-level evidence a refused scan must not change.
type snapshot struct {
	Files, Symbols, Edges, Refs, Imports, ScopeEvidence, TestLinks, Scans int
	// ScopeImports and ModuleCandidates are the only evidence a zero-symbol
	// re-export-only file has, so a snapshot that omitted them could not prove
	// a refused transition left such a file untouched.
	ScopeImports, ModuleCandidates int
	Provenance                     string
}

func graphSnapshot(t *testing.T, s *profileStore) snapshot {
	t.Helper()
	var out snapshot
	row := s.raw(t).QueryRowContext(context.Background(), `
		SELECT (SELECT COUNT(*) FROM files),
		       (SELECT COUNT(*) FROM symbols),
		       (SELECT COUNT(*) FROM edges),
		       (SELECT COUNT(*) FROM references_tbl),
		       (SELECT COUNT(*) FROM file_imports),
		       (SELECT COUNT(*) FROM file_scope_evidence),
		       (SELECT COUNT(*) FROM test_links),
		       (SELECT COUNT(*) FROM scans),
		       (SELECT COUNT(*) FROM scope_import_evidence),
		       (SELECT COUNT(*) FROM scope_module_candidate_evidence),
		       (SELECT COALESCE(GROUP_CONCAT(path || '=' || parser_profile || ':' || parser_call_edges), '')
		          FROM (SELECT path, parser_profile, parser_call_edges FROM files ORDER BY path))
	`)
	if err := row.Scan(&out.Files, &out.Symbols, &out.Edges, &out.Refs, &out.Imports,
		&out.ScopeEvidence, &out.TestLinks, &out.Scans,
		&out.ScopeImports, &out.ModuleCandidates, &out.Provenance); err != nil {
		t.Fatalf("snapshot error = %v", err)
	}
	return out
}

// A path-scoped flush that only names paths the scan would not parse -- deleted
// files, excluded files -- cannot create a mixed graph, so it must not be
// refused. The watcher drains deletions this way, and refusing them would stall
// it over files it was only going to retire.
func TestParserProfilePathScopedIgnoresUnparsedCandidates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "A.java"), "class A {}\n")
	writeProfileFile(t, filepath.Join(root, "gone.java"), "class Gone {}\n")
	writeProfileFile(t, filepath.Join(root, "main.go"), "package main\n")
	s := newProfileStore(t)

	goAdapter := callCapable("go", ".go", "go-ast:go:v1")
	old := New(s.Store, parser.NewRegistry(symbolsOnly("java", ".java", "heuristic:java:v1"), goAdapter), nil)
	if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	upgraded := New(s.Store, parser.NewRegistry(callCapable("java", ".java", "treesitter:java:v1"), goAdapter), nil)
	if err := os.Remove(filepath.Join(root, "gone.java")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"gone.java"}}); err != nil {
		t.Fatalf("deletion-only flush err = %v, want nil", err)
	}
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"A.java"}, Exclude: []string{"A.java"}}); err != nil {
		t.Fatalf("excluded-path flush err = %v, want nil", err)
	}
	// A path the run WOULD parse still refuses.
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"A.java"}}); !errors.Is(err, ErrParserProfileTransitionRequired) {
		t.Fatalf("err = %v, want ErrParserProfileTransitionRequired", err)
	}
}

// evidenceCounts is the parser-owned footprint of one file, read straight from
// the tables insertParsedFileGraph writes.
type evidenceCounts struct {
	Symbols      int
	Imports      int
	ScopeImports int
}

func evidenceFor(t *testing.T, s *profileStore, root, path string) evidenceCounts {
	t.Helper()
	id := repoID(t, s, root)
	var got evidenceCounts
	if err := s.raw(t).QueryRowContext(context.Background(), `
		SELECT
			(SELECT COUNT(*) FROM symbols WHERE file_id = f.id),
			(SELECT COUNT(*) FROM file_imports WHERE file_id = f.id),
			(SELECT COUNT(*) FROM scope_import_evidence WHERE repo_id = f.repo_id AND file_id = f.id)
		FROM files f WHERE f.repo_id = ? AND f.path = ?
	`, id, path).Scan(&got.Symbols, &got.Imports, &got.ScopeImports); err != nil {
		t.Fatalf("evidence for %s error = %v", path, err)
	}
	return got
}

// A file can declare zero symbols and still own parser evidence a reparse would
// rewrite: `import "./bootstrap"` persists a raw import and a side-effect scope
// import, and `export * from "./api"` persists a namespace re-export and no raw
// import at all. Before P22.29-F1 the provenance population was a bare EXISTS
// over `symbols`, so a repository made only of such files reported NO
// provenance -- and every transition rule downstream of that answer, refusal
// included, silently did not apply to them.
func TestParserProfileEvidenceOnlyFilesPinProvenance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "bootstrap.ts"), "IMPORT_ONLY\n")
	writeProfileFile(t, filepath.Join(root, "reexport.ts"), "REEXPORT_ONLY\n")
	s := newProfileStore(t)

	degraded := New(s.Store, parser.NewRegistry(symbolsOnly("typescript", ".ts", "heuristic:typescript:v1")), nil)
	if _, err := degraded.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	// Both files are genuinely symbol-free and genuinely evidence-bearing.
	if got := evidenceFor(t, s, root, "bootstrap.ts"); got != (evidenceCounts{Imports: 1, ScopeImports: 1}) {
		t.Fatalf("import-only evidence = %#v, want zero symbols with import evidence", got)
	}
	if got := evidenceFor(t, s, root, "reexport.ts"); got != (evidenceCounts{ScopeImports: 1}) {
		t.Fatalf("re-export-only evidence = %#v, want zero symbols and zero raw imports", got)
	}

	groups := profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != "heuristic:typescript:v1" || groups[0].Files != 2 || groups[0].CallEdges {
		t.Fatalf("groups = %#v, want both evidence-only files under the call-less profile", groups)
	}

	upgraded := New(s.Store, parser.NewRegistry(callCapable("typescript", ".ts", "treesitter:typescript:v1")), nil)

	// A path-scoped scan cannot converge a language, so it must refuse rather
	// than rewrite one evidence-only file under the new parser.
	before := graphSnapshot(t, s)
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"bootstrap.ts"}}); !errors.Is(err, ErrParserProfileTransitionRequired) {
		t.Fatalf("path-scoped err = %v, want ErrParserProfileTransitionRequired", err)
	}
	if after := graphSnapshot(t, s); before != after {
		t.Fatalf("path-scoped refusal mutated the graph:\nbefore %#v\nafter  %#v", before, after)
	}

	// A full run converges them and stamps the current profile.
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("full Update() error = %v", err)
	}
	if summary.FilesChanged != 2 {
		t.Fatalf("FilesChanged = %d, want 2 (both evidence-only files reparsed)", summary.FilesChanged)
	}
	groups = profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != "treesitter:typescript:v1" || groups[0].Files != 2 || !groups[0].CallEdges {
		t.Fatalf("groups = %#v, want both files stamped call-capable", groups)
	}

	// And the converged provenance now protects them from the reverse move.
	if _, err := degraded.Update(ctx, Options{RepoRoot: root}); !errors.Is(err, ErrParserDowngradeRefused) {
		t.Fatalf("downgrade err = %v, want ErrParserDowngradeRefused", err)
	}
}

// Legacy rows carry no profile at all, and an evidence-only file is no more
// exempt from that than a symbol-bearing one: UNKNOWN provenance converges
// under a call-capable parser and fails closed under a call-less one.
func TestParserProfileEvidenceOnlyUnknownProvenance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "reexport.ts"), "REEXPORT_ONLY\n")
	s := newProfileStore(t)

	capable := New(s.Store, parser.NewRegistry(callCapable("typescript", ".ts", "treesitter:typescript:v1")), nil)
	if _, err := capable.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	// Rewind to what migration 036 leaves behind for a pre-036 database.
	if _, err := s.raw(t).ExecContext(ctx,
		`UPDATE files SET parser_profile = '', parser_call_edges = 0`); err != nil {
		t.Fatalf("clear provenance: %v", err)
	}

	groups := profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != "" || groups[0].Files != 1 || groups[0].CallEdges {
		t.Fatalf("groups = %#v, want the evidence-only file reported as UNKNOWN, not ignored", groups)
	}

	degraded := New(s.Store, parser.NewRegistry(symbolsOnly("typescript", ".ts", "heuristic:typescript:v1")), nil)
	if _, err := degraded.Update(ctx, Options{RepoRoot: root}); !errors.Is(err, ErrParserDowngradeRefused) {
		t.Fatalf("call-less err = %v, want ErrParserDowngradeRefused on unknown provenance", err)
	}
	if _, err := capable.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("call-capable convergence err = %v, want nil", err)
	}
	groups = profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != "treesitter:typescript:v1" || !groups[0].CallEdges {
		t.Fatalf("groups = %#v, want the evidence-only file stamped call-capable", groups)
	}
}

// A comments-only or empty source file owns no parser evidence at all, so it
// must NOT pin its language forever. This is the boundary the wider predicate
// must not cross: nothing a reparse could change was ever persisted.
func TestParserProfileEvidencelessFilesDoNotPinProvenance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "empty.ts"), "")
	writeProfileFile(t, filepath.Join(root, "comments.ts"), "EMPTY_PARSE\n")
	s := newProfileStore(t)

	empty := &profileAdapter{
		language: "typescript", ext: ".ts",
		profile: parser.Profile{ID: "treesitter:typescript:v1", EmitsCallEdges: true},
		emptyFn: true,
	}
	capable := New(s.Store, parser.NewRegistry(empty), nil)
	if _, err := capable.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if got := evidenceFor(t, s, root, "comments.ts"); got != (evidenceCounts{}) {
		t.Fatalf("comments-only evidence = %#v, want none", got)
	}
	if groups := profilesInDB(t, s, repoID(t, s, root)); len(groups) != 0 {
		t.Fatalf("groups = %#v, want no provenance from files that declare nothing", groups)
	}

	// With nothing pinned, a call-less parser is free to take over.
	degraded := New(s.Store, parser.NewRegistry(symbolsOnly("typescript", ".ts", "heuristic:typescript:v1")), nil)
	if _, err := degraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Update() error = %v, want nil for an evidence-free repository", err)
	}
}

// Same rule, on a file whose last-good graph is evidence only. A failed reparse
// leaves the previous parser's imports and re-exports in place, so the file
// still belongs to the profile that wrote them and the language is still mixed
// -- provenance follows the surviving evidence, never `parse_state`.
func TestParserProfileParseFailureKeepsEvidenceOnlyProvenance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "bootstrap.ts"), "IMPORT_ONLY\n")
	writeProfileFile(t, filepath.Join(root, "reexport.ts"), "REEXPORT_ONLY\n")
	if err := os.MkdirAll(filepath.Join(root, ".codegraph"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeProfileFile(t, filepath.Join(root, ".codegraph", "config.json"), `{"parse_error_policy":"best_effort"}`)
	s := newProfileStore(t)

	old := New(s.Store, parser.NewRegistry(symbolsOnly("typescript", ".ts", "heuristic:typescript:v1")), nil)
	if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	writeProfileFile(t, filepath.Join(root, "reexport.ts"), "PARSE_FAIL\n")
	upgraded := New(s.Store, parser.NewRegistry(callCapable("typescript", ".ts", "treesitter:typescript:v1")), nil)
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	// The last-good re-export row survived, and still belongs to the parser
	// that wrote it.
	if got := evidenceFor(t, s, root, "reexport.ts"); got != (evidenceCounts{ScopeImports: 1}) {
		t.Fatalf("last-good evidence = %#v, want the previous parser's re-export intact", got)
	}
	var stamped string
	if err := s.raw(t).QueryRowContext(ctx,
		`SELECT parser_profile FROM files WHERE path = 'reexport.ts'`).Scan(&stamped); err != nil {
		t.Fatalf("scan error = %v", err)
	}
	if stamped != "heuristic:typescript:v1" {
		t.Fatalf("failed file parser_profile = %q, want the profile that actually wrote its graph", stamped)
	}

	groups := profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 2 {
		t.Fatalf("groups = %#v, want both profiles while convergence is incomplete", groups)
	}
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"bootstrap.ts"}}); !errors.Is(err, ErrParserProfileTransitionRequired) {
		t.Fatalf("path-scoped err = %v, want ErrParserProfileTransitionRequired", err)
	}

	writeProfileFile(t, filepath.Join(root, "reexport.ts"), "REEXPORT_ONLY\n")
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("converging Update() error = %v", err)
	}
	groups = profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != "treesitter:typescript:v1" {
		t.Fatalf("groups = %#v, want a single converged profile", groups)
	}
}
