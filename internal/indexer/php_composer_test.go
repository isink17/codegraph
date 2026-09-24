package indexer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func TestDiscoverPHPComposerPSR4(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		state    phpComposerPSR4State
		want     []phpComposerPSR4Mapping
	}{
		{"scalar and dev", `{"autoload":{"psr-4":{"App\\":"src/"}},"autoload-dev":{"psr-4":{"App\\":"test/"}}}`, phpComposerPSR4Valid, []phpComposerPSR4Mapping{{"composer.json", "autoload", "App\\", "src", 0}, {"composer.json", "autoload-dev", "App\\", "test", 0}}},
		{"arrays and overlaps", `{"autoload":{"psr-4":{"App\\":["src","generated"],"App\\Special\\":"special"}}}`, phpComposerPSR4Valid, []phpComposerPSR4Mapping{{"composer.json", "autoload", "App\\", "src", 0}, {"composer.json", "autoload", "App\\", "generated", 1}, {"composer.json", "autoload", "App\\Special\\", "special", 0}}},
		{"root and trailing slash", `{"autoload":{"psr-4":{"Root\\":"./","Src\\":"src/"}}}`, phpComposerPSR4Valid, []phpComposerPSR4Mapping{{"composer.json", "autoload", "Root\\", ".", 0}, {"composer.json", "autoload", "Src\\", "src", 0}}},
		{"PHP Parser corpus shape", `{"autoload":{"psr-4":{"PhpParser\\":"lib/PhpParser"}},"autoload-dev":{"psr-4":{"PhpParser\\":"test/PhpParser"}}}`, phpComposerPSR4Valid, []phpComposerPSR4Mapping{{"composer.json", "autoload", "PhpParser\\", "lib/PhpParser", 0}, {"composer.json", "autoload-dev", "PhpParser\\", "test/PhpParser", 0}}},
		{"invalid prefix", `{"autoload":{"psr-4":{"App":"src"}}}`, phpComposerPSR4Disabled, nil},
		{"empty prefix", `{"autoload":{"psr-4":{"":"src"}}}`, phpComposerPSR4Disabled, nil},
		{"wrong root type", `{"autoload":{"psr-4":{"App\\":{}}}}`, phpComposerPSR4Disabled, nil},
		{"malformed", `{`, phpComposerPSR4Disabled, nil},
		{"traversal", `{"autoload":{"psr-4":{"App\\":"../outside"}}}`, phpComposerPSR4Disabled, nil},
		{"absolute", `{"autoload":{"psr-4":{"App\\":"/outside"}}}`, phpComposerPSR4Disabled, nil},
		{"windows absolute", `{"autoload":{"psr-4":{"App\\":"C:\\outside"}}}`, phpComposerPSR4Disabled, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "composer.json"), []byte(tc.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := discoverPHPComposerPSR4(root)
			if err != nil || got.State != tc.state || !reflect.DeepEqual(got.Mappings, tc.want) {
				t.Fatalf("discovery = %#v, %v; want state %q mappings %#v", got, err, tc.state, tc.want)
			}
		})
	}
}

