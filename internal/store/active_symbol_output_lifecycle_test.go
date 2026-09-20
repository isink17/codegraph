package store

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// activeOutputFixture is the committed mark-deleted / pre-purge state F6A3
// owns: two files holding same-named symbols, one file soft-deleted with every
// graph row it owns still physically present.
type activeOutputFixture struct {
	s        *Store
	repoID   int64
	ctx      context.Context
	liveFile int64
	deadFile int64
	liveID   int64
	deadID   int64
}

func newActiveOutputFixture(t *testing.T) activeOutputFixture {
	t.Helper()
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	fx := activeOutputFixture{s: s, repoID: repoID, ctx: ctx}

	var err error
	// "active.go" sorts before "deleted.go", so the deleted candidate would
	// take offset 0 of every path-ordered page if it were visible; the
	// qualified names are ordered the other way for the name-ordered pages.
	if fx.deadFile, err = insertTestFile(ctx, s, repoID, "deleted.go"); err != nil {
		t.Fatal(err)
	}
	if fx.liveFile, err = insertTestFile(ctx, s, repoID, "live.go"); err != nil {
		t.Fatal(err)
	}
	if fx.deadID, err = insertTestSymbol(ctx, s, repoID, fx.deadFile, "Renew", "aaa.Renew"); err != nil {
		t.Fatal(err)
	}
	if fx.liveID, err = insertTestSymbol(ctx, s, repoID, fx.liveFile, "Renew", "zzz.Renew"); err != nil {
		t.Fatal(err)
	}
	for _, sym := range []struct {
		id    int64
		qname string
	}{{fx.deadID, "aaa.Renew"}, {fx.liveID, "zzz.Renew"}} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO symbol_fts(repo_id, symbol_id, name, qualified_name, signature, doc_summary) VALUES(?, ?, 'Renew', ?, '', '')`,
			repoID, sym.id, sym.qname); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?, 'renew', 1.0)`, sym.id); err != nil {
			t.Fatal(err)
		}
	}
	// The deleted symbol carries the stronger vector evidence, so a leak wins
	// the first visible slot rather than merely appearing somewhere.
	fx.embed(t, fx.deadID, fx.deadFile, []float32{1, 0})
	fx.embed(t, fx.liveID, fx.liveFile, []float32{0.6, 0.8})
	return fx
}

func (fx activeOutputFixture) embed(t *testing.T, symbolID, fileID int64, vec []float32) {
	t.Helper()
	if _, err := fx.s.db.ExecContext(fx.ctx, `
		INSERT INTO symbol_embeddings(symbol_id, file_id, repo_id, embedding, dimensions, model_name, updated_at)
		VALUES(?, ?, ?, ?, 2, 'test', '2026-01-01T00:00:00Z')
	`, symbolID, fileID, fx.repoID, float32ToBytes(vec)); err != nil {
		t.Fatal(err)
	}
}

func (fx activeOutputFixture) setDeleted(t *testing.T, fileID int64, deleted int) {
	t.Helper()
	if _, err := fx.s.db.ExecContext(fx.ctx, `UPDATE files SET is_deleted = ? WHERE id = ?`, deleted, fileID); err != nil {
		t.Fatal(err)
	}
}

