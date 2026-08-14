package query

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// TestSeedRefRoundTripThroughProductionIndexer is the regression for the Windows
// CI failure: the whole seed bridge, over a repository indexed by the production
// indexer rather than by hand-written rows.
//
// The indexer derives `files.path` with filepath.Rel/filepath.Clean, so the
// stored value is native-form (`paymentsvc\service.go` on Windows). Every value
// the store hands back to a client is slash-normalized instead. Before the fix
// SymbolsForRefs keyed its wanted-set on the producer's raw path and compared it
// against the scanned symbol's normalized path, so on Windows no seed ever
// matched and context_for_task returned `{"files":null}` again -- the exact bug
// P14 set out to fix. The store-level tests could not catch it because they
// insert `billing/renew.go` literally, which is already canonical on every host.
func TestSeedRefRoundTripThroughProductionIndexer(t *testing.T) {
	ctx := context.Background()
	for name, fx := range map[string]*contextFixture{
		"token_overlap": newContextFixture(t),
		"hybrid":        newContextFixtureWithEmbedder(t, bagOfWordsEmbedder{}),
	} {
		raw, err := fx.svc.SemanticSearch(ctx, fx.repoID, taskPayment, 30, 0)
		if err != nil {
			t.Fatalf("%s: SemanticSearch() error = %v", name, err)
		}
		hits := parseSearchHits(raw)
		if len(hits) == 0 {
			t.Fatalf("%s: no hits to resolve", name)
		}
		for _, hit := range hits {
			// Whatever the host, a hit's path is the canonical identity.
			if hit.File != store.CanonicalRelPath(hit.File) {
				t.Fatalf("%s: hit path %q is not canonical", name, hit.File)
			}
			if strings.Contains(hit.File, "\\") {
				t.Fatalf("%s: hit path %q carries a native separator", name, hit.File)
			}
		}

		resolved, err := fx.svc.store.SymbolsForRefs(ctx, fx.repoID, hitRefs(hits))
		if err != nil {
			t.Fatalf("%s: SymbolsForRefs() error = %v", name, err)
		}
		if len(resolved) == 0 {
			t.Fatalf("%s: every one of %d refs from the production indexer failed to resolve", name, len(hits))
		}
		for ref, sym := range resolved {
			if ref.File != sym.FilePath {
				t.Fatalf("%s: ref key %q disagrees with the resolved symbol's path %q", name, ref.File, sym.FilePath)
			}
			if sym.ID == 0 || sym.StableKey == "" {
				t.Fatalf("%s: resolved symbol lacks identity: %+v", name, sym)
			}
		}
		// The bridge must resolve the seed the ranking depends on, not merely
		// something.
		want := store.SymbolRef{File: "paymentsvc/service.go", QualifiedName: "paymentsvc.ProcessPayment"}
		if _, ok := resolved[want]; !ok {
			t.Fatalf("%s: %+v missing from %d resolved refs", name, want, len(resolved))
		}
	}
}

// A ref built from a native-form path -- what a producer reading `files.path`
// directly would hand over -- must address the same indexed file as the
// canonical one, and must come back keyed canonically.
func TestSeedRefAcceptsNativeSeparatorForm(t *testing.T) {
	ctx := context.Background()
	fx := newContextFixture(t)

	canonical := store.SymbolRef{File: "paymentsvc/service.go", QualifiedName: "paymentsvc.ProcessPayment"}
	native := store.SymbolRef{File: filepath.FromSlash(canonical.File), QualifiedName: canonical.QualifiedName}

	fromCanonical, err := fx.store.SymbolsForRefs(ctx, fx.repoID, []store.SymbolRef{canonical})
	if err != nil {
		t.Fatalf("SymbolsForRefs(canonical) error = %v", err)
	}
	fromNative, err := fx.store.SymbolsForRefs(ctx, fx.repoID, []store.SymbolRef{native})
	if err != nil {
		t.Fatalf("SymbolsForRefs(native) error = %v", err)
	}
	if len(fromCanonical) != 1 || len(fromNative) != 1 {
		t.Fatalf("canonical resolved %d, native resolved %d; want 1 each", len(fromCanonical), len(fromNative))
	}
	if fromCanonical[canonical].ID != fromNative[canonical].ID {
		t.Fatalf("the two ref forms resolved to different symbols: %d vs %d",
			fromCanonical[canonical].ID, fromNative[canonical].ID)
	}
	if _, ok := fromNative[native]; ok && native.File != canonical.File {
		t.Fatal("result is keyed by the native form; callers compare canonical paths")
	}
}

