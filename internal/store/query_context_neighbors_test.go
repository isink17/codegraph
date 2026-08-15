package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// ambiguityFixture is the same-short-name shape P19 exists to get right:
//
//	billing.Renew and subscription.Renew -- one short name, two symbols
//	unique.Settle                        -- a short name only one symbol has
//
// and, pointing at billing.Renew, one caller per evidence class:
//
//	A resolved      dst_symbol_id = billing.Renew
//	B exact qname   dst_name = "billing.Renew"
//	C exact short   dst_name = "Renew"
//	D suffix        dst_name = "foo.Renew"
//
// C and D fit subscription.Renew exactly as well as they fit billing.Renew, so
// neither is evidence about either. A and B name one symbol and nothing else.
type ambiguityFixture struct {
	store  *Store
	repoID int64
	sym    map[string]int64
	fileID int64
}

func newAmbiguityFixture(t *testing.T) *ambiguityFixture {
	t.Helper()
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}

	fx := &ambiguityFixture{store: s, repoID: repo.ID, sym: map[string]int64{}, fileID: fileID}
	for _, qname := range []string{
		"billing.Renew", "subscription.Renew", "unique.Settle",
		"caller.Resolved", "caller.Qualified", "caller.Short", "caller.Suffix",
		"caller.SettleResolved", "caller.SettleShort", "caller.SettleSuffix",
	} {
		fx.add(t, qname)
	}
	return fx
}

func (fx *ambiguityFixture) add(t *testing.T, qname string) {
	t.Helper()
	short := lookupSymbolShortName(qname)
	id, err := insertTestSymbol(context.Background(), fx.store, fx.repoID, fx.fileID, short, qname)
	if err != nil {
		t.Fatalf("insertTestSymbol(%q) error = %v", qname, err)
	}
	fx.sym[qname] = id
}

// resolvedEdge inserts src -> dst bound to a symbol id.
func (fx *ambiguityFixture) resolvedEdge(t *testing.T, src, dst string) {
	t.Helper()
	if _, err := fx.store.db.ExecContext(context.Background(), `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, ?, ?, 'call', '', ?, 1)
	`, fx.repoID, fx.sym[src], fx.sym[dst], dst, fx.fileID); err != nil {
		t.Fatalf("resolvedEdge(%s -> %s) error = %v", src, dst, err)
	}
}

// unresolvedEdge inserts src -> dstName with no bound destination.
func (fx *ambiguityFixture) unresolvedEdge(t *testing.T, src, dstName string) {
	t.Helper()
	if _, err := fx.store.db.ExecContext(context.Background(), `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, NULL, ?, 'call', '', ?, 1)
	`, fx.repoID, fx.sym[src], dstName, fx.fileID); err != nil {
		t.Fatalf("unresolvedEdge(%s -> %q) error = %v", src, dstName, err)
	}
}

func (fx *ambiguityFixture) seed(qname string, allowShort bool) ContextSeed {
	return ContextSeed{
		SymbolID:           fx.sym[qname],
		QualifiedName:      qname,
		ShortName:          LookupSymbolShortName(qname),
		AllowShortEvidence: allowShort,
	}
}

func callerNames(n ContextNeighbors) []string {
	out := make([]string, 0, len(n.Callers))
	for _, c := range n.Callers {
		out = append(out, c.QualifiedName)
	}
	return out
}

