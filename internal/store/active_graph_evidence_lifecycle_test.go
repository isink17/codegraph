package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// activeGraphFixture is the committed soft-delete window: every graph row is
// retained while deleted.go is inactive.
type activeGraphFixture struct {
	s                      *Store
	ctx                    context.Context
	repoID                 int64
	activeA, activeB, dead int64
	a, b, ghost            int64
	visible, unresolved    int64
}

func newActiveGraphFixture(t *testing.T) activeGraphFixture {
	t.Helper()
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	fx := activeGraphFixture{s: s, repoID: repoID, ctx: ctx}
	fx.dead = must(insertTestFile(ctx, s, repoID, "deleted.go"))
	fx.activeA = must(insertTestFile(ctx, s, repoID, "active_a.go"))
	fx.activeB = must(insertTestFile(ctx, s, repoID, "active_b.go"))
	fx.ghost = must(insertTestSymbol(ctx, s, repoID, fx.dead, "Ghost", "aaa.Ghost"))
	// Duplicate active/deleted identity proves inactive candidates do not create
	// DOT duplication or trace ambiguity.
	must(insertTestSymbol(ctx, s, repoID, fx.dead, "B", "pkg.B"))
	fx.a = must(insertTestSymbol(ctx, s, repoID, fx.activeA, "A", "pkg.A"))
	fx.b = must(insertTestSymbol(ctx, s, repoID, fx.activeB, "B", "pkg.B"))

	edge := func(fileID, src, dst int64, name string) int64 {
		t.Helper()
		id := must(insertTestEdge(ctx, s, repoID, fileID, src, name))
		if dst != 0 {
			if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, dst, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	// Hidden rows deliberately precede visible rows by edge id.
	edge(fx.activeA, fx.a, fx.ghost, "aaa.Ghost")
	edge(fx.dead, fx.ghost, fx.b, "pkg.B")
	edge(fx.dead, fx.ghost, fx.ghost, "pkg.Ghost")
	edge(fx.dead, fx.ghost, 0, "missing.Dead")
	fx.visible = edge(fx.activeA, fx.a, fx.b, "pkg.B")
	fx.unresolved = edge(fx.activeB, fx.b, 0, "missing.Live")

	for _, row := range []struct {
		file, symbol int64
		name         string
	}{{fx.activeA, fx.ghost, "Ghost"}, {fx.dead, fx.b, "B"}} {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO references_tbl(repo_id, file_id, symbol_id, ref_kind, name, start_line, start_col, end_line, end_col)
			VALUES(?, ?, ?, 'call', ?, 1, 1, 1, 1)`, repoID, row.file, row.symbol, row.name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, fx.dead); err != nil {
		t.Fatal(err)
	}
	return fx
}

func TestActiveGraphEvidenceLifecycle(t *testing.T) {
	fx := newActiveGraphFixture(t)
	s, ctx, repoID := fx.s, fx.ctx, fx.repoID

	t.Run("exports filter before pagination", func(t *testing.T) {
		syms := must(s.ExportSymbolsPage(ctx, repoID, 10, 0))
		if len(syms) != 2 {
			t.Fatalf("ExportSymbolsPage returned %d symbols, want 2 active symbols: %+v", len(syms), syms)
		}
		names := must(s.ExportDOTNodeNamesPage(ctx, repoID, 10, 0))
		if len(names) != 2 {
			t.Fatalf("ExportDOTNodeNamesPage = %v, want two active names", names)
		}
		if page := must(s.ExportSymbolsPage(ctx, repoID, 1, 0)); len(page) != 1 || page[0].ID != fx.a {
			t.Fatalf("ExportSymbolsPage(limit=1) = %+v, want first active symbol", page)
		}
		if page := must(s.ExportDOTNodeNamesPage(ctx, repoID, 1, 0)); len(page) != 1 || page[0] != "pkg.A" {
			t.Fatalf("ExportDOTNodeNamesPage(limit=1) = %v, want [pkg.A]", page)
		}
		edges := must(s.ExportEdgesPage(ctx, repoID, 1, 0))
		if len(edges) != 1 || edges[0].ID != fx.visible {
			t.Fatalf("ExportEdgesPage(limit=1) = %+v, want visible edge %d", edges, fx.visible)
		}
		all := must(s.ExportEdgesPage(ctx, repoID, 10, 0))
		if len(all) != 2 || all[1].ID != fx.unresolved || all[1].DstSymbolID != nil || all[1].DstName != "missing.Live" {
			t.Fatalf("ExportEdgesPage = %+v, want visible resolved and active unresolved edges", all)
		}
	})

	t.Run("snapshot is coherent", func(t *testing.T) {
		syms, edges, err := s.GraphSnapshot(ctx, repoID, "", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(syms) != 2 || len(edges) != 2 {
			t.Fatalf("GraphSnapshot = %d symbols, %d edges; want 2, 2", len(syms), len(edges))
		}
		focused, focusedEdges, err := s.GraphSnapshot(ctx, repoID, "pkg.A", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(focused) != 2 || len(focusedEdges) != 2 {
			t.Fatalf("focused GraphSnapshot = %d symbols, %d edges; want 2, 2", len(focused), len(focusedEdges))
		}
	})

	t.Run("traversals exclude ghost bridge", func(t *testing.T) {
		trace := must(s.TraceDependenciesResult(ctx, repoID, "pkg.A", "downstream", 3, 10, 0))
		if !trace.TargetFound || trace.Total != 2 {
			t.Fatalf("TraceDependenciesResult = %+v, want active A and B only", trace)
		}
		tracePage := must(s.TraceDependenciesResult(ctx, repoID, "pkg.A", "downstream", 3, 1, 1))
		if tracePage.Total != 2 || len(tracePage.Dependencies) != 1 || tracePage.Dependencies[0]["symbol"] != "pkg.B" {
			t.Fatalf("TraceDependenciesResult(limit=1, offset=1) = %+v, want pkg.B", tracePage)
		}
		missing := must(s.TraceDependenciesResult(ctx, repoID, "aaa.Ghost", "downstream", 3, 10, 0))
		if missing.TargetFound || missing.Total != 0 || len(missing.Dependencies) != 0 {
			t.Fatalf("deleted trace seed = %+v, want missing", missing)
		}
		impact := must(s.ImpactRadius(ctx, repoID, []string{"pkg.A"}, nil, 3, 10, 0))
		summary := impact["summary"].(map[string]any)
		if summary["affected_symbols"] != 2 || summary["affected_files"] != 2 || summary["unresolved_edges"] != 1 {
			t.Fatalf("ImpactRadius counts = %+v, want two active symbols/files", impact)
		}
		impactPage := must(s.ImpactRadius(ctx, repoID, []string{"pkg.A"}, nil, 3, 1, 1))
		pageSymbols := impactPage["symbols"].([]graph.Symbol)
		if impactPage["summary"].(map[string]any)["affected_symbols"] != 2 || len(pageSymbols) != 1 || pageSymbols[0].QualifiedName != "pkg.B" {
			t.Fatalf("ImpactRadius(limit=1, offset=1) = %+v, want pkg.B", impactPage)
		}
		deletedFile := must(s.ImpactRadius(ctx, repoID, nil, []string{"deleted.go"}, 1, 10, 0))
		presence := deletedFile["seed_presence"].(ImpactSeedPresence)
		if presence.Found != 0 || len(presence.Missing) != 1 {
			t.Fatalf("deleted file seed presence = %+v, want missing", presence)
		}

		clean, cleanRepo := newQueryTestStore(t)
		ca := must(insertTestFile(ctx, clean, cleanRepo, "active_a.go"))
		cb := must(insertTestFile(ctx, clean, cleanRepo, "active_b.go"))
		as := must(insertTestSymbol(ctx, clean, cleanRepo, ca, "A", "pkg.A"))
		bs := must(insertTestSymbol(ctx, clean, cleanRepo, cb, "B", "pkg.B"))
		resolved := must(insertTestEdge(ctx, clean, cleanRepo, ca, as, "pkg.B"))
		if _, err := clean.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, bs, resolved); err != nil {
			t.Fatal(err)
		}
		must(insertTestEdge(ctx, clean, cleanRepo, cb, bs, "missing.Live"))
		cleanTrace := must(clean.TraceDependenciesResult(ctx, cleanRepo, "pkg.A", "downstream", 3, 10, 0))
		if !reflect.DeepEqual(trace.Dependencies, cleanTrace.Dependencies) || trace.Total != cleanTrace.Total {
			t.Fatalf("trace ghost/clean parity mismatch:\nghost=%+v\nclean=%+v", trace, cleanTrace)
		}
		cleanImpact := must(clean.ImpactRadius(ctx, cleanRepo, []string{"pkg.A"}, nil, 3, 10, 0))
		ghostSummary, cleanSummary := impact["summary"].(map[string]any), cleanImpact["summary"].(map[string]any)
		ghostFiles, cleanFiles := impact["files"].([]string), cleanImpact["files"].([]string)
		ghostSymbols, cleanSymbols := impact["symbols"].([]graph.Symbol), cleanImpact["symbols"].([]graph.Symbol)
		ghostNames, cleanNames := make([]string, len(ghostSymbols)), make([]string, len(cleanSymbols))
		for i := range ghostSymbols {
			ghostNames[i] = ghostSymbols[i].QualifiedName
		}
		for i := range cleanSymbols {
			cleanNames[i] = cleanSymbols[i].QualifiedName
		}
		if !reflect.DeepEqual(ghostNames, cleanNames) || !reflect.DeepEqual(ghostFiles, cleanFiles) || !reflect.DeepEqual(ghostSummary, cleanSummary) {
			t.Fatalf("impact ghost/clean parity mismatch:\nghost=%+v\nclean=%+v", impact, cleanImpact)
		}
	})

	t.Run("metadata uses visible evidence", func(t *testing.T) {
		stats := must(s.Stats(ctx, repoID))
		if stats.Symbols != 2 || stats.Edges != 2 || stats.References != 1 {
			t.Fatalf("Stats = %+v, want symbols=2 edges=2 references=1", stats)
		}
		overview := must(s.ArchitectureOverview(ctx, repoID))
		totals := overview["totals"].(map[string]any)
		if totals["symbols"] != int64(2) || totals["edges"] != int64(2) || totals["references"] != int64(1) {
			t.Fatalf("Architecture totals = %+v, want active graph counts", totals)
		}
	})
}

func TestActiveGraphAnalyticsExcludeDeletedEvidence(t *testing.T) {
	fx := newActiveGraphFixture(t)
	for range 3 {
		id := must(insertTestEdge(fx.ctx, fx.s, fx.repoID, fx.activeA, fx.a, "pkg.B"))
		if _, err := fx.s.db.ExecContext(fx.ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, fx.b, id); err != nil {
			t.Fatal(err)
		}
	}
	coupling := must(fx.s.CouplingMetrics(fx.ctx, fx.repoID, 10))
	if len(coupling) != 1 || coupling[0]["edge_count"] != 4 || coupling[0]["coupling"] != "low" {
		t.Fatalf("CouplingMetrics = %+v, want four visible edges and low classification", coupling)
	}

	// The deleted node is necessary for A -> Ghost -> B -> A. C <-> D is an
	// independent active positive control and must remain the only cycle.
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	file := func(path string) int64 { return must(insertTestFile(ctx, s, repoID, path)) }
	symbol := func(fileID int64, name string) int64 {
		return must(insertTestSymbol(ctx, s, repoID, fileID, name, "pkg."+name))
	}
	fa, fb, fc, fd, fg := file("a.go"), file("b.go"), file("c.go"), file("d.go"), file("ghost.go")
	a, b, c, d, ghost := symbol(fa, "A"), symbol(fb, "B"), symbol(fc, "C"), symbol(fd, "D"), symbol(fg, "Ghost")
	link := func(fileID, src, dst int64) {
		t.Helper()
		id := must(insertTestEdge(ctx, s, repoID, fileID, src, "target"))
		if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, dst, id); err != nil {
			t.Fatal(err)
		}
	}
	link(fa, a, ghost)
	link(fg, ghost, b)
	link(fb, b, a)
	link(fc, c, d)
	link(fd, d, c)
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, fg); err != nil {
		t.Fatal(err)
	}
	cycles := must(s.DetectCycles(ctx, repoID, 10))
	if len(cycles) != 1 || cycles[0]["length"] != 2 {
		t.Fatalf("DetectCycles = %+v, want only the active C/D cycle", cycles)
	}
	for _, name := range cycles[0]["cycle"].([]string) {
		if name == "ghost.go" || name == "a.go" || name == "b.go" {
			t.Fatalf("deleted bridge created a cycle: %+v", cycles)
		}
	}
}

func TestActiveGraphOutputsTrackReactivation(t *testing.T) {
	fx := newActiveGraphFixture(t)
	check := func(stage string, active bool) {
		t.Helper()
		syms, edges, err := fx.s.GraphSnapshot(fx.ctx, fx.repoID, "", 2)
		if err != nil {
			t.Fatal(err)
		}
		stats := must(fx.s.Stats(fx.ctx, fx.repoID))
		trace := must(fx.s.TraceDependenciesResult(fx.ctx, fx.repoID, "aaa.Ghost", "downstream", 1, 10, 0))
		wantSymbols, wantEdges, wantRefs := 2, 2, 1
		if active {
			wantSymbols, wantEdges, wantRefs = 4, 6, 2
		}
		if len(syms) != wantSymbols || len(edges) != wantEdges || stats.Symbols != int64(wantSymbols) || stats.Edges != int64(wantEdges) || stats.References != int64(wantRefs) || trace.TargetFound != active {
			t.Fatalf("%s: snapshot=%d/%d stats=%+v trace_found=%v", stage, len(syms), len(edges), stats, trace.TargetFound)
		}
	}
	check("soft-deleted", false)
	if _, err := fx.s.db.ExecContext(fx.ctx, `UPDATE files SET is_deleted = 0 WHERE id = ?`, fx.dead); err != nil {
		t.Fatal(err)
	}
	check("reactivated", true)
	if _, err := fx.s.db.ExecContext(fx.ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, fx.dead); err != nil {
		t.Fatal(err)
	}
	check("deleted again", false)
}

func TestActiveGraphQueriesDoNotMutate(t *testing.T) {
	fx := newActiveGraphFixture(t)
	snapshot := func() map[string]string {
		t.Helper()
		queries := map[string]string{
			"files":          `SELECT COALESCE(group_concat(v, ','), '') FROM (SELECT printf('%d:%d', id, is_deleted) v FROM files ORDER BY id)`,
			"symbols":        `SELECT COALESCE(group_concat(v, ','), '') FROM (SELECT printf('%d:%d', id, file_id) v FROM symbols ORDER BY id)`,
			"edges":          `SELECT COALESCE(group_concat(v, ','), '') FROM (SELECT printf('%d:%d:%d:%d', id, src_symbol_id, COALESCE(dst_symbol_id, -1), file_id) v FROM edges ORDER BY id)`,
			"references_tbl": `SELECT COALESCE(group_concat(v, ','), '') FROM (SELECT printf('%d:%d:%d', id, file_id, COALESCE(symbol_id, -1)) v FROM references_tbl ORDER BY id)`,
			"test_links":     `SELECT COALESCE(group_concat(v, ','), '') FROM (SELECT printf('%d:%d:%d:%d', id, test_file_id, COALESCE(target_file_id, -1), COALESCE(target_symbol_id, -1)) v FROM test_links ORDER BY id)`,
		}
		out := make(map[string]string, len(queries))
		for table, query := range queries {
			var value string
			if err := fx.s.db.QueryRowContext(fx.ctx, query).Scan(&value); err != nil {
				t.Fatal(err)
			}
			out[table] = value
		}
		return out
	}
	before := snapshot()
	must(fx.s.ExportSymbolsPage(fx.ctx, fx.repoID, 10, 0))
	must(fx.s.ExportEdgesPage(fx.ctx, fx.repoID, 10, 0))
	_, _, err := fx.s.GraphSnapshot(fx.ctx, fx.repoID, "pkg.A", 2)
	if err != nil {
		t.Fatal(err)
	}
	must(fx.s.TraceDependenciesResult(fx.ctx, fx.repoID, "pkg.A", "both", 2, 10, 0))
	must(fx.s.ImpactRadius(fx.ctx, fx.repoID, []string{"pkg.A"}, nil, 2, 10, 0))
	must(fx.s.CouplingMetrics(fx.ctx, fx.repoID, 10))
	must(fx.s.DetectCycles(fx.ctx, fx.repoID, 10))
	must(fx.s.Stats(fx.ctx, fx.repoID))
	must(fx.s.ArchitectureOverview(fx.ctx, fx.repoID))
	if after := snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("graph queries mutated persisted evidence:\nbefore=%v\nafter=%v", before, after)
	}
}