// TestActiveSymbolDirectOutputsExcludeDeletedFiles pins the F6A3 contract on
// every direct symbol-output surface: a ghost symbol is absent, and it never
// consumes a page slot an active symbol should have had.
func TestActiveSymbolDirectOutputsExcludeDeletedFiles(t *testing.T) {
	fx := newActiveOutputFixture(t)
	s, repoID, ctx := fx.s, fx.repoID, fx.ctx
	fx.setDeleted(t, fx.deadFile, 1)

	t.Run("FindSymbolExact", func(t *testing.T) {
		got := must(s.FindSymbolExact(ctx, repoID, "Renew", 10, 0))
		if names := qnames(got); len(names) != 1 || names[0] != "zzz.Renew" {
			t.Fatalf("FindSymbolExact = %v, want [zzz.Renew]", names)
		}
		// The deleted candidate sorts first; a leak would make limit=1 return
		// it and push the active symbol to offset 1.
		if names := qnames(must(s.FindSymbolExact(ctx, repoID, "Renew", 1, 0))); len(names) != 1 || names[0] != "zzz.Renew" {
			t.Fatalf("FindSymbolExact(limit=1) = %v, want [zzz.Renew]", names)
		}
	})

	t.Run("FindSymbolExactResult", func(t *testing.T) {
		res := must(s.FindSymbolExactResult(ctx, repoID, "Renew", 1, 0))
		if !res.Matched || len(res.Matches) != 1 || res.Matches[0].QualifiedName != "zzz.Renew" {
			t.Fatalf("mixed exact result = %+v", res)
		}
		if res := must(s.FindSymbolExactResult(ctx, repoID, "Renew", 10, 5)); !res.Matched || len(res.Matches) != 0 {
			t.Fatalf("high-offset exact result = %+v, want matched-empty", res)
		}
		if res := must(s.FindSymbolExactResult(ctx, repoID, "aaa.Renew", 10, 0)); res.Matched || len(res.Matches) != 0 {
			t.Fatalf("deleted-only exact result = %+v, want unmatched-empty", res)
		}
	})

	t.Run("SearchSymbols", func(t *testing.T) {
		if names := qnames(must(s.SearchSymbols(ctx, repoID, "Renew", 10, 0))); len(names) != 1 || names[0] != "zzz.Renew" {
			t.Fatalf("SearchSymbols = %v, want [zzz.Renew]", names)
		}
		if names := qnames(must(s.SearchSymbols(ctx, repoID, "Renew", 1, 0))); len(names) != 1 || names[0] != "zzz.Renew" {
			t.Fatalf("SearchSymbols(limit=1) = %v, want [zzz.Renew]", names)
		}
	})

	t.Run("SearchSymbolsResult", func(t *testing.T) {
		res := must(s.SearchSymbolsResult(ctx, repoID, "Renew", 1, 0))
		if !res.Matched || len(res.Matches) != 1 || res.Matches[0].QualifiedName != "zzz.Renew" {
			t.Fatalf("mixed search result = %+v", res)
		}
		if res := must(s.SearchSymbolsResult(ctx, repoID, "Renew", 10, 5)); !res.Matched || len(res.Matches) != 0 {
			t.Fatalf("high-offset search result = %+v, want matched-empty", res)
		}
		if res := must(s.SearchSymbolsResult(ctx, repoID, "aaa", 10, 0)); res.Matched || len(res.Matches) != 0 {
			t.Fatalf("deleted-only search result = %+v, want unmatched-empty", res)
		}
	})

	t.Run("presence helpers", func(t *testing.T) {
		if matched, err := s.searchSymbolsMatched(ctx, repoID, "aaa"); err != nil || matched {
			t.Fatalf("searchSymbolsMatched(deleted-only) = %v, %v", matched, err)
		}
		if matched, err := s.likeSymbolsMatched(ctx, repoID, "aaa"); err != nil || matched {
			t.Fatalf("likeSymbolsMatched(deleted-only) = %v, %v", matched, err)
		}
		if matched, err := s.likeSymbolsMatched(ctx, repoID, "Renew"); err != nil || !matched {
			t.Fatalf("likeSymbolsMatched(mixed) = %v, %v", matched, err)
		}
	})

	t.Run("SemanticSearch", func(t *testing.T) {
		got := must(s.SemanticSearch(ctx, repoID, "renew", 10, 0))
		if len(got) != 1 || got[0]["symbol"] != "zzz.Renew" {
			t.Fatalf("SemanticSearch = %v, want only zzz.Renew", got)
		}
		if got := must(s.SemanticSearch(ctx, repoID, "renew", 1, 0)); len(got) != 1 || got[0]["symbol"] != "zzz.Renew" {
			t.Fatalf("SemanticSearch(limit=1) = %v", got)
		}
	})

	t.Run("VectorSearch", func(t *testing.T) {
		got := must(s.VectorSearch(ctx, repoID, []float32{1, 0}, 1, 0))
		if len(got) != 1 || got[0]["symbol"] != "zzz.Renew" {
			t.Fatalf("VectorSearch(limit=1) = %v, want the active symbol", got)
		}
		if got := must(s.VectorSearch(ctx, repoID, []float32{1, 0}, 10, 1)); len(got) != 0 {
			t.Fatalf("VectorSearch(offset=1) = %v, want empty", got)
		}
		// Capped scan: the cap must be spent on active candidates, so a scanCap
		// of 1 must still find the active symbol behind the stronger ghost.
		got = must(s.vectorSearch(ctx, repoID, []float32{1, 0}, 10, 0, 1))
		if len(got) != 1 || got[0]["symbol"] != "zzz.Renew" {
			t.Fatalf("capped vectorSearch = %v, want the active symbol", got)
		}
	})

	t.Run("HybridSearch", func(t *testing.T) {
		got := must(s.HybridSearch(ctx, repoID, "Renew", []float32{1, 0}, 10, 0))
		if len(got) != 1 || got[0]["symbol"] != "zzz.Renew" {
			t.Fatalf("HybridSearch = %v, want only zzz.Renew", got)
		}
	})

	t.Run("FindDeadCode", func(t *testing.T) {
		// Neither symbol has an incoming edge or a reference, so both are
		// dead-code candidates and the deleted one sorts first by path.
		got := must(s.FindDeadCode(ctx, repoID, 10, 0))
		if len(got) != 1 || got[0]["file"] != "live.go" {
			t.Fatalf("FindDeadCode = %v, want only live.go", got)
		}
		if got := must(s.FindDeadCode(ctx, repoID, 1, 0)); len(got) != 1 || got[0]["file"] != "live.go" {
			t.Fatalf("FindDeadCode(limit=1) = %v; a ghost consumed the page slot", got)
		}
	})

	t.Run("SymbolsForIDs", func(t *testing.T) {
		got := must(s.SymbolsForIDs(ctx, repoID, []int64{fx.liveID, fx.deadID}))
		if _, ok := got[fx.deadID]; ok {
			t.Fatalf("SymbolsForIDs resolved the deleted symbol: %v", got)
		}
		if _, ok := got[fx.liveID]; !ok {
			t.Fatalf("SymbolsForIDs dropped the active symbol: %v", got)
		}
	})

	t.Run("SymbolsForRefs", func(t *testing.T) {
		live := SymbolRef{File: "live.go", QualifiedName: "zzz.Renew"}
		dead := SymbolRef{File: "deleted.go", QualifiedName: "aaa.Renew"}
		got := must(s.SymbolsForRefs(ctx, repoID, []SymbolRef{live, dead}))
		if _, ok := got[dead]; ok {
			t.Fatalf("SymbolsForRefs resolved a deleted file: %v", got)
		}
		if _, ok := got[live]; !ok {
			t.Fatalf("SymbolsForRefs dropped the active ref: %v", got)
		}
	})

	t.Run("SymbolNameCounts", func(t *testing.T) {
		got := must(s.SymbolNameCounts(ctx, repoID, []string{"Renew"}))
		if got["Renew"] != 1 {
			t.Fatalf("SymbolNameCounts = %v, want Renew:1 (ghost must not create ambiguity)", got)
		}
	})
}

