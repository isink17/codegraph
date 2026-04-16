package indexer

import (
	"testing"
)

func TestShouldSkipDirRegression(t *testing.T) {
	// Hardcoded skips: .git, node_modules, .next, .nuxt, .svelte-kit, .turbo, .pnpm-store, .yarn, .parcel-cache
	// These directories are skipped by filepath.WalkDir early and cannot be bypassed
	// by adding !pattern to .codegraphignore.
	excludes := []string{"node_modules/**", "dist/**", ".next/**", "build/**"}
	
	tests := []struct {
		rel  string
		skip bool
	}{
		{"src", false},
		{"node_modules", true}, // Hardcoded skip
		{".next", true},        // Hardcoded skip
		{"dist", true},         // Config-default exclude
	}

	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			got := shouldSkipDir(tt.rel, excludes)
			if got != tt.skip {
				t.Errorf("shouldSkipDir(%q) = %v, want %v", tt.rel, got, tt.skip)
			}
		})
	}
}
