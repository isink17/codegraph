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
	for _, rel := range []string{"Sources/Foo/Service.swift", "Examples/math/main.swift", "Tests/FooTests/test.swift"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
	for _, rel := range []string{"Sources/Core/A.swift", "Vendor/Foo/Sources/Core/A.swift"} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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

func TestSwiftPackageExcludesAreComponentSafe(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"Sources/Core/Excluded.swift", "Sources/Core/ExcludedSupport.swift", "Sources/Core/Dir/Hidden.swift", "Sources/Other/Hidden.swift"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte("import Foundation\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `.target(name: "Core", exclude: ["Excluded.swift", "Dir"])
.target(name: "Other", path: "Sources/Other")`
	if err := os.WriteFile(filepath.Join(root, "Package.swift"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := discoverSwiftModules(root)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"Sources/Core/Excluded.swift":        "",
		"Sources/Core/ExcludedSupport.swift": "Core",
		"Sources/Core/Dir/Hidden.swift":      "",
		"Sources/Other/Hidden.swift":         "Other",
	} {
		if got := m.moduleFor(path); got != want {
			t.Errorf("%s: module %q, want %q", path, got, want)
		}
	}
}

func TestSwiftPackageLiteralBackslashRoot(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("literal backslash is a path separator on Windows")
	}
	root := t.TempDir()
	pkg := filepath.Join(root, "Vendor\\Foo")
	if err := os.MkdirAll(filepath.Join(pkg, "Sources", "Core"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "Package.swift"), []byte(`.target(name: "Core")`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "Sources", "Core", "A.swift"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := discoverSwiftModules(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.packageFor(`Vendor\Foo/Sources/Core/A.swift`); got != `Vendor\Foo` {
		t.Fatalf("package scope %q", got)
	}
}

func TestSwiftPackageExactSourceMembership(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"Sources/Core/Public/A.swift", "Sources/Core/Public/Internal/B.swift", "Sources/Core/Single.swift", "Sources/Core/Private/C.swift", "Sources/Core/SingleSupport.swift"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `.target(name: "Core", sources: ["Public", "Single.swift"], exclude: ["Public/Internal", "Single.swift"])`
	if err := os.WriteFile(filepath.Join(root, "Package.swift"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := discoverSwiftModules(root)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"Sources/Core/Public/A.swift": "Core", "Sources/Core/Public/Internal/B.swift": "", "Sources/Core/Single.swift": "", "Sources/Core/Private/C.swift": "", "Sources/Core/SingleSupport.swift": ""} {
		if got := m.moduleFor(rel); got != want {
			t.Errorf("%s module %q, want %q", rel, got, want)
		}
		pkg := m.packageFor(rel)
		if (want == "") != (pkg == "") {
			t.Errorf("%s package %q with module %q", rel, pkg, want)
		}
	}
}

func TestSwiftPackageOverlappingClaimsFailClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Sources", "Shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Sources", "Shared", "B.swift"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Package.swift"), []byte(`.target(name: "Alpha", path: "Sources")
.target(name: "Beta", path: "Sources/Shared")`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := discoverSwiftModules(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.moduleFor("Sources/Shared/B.swift") != "" || m.packageFor("Sources/Shared/B.swift") != "" {
		t.Fatal("overlapping claims did not fail closed")
	}
}