// TestActiveSymbolAnalyticsExcludeDeletedFiles covers the analytics surfaces,
// where the ghost has to be gone before the numbers are computed rather than
// merely absent from the rows returned.
func TestActiveSymbolAnalyticsExcludeDeletedFiles(t *testing.T) {
	fx := newActiveOutputFixture(t)
	s, repoID, ctx := fx.s, fx.repoID, fx.ctx

	// A third file gives the deleted symbol real graph weight: it is both a
	// PageRank node with an incoming edge and a top-degree candidate.
	hubFile, err := insertTestFile(ctx, s, repoID, "hub.go")
	if err != nil {
		t.Fatal(err)
	}
	hubID, err := insertTestSymbol(ctx, s, repoID, hubFile, "Hub", "hub.Hub")
	if err != nil {
		t.Fatal(err)
	}
	link := func(src, dst int64, fileID int64, name string) {
		t.Helper()
		id, err := insertTestEdge(ctx, s, repoID, fileID, src, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, dst, id); err != nil {
			t.Fatal(err)
		}
	}
	link(hubID, fx.deadID, hubFile, "aaa.Renew")
	link(hubID, fx.liveID, hubFile, "zzz.Renew")

	before := must(s.PageRank(ctx, repoID, 10))
	if len(before) != 3 {
		t.Fatalf("pre-deletion PageRank = %v, want three nodes", before)
	}
	fx.setDeleted(t, fx.deadFile, 1)

	t.Run("PageRank", func(t *testing.T) {
		got := must(s.PageRank(ctx, repoID, 10))
		if len(got) != 2 {
			t.Fatalf("PageRank = %v, want the two active nodes", got)
		}
		for _, row := range got {
			if row["symbol"] == "aaa.Renew" {
				t.Fatalf("PageRank returned the deleted node: %v", got)
			}
		}
		// Parity against the same graph with the deleted symbol never indexed
		// at all. This, not a "the number moved" check, is the real assertion:
		// a ghost node changes n, the damping base and every share, so only
		// matching the clean graph exactly proves it left before ranking.
		peer, peerRepo := newQueryTestStore(t)
		pf, err := insertTestFile(ctx, peer, peerRepo, "hub.go")
		if err != nil {
			t.Fatal(err)
		}
		lf, err := insertTestFile(ctx, peer, peerRepo, "live.go")
		if err != nil {
			t.Fatal(err)
		}
		ph, err := insertTestSymbol(ctx, peer, peerRepo, pf, "Hub", "hub.Hub")
		if err != nil {
			t.Fatal(err)
		}
		pl, err := insertTestSymbol(ctx, peer, peerRepo, lf, "Renew", "zzz.Renew")
		if err != nil {
			t.Fatal(err)
		}
		eid, err := insertTestEdge(ctx, peer, peerRepo, pf, ph, "zzz.Renew")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := peer.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, pl, eid); err != nil {
			t.Fatal(err)
		}
		want := must(peer.PageRank(ctx, peerRepo, 10))
		if len(want) != len(got) {
			t.Fatalf("PageRank parity: got %v, want %v", got, want)
		}
		for i := range got {
			if got[i]["symbol"] != want[i]["symbol"] || got[i]["rank"] != want[i]["rank"] {
				t.Fatalf("PageRank parity at %d: got %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("ArchitectureOverview", func(t *testing.T) {
		overview := must(s.ArchitectureOverview(ctx, repoID))
		totals := overview["totals"].(map[string]any)
		if got, _ := totals["symbols"].(int64); got != 2 {
			t.Fatalf("architecture totals.symbols = %#v, want 2", totals["symbols"])
		}
		for _, list := range []string{"entry_points", "hub_symbols"} {
			for _, row := range overview[list].([]map[string]any) {
				if row["file"] == "deleted.go" {
					t.Fatalf("%s included a deleted file: %v", list, overview[list])
				}
			}
		}
		// The hub's outgoing degree counted two edges before deletion; the edge
		// into the ghost is not user-visible evidence any more.
		for _, row := range overview["hub_symbols"].([]map[string]any) {
			if got, _ := row["callee_count"].(int); row["qualified_name"] == "hub.Hub" && got != 1 {
				t.Fatalf("hub.Hub callee_count = %v, want 1", row["callee_count"])
			}
		}
	})

	t.Run("FindCallersResult/FindCalleesResult", func(t *testing.T) {
		// The hub calls both Renew symbols; only the active one is a visible
		// callee, and it must hold the single slot at limit=1.
		res := must(s.FindCalleesResult(ctx, repoID, "hub.Hub", 0, 10, 0))
		if !res.TargetFound || len(res.Callees) != 1 || res.Callees[0].QualifiedName != "zzz.Renew" {
			t.Fatalf("FindCalleesResult = %+v, want only zzz.Renew", res)
		}
		if res := must(s.FindCalleesResult(ctx, repoID, "hub.Hub", 0, 1, 0)); len(res.Callees) != 1 || res.Callees[0].QualifiedName != "zzz.Renew" {
			t.Fatalf("FindCalleesResult(limit=1) = %+v; a ghost consumed the page slot", res)
		}
		if res := must(s.FindCallersResult(ctx, repoID, "zzz.Renew", 0, 10, 0)); !res.TargetFound || len(res.Callers) != 1 || res.Callers[0].QualifiedName != "hub.Hub" {
			t.Fatalf("FindCallersResult(active) = %+v, want [hub.Hub]", res)
		}
		// Seed resolution for the ghost's own name is F6A2's: the short-name
		// cascade legitimately lands on the active symbol. What F6A3 owns is
		// that no page ever contains the ghost row itself.
		res = must(s.FindCallersResult(ctx, repoID, "aaa.Renew", 0, 10, 0))
		for _, sym := range append(append([]graph.Symbol{}, res.Callers...), res.Callees...) {
			if sym.FilePath == "deleted.go" {
				t.Fatalf("neighbor page contained a ghost: %+v", res)
			}
		}
	})

	t.Run("FindContextNeighbors", func(t *testing.T) {
		got := must(s.FindContextNeighbors(ctx, repoID, []ContextSeed{{SymbolID: hubID, QualifiedName: "hub.Hub"}}, 10))
		if len(got) != 1 {
			t.Fatalf("FindContextNeighbors = %v", got)
		}
		for _, sym := range got[0].Callees {
			if sym.FilePath == "deleted.go" {
				t.Fatalf("context callees included a ghost: %v", got[0].Callees)
			}
		}
		if len(got[0].Callees) != 1 {
			t.Fatalf("context callees = %v, want the one active callee", got[0].Callees)
		}
		// fanout=1: the ghost must not consume the single ROW_NUMBER slot.
		one := must(s.FindContextNeighbors(ctx, repoID, []ContextSeed{{SymbolID: hubID, QualifiedName: "hub.Hub"}}, 1))
		if len(one[0].Callees) != 1 || one[0].Callees[0].QualifiedName != "zzz.Renew" {
			t.Fatalf("fanout=1 callees = %v, want [zzz.Renew]", one[0].Callees)
		}
	})
}

// TestActiveSymbolOutputsTrackReactivation proves visibility is a read-time
// property of files.is_deleted, independent of purge.
func TestActiveSymbolOutputsTrackReactivation(t *testing.T) {
	fx := newActiveOutputFixture(t)
	s, repoID, ctx := fx.s, fx.repoID, fx.ctx

	visible := func(want bool, stage string) {
		t.Helper()
		found := false
		for _, sym := range must(s.FindSymbolExact(ctx, repoID, "aaa.Renew", 10, 0)) {
			if sym.ID == fx.deadID {
				found = true
			}
		}
		if found != want {
			t.Fatalf("%s: FindSymbolExact visible = %v, want %v", stage, found, want)
		}
		counts := must(s.SymbolNameCounts(ctx, repoID, []string{"Renew"}))
		wantCount := 1
		if want {
			wantCount = 2
		}
		if counts["Renew"] != wantCount {
			t.Fatalf("%s: SymbolNameCounts = %v, want %d", stage, counts, wantCount)
		}
	}

	visible(true, "indexed")
	fx.setDeleted(t, fx.deadFile, 1)
	visible(false, "soft-deleted")
	fx.setDeleted(t, fx.deadFile, 0)
	visible(true, "reactivated")
}

// TestActiveSymbolOutputQueriesDoNotMutate pins that the read path never
// repairs or purges the stale rows it is now hiding.
func TestActiveSymbolOutputQueriesDoNotMutate(t *testing.T) {
	fx := newActiveOutputFixture(t)
	s, repoID, ctx := fx.s, fx.repoID, fx.ctx
	fx.setDeleted(t, fx.deadFile, 1)

	counts := func() map[string]int {
		out := map[string]int{}
		for _, table := range []string{"files", "symbols", "symbol_fts", "symbol_tokens", "symbol_embeddings", "edges", "references_tbl", "test_links"} {
			var n int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[table] = n
		}
		return out
	}

	before := counts()
	must(s.FindSymbolExact(ctx, repoID, "Renew", 10, 0))
	must(s.SearchSymbols(ctx, repoID, "Renew", 10, 0))
	must(s.SearchSymbolsResult(ctx, repoID, "Renew", 10, 0))
	must(s.SemanticSearch(ctx, repoID, "renew", 10, 0))
	must(s.VectorSearch(ctx, repoID, []float32{1, 0}, 10, 0))
	must(s.FindDeadCode(ctx, repoID, 10, 0))
	must(s.ArchitectureOverview(ctx, repoID))
	must(s.PageRank(ctx, repoID, 10))
	must(s.SymbolNameCounts(ctx, repoID, []string{"Renew"}))
	after := counts()

	for table, n := range before {
		if after[table] != n {
			t.Fatalf("%s row count changed by queries: %d -> %d", table, n, after[table])
		}
	}
	var deleted int
	if err := s.db.QueryRowContext(ctx, `SELECT is_deleted FROM files WHERE id = ?`, fx.deadFile).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("files.is_deleted = %d, want 1", deleted)
	}
}
