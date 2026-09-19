package store

import (
	"runtime"
	"testing"
)

func TestFileSourceStatesUsesLogicalPaths(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	stored := []string{"a.go", "dir/b.go", " leading.go", "trailing.go "}
	if runtime.GOOS != "windows" {
		stored = append(stored, `weird\name.go`)
	}
	for _, path := range stored {
		if _, err := insertTestFile(ctx, s, repoID, path); err != nil {
			t.Fatal(err)
		}
	}

	requested := append([]string{"a.go", "dir/b.go", "a.go"}, stored[2:]...)
	got, err := s.FileSourceStates(ctx, repoID, requested)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(stored) {
		t.Fatalf("FileSourceStates() keys = %v, want %v", got, stored)
	}
	for _, path := range stored {
		if _, ok := got[path]; !ok {
			t.Errorf("FileSourceStates() missing exact key %q", path)
		}
	}
}

func TestFileSourceStatesRejectsInvalidLogicalPaths(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	for _, path := range []string{"../outside.go", "C:foo.go"} {
		if _, err := s.FileSourceStates(testContext(), repoID, []string{path}); err == nil {
			t.Errorf("FileSourceStates(%q) returned no error", path)
		}
	}
}