func TestDiscoverPHPComposerPSR4AbsentNestedAndFingerprint(t *testing.T) {
	root := t.TempDir()
	absent, err := discoverPHPComposerPSR4(root)
	if err != nil || absent.State != phpComposerPSR4Absent || len(absent.Mappings) != 0 {
		t.Fatalf("absent = %#v, %v", absent, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "packages", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packages", "foo", "composer.json"), []byte(`{"autoload":{"psr-4":{"Nested\\":"src"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	nested, err := discoverPHPComposerPSR4(root)
	if err != nil || nested.State != phpComposerPSR4Absent || nested.Fingerprint != absent.Fingerprint {
		t.Fatalf("nested-only = %#v, %v", nested, err)
	}
	manifest := filepath.Join(root, "composer.json")
	if err := os.WriteFile(manifest, []byte(`{"autoload":{"psr-4":{"App\\":"src"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	valid, err := discoverPHPComposerPSR4(root)
	if err != nil || valid.State != phpComposerPSR4Valid {
		t.Fatalf("valid = %#v, %v", valid, err)
	}
	if err := os.Chtimes(manifest, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	touched, err := discoverPHPComposerPSR4(root)
	if err != nil || touched.Fingerprint != valid.Fingerprint {
		t.Fatalf("mtime fingerprint = %#v, %v", touched, err)
	}
	if err := os.WriteFile(manifest, []byte(`{"autoload":{"psr-4":{"App\\":"source"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := discoverPHPComposerPSR4(root)
	if err != nil || changed.State != phpComposerPSR4Valid || changed.Fingerprint == valid.Fingerprint {
		t.Fatalf("raw-byte change = %#v, %v", changed, err)
	}
	if err := os.WriteFile(manifest, []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	malformed, err := discoverPHPComposerPSR4(root)
	if err != nil || malformed.State != phpComposerPSR4Disabled || malformed.Fingerprint == absent.Fingerprint || malformed.Fingerprint == valid.Fingerprint || malformed.Fingerprint == changed.Fingerprint {
		t.Fatalf("malformed = %#v, %v", malformed, err)
	}
}

func TestPHPComposerRootPathP23Backslash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a native hierarchy separator")
	}
	if got, err := phpComposerRootPath(t.TempDir(), `literal\name`); err != nil || got != `literal\name` {
		t.Fatalf("literal backslash root = %q, %v", got, err)
	}
}

func TestPHPComposerPSR4FirstIndexStates(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     []store.PHPComposerPSR4Mapping
	}{
		{"valid", `{"autoload":{"psr-4":{"App\\":"src/"}},"autoload-dev":{"psr-4":{"App\\":"test/"}}}`, []store.PHPComposerPSR4Mapping{
			{ManifestPath: "composer.json", MappingRole: "autoload", NamespacePrefix: "App\\", RootPath: "src", RootOrdinal: 0},
			{ManifestPath: "composer.json", MappingRole: "autoload-dev", NamespacePrefix: "App\\", RootPath: "test", RootOrdinal: 0},
		}},
		{"absent", "", nil},
		{"malformed", `{`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, root := context.Background(), t.TempDir()
			writeFile(t, filepath.Join(root, "src", "a.go"), "package src\nfunc A() {}\n")
			if tc.manifest != "" {
				writeFile(t, filepath.Join(root, "composer.json"), tc.manifest)
			}
			s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := New(s, parser.NewRegistry(goparser.New()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatal(err)
			}
			repo, err := s.UpsertRepo(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.PHPComposerPSR4Mappings(ctx, repo.ID)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mappings = %#v, %v; want %#v", got, err, tc.want)
			}
			discovery, err := discoverPHPComposerPSR4(root)
			if err != nil {
				t.Fatal(err)
			}
			fingerprint, known, err := s.PHPComposerPSR4Fingerprint(ctx, repo.ID)
			if err != nil || !known || fingerprint != discovery.Fingerprint {
				t.Fatalf("fingerprint = %q, known=%v, %v; want %q", fingerprint, known, err, discovery.Fingerprint)
			}
		})
	}
}

func TestPHPComposerPSR4Lifecycle(t *testing.T) {
	ctx := context.Background()
	root, dbPath := t.TempDir(), filepath.Join(t.TempDir(), "graph.sqlite")
	writeFile(t, filepath.Join(root, "src", "a.go"), "package src\nfunc A() {}\n")
	writeFile(t, filepath.Join(root, "test", "t.go"), "package test\nfunc T() {}\n")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, phpComposerManifestPath)
	setManifest := func(body string) { writeFile(t, manifest, body) }
	removeManifest := func() {
		if err := os.Remove(manifest); err != nil {
			t.Fatal(err)
		}
	}
	update := func(paths ...string) store.ScanSummary {
		t.Helper()
		summary, err := idx.Update(ctx, Options{RepoRoot: root, Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		return summary
	}
	discovery := func() phpComposerPSR4Discovery {
		t.Helper()
		got, err := discoverPHPComposerPSR4(root)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	assertEvidence := func(want []store.PHPComposerPSR4Mapping) {
		t.Helper()
		got, err := s.PHPComposerPSR4Mappings(ctx, repo.ID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("mappings = %#v, %v; want %#v", got, err, want)
		}
		fingerprint, known, err := s.PHPComposerPSR4Fingerprint(ctx, repo.ID)
		if err != nil || !known || fingerprint != discovery().Fingerprint {
			t.Fatalf("fingerprint = %q, known=%v, %v; want %q", fingerprint, known, err, discovery().Fingerprint)
		}
	}
	mapping := func(role, root string, ordinal int) store.PHPComposerPSR4Mapping {
		return store.PHPComposerPSR4Mapping{ManifestPath: "composer.json", MappingRole: role, NamespacePrefix: "App\\", RootPath: root, RootOrdinal: ordinal}
	}

	// No manifest is a converged state, not a permanently missing marker.
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	assertEvidence(nil)
	beforeSymbols, beforeEdges, err := s.GraphSnapshot(ctx, repo.ID, "", 0)
	if err != nil {
		t.Fatal(err)
	}

	setManifest(`{"autoload":{"psr-4":{"App\\":"src/"}},"autoload-dev":{"psr-4":{"App\\":"test/"}}}`)
	update("src/a.go") // root discovery deliberately ignores path-scoped events.
	assertEvidence([]store.PHPComposerPSR4Mapping{mapping("autoload", "src", 0), mapping("autoload-dev", "test", 0)})

	// Unchanged manifest does not replace evidence; AUTOINCREMENT makes rewrites observable.
	raw, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var beforeID int64
	if err := raw.QueryRowContext(ctx, `SELECT id FROM php_composer_psr4_mapping WHERE repo_id=? AND mapping_role='autoload'`, repo.ID).Scan(&beforeID); err != nil {
		t.Fatal(err)
	}
	update("composer.json")
	var afterID int64
	if err := raw.QueryRowContext(ctx, `SELECT id FROM php_composer_psr4_mapping WHERE repo_id=? AND mapping_role='autoload'`, repo.ID).Scan(&afterID); err != nil || afterID != beforeID {
		t.Fatalf("unchanged Composer rewrote mapping id %d to %d: %v", beforeID, afterID, err)
	}

	// A B1a-era database self-heals even though source bytes did not change.
	if _, err := raw.ExecContext(ctx, `DELETE FROM php_composer_psr4_mapping WHERE repo_id=?`, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, "scope.php_composer_psr4.v2."+strconv.FormatInt(repo.ID, 10)); err != nil {
		t.Fatal(err)
	}
	update()
	assertEvidence([]store.PHPComposerPSR4Mapping{mapping("autoload", "src", 0), mapping("autoload-dev", "test", 0)})

	setManifest(`{"autoload":{"psr-4":{"App\\":"lib/"}}}`)
	update("src/a.go")
	assertEvidence([]store.PHPComposerPSR4Mapping{mapping("autoload", "lib", 0)})

	removeManifest()
	update()
	assertEvidence(nil)

	setManifest(`{`)
	update("composer.json")
	assertEvidence(nil)

	setManifest(`{"autoload-dev":{"psr-4":{"App\\":"test/"}}}`)
	update("composer.json")
	assertEvidence([]store.PHPComposerPSR4Mapping{mapping("autoload-dev", "test", 0)})

	setManifest(`{"autoload":{"psr-4":{"App\\":["src","generated"]}}}`)
	update()
	assertEvidence([]store.PHPComposerPSR4Mapping{mapping("autoload", "src", 0), mapping("autoload", "generated", 1)})
	setManifest(`{"autoload":{"psr-4":{"App\\":["generated","src"]}}}`)
	update()
	finalMappings := []store.PHPComposerPSR4Mapping{mapping("autoload", "generated", 0), mapping("autoload", "src", 1)}
	assertEvidence(finalMappings)

	// Failed scan cannot advance either evidence or marker; retry converges.
	oldFingerprint := discovery().Fingerprint
	setManifest(`{"autoload":{"psr-4":{"App\\":"recovered/"}}}`)
	t.Run("failure before complete scan", func(t *testing.T) {
		withWalkDir(t, failingWalk(0, errInjectedWalk))
		if _, err := idx.Update(ctx, Options{RepoRoot: root}); !errors.Is(err, errInjectedWalk) {
			t.Fatalf("failed update = %v, want %v", err, errInjectedWalk)
		}
	})
	gotFingerprint, known, err := s.PHPComposerPSR4Fingerprint(ctx, repo.ID)
	if err != nil || !known || gotFingerprint != oldFingerprint {
		t.Fatalf("failed update fingerprint = %q, %v, %v; want %q", gotFingerprint, known, err, oldFingerprint)
	}
	gotMappings, err := s.PHPComposerPSR4Mappings(ctx, repo.ID)
	if err != nil || !reflect.DeepEqual(gotMappings, finalMappings) {
		t.Fatalf("failed update mappings = %#v, %v; want %#v", gotMappings, err, finalMappings)
	}
	update()
	assertEvidence([]store.PHPComposerPSR4Mapping{mapping("autoload", "recovered", 0)})
	afterSymbols, afterEdges, err := s.GraphSnapshot(ctx, repo.ID, "", 0)
	if err != nil || !reflect.DeepEqual(afterSymbols, beforeSymbols) || !reflect.DeepEqual(afterEdges, beforeEdges) {
		t.Fatalf("Composer lifecycle changed graph: symbols=%v edges=%v err=%v", !reflect.DeepEqual(afterSymbols, beforeSymbols), !reflect.DeepEqual(afterEdges, beforeEdges), err)
	}

	// Fresh index has same evidence projection as converged incremental state.
	fresh, err := store.Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err := New(fresh, parser.NewRegistry(goparser.New()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	freshRepo, err := fresh.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	freshMappings, err := fresh.PHPComposerPSR4Mappings(ctx, freshRepo.ID)
	if err != nil || !reflect.DeepEqual(freshMappings, []store.PHPComposerPSR4Mapping{mapping("autoload", "recovered", 0)}) {
		t.Fatalf("fresh mappings = %#v, %v", freshMappings, err)
	}
	freshFingerprint, freshKnown, err := fresh.PHPComposerPSR4Fingerprint(ctx, freshRepo.ID)
	if err != nil || !freshKnown || freshFingerprint != discovery().Fingerprint {
		t.Fatalf("fresh fingerprint = %q, known=%v, %v", freshFingerprint, freshKnown, err)
	}
}
