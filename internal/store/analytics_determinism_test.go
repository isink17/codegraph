package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// analyticsGraph is a graph described semantically -- by file and qualified
// name, never by row id -- so the same graph can be inserted into two databases
// in two different orders and still be the same graph.
type analyticsGraph struct {
	files []string
	// symbols is file -> qualified names.
	symbols map[string][]string
	// edges is src qualified name -> dst qualified names.
	edges map[string][]string
}

// newAnalyticsGraph is a shape chosen to make every analysis do real work and
// to make ties unavoidable:
//
//   - hub.Hub is called by eight symbols, so it wins pagerank outright;
//   - the eight callers are structurally identical, so their ranks tie exactly
//     and something other than the score has to order them;
//   - cyc/a.go -> cyc/b.go -> cyc/c.go -> cyc/a.go is a cycle;
//   - two file pairs share the same cross-file edge count, so coupling ties.
func newAnalyticsGraph() analyticsGraph {
	g := analyticsGraph{
		symbols: map[string][]string{},
		edges:   map[string][]string{},
	}
	add := func(file, qname string) {
		if _, ok := g.symbols[file]; !ok {
			g.files = append(g.files, file)
		}
		g.symbols[file] = append(g.symbols[file], qname)
	}
	add("hub/hub.go", "hub.Hub")
	for i := range 8 {
		file := fmt.Sprintf("caller%d/caller.go", i)
		qname := fmt.Sprintf("caller%d.Call", i)
		add(file, qname)
		g.edges[qname] = []string{"hub.Hub"}
	}
	// A cycle at file level.
	add("cyc/a.go", "cyc.A")
	add("cyc/b.go", "cycb.B")
	add("cyc/c.go", "cycc.C")
	g.edges["cyc.A"] = []string{"cycb.B"}
	g.edges["cycb.B"] = []string{"cycc.C"}
	g.edges["cycc.C"] = []string{"cyc.A"}
	return g
}

// build materializes the graph, walking files, symbols and edges in the order
// the permutation gives. Row ids therefore differ between two permutations
// while the graph does not.
func (g analyticsGraph) build(t *testing.T, perm func(int) int) (*Store, int64) {
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

	fileIDs := map[string]int64{}
	symIDs := map[string]int64{}
	order := permute(g.files, perm)
	for _, f := range order {
		fid, err := insertTestFile(ctx, s, repo.ID, f)
		if err != nil {
			t.Fatalf("insertTestFile(%q) error = %v", f, err)
		}
		fileIDs[f] = fid
	}
	for _, f := range order {
		for _, qname := range g.symbols[f] {
			id, err := insertTestSymbol(ctx, s, repo.ID, fileIDs[f], lookupSymbolShortName(qname), qname)
			if err != nil {
				t.Fatalf("insertTestSymbol(%q) error = %v", qname, err)
			}
			symIDs[qname] = id
		}
	}
	srcs := make([]string, 0, len(g.edges))
	for _, f := range g.files {
		for _, qname := range g.symbols[f] {
			if len(g.edges[qname]) > 0 {
				srcs = append(srcs, qname)
			}
		}
	}
	for _, src := range permute(srcs, perm) {
		for _, dst := range g.edges[src] {
			if _, err := s.db.ExecContext(ctx, `
				INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
				VALUES(?, ?, ?, ?, 'call', '', ?, 1)
			`, repo.ID, symIDs[src], symIDs[dst], dst, fileIDs[fileOf(g, src)]); err != nil {
				t.Fatalf("insert edge %s -> %s error = %v", src, dst, err)
			}
		}
	}
	return s, repo.ID
}

func fileOf(g analyticsGraph, qname string) string {
	for _, f := range g.files {
		for _, q := range g.symbols[f] {
			if q == qname {
				return f
			}
		}
	}
	return g.files[0]
}

// permute reorders a slice by index mapping, without mutating the input.
func permute(in []string, perm func(int) int) []string {
	out := make([]string, len(in))
	for i := range in {
		out[i] = in[perm(i)%len(in)]
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(in))
	for _, v := range out {
		if !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	// A permutation function that collides still has to produce every element.
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	return uniq
}

func analyticsJSON(t *testing.T, s *Store, repoID int64, analysis string, limit int) string {
	t.Helper()
	ctx := context.Background()
	var (
		rows []map[string]any
		err  error
	)
	switch analysis {
	case "pagerank":
		rows, err = s.PageRank(ctx, repoID, limit)
	case "coupling":
		rows, err = s.CouplingMetrics(ctx, repoID, limit)
	case "cycles":
		rows, err = s.DetectCycles(ctx, repoID, limit)
	default:
		t.Fatalf("unknown analysis %q", analysis)
	}
	if err != nil {
		t.Fatalf("%s error = %v", analysis, err)
	}
	blob, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal %s error = %v", analysis, err)
	}
	return string(blob)
}

