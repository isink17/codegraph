package indexer

import (
	"testing"
)

func TestShouldSkipDir(t *testing.T) {
	excludes := []string{"node_modules/**", "dist/**", ".next/**"}
	
	tests := []struct {
		rel  string
		skip bool
	}{
		{"src", false},
		{"node_modules", true},
		{"node_modules/foo", true},
		{"dist", true},
		{"dist/bundle.js", true},
		{".next", true},
		{".next/cache", true},
		{"src/components", false},
	}

	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			if got := shouldSkipDir(tt.rel, excludes); got != tt.skip {
				t.Errorf("shouldSkipDir(%q) = %v, want %v", tt.rel, got, tt.skip)
			}
		})
	}
}