// Ranking identity and the ranking fingerprint are built from paths that came
// out of the store, so the assertion that matters is that the store's own output
// is canonical -- that is the point where a native path would otherwise leak into
// a fingerprint the cursor validates against.
//
// The candidate-level comparison below is an identity transform on a slash host
// and the real divergence on Windows; the store round trip in front of it is what
// makes the test more than a restatement of CanonicalRelPath.
func TestRankingIdentityUsesCanonicalStorePaths(t *testing.T) {
	ctx := context.Background()
	fx := newContextFixture(t)

	ref := store.SymbolRef{File: "paymentsvc/service.go", QualifiedName: "paymentsvc.ProcessPayment"}
	resolved, err := fx.store.SymbolsForRefs(ctx, fx.repoID, []store.SymbolRef{ref})
	if err != nil {
		t.Fatalf("SymbolsForRefs() error = %v", err)
	}
	sym, ok := resolved[ref]
	if !ok {
		t.Fatalf("seed did not resolve: %+v", resolved)
	}
	if sym.FilePath != store.CanonicalRelPath(sym.FilePath) {
		t.Fatalf("store returned a non-canonical path %q", sym.FilePath)
	}

	fromStore := contextCandidate{sym: sym, relevance: relevanceDirectMatch, taskSignal: 1}
	if !strings.Contains(fromStore.identity(), "paymentsvc/service.go") {
		t.Fatalf("identity %q does not carry the canonical path", fromStore.identity())
	}

	// A candidate whose path arrived in native form must land on the same identity
	// and the same fingerprint once canonicalized.
	native := fromStore
	native.sym.FilePath = filepath.FromSlash(sym.FilePath)
	native.sym.FilePath = store.CanonicalRelPath(native.sym.FilePath)
	if fromStore.identity() != native.identity() {
		t.Fatalf("identity differs by separator form: %q vs %q", fromStore.identity(), native.identity())
	}
	if rankingFingerprint([]contextCandidate{fromStore}) != rankingFingerprint([]contextCandidate{native}) {
		t.Fatal("ranking fingerprint differs by separator form")
	}
}

func TestCanonicalRelPathIsIdempotentAndNativeTolerant(t *testing.T) {
	for _, want := range []string{"a.go", "pkg/a.go", "a/b/c/d.go"} {
		if got := store.CanonicalRelPath(want); got != want {
			t.Fatalf("CanonicalRelPath(%q) = %q", want, got)
		}
		// filepath.FromSlash is the shape a native-form producer supplies. On a
		// slash host this is the identity; on Windows it is the failing case.
		if got := store.CanonicalRelPath(filepath.FromSlash(want)); got != want {
			t.Fatalf("CanonicalRelPath(native %q) = %q, want %q", filepath.FromSlash(want), got, want)
		}
		if got := store.CanonicalRelPath("./" + want); got != want {
			t.Fatalf("CanonicalRelPath(%q) = %q, want %q", "./"+want, got, want)
		}
	}
	for _, empty := range []string{"", "."} {
		if got := store.CanonicalRelPath(empty); got != "" {
			t.Fatalf("CanonicalRelPath(%q) = %q, want empty", empty, got)
		}
	}
	// Whitespace is part of a POSIX filename, so the identity function must not
	// trim it: a file called " spaced.go" has to keep resolving.
	for _, spaced := range []string{" spaced.go", "dir/trailing .go", "   "} {
		if got := store.CanonicalRelPath(spaced); got != spaced {
			t.Fatalf("CanonicalRelPath(%q) = %q; whitespace must survive", spaced, got)
		}
	}
	// Case is identity-bearing and must survive untouched.
	if got := store.CanonicalRelPath("Pkg/File.go"); got != "Pkg/File.go" {
		t.Fatalf("CanonicalRelPath folded case: %q", got)
	}
}