// Two databases holding the same graph, built in different orders, must answer
// identically. Row ids differ between them by construction, so any tie-break
// that reaches for one -- or any arithmetic ordered by one -- fails here.
func TestGraphAnalyticsIsInsertionOrderIndependent(t *testing.T) {
	g := newAnalyticsGraph()
	forward, repoA := g.build(t, func(i int) int { return i })
	reverse, repoB := g.build(t, func(i int) int { return len(g.files) - 1 - i })

	for _, analysis := range []string{"pagerank", "coupling", "cycles"} {
		for _, limit := range []int{3, 5, 20} {
			a := analyticsJSON(t, forward, repoA, analysis, limit)
			b := analyticsJSON(t, reverse, repoB, analysis, limit)
			if a != b {
				t.Fatalf("%s limit=%d differs by insertion order:\nforward: %s\nreverse: %s", analysis, limit, a, b)
			}
		}
	}
}

// The same call on the same database must answer identically every time. This
// is what the P16 gateway parity waiver was standing in for.
func TestGraphAnalyticsIsRepeatable(t *testing.T) {
	g := newAnalyticsGraph()
	s, repoID := g.build(t, func(i int) int { return i })
	for _, analysis := range []string{"pagerank", "coupling", "cycles"} {
		want := analyticsJSON(t, s, repoID, analysis, 5)
		for i := range 20 {
			if got := analyticsJSON(t, s, repoID, analysis, 5); got != want {
				t.Fatalf("%s call %d differs:\nfirst: %s\nnow  : %s", analysis, i+2, want, got)
			}
		}
	}
}

// The tie-break has to decide *membership*, not just order: eight symbols tie
// exactly for pagerank, and a limit of 3 has to pick the same three every time.
func TestPageRankTiedPageMembershipIsStable(t *testing.T) {
	g := newAnalyticsGraph()
	forward, repoA := g.build(t, func(i int) int { return i })
	reverse, repoB := g.build(t, func(i int) int { return len(g.files) - 1 - i })

	full := analyticsJSON(t, forward, repoA, "pagerank", 20)
	var all []map[string]any
	if err := json.Unmarshal([]byte(full), &all); err != nil {
		t.Fatalf("unmarshal error = %v", err)
	}
	tied := 0
	for _, row := range all {
		if row["rank"] == all[1]["rank"] {
			tied++
		}
	}
	if tied < 2 {
		t.Fatalf("fixture produced no pagerank ties; the test proves nothing: %s", full)
	}
	for _, limit := range []int{2, 3, 4, 6} {
		a := analyticsJSON(t, forward, repoA, "pagerank", limit)
		b := analyticsJSON(t, reverse, repoB, "pagerank", limit)
		if a != b {
			t.Fatalf("pagerank limit=%d page membership differs:\nforward: %s\nreverse: %s", limit, a, b)
		}
	}
}

// A limited page must be a prefix of the unlimited answer. Without that, "top
// 3" and "top 20" disagree about what the top 3 are.
func TestPageRankLimitedPageIsAPrefix(t *testing.T) {
	g := newAnalyticsGraph()
	s, repoID := g.build(t, func(i int) int { return i })
	var full []map[string]any
	if err := json.Unmarshal([]byte(analyticsJSON(t, s, repoID, "pagerank", 50)), &full); err != nil {
		t.Fatalf("unmarshal error = %v", err)
	}
	for _, limit := range []int{1, 3, 7} {
		var page []map[string]any
		if err := json.Unmarshal([]byte(analyticsJSON(t, s, repoID, "pagerank", limit)), &page); err != nil {
			t.Fatalf("unmarshal error = %v", err)
		}
		if len(page) != min(limit, len(full)) {
			t.Fatalf("limit=%d returned %d rows, want %d", limit, len(page), min(limit, len(full)))
		}
		for i := range page {
			if fmt.Sprint(page[i]) != fmt.Sprint(full[i]) {
				t.Fatalf("limit=%d row %d = %v, full answer has %v", limit, i, page[i], full[i])
			}
		}
	}
}

