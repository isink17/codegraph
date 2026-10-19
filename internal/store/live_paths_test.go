package store

import (
	"context"
	"slices"
	"testing"
)

func TestLiveFilePathsUnderMatchesOnlyThePrefix(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	for _, p := range []string{"pkg/a.go", "pkg/sub/b.go", "pkg.go", "pkg0/c.go", "pkgx/d.go", "pk/e.go", `pkg\f.go`} {
		if _, err := insertTestFile(ctx, s, repoID, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LiveFilePathsUnder(ctx, repoID, "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pkg/a.go", "pkg/sub/b.go"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
