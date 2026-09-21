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
