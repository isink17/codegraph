package indexer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSwiftPackageTargetMapping(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Sources", "FooSupport"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Examples", "math"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `let package = Package(targets: [
 .target(name: "Foo"),
 .executableTarget(name: "math", path: "Examples/math"),
 .testTarget(name: "FooTests")
])`
	if err := os.WriteFile(filepath.Join(root, "Package.swift"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := discoverSwiftModules(root)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"Sources/Foo/Service.swift":        "Foo",
		"Sources/FooSupport/Service.swift": "",
		"Examples/math/main.swift":         "math",
		"Tests/FooTests/test.swift":        "FooTests",
	} {
		if got := m.moduleFor(path); got != want {
			t.Errorf("%s: module %q, want %q", path, got, want)
		}
	}
	if got := m.packageFor("Sources/Foo/Service.swift"); got != "." {
		t.Fatalf("root package %q", got)
	}
}

func TestSwiftPackageDynamicOwnershipFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Package.swift"), []byte(`.target(name: makeName(), path: makePath())`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := discoverSwiftModules(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.moduleFor("Sources/Foo/x.swift"); got != "" {
		t.Fatalf("dynamic target guessed module %q", got)
	}
}

func TestSwiftPackageNestedIdentityAndManifestScope(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"Sources/Core", "Vendor/Foo/Sources/Core"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := []byte(`.target(name: "Core")`)
	if err := os.WriteFile(filepath.Join(root, "Package.swift"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Vendor/Foo/Package.swift"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := discoverSwiftModules(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.moduleFor("Sources/Core/A.swift"); got != "Core" {
		t.Fatalf("root module %q", got)
	}
	if got := m.moduleFor("Vendor/Foo/Sources/Core/A.swift"); got != "Core" {
		t.Fatalf("nested module %q", got)
	}
	if got := m.packageFor("Sources/Core/A.swift"); got != "." {
		t.Fatalf("root package %q", got)
	}
	if got := m.packageFor("Vendor/Foo/Sources/Core/A.swift"); got != "Vendor/Foo" {
		t.Fatalf("nested package %q", got)
	}
	if got := m.moduleFor("Package.swift"); got != "" {
		t.Fatalf("manifest module %q", got)
	}
	if got := m.packageFor("Package.swift"); got != "" {
		t.Fatalf("manifest package %q", got)
	}
}
