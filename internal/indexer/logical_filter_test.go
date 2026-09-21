package indexer

import "testing"

func TestLogicalFilterDoesNotTreatBackslashAsHierarchy(t *testing.T) {
	if shouldSkipDir(`src\node_modules`, nil) {
		t.Fatal(`literal backslash path treated as nested node_modules`)
	}
	if shouldIgnorePath(`vendor\private/file.go`, []string{"vendor/**"}) {
		t.Fatal(`literal backslash path matched vendor hierarchy`)
	}
	if matchesAny(`src\file.go`, []string{"src/**"}) {
		t.Fatal(`literal backslash path matched slash pattern`)
	}
	if shouldSkipFile(`src\file.go`, nil, []string{"src/**"}) {
		t.Fatal(`literal backslash path matched slash exclude`)
	}
}

func TestLogicalFilterSlashControls(t *testing.T) {
	if !shouldSkipDir("src/node_modules", nil) {
		t.Fatal("slash node_modules not skipped")
	}
	if !shouldIgnorePath("vendor/private/file.go", []string{"vendor/**"}) {
		t.Fatal("slash vendor path not ignored")
	}
	if !matchesAny("src/file.go", []string{"src/**"}) {
		t.Fatal("slash include did not match")
	}
	if !matchesIgnore("vendor/private/file.go", []string{"vendor/**"}) {
		t.Fatal("slash exclude did not match")
	}
}

func TestLogicalIgnoreAncestorUsesSlashHierarchy(t *testing.T) {
	excludes := []string{"vendor/**", "!vendor/keep.go"}
	if !shouldIgnorePath("vendor/private/file.go", excludes) {
		t.Fatal("nested vendor file not ignored")
	}
	if shouldIgnorePath("vendor/keep.go", excludes) {
		t.Fatal("negated vendor file remained ignored")
	}
	if shouldIgnorePath(`vendor\private/file.go`, excludes) {
		t.Fatal("backslash component changed ancestor hierarchy")
	}
}

func TestLogicalGlobBoundariesAndBackslashComponents(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		pattern string
		want    bool
	}{
		{"backslash component", `dir\name/file.go`, "**/*.go", true},
		{"backslash component basename", `dir\name/file.go`, "*.go", true},
		{"backslash is not hierarchy", `src\file.go`, "src/**", false},
		{"backslash nested is not hierarchy", `vendor\private/file.go`, "vendor/**", false},
		{"prefix collision", "vendor2/file.go", "vendor/**", false},
		{"old prefix collision", "vendor-old/file.go", "vendor/**", false},
		{"root", "vendor", "vendor/**", true},
		{"direct child", "vendor/file.go", "vendor/**", true},
		{"descendant", "vendor/private/file.go", "vendor/**", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchPattern(tt.path, tt.pattern); got != tt.want {
				t.Fatalf("matchPattern(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestLogicalGlobBoundaryHelpers(t *testing.T) {
	if shouldSkipFile(`dir\name/file.go`, []string{"**/*.go"}, nil) {
		t.Fatal("backslash component rejected by include")
	}
	if shouldIgnorePath("vendor2/file.go", []string{"vendor/**"}) {
		t.Fatal("vendor glob affected sibling prefix")
	}
	if !shouldIgnorePath("vendor/file.go", []string{"vendor/**"}) {
		t.Fatal("vendor glob missed direct child")
	}
	excludes := []string{"vendor/**", "!vendor/keep.go"}
	if shouldIgnorePath("vendor/keep.go", excludes) || shouldIgnorePath("vendor2/keep.go", excludes) {
		t.Fatal("vendor negation boundary changed")
	}
}
