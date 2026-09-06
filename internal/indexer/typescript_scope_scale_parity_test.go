//go:build cgo

package indexer

import (
	"fmt"
	"strings"
	"testing"
)

// TestTypeScriptModuleScopeLargeIncrementalParity indexes a TypeScript module
// component wide enough to span several scoped-resolver batches, then drives
// create, edit and delete through the incremental path. It proves parity, not
// the parameter budget: the oracle is the fresh-index projection, so a batch
// boundary must not change a single edge target, strategy or confidence. The
// portable budget itself is enforced in internal/store's budget tests, because
// the locally linked SQLite accepts far more than 999 variables.
func TestTypeScriptModuleScopeLargeIncrementalParity(t *testing.T) {
	const callers = 950
	files := tree{"shared.ts": "export function shared() {}\n"}
	for i := range callers {
		files[fmt.Sprintf("call%04d.ts", i)] = fmt.Sprintf(
			"import { shared } from \"./shared\";\nfunction run%04d() { shared(); }\n", i)
	}
	r := newLifecycleRepo(t, files)
	for _, probe := range []string{"call0000.ts", "call0500.ts", "call0949.ts"} {
		if got := r.edgeState(t, probe, "shared"); !strings.Contains(got, "shared.ts:shared.shared") {
			t.Fatalf("%s did not bind through the module scope: %s", probe, got)
		}
	}
	r.assertFreshParity(t, "fresh large component")

	// Editing the shared module invalidates every dependent caller at once, so
	// the affected set crosses the scoped resolver's per-statement batch
	// boundary in a single incremental update.
	r.write(t, "shared.ts", "export function shared() {}\nexport function extra() {}\n")
	r.update(t, "shared.ts")
	r.assertFreshParity(t, "shared module edited")

	// A new caller and a deleted caller cross the same batched paths.
	r.write(t, "late.ts", "import { extra } from \"./shared\";\nfunction runLate() { extra(); }\n")
	r.remove(t, "call0000.ts")
	r.update(t)
	if got := r.edgeState(t, "late.ts", "extra"); !strings.Contains(got, "shared.ts:shared.extra") {
		t.Fatalf("late caller did not bind: %s", got)
	}
	r.assertFreshParity(t, "caller created and deleted")

	// Removing the target leaves every dependent edge honestly unresolved.
	r.remove(t, "shared.ts")
	r.update(t)
	if got := r.edgeState(t, "call0500.ts", "shared"); !strings.Contains(got, ":: [/") {
		t.Fatalf("deleted target left a stale binding: %s", got)
	}
	r.assertFreshParity(t, "shared module deleted")
}
