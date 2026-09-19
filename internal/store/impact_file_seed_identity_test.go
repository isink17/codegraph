package store

import (
	"context"
	"runtime"
	"slices"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// impactSeedFixture stores two files whose paths differ only in one separator
// byte, each defining one symbol with its own resolved caller, so a wrong
// seed lookup shows up as the sibling's symbols in the closure.
type impactSeedFixture struct {
	s      *Store
	repoID int64
}

func newImpactSeedFixture(t *testing.T) impactSeedFixture {
	t.Helper()
	s, repoID := newQueryTestStore(t)
	ctx := context.Background()
	callerFile, err := insertTestFile(ctx, s, repoID, "caller.go")
	if err != nil {
		t.Fatalf("insertTestFile(caller.go) error = %v", err)
	}
	for _, target := range []struct{ path, sym, caller string }{
		{"pkg/x/y.go", "YSlash", "CallsSlash"},
		{`pkg/x\y.go`, "YBack", "CallsBack"},
	} {
		fileID, err := insertTestFile(ctx, s, repoID, target.path)
		if err != nil {
			t.Fatalf("insertTestFile(%q) error = %v", target.path, err)
		}
		symID, err := insertTestSymbol(ctx, s, repoID, fileID, target.sym, "pkg."+target.sym)
		if err != nil {
			t.Fatalf("insertTestSymbol(%q) error = %v", target.sym, err)
		}
		callerID, err := insertTestSymbol(ctx, s, repoID, callerFile, target.caller, "pkg."+target.caller)
		if err != nil {
			t.Fatalf("insertTestSymbol(%q) error = %v", target.caller, err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, ?, 'call', '', ?, 1)`, repoID, callerID, symID, target.sym, callerFile); err != nil {
			t.Fatalf("insert resolved edge %s -> %s error = %v", target.caller, target.sym, err)
		}
	}
	return impactSeedFixture{s: s, repoID: repoID}
}

func (f impactSeedFixture) impact(t *testing.T, files ...string) (map[string]any, []string, []string, ImpactSeedPresence) {
	t.Helper()
	data, err := f.s.ImpactRadius(context.Background(), f.repoID, nil, files, 1, 100, 0)
	if err != nil {
		t.Fatalf("ImpactRadius(files=%q) error = %v", files, err)
	}
	syms, ok := data["symbols"].([]graph.Symbol)
	if !ok {
		t.Fatalf("symbols = %T, want []graph.Symbol", data["symbols"])
	}
	names := make([]string, 0, len(syms))
	for _, sym := range syms {
		names = append(names, sym.Name)
	}
	slices.Sort(names)
	paths, ok := data["files"].([]string)
	if !ok {
		t.Fatalf("files = %T, want []string", data["files"])
	}
	presence, ok := data["seed_presence"].(ImpactSeedPresence)
	if !ok {
		t.Fatalf("seed_presence = %T, want ImpactSeedPresence", data["seed_presence"])
	}
	return data, names, paths, presence
}

// A file seed names exactly one stored files.path row. The former
// normalizeRepoRelPath ran filepath.FromSlash over the seed, so on Windows a
// slash-spelled seed became a backslash string that files.path never holds and
// every multi-segment file seed reported missing.
func TestImpactRadiusFileSeedBindsStoredIdentity(t *testing.T) {
	f := newImpactSeedFixture(t)

	cases := []struct {
		seed          string
		wantSyms      []string
		wantFiles     []string
		windowsStores bool
	}{
		{seed: "pkg/x/y.go", wantSyms: []string{"CallsSlash", "YSlash"}, wantFiles: []string{"caller.go", "pkg/x/y.go"}, windowsStores: true},
		// A public spelling is accepted once at the boundary and lands on the
		// same row as the stored identity.
		{seed: "./pkg/x/y.go", wantSyms: []string{"CallsSlash", "YSlash"}, wantFiles: []string{"caller.go", "pkg/x/y.go"}, windowsStores: true},
		// The indexer writes files.path through NativeRelativeToLogical, so a
		// literal backslash identity cannot exist on Windows and the public
		// boundary legitimately reads the backslash as a separator there.
		{seed: `pkg/x\y.go`, wantSyms: []string{"CallsBack", "YBack"}, wantFiles: []string{"caller.go", `pkg/x\y.go`}},
	}
	for _, tc := range cases {
		if runtime.GOOS == "windows" && !tc.windowsStores {
			continue
		}
		_, names, paths, presence := f.impact(t, tc.seed)
		if !slices.Equal(names, tc.wantSyms) {
			t.Fatalf("ImpactRadius(files=%q) symbols = %v, want %v", tc.seed, names, tc.wantSyms)
		}
		if !slices.Equal(paths, tc.wantFiles) {
			t.Fatalf("ImpactRadius(files=%q) files = %v, want %v", tc.seed, paths, tc.wantFiles)
		}
		if presence.Requested != 1 || presence.Found != 1 || len(presence.Missing) != 0 {
			t.Fatalf("ImpactRadius(files=%q) presence = %+v, want one found seed", tc.seed, presence)
		}
	}

	// Control: both seeds together reach both graphs, so the single-seed
	// assertions above were excluding real rows rather than an empty store.
	if runtime.GOOS != "windows" {
		_, names, _, presence := f.impact(t, "pkg/x/y.go", `pkg/x\y.go`)
		want := []string{"CallsBack", "CallsSlash", "YBack", "YSlash"}
		if !slices.Equal(names, want) || presence.Found != 2 {
			t.Fatalf("both seeds: symbols = %v presence = %+v, want %v with 2 found", names, presence, want)
		}
	}
}

// Spellings that are not valid public repository paths address no row and are
// reported missing under the requested name, so Requested == Found + Missing
// always holds. The former helper trimmed whitespace and silently dropped
// empty seeds from both counts.
func TestImpactRadiusFileSeedInvalidSpellingIsMissing(t *testing.T) {
	f := newImpactSeedFixture(t)
	seeds := []string{" pkg/x/y.go", "pkg/x/y.go ", "../pkg/x/y.go", "/pkg/x/y.go", "", ".", "nope.go"}
	_, names, paths, presence := f.impact(t, seeds...)
	if len(names) != 0 || len(paths) != 0 {
		t.Fatalf("invalid seeds reached symbols %v files %v", names, paths)
	}
	if presence.Requested != len(seeds) || presence.Found != 0 || !slices.Equal(presence.Missing, seeds) {
		t.Fatalf("presence = %+v, want every seed missing in request order", presence)
	}
}