func calleeNames(n ContextNeighbors) []string {
	out := make([]string, 0, len(n.Callees))
	for _, c := range n.Callees {
		out = append(out, c.QualifiedName)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (fx *ambiguityFixture) allEvidenceTowards(t *testing.T, target string) {
	t.Helper()
	short := lookupSymbolShortName(target)
	fx.resolvedEdge(t, "caller.Resolved", target)
	fx.unresolvedEdge(t, "caller.Qualified", target)
	fx.unresolvedEdge(t, "caller.Short", short)
	fx.unresolvedEdge(t, "caller.Suffix", "foo."+short)
}

// The core P19 recall fix: an ambiguous short name must not cost the seed its
// exact qualified-name evidence.
func TestContextNeighborsKeepsQualifiedEvidenceForAmbiguousShortName(t *testing.T) {
	fx := newAmbiguityFixture(t)
	fx.allEvidenceTowards(t, "billing.Renew")

	got, err := fx.store.FindContextNeighbors(context.Background(), fx.repoID,
		[]ContextSeed{fx.seed("billing.Renew", false)}, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	want := []string{"caller.Qualified", "caller.Resolved"}
	if !equalStrings(callerNames(got[0]), want) {
		t.Fatalf("callers = %v, want %v (resolved + exact qualified, no short, no suffix)",
			callerNames(got[0]), want)
	}
}

// The other half: the short and suffix legs stay off, so subscription.Renew's
// callers never become billing.Renew's.
func TestContextNeighborsBlocksShortAndSuffixEvidenceForAmbiguousName(t *testing.T) {
	fx := newAmbiguityFixture(t)
	fx.allEvidenceTowards(t, "billing.Renew")

	got, err := fx.store.FindContextNeighbors(context.Background(), fx.repoID,
		[]ContextSeed{fx.seed("billing.Renew", false), fx.seed("subscription.Renew", false)}, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	for _, name := range callerNames(got[0]) {
		if name == "caller.Short" || name == "caller.Suffix" {
			t.Fatalf("billing.Renew inherited ambiguous evidence %q", name)
		}
	}
	// subscription.Renew has no evidence of its own at all, and must not pick up
	// billing's bare-name callers either.
	if len(got[1].Callers) != 0 {
		t.Fatalf("subscription.Renew callers = %v, want none", callerNames(got[1]))
	}
}

// A short name only one symbol carries keeps the bare-name recall, and a
// spelling that extends the seed's identity at a boundary keeps the suffix
// recall. (`pkg.Settle` -- a foreign qualifier -- would have matched before
// P22.1; it is exactly the `rows.Close` fabrication and stays out now.)
func TestContextNeighborsKeepsShortAndSuffixRecallForUniqueName(t *testing.T) {
	fx := newAmbiguityFixture(t)
	fx.resolvedEdge(t, "caller.SettleResolved", "unique.Settle")
	fx.unresolvedEdge(t, "caller.SettleShort", "Settle")
	fx.unresolvedEdge(t, "caller.SettleSuffix", "x.unique.Settle")

	got, err := fx.store.FindContextNeighbors(context.Background(), fx.repoID,
		[]ContextSeed{fx.seed("unique.Settle", true)}, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	want := []string{"caller.SettleResolved", "caller.SettleShort", "caller.SettleSuffix"}
	if !equalStrings(callerNames(got[0]), want) {
		t.Fatalf("callers = %v, want %v", callerNames(got[0]), want)
	}
}

// With short evidence enabled the batch answer is exactly the public
// FindCallers answer for the same symbol. This is the parity that lets
// ContextForTask stop calling the public query.
func TestContextNeighborsMatchesPublicFindCallers(t *testing.T) {
	fx := newAmbiguityFixture(t)
	fx.resolvedEdge(t, "caller.SettleResolved", "unique.Settle")
	fx.unresolvedEdge(t, "caller.SettleShort", "Settle")
	fx.unresolvedEdge(t, "caller.SettleSuffix", "x.unique.Settle")
	ctx := context.Background()

	want, err := fx.store.FindCallers(ctx, fx.repoID, "unique.Settle", fx.sym["unique.Settle"], 10, 0)
	if err != nil {
		t.Fatalf("FindCallers() error = %v", err)
	}
	got, err := fx.store.FindContextNeighbors(ctx, fx.repoID, []ContextSeed{fx.seed("unique.Settle", true)}, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if len(want) != len(got[0].Callers) {
		t.Fatalf("batch returned %d callers, public returned %d", len(got[0].Callers), len(want))
	}
	for i := range want {
		if want[i].ID != got[0].Callers[i].ID {
			t.Fatalf("caller[%d] = %d (%s), public = %d (%s)",
				i, got[0].Callers[i].ID, got[0].Callers[i].QualifiedName, want[i].ID, want[i].QualifiedName)
		}
	}
}

// One caller can neighbour two seeds. It belongs to both pages; deduplication
// across seeds here would silently drop context.
func TestContextNeighborsDoesNotDedupeAcrossSeeds(t *testing.T) {
	fx := newAmbiguityFixture(t)
	fx.resolvedEdge(t, "caller.Resolved", "billing.Renew")
	fx.resolvedEdge(t, "caller.Resolved", "unique.Settle")

	got, err := fx.store.FindContextNeighbors(context.Background(), fx.repoID,
		[]ContextSeed{fx.seed("billing.Renew", false), fx.seed("unique.Settle", true)}, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	for i := range got {
		if !equalStrings(callerNames(got[i]), []string{"caller.Resolved"}) {
			t.Fatalf("seed %d callers = %v, want [caller.Resolved]", i, callerNames(got[i]))
		}
	}
}

// The same caller reached through the resolved edge, the qualified-name edge
// and the short-name edge is one caller.
func TestContextNeighborsDedupesEvidenceWithinASeed(t *testing.T) {
	fx := newAmbiguityFixture(t)
	fx.resolvedEdge(t, "caller.Resolved", "unique.Settle")
	fx.unresolvedEdge(t, "caller.Resolved", "unique.Settle")
	fx.unresolvedEdge(t, "caller.Resolved", "Settle")
	fx.unresolvedEdge(t, "caller.Resolved", "pkg.Settle")

	got, err := fx.store.FindContextNeighbors(context.Background(), fx.repoID,
		[]ContextSeed{fx.seed("unique.Settle", true)}, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if !equalStrings(callerNames(got[0]), []string{"caller.Resolved"}) {
		t.Fatalf("callers = %v, want one [caller.Resolved]", callerNames(got[0]))
	}
}

// Callee association is per seed: seed A's unresolved destinations must not
// become seed B's callees. This is the failure mode a naive "collect every
// name, resolve once, attach to everyone" batch would have.
func TestContextNeighborsKeepsCalleeAssociationPerSeed(t *testing.T) {
	fx := newAmbiguityFixture(t)
	fx.resolvedEdge(t, "billing.Renew", "caller.Resolved")
	fx.unresolvedEdge(t, "billing.Renew", "unique.Settle")
	fx.unresolvedEdge(t, "subscription.Renew", "caller.Qualified")
	ctx := context.Background()

	got, err := fx.store.FindContextNeighbors(ctx, fx.repoID,
		[]ContextSeed{fx.seed("billing.Renew", false), fx.seed("subscription.Renew", false)}, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if want := []string{"caller.Resolved", "unique.Settle"}; !equalStrings(calleeNames(got[0]), want) {
		t.Fatalf("billing.Renew callees = %v, want %v", calleeNames(got[0]), want)
	}
	if want := []string{"caller.Qualified"}; !equalStrings(calleeNames(got[1]), want) {
		t.Fatalf("subscription.Renew callees = %v, want %v", calleeNames(got[1]), want)
	}
}

// Callee parity with the public query, including the unresolved-name cascade.
func TestContextNeighborsMatchesPublicFindCallees(t *testing.T) {
	fx := newAmbiguityFixture(t)
	fx.resolvedEdge(t, "billing.Renew", "caller.Resolved")
	fx.unresolvedEdge(t, "billing.Renew", "unique.Settle")
	fx.unresolvedEdge(t, "billing.Renew", "Settle")
	ctx := context.Background()

	want, err := fx.store.FindCallees(ctx, fx.repoID, "billing.Renew", fx.sym["billing.Renew"], 10, 0)
	if err != nil {
		t.Fatalf("FindCallees() error = %v", err)
	}
	got, err := fx.store.FindContextNeighbors(ctx, fx.repoID, []ContextSeed{fx.seed("billing.Renew", false)}, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if len(want) != len(got[0].Callees) {
		t.Fatalf("batch returned %d callees %v, public returned %d %v",
			len(got[0].Callees), calleeNames(got[0]), len(want), want)
	}
	for i := range want {
		if want[i].ID != got[0].Callees[i].ID {
			t.Fatalf("callee[%d] = %s, public = %s", i, got[0].Callees[i].QualifiedName, want[i].QualifiedName)
		}
	}
}

// Fanout is per seed, not per batch.
func TestContextNeighborsFanoutIsPerSeed(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}

	const hubs = 3
	const callersPerHub = 25
	var seeds []ContextSeed
	for h := range hubs {
		qname := fmt.Sprintf("pkg.Hub%d", h)
		hubID, err := insertTestSymbol(ctx, s, repo.ID, fileID, fmt.Sprintf("Hub%d", h), qname)
		if err != nil {
			t.Fatalf("insertTestSymbol() error = %v", err)
		}
		for c := range callersPerHub {
			cid, err := insertTestSymbol(ctx, s, repo.ID, fileID,
				fmt.Sprintf("C%d_%02d", h, c), fmt.Sprintf("pkg.C%d_%02d", h, c))
			if err != nil {
				t.Fatalf("insertTestSymbol() error = %v", err)
			}
			if _, err := s.db.ExecContext(ctx, `
				INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
				VALUES(?, ?, ?, ?, 'call', '', ?, 1)
			`, repo.ID, cid, hubID, qname, fileID); err != nil {
				t.Fatalf("insert edge error = %v", err)
			}
		}
		seeds = append(seeds, ContextSeed{SymbolID: hubID, QualifiedName: qname,
			ShortName: LookupSymbolShortName(qname), AllowShortEvidence: true})
	}

	got, err := s.FindContextNeighbors(ctx, repo.ID, seeds, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	for i, n := range got {
		if len(n.Callers) != 10 {
			t.Fatalf("seed %d got %d callers, want the per-seed fanout of 10", i, len(n.Callers))
		}
		// Ordered by qualified_name, so the page is the first ten of that hub's
		// callers -- and of that hub's only.
		for _, c := range n.Callers {
			if want := fmt.Sprintf("pkg.C%d_", i); len(c.QualifiedName) < len(want) || c.QualifiedName[:len(want)] != want {
				t.Fatalf("seed %d got caller %q from another hub", i, c.QualifiedName)
			}
		}
	}
}

// Result slots line up with input slots even when a seed contributes nothing.
func TestContextNeighborsPreservesSeedSlots(t *testing.T) {
	fx := newAmbiguityFixture(t)
	fx.resolvedEdge(t, "caller.Resolved", "unique.Settle")

	seeds := []ContextSeed{
		{SymbolID: 0, QualifiedName: ""},
		fx.seed("subscription.Renew", false),
		fx.seed("unique.Settle", true),
	}
	got, err := fx.store.FindContextNeighbors(context.Background(), fx.repoID, seeds, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if len(got) != len(seeds) {
		t.Fatalf("got %d result slots for %d seeds", len(got), len(seeds))
	}
	if len(got[0].Callers) != 0 || len(got[1].Callers) != 0 {
		t.Fatalf("empty seeds produced callers: %v / %v", callerNames(got[0]), callerNames(got[1]))
	}
	if !equalStrings(callerNames(got[2]), []string{"caller.Resolved"}) {
		t.Fatalf("seed 2 callers = %v", callerNames(got[2]))
	}
}
