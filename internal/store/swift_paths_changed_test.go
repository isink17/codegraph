package store

import "testing"

func TestSwiftPathsChangedUsesCanonicalPaths(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	if _, err := insertTestFileLang(ctx, s, repoID, "Sources/App/Foo.swift", "swift"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestFileLang(ctx, s, repoID, "Sources/App/Bar.kt", "kotlin"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		paths []string
		want  bool
	}{
		{"Swift file", []string{"Sources/App/Foo.swift"}, true},
		{"missing file", []string{"Sources/App/Missing.swift"}, false},
		{"non-Swift file", []string{"Sources/App/Bar.kt"}, false},
		{"duplicate Swift file", []string{"Sources/App/Foo.swift", "Sources/App/Foo.swift"}, true},
		{"mixed files", []string{"Sources/App/Bar.kt", "Sources/App/Foo.swift"}, true},
		{"empty paths", []string{"", "."}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.swiftPathsChanged(ctx, repoID, tc.paths)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("swiftPathsChanged(%v) = %v, want %v", tc.paths, got, tc.want)
			}
		})
	}
}

// TestSwiftPathsChangedKeepsDistinctLogicalIdentities pins the Swift changed-path
// probe to raw logical identities. `Sources/App/x/y.swift` and
// `Sources/App/x\y.swift` are two stored files under P23; the former
// CanonicalRelPath call folded the backslash spelling onto the slash one on
// Windows, so a changed Swift file whose name holds a backslash looked like the
// unrelated Kotlin sibling and reported no Swift change. On POSIX the old code
// also passes; a Windows host is where it reverse-fails.
func TestSwiftPathsChangedKeepsDistinctLogicalIdentities(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	if _, err := insertTestFileLang(ctx, s, repoID, "Sources/App/x/y.swift", "kotlin"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestFileLang(ctx, s, repoID, `Sources/App/x\y.swift`, "swift"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		paths []string
		want  bool
	}{
		{"backslash identity is the swift file", []string{`Sources/App/x\y.swift`}, true},
		{"slash sibling is not the swift file", []string{"Sources/App/x/y.swift"}, false},
		{"exact duplicates deduped", []string{`Sources/App/x\y.swift`, `Sources/App/x\y.swift`}, true},
		{"empty entries ignored", []string{"", "Sources/App/x/y.swift", ""}, false},
	} {
		got, err := s.swiftPathsChanged(ctx, repoID, tc.paths)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: swiftPathsChanged(%q) = %v, want %v", tc.name, tc.paths, got, tc.want)
		}
	}
}
