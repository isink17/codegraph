package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// Semantic parity and determinism for the P12 query rewrites.
//
// The rewrites moved dedup, ordering and paging from Go into SQL and replaced a
// per-name cascade with a batched one. All of that is only acceptable if the
// answers are identical, so these tests check the answers against independent
// oracles rather than against the new implementation's own output.

func newQueryTestStore(t *testing.T) (*Store, int64) {
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
	return s, repo.ID
}

func qnames(syms []graph.Symbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.QualifiedName)
	}
	return out
}

// callersOracle recomputes the caller set independently of the production SQL:
// union the resolved-edge leg with the P22.1 name-evidence rule (exact
// spellings, boundary-suffix spellings of the target identities, and spellings
// that extend an identity at a boundary), deduplicate by id, load, sort in Go,
// page. The pre-P22.1 oracle matched `%.` + bare short here, which is exactly
// the qualifier-discarding claim the production query no longer makes.
func callersOracle(t *testing.T, s *Store, repoID int64, symbol string, limit, offset int) []graph.Symbol {
	t.Helper()
	ctx := context.Background()
	targetIDs, err := s.lookupSymbolIDs(ctx, repoID, symbol, 0)
	if err != nil {
		t.Fatalf("lookupSymbolIDs: %v", err)
	}
	short := lookupSymbolShortName(symbol)

	seen := map[int64]bool{}
	var ids []int64
	collect := func(query string, args ...any) {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("oracle query: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("oracle scan: %v", err)
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	for _, id := range targetIDs {
		collect(`SELECT DISTINCT src_symbol_id FROM edges WHERE repo_id = ? AND dst_symbol_id = ?`, repoID, id)
	}
	if short != "" {
		qnames := []string{symbol}
		targetQNames, err := s.qualifiedNamesByIDs(ctx, repoID, targetIDs)
		if err != nil {
			t.Fatalf("oracle qnames: %v", err)
		}
		qnames = append(qnames, targetQNames...)
		claimed := map[string]bool{symbol: true, short: true}
		for _, qname := range qnames {
			for _, spelling := range boundaryProperSuffixes(qname) {
				claimed[spelling] = true
			}
		}
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT src_symbol_id, dst_name FROM edges
			WHERE repo_id = ? AND dst_symbol_id IS NULL`, repoID)
		if err != nil {
			t.Fatalf("oracle name query: %v", err)
		}
		for rows.Next() {
			var id int64
			var dst string
			if err := rows.Scan(&id, &dst); err != nil {
				t.Fatalf("oracle name scan: %v", err)
			}
			match := claimed[dst]
			for _, qname := range qnames {
				if match {
					break
				}
				match = foldedExtendsAtBoundary(dst, qname)
			}
			if match && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("oracle name rows: %v", err)
		}
	}

	var syms []graph.Symbol
	for _, id := range ids {
		rows, err := s.db.QueryContext(ctx, `
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM symbols s JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND s.id = ?`, repoID, id)
		if err != nil {
			t.Fatalf("oracle load: %v", err)
		}
		loaded, err := scanSymbols(rows)
		if err != nil {
			t.Fatalf("oracle scan symbols: %v", err)
		}
		syms = append(syms, loaded...)
	}
	sort.Slice(syms, func(i, j int) bool {
		if syms[i].QualifiedName != syms[j].QualifiedName {
			return syms[i].QualifiedName < syms[j].QualifiedName
		}
		if syms[i].Range.StartLine != syms[j].Range.StartLine {
			return syms[i].Range.StartLine < syms[j].Range.StartLine
		}
		if syms[i].Range.StartCol != syms[j].Range.StartCol {
			return syms[i].Range.StartCol < syms[j].Range.StartCol
		}
		return syms[i].ID < syms[j].ID
	})
	if offset >= len(syms) {
		return []graph.Symbol{}
	}
	syms = syms[offset:]
	if limit < len(syms) {
		syms = syms[:limit]
	}
	return syms
}

// seedCallerGraph builds a graph where the target is reached both by bound
// edges and by unresolved ones spelled three different ways, plus a decoy that
// must not match.
func seedCallerGraph(t *testing.T, s *Store, repoID int64) {
	t.Helper()
	ctx := context.Background()
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	targetID, err := insertTestSymbol(ctx, s, repoID, fileID, "Target", "pkg.Target")
	if err != nil {
		t.Fatalf("insertTestSymbol(target): %v", err)
	}

	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("Caller%02d", i)
		id, err := insertTestSymbol(ctx, s, repoID, fileID, name, "pkg."+name)
		if err != nil {
			t.Fatalf("insertTestSymbol(%s): %v", name, err)
		}
		switch i % 4 {
		case 0: // bound edge
			if _, err := s.db.ExecContext(ctx, `
				INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
				VALUES(?, ?, ?, 'pkg.Target', 'call', '', ?, 1)`, repoID, id, targetID, fileID); err != nil {
				t.Fatalf("insert bound edge: %v", err)
			}
			// A second bound edge from the same caller: the result must not
			// contain the caller twice.
			if _, err := s.db.ExecContext(ctx, `
				INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
				VALUES(?, ?, ?, 'pkg.Target', 'call', '', ?, 2)`, repoID, id, targetID, fileID); err != nil {
				t.Fatalf("insert duplicate bound edge: %v", err)
			}
		case 1: // unresolved, fully qualified
			if _, err := insertTestEdge(ctx, s, repoID, fileID, id, "pkg.Target"); err != nil {
				t.Fatalf("insert unresolved edge: %v", err)
			}
		case 2: // unresolved, extends the target identity at a boundary
			if _, err := insertTestEdge(ctx, s, repoID, fileID, id, "other.pkg.Target"); err != nil {
				t.Fatalf("insert suffix edge: %v", err)
			}
		case 3: // unresolved, foreign scope qualifier: must NOT claim (P22.1)
			if _, err := insertTestEdge(ctx, s, repoID, fileID, id, "ns::Target"); err != nil {
				t.Fatalf("insert colon edge: %v", err)
			}
		}
	}
	// Decoy: a name that merely contains the target name must not match.
	decoy, err := insertTestSymbol(ctx, s, repoID, fileID, "Decoy", "pkg.Decoy")
	if err != nil {
		t.Fatalf("insertTestSymbol(decoy): %v", err)
	}
	if _, err := insertTestEdge(ctx, s, repoID, fileID, decoy, "pkg.TargetHelper"); err != nil {
		t.Fatalf("insert decoy edge: %v", err)
	}
}

func TestFindCallersMatchesPreRewriteSemantics(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	seedCallerGraph(t, s, repoID)

	for _, tc := range []struct{ limit, offset int }{
		{20, 0}, {5, 0}, {5, 5}, {5, 10}, {5, 100}, {1, 2},
	} {
		got, err := s.FindCallers(ctx, repoID, "pkg.Target", 0, tc.limit, tc.offset)
		if err != nil {
			t.Fatalf("FindCallers(limit=%d offset=%d): %v", tc.limit, tc.offset, err)
		}
		want := callersOracle(t, s, repoID, "pkg.Target", tc.limit, tc.offset)
		if fmt.Sprint(qnames(got)) != fmt.Sprint(qnames(want)) {
			t.Errorf("FindCallers(limit=%d offset=%d) = %v, want %v", tc.limit, tc.offset, qnames(got), qnames(want))
		}
		if got == nil {
			t.Errorf("FindCallers(limit=%d offset=%d) returned nil, want empty slice", tc.limit, tc.offset)
		}
	}

	// The decoy must never appear.
	all, err := s.FindCallers(ctx, repoID, "pkg.Target", 0, 1000, 0)
	if err != nil {
		t.Fatalf("FindCallers(all): %v", err)
	}
	for _, sym := range all {
		if sym.QualifiedName == "pkg.Decoy" {
			t.Fatalf("decoy caller matched on a substring of the target name")
		}
	}
}

func TestFindCallersPagesArePartitionOfFullResult(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	seedCallerGraph(t, s, repoID)

	full, err := s.FindCallers(ctx, repoID, "pkg.Target", 0, 1000, 0)
	if err != nil {
		t.Fatalf("FindCallers(full): %v", err)
	}
	var paged []string
	for offset := 0; ; offset += 3 {
		page, err := s.FindCallers(ctx, repoID, "pkg.Target", 0, 3, offset)
		if err != nil {
			t.Fatalf("FindCallers(page %d): %v", offset, err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, qnames(page)...)
		if offset > 1000 {
			t.Fatal("pagination did not terminate")
		}
	}
	if fmt.Sprint(paged) != fmt.Sprint(qnames(full)) {
		t.Fatalf("paged traversal = %v, want %v", paged, qnames(full))
	}
	seen := map[string]bool{}
	for _, name := range paged {
		if seen[name] {
			t.Fatalf("caller %q returned on more than one page", name)
		}
		seen[name] = true
	}
}

func TestFindCalleesResolvedAndUnresolvedLegs(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	srcID, err := insertTestSymbol(ctx, s, repoID, fileID, "Src", "pkg.Src")
	if err != nil {
		t.Fatalf("insertTestSymbol(src): %v", err)
	}
	boundID, err := insertTestSymbol(ctx, s, repoID, fileID, "Bound", "pkg.Bound")
	if err != nil {
		t.Fatalf("insertTestSymbol(bound): %v", err)
	}
	if _, err := insertTestSymbol(ctx, s, repoID, fileID, "ByName", "pkg.ByName"); err != nil {
		t.Fatalf("insertTestSymbol(byname): %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, ?, 'pkg.Bound', 'call', '', ?, 1)`, repoID, srcID, boundID, fileID); err != nil {
		t.Fatalf("insert bound edge: %v", err)
	}
	// Unresolved edge that the name fallback should still surface.
	if _, err := insertTestEdge(ctx, s, repoID, fileID, srcID, "pkg.ByName"); err != nil {
		t.Fatalf("insert unresolved edge: %v", err)
	}
	// Unresolved edge that resolves to nothing.
	if _, err := insertTestEdge(ctx, s, repoID, fileID, srcID, "vendor.Absent"); err != nil {
		t.Fatalf("insert absent edge: %v", err)
	}

	got, err := s.FindCallees(ctx, repoID, "pkg.Src", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees: %v", err)
	}
	if want := "[pkg.Bound pkg.ByName]"; fmt.Sprint(qnames(got)) != want {
		t.Fatalf("FindCallees = %v, want %s", qnames(got), want)
	}
}

// TestLookupSymbolIDsByNameMemberCallSemantics pins the batched edge-evidence
// cascade per name. Since P22.1 it deliberately differs from the forgiving
// single-name cascade (`lookupSymbolIDs`, user input): a member-qualified
// spelling never degrades to its bare tail, so a foreign qualifier resolves to
// nothing instead of to every symbol sharing the tail. Import-path spellings
// (with '/') keep the legacy tail fallback -- the own-module class is deferred.
func TestLookupSymbolIDsByNameMemberCallSemantics(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	ids := map[string]int64{}
	for i, sd := range []struct{ name, qualified string }{
		{"Alpha", "pkg.Alpha"},
		{"Beta", "other.Beta"},
		{"Gamma", "deep.nested.Gamma"},
		{"Delta", "ns::Delta"},
		{"Epsilon", "pkg.Epsilon"},
		{"MixedCase", "pkg.mixedcase"},
		{"Shadow", "one.Shadow"},
		{"Shadow2", "two.Shadow"},
		{"do_work", "pkg.do_work"},
		{"Registry", "parser.NewRegistry"},
	} {
		id, err := insertTestSymbol(ctx, s, repoID, fileID, sd.name, sd.qualified)
		if err != nil {
			t.Fatalf("insertTestSymbol(%d): %v", i, err)
		}
		ids[sd.qualified] = id
	}

	cases := []struct {
		name string
		want []int64
	}{
		// Exact stages, unchanged.
		{"pkg.Alpha", []int64{ids["pkg.Alpha"]}},
		{"::pkg.Alpha", []int64{ids["pkg.Alpha"]}},
		{"Beta", []int64{ids["other.Beta"]}},
		// Member spelling whose qualifier a deeper identity confirms.
		{"nested.Gamma", []int64{ids["deep.nested.Gamma"]}},
		// Member spelling that extends an identity at a boundary: the
		// identity is one of the spelling's own qualified suffixes.
		{"x.deep.nested.Gamma", []int64{ids["deep.nested.Gamma"]}},
		{"unknown.pkg.Epsilon", []int64{ids["pkg.Epsilon"]}},
		// Foreign qualifiers: no identity confirms them, nothing resolves.
		// Every one of these bound (or fanned out) via the bare tail before.
		{"whatever.Gamma", nil},
		{"whatever::Delta", nil},
		{"rows.Close", nil},
		{"pkg.MIXEDCASE", nil},
		{"anything.Shadow", nil},
		{"vendor.do_work", nil},
		{"nothing.AtAll", nil},
		// Import-path spelling: legacy tail fallback retained (deferred class).
		{"github.com/org/repo/internal/parser.NewRegistry", []int64{ids["parser.NewRegistry"]}},
	}

	names := make([]nameLanguages, 0, len(cases))
	for _, tc := range cases {
		names = append(names, nameLanguages{name: tc.name, languages: []string{"go"}})
	}
	resolved, err := s.lookupSymbolIDsByNameLanguage(ctx, repoID, names)
	if err != nil {
		t.Fatalf("lookupSymbolIDsByNameLanguage: %v", err)
	}
	for _, tc := range cases {
		got := resolved[symbolLangKey{name: trimLookupName(tc.name), language: "go"}]
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("lookupSymbolIDsByNameLanguage[%q] = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSymbolSearchOrderingIsStableAcrossPages(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("Widget%02d", i)
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name,
			                    start_line, start_col, end_line, end_col, stable_key, doc_summary)
			VALUES(?, ?, 'go', 'function', ?, ?, '', ?, 1, ?, 1, ?, 'widget helper')`,
			repoID, fileID, name, "pkg."+name, i+1, i+1, "go:pkg."+name); err != nil {
			t.Fatalf("insert symbol: %v", err)
		}
	}
	// symbol_fts is populated by the write path; seed it directly to match.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO symbol_fts(repo_id, symbol_id, name, qualified_name, signature, doc_summary)
		SELECT repo_id, id, name, qualified_name, signature, doc_summary FROM symbols WHERE repo_id = ?`, repoID); err != nil {
		t.Fatalf("seed fts: %v", err)
	}

	for _, fn := range []struct {
		label string
		call  func(limit, offset int) ([]graph.Symbol, error)
	}{
		{"SearchSymbols", func(limit, offset int) ([]graph.Symbol, error) {
			return s.SearchSymbols(ctx, repoID, "Widget", limit, offset)
		}},
	} {
		full, err := fn.call(100, 0)
		if err != nil {
			t.Fatalf("%s(full): %v", fn.label, err)
		}
		if len(full) < 25 {
			t.Fatalf("%s(full) returned %d symbols, want >= 25", fn.label, len(full))
		}
		if !sort.SliceIsSorted(full, func(i, j int) bool { return full[i].QualifiedName < full[j].QualifiedName }) {
			t.Fatalf("%s(full) is not ordered by qualified_name: %v", fn.label, qnames(full))
		}
		var paged []string
		for offset := 0; offset < len(full); offset += 7 {
			page, err := fn.call(7, offset)
			if err != nil {
				t.Fatalf("%s(page %d): %v", fn.label, offset, err)
			}
			paged = append(paged, qnames(page)...)
		}
		if fmt.Sprint(paged) != fmt.Sprint(qnames(full)) {
			t.Fatalf("%s paged = %v, want %v", fn.label, paged, qnames(full))
		}
		// Repeat runs must be byte-identical: no reliance on storage order.
		again, err := fn.call(100, 0)
		if err != nil {
			t.Fatalf("%s(repeat): %v", fn.label, err)
		}
		if fmt.Sprint(qnames(again)) != fmt.Sprint(qnames(full)) {
			t.Fatalf("%s is not deterministic across runs", fn.label)
		}
	}

	exact, err := s.FindSymbolExact(ctx, repoID, "Widget03", 10, 0)
	if err != nil {
		t.Fatalf("FindSymbolExact: %v", err)
	}
	if len(exact) != 1 || exact[0].QualifiedName != "pkg.Widget03" {
		t.Fatalf("FindSymbolExact = %v, want [pkg.Widget03]", qnames(exact))
	}
}

func TestArchitectureOverviewTopDegreeAndTotals(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	ids := map[string]int64{}
	for _, name := range []string{"Hot", "Warm", "Cold", "Src"} {
		id, err := insertTestSymbol(ctx, s, repoID, fileID, name, "pkg."+name)
		if err != nil {
			t.Fatalf("insertTestSymbol(%s): %v", name, err)
		}
		ids[name] = id
	}
	edge := func(src, dst int64) {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, '', 'call', '', ?, 1)`, repoID, src, dst, fileID); err != nil {
			t.Fatalf("insert edge: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		edge(ids["Src"], ids["Hot"])
	}
	edge(ids["Src"], ids["Warm"])

	overview, err := s.ArchitectureOverview(ctx, repoID)
	if err != nil {
		t.Fatalf("ArchitectureOverview: %v", err)
	}

	// Four symbols exist, so the padded list is four long: two with a degree,
	// then the zero-degree remainder -- exactly what the pre-P12 LEFT JOIN
	// with LIMIT 15 produced.
	entry := overview["entry_points"].([]map[string]any)
	if len(entry) != 4 {
		t.Fatalf("entry_points len = %d, want 4 (2 referenced + zero-degree padding)", len(entry))
	}
	if entry[0]["qualified_name"] != "pkg.Hot" || entry[0]["caller_count"] != 5 {
		t.Fatalf("entry_points[0] = %v, want pkg.Hot with 5 callers", entry[0])
	}
	if entry[1]["qualified_name"] != "pkg.Warm" || entry[1]["caller_count"] != 1 {
		t.Fatalf("entry_points[1] = %v, want pkg.Warm with 1 caller", entry[1])
	}
	// Everything after the referenced symbols is padded with zero-degree ones,
	// exactly as the pre-P12 LEFT JOIN did.
	for _, row := range entry[2:] {
		if row["caller_count"] != 0 {
			t.Fatalf("padding row %v has a non-zero degree", row)
		}
	}

	hub := overview["hub_symbols"].([]map[string]any)
	if hub[0]["qualified_name"] != "pkg.Src" || hub[0]["callee_count"] != 6 {
		t.Fatalf("hub_symbols[0] = %v, want pkg.Src with 6 callees", hub[0])
	}

	totals := overview["totals"].(map[string]any)
	stats, err := s.Stats(ctx, repoID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if totals["files"] != stats.Files || totals["symbols"] != stats.Symbols ||
		totals["edges"] != stats.Edges || totals["references"] != stats.References {
		t.Fatalf("totals %v disagree with Stats %+v", totals, stats)
	}
}

func TestListFilesMatchesMixedPersistedSeparators(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, `pkg\win.go`)
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET path = ? WHERE id = ?`, `pkg\win.go`, fileID); err != nil {
		t.Fatalf("set Windows path: %v", err)
	}
	rows, err := s.ListFiles(ctx, repoID, "pkg/", 20, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(rows) != 1 || rows[0]["path"] != filepath.ToSlash(`pkg\win.go`) {
		t.Fatalf("ListFiles = %v, want canonical mixed-path row", rows)
	}
}

// TestQueryPathsAreRepoIsolated pins that none of the rewritten paths can see
// another repository's rows.
func TestQueryPathsAreRepoIsolated(t *testing.T) {
	ctx := context.Background()
	s, repoA := newQueryTestStore(t)
	repoB, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo(B): %v", err)
	}

	seed := func(repoID int64, suffix string) int64 {
		fileID, err := insertTestFile(ctx, s, repoID, "a"+suffix+".go")
		if err != nil {
			t.Fatalf("insertTestFile: %v", err)
		}
		target, err := insertTestSymbol(ctx, s, repoID, fileID, "Target", "pkg.Target")
		if err != nil {
			t.Fatalf("insertTestSymbol: %v", err)
		}
		caller, err := insertTestSymbol(ctx, s, repoID, fileID, "Caller"+suffix, "pkg.Caller"+suffix)
		if err != nil {
			t.Fatalf("insertTestSymbol: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, 'pkg.Target', 'call', '', ?, 1)`, repoID, caller, target, fileID); err != nil {
			t.Fatalf("insert edge: %v", err)
		}
		if _, err := insertTestEdge(ctx, s, repoID, fileID, caller, "other.Target"); err != nil {
			t.Fatalf("insert unresolved edge: %v", err)
		}
		return target
	}
	seed(repoA, "A")
	seed(repoB.ID, "B")

	callers, err := s.FindCallers(ctx, repoA, "pkg.Target", 0, 100, 0)
	if err != nil {
		t.Fatalf("FindCallers: %v", err)
	}
	if want := "[pkg.CallerA]"; fmt.Sprint(qnames(callers)) != want {
		t.Fatalf("FindCallers leaked across repos: %v, want %s", qnames(callers), want)
	}

	dead, err := s.FindDeadCode(ctx, repoA, 100, 0)
	if err != nil {
		t.Fatalf("FindDeadCode: %v", err)
	}
	for _, row := range dead {
		if name, _ := row["file"].(string); name == "aB.go" {
			t.Fatalf("FindDeadCode leaked repo B file: %v", row)
		}
	}

	ids, err := s.lookupSymbolIDsForNameLanguages(ctx, repoA, []nameLanguages{
		{name: "pkg.Target", languages: []string{"go"}},
		{name: "x.Target", languages: []string{"go"}},
	})
	if err != nil {
		t.Fatalf("lookupSymbolIDsForNameLanguages: %v", err)
	}
	for _, id := range ids {
		var owner int64
		if err := s.db.QueryRowContext(ctx, `SELECT repo_id FROM symbols WHERE id = ?`, id).Scan(&owner); err != nil {
			t.Fatalf("owner lookup: %v", err)
		}
		if owner != repoA {
			t.Fatalf("lookupSymbolIDsForNameLanguages returned a symbol from repo %d", owner)
		}
	}
}

// TestArchitectureOverviewRejectsUnknownRepo pins that the overview still fails
// for a repository id that does not exist. Deriving the totals from the
// breakdowns removed the Stats call that used to provide that error, and a
// well-formed document full of zeroes is a worse answer than an error.
func TestArchitectureOverviewRejectsUnknownRepo(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	if _, err := s.ArchitectureOverview(ctx, repoID+9999); err == nil {
		t.Fatal("ArchitectureOverview on an unknown repo id returned no error")
	}
}

// TestSemanticSearchPagesArePartitionOfFullResult pins the ordering fix for
// token-overlap search: weights come from a small fixed set, so score alone
// leaves most rows tied and page boundaries land arbitrarily.
func TestSemanticSearchPagesArePartitionOfFullResult(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("Widget%02d", i)
		symID, err := insertTestSymbol(ctx, s, repoID, fileID, name, "pkg."+name)
		if err != nil {
			t.Fatalf("insertTestSymbol: %v", err)
		}
		// Identical token and weight for every symbol: every score ties.
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?, 'widget', 1.0)`, symID); err != nil {
			t.Fatalf("insert token: %v", err)
		}
	}

	full, err := s.SemanticSearch(ctx, repoID, "widget", 100, 0)
	if err != nil {
		t.Fatalf("SemanticSearch(full): %v", err)
	}
	if len(full) < 25 {
		t.Fatalf("SemanticSearch(full) returned %d rows, want >= 25", len(full))
	}
	symbolOf := func(rows []map[string]any) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, fmt.Sprint(r["symbol"]))
		}
		return out
	}
	var paged []string
	for offset := 0; offset < len(full); offset += 7 {
		page, err := s.SemanticSearch(ctx, repoID, "widget", 7, offset)
		if err != nil {
			t.Fatalf("SemanticSearch(page %d): %v", offset, err)
		}
		paged = append(paged, symbolOf(page)...)
	}
	if fmt.Sprint(paged) != fmt.Sprint(symbolOf(full)) {
		t.Fatalf("paged traversal = %v, want %v", paged, symbolOf(full))
	}
}

// TestSuffixMatcherAgreesWithSQLiteLike checks the Go stage-3 matcher against
// the authority it is replacing: SQLite's own LIKE. The matcher exists so the
// cascade's third stage costs one scan instead of one scan per name, which is
// only acceptable if it decides exactly what the SQL decided.
func TestSuffixMatcherAgreesWithSQLiteLike(t *testing.T) {
	ctx := context.Background()
	s, _ := newQueryTestStore(t)

	qnames := []string{
		"pkg.Target", "Target", "pkg.Type.Target", "ns::Target", "a::b::Target",
		"pkg.TargetHelper", "pkg.do_work", "pkg.doXwork", "pkg.do.work",
		"pkg.Foo.Bar", "x::Foo.Bar", "left.Fold", "right.fold", "PKG.TARGET",
		"internal/x/pkg.Target", ".Target", "::Target", "pkg.", "", "a.b.c.d",
		// A literal '%' in the value facing a '%' in the pattern: a matcher
		// that consumes the wildcard as a literal records no backtrack point
		// and can never recover.
		"pkg.pre%post", "pkg.prexpost", "pkg.pre%zpost", "x.a%%b", "x.a%b",
		// '_' matches one character, not one byte.
		"pkg.aéb", "pkg.a\u00e9\u00e9b", "pkg.日本語", "pkg.日x語", "a::b::c",
	}
	shorts := []string{
		"Target", "target", "TARGET", "do_work", "Foo.Bar", "Fold", "fold",
		"pre%post", "d", "work", "_arget", "%", "_", "Bar",
		"a%b", "a_b", "a__b", "b::c", "日_語", "a_", "", "%%",
	}

	likeInSQLite := func(value, pattern string) bool {
		t.Helper()
		var hit int
		err := s.db.QueryRowContext(ctx, `SELECT 1 WHERE ? LIKE ?`, value, pattern).Scan(&hit)
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatalf("SQLite LIKE(%q, %q): %v", value, pattern, err)
		}
		return hit == 1
	}

	matcher := newSuffixMatcher(shorts)
	for _, qname := range qnames {
		got := map[string]bool{}
		matcher.match(asciiLower(qname), func(short string) { got[short] = true })
		for _, short := range shorts {
			want := likeInSQLite(qname, "%."+short) || likeInSQLite(qname, "%::"+short)
			if got[short] != want {
				t.Errorf("qname=%q short=%q: matcher=%v, SQLite LIKE=%v", qname, short, got[short], want)
			}
		}
	}
}
