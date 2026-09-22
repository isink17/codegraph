package store

import "testing"

func TestSwiftBuildScopeSameFileVisibility(t *testing.T) {
	s := swiftBuildScopes{files: map[int64]swiftBuildScope{
		1: {packageID: ".", module: "Alpha"},
		2: {packageID: ".", module: "Alpha"},
	}}
	for _, visibility := range []string{"private", "fileprivate", "internal", "package", "public", "open"} {
		if got := s.candidateEligible(1, 1, visibility); got != swiftScopeSameFile {
			t.Errorf("same-file %s = %v, want same-file", visibility, got)
		}
	}
	if got := (swiftBuildScopes{}).candidateEligible(1, 1, "fileprivate"); got != swiftScopeSameFile {
		t.Fatalf("unknown same-file = %v, want same-file", got)
	}
}

func TestSwiftBuildScopeCrossFilePrivateVisibility(t *testing.T) {
	s := swiftBuildScopes{files: map[int64]swiftBuildScope{
		1: {packageID: ".", module: "Alpha"},
		2: {packageID: ".", module: "Alpha"},
	}}
	for _, visibility := range []string{"private", "fileprivate"} {
		if got := s.candidateEligible(1, 2, visibility); got != swiftScopeIneligible {
			t.Errorf("cross-file %s = %v, want ineligible", visibility, got)
		}
	}
}
