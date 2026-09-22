package indexer

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
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
