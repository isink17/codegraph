package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSwiftPMManifestFingerprintLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	repo, err := s.UpsertRepo(ctx, filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if value, ok, err := s.SwiftPMManifestFingerprint(ctx, repo.ID); err != nil || ok || value != "" {
		t.Fatalf("missing marker = %q, %v, %v", value, ok, err)
	}
	if err := s.SetSwiftPMManifestFingerprint(ctx, repo.ID, "empty-set"); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := s.SwiftPMManifestFingerprint(ctx, repo.ID); err != nil || !ok || value != "empty-set" {
		t.Fatalf("stored marker = %q, %v, %v", value, ok, err)
	}
}