// newFloatSensitiveGraph aims at the *arithmetic* rather than the tie-break.
//
// Two targets receive from source sets with the same multiset of out-degrees
// {3, 7, 11, 13}, so their ranks are mathematically equal while each is a sum
// of four different doubles -- the shape where a changed summation order could
// leave two mathematically-tied nodes bitwise unequal, so that the identity
// tie-break never sees a tie to break.
//
// Be honest about what this proves. Reverting PageRank to map-order
// accumulation does not make it fail: the sums here turn out to be
// order-insensitive in practice. It is a determinism assertion over a shape
// designed to be fragile, not a regression test with a demonstrated failing
// case behind it.
func newFloatSensitiveGraph() analyticsGraph {
	g := analyticsGraph{symbols: map[string][]string{}, edges: map[string][]string{}}
	add := func(file, qname string) {
		if _, ok := g.symbols[file]; !ok {
			g.files = append(g.files, file)
		}
		g.symbols[file] = append(g.symbols[file], qname)
	}
	degrees := []int{3, 7, 11, 13}
	for _, target := range []string{"t1", "t2"} {
		add(target+"/target.go", target+".Target")
		for si, deg := range degrees {
			src := fmt.Sprintf("%s.Src%d", target, si)
			add(fmt.Sprintf("%s/src%d.go", target, si), src)
			dsts := []string{target + ".Target"}
			for f := 1; f < deg; f++ {
				filler := fmt.Sprintf("%s.Fill%d_%d", target, si, f)
				add(fmt.Sprintf("%s/fill%d_%d.go", target, si, f), filler)
				dsts = append(dsts, filler)
			}
			g.edges[src] = dsts
		}
	}
	return g
}

// The two targets are mathematically tied, so their relative order must come
// from the identity tie-break -- which requires the scores to be bitwise equal,
// which requires the summation order to be fixed.
func TestPageRankTiesSurviveFloatingPointAccumulation(t *testing.T) {
	g := newFloatSensitiveGraph()
	forward, repoA := g.build(t, func(i int) int { return i })
	reverse, repoB := g.build(t, func(i int) int { return len(g.files) - 1 - i })

	rankOf := func(s *Store, repoID int64) []map[string]any {
		var rows []map[string]any
		if err := json.Unmarshal([]byte(analyticsJSON(t, s, repoID, "pagerank", 200)), &rows); err != nil {
			t.Fatalf("unmarshal error = %v", err)
		}
		return rows
	}
	a, b := rankOf(forward, repoA), rankOf(reverse, repoB)
	if len(a) != len(b) {
		t.Fatalf("row counts differ: %d vs %d", len(a), len(b))
	}
	var seenTargets int
	for i := range a {
		if fmt.Sprint(a[i]) != fmt.Sprint(b[i]) {
			t.Fatalf("row %d differs by insertion order:\nforward: %v\nreverse: %v", i, a[i], b[i])
		}
		if a[i]["symbol"] == "t1.Target" || a[i]["symbol"] == "t2.Target" {
			seenTargets++
		}
	}
	if seenTargets != 2 {
		t.Fatalf("fixture lost its tied targets; the test proves nothing")
	}
}

// The coupling page's order is total by construction: the tie-break is the pair
// of grouping keys, which are unique per row. This asserts the contract
// directly, because the insertion-order fixture above cannot distinguish it --
// SQLite's GROUP BY already emits key order on this shape, so removing the
// tie-break happens to produce the same rows. The ORDER BY is what makes that a
// guarantee rather than a coincidence of the query plan.
func TestCouplingPageIsTotallyOrdered(t *testing.T) {
	g := newAnalyticsGraph()
	s, repoID := g.build(t, func(i int) int { return i })
	rows, err := s.CouplingMetrics(context.Background(), repoID, 50)
	if err != nil {
		t.Fatalf("CouplingMetrics() error = %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("fixture produced %d coupling rows; the test proves nothing", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		pc, cc := prev["edge_count"].(int), cur["edge_count"].(int)
		if pc < cc {
			t.Fatalf("row %d has a higher edge_count than row %d", i, i-1)
		}
		if pc != cc {
			continue
		}
		pa, ca := prev["file_a"].(string), cur["file_a"].(string)
		if pa > ca || (pa == ca && prev["file_b"].(string) >= cur["file_b"].(string)) {
			t.Fatalf("tied rows %d and %d are not in path order: %v then %v", i-1, i, prev, cur)
		}
	}
}
