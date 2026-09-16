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
