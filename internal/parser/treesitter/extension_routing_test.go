//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestReferenceExtensionRouting(t *testing.T) {
	for _, tc := range []struct {
		name, path, wantLanguage, source, wantSymbol string
		adapter                                      interface {
			Supports(string) bool
			Extensions() []string
			Parse(context.Context, string, []byte) (graph.ParsedFile, error)
		}
	}{
		{"JavaScript CommonJS", "file.cjs", "typescript", "function run() {}", "run", NewTypeScript()},
		{"TypeScript ESM", "file.mts", "typescript", "function run() {}", "run", NewTypeScript()},
		{"TypeScript CommonJS", "file.cts", "typescript", "function run() {}", "run", NewTypeScript()},
		{"Python Windows", "file.pyw", "python", "def run():\n    pass\n", "run", NewPython()},
		{"Rake", "Rakefile.rake", "ruby", "def run\nend\n", "run", NewRuby()},
		{"Drupal module", "sample.module", "php", "<?php\nfunction run() {}\n", "run", NewPHP()},
		{"Drupal install", "sample.install", "php", "<?php\nfunction run() {}\n", "run", NewPHP()},
		{"Drupal theme", "sample.theme", "php", "<?php\nfunction run() {}\n", "run", NewPHP()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.adapter.Supports(tc.path) || !slices.Contains(tc.adapter.Extensions(), filepath.Ext(tc.path)) {
				t.Fatalf("adapter does not declare support for %s", tc.path)
			}
			parsed, err := tc.adapter.Parse(context.Background(), tc.path, []byte(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Language != tc.wantLanguage {
				t.Fatalf("language = %q, want %q", parsed.Language, tc.wantLanguage)
			}
			found := false
			for _, symbol := range parsed.Symbols {
				if symbol.Name == tc.wantSymbol && symbol.Range.StartLine > 0 && symbol.Range.StartCol > 0 {
					found = true
				}
			}
			if !found {
				t.Fatalf("symbol %q not extracted with a source position: %+v", tc.wantSymbol, parsed.Symbols)
			}
		})
	}
}
