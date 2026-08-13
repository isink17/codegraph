package store

import (
	"os"
	"testing"
)

// TestMain exists to clean up the ~100k-symbol query-latency fixture.
//
// The fixture is built once per test binary and deliberately lives outside
// t.TempDir(), because it outlives the first test or benchmark that asks for
// it. Nothing else would ever delete it: it is a ~230MB database, and without
// this hook every non-short `go test ./internal/store` left another copy behind
// until the machine's temp volume filled up.
func TestMain(m *testing.M) {
	code := m.Run()
	releaseBigGraph()
	os.Exit(code)
}
