package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// neighborBudgetFixture is a repository whose neighbour candidate order is
// deliberately unrelated to the order of the ids the query batches.
//
// Pair i binds src i to dst i, but the qualified name of both is index
// (i*7)%n -- a bijection, so every name exists exactly once and the canonical
// sort of the candidates interleaves the transport batches instead of
// following them. A page read at an offset near a batch boundary is therefore
// wrong unless the ordering really is global.
type neighborBudgetFixture struct {
	srcIDs, dstIDs []int64
	srcNames       []string
	dstNames       []string
}

func buildNeighborBudgetFixture(t *testing.T, s *Store, repoID int64, n int) neighborBudgetFixture {
	t.Helper()
	ctx := context.Background()
	file, err := insertTestFile(ctx, s, repoID, "a/a.go")
	if err != nil {
		t.Fatal(err)
	}
	var fx neighborBudgetFixture
	for i := range n {
		srcName := fmt.Sprintf("a.S%04d", (i*7)%n)
		dstName := fmt.Sprintf("a.D%04d", (i*7)%n)
		src, err := insertTestSymbol(ctx, s, repoID, file, fmt.Sprintf("S%04d", i), srcName)
		if err != nil {
			t.Fatal(err)
		}
		dst, err := insertTestSymbol(ctx, s, repoID, file, fmt.Sprintf("D%04d", i), dstName)
		if err != nil {
			t.Fatal(err)
		}
		// Two identical edges for the same pair, so a candidate reached twice
		// must still surface once.
		for range 2 {
			insertResolvedTestEdge(t, s, repoID, file, src, dst, dstName)
		}
		fx.srcIDs = append(fx.srcIDs, src)
		fx.dstIDs = append(fx.dstIDs, dst)
		fx.srcNames = append(fx.srcNames, srcName)
		fx.dstNames = append(fx.dstNames, dstName)
	}
	// One source that reaches many destinations, and therefore one caller that
	// many targets reach: the cross-batch duplicate the candidate set must fold.
	for i := 1; i < min(n, 500); i++ {
		insertResolvedTestEdge(t, s, repoID, file, fx.srcIDs[0], fx.dstIDs[i], fx.dstNames[i])
	}
	slices.Sort(fx.srcNames)
	slices.Sort(fx.dstNames)
	return fx
}

func insertResolvedTestEdge(t *testing.T, s *Store, repoID, fileID, src, dst int64, dstName string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, ?, ?, 'call', '', ?, 1)`, repoID, src, dst, dstName, fileID); err != nil {
		t.Fatal(err)
	}
}

// guardNeighborStatements routes every statement the neighbour page pipeline
// issues through one budgetQuerier, whichever transport answers.
func guardNeighborStatements(s *Store) *budgetQuerier {
	guard := newBudgetQuerier(nil)
	s.neighborStatementWrap = func(target execQuerier) execQuerier {
		guard.inner = target
		return guard
	}
	return guard
}

func qualifiedNames(symbols []graph.Symbol) []string {
	out := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		out = append(out, sym.QualifiedName)
	}
	return out
}

func assertNeighborBudget(t *testing.T, guard *budgetQuerier) {
	t.Helper()
	if guard.maxArgs > sqliteInClauseBatchSize {
		t.Fatalf("max bound args = %d, want <= %d (hard limit %d)",
			guard.maxArgs, sqliteInClauseBatchSize, sqliteDefaultMaxVariables)
	}
}

// TestFindCalleesResolvedStaysInVariableBudget pins the resolved-callee
// transport: 2000 source ids used to bind 2006 arguments into one page
// statement, which is over the portable ceiling on any build that still uses
// 999.
func TestFindCalleesResolvedStaysInVariableBudget(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	const n = 2000
	fx := buildNeighborBudgetFixture(t, s, repo.ID, n)
	guard := guardNeighborStatements(s)

	page, err := s.findCalleesResolved(ctx, repo.ID, fx.srcIDs, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertNeighborBudget(t, guard)
	if got := qualifiedNames(page); !slices.Equal(got, fx.dstNames[:20]) {
		t.Fatalf("first page = %v, want %v", got, fx.dstNames[:20])
	}
	// Statement count is driven by the batch count, not by the id count.
	inserts := guard.matching("INSERT OR IGNORE INTO " + neighborKeysTable)
	wantInserts := (n + sqliteBatchSize(0, 2) - 1) / sqliteBatchSize(0, 2)
	if len(inserts) != wantInserts {
		t.Fatalf("staging statements = %d, want %d", len(inserts), wantInserts)
	}
	if len(guard.statements) > wantInserts+8 {
		t.Fatalf("neighbour statements = %d, want O(batches)", len(guard.statements))
	}
}

// TestFindCallersResolvedStaysInVariableBudget is the caller-side twin: the
// candidate relationship is edges.dst_symbol_id IN targetIDs and the selected
// symbol is src_symbol_id. Duplicates arrive both ways -- two edges for one
// pair, and one caller reached through 500 different targets.
func TestFindCallersResolvedStaysInVariableBudget(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	const n = 2000
	fx := buildNeighborBudgetFixture(t, s, repo.ID, n)
	guard := guardNeighborStatements(s)

	page, err := s.findCallersResolved(ctx, repo.ID, "", fx.dstIDs, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertNeighborBudget(t, guard)
	if got := qualifiedNames(page); !slices.Equal(got, fx.srcNames[:20]) {
		t.Fatalf("first page = %v, want %v", got, fx.srcNames[:20])
	}
	all := readAllNeighborPages(t, func(limit, offset int) []graph.Symbol {
		out, err := s.findCallersResolved(ctx, repo.ID, "", fx.dstIDs, limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}, 250)
	if !slices.Equal(qualifiedNames(all), fx.srcNames) {
		t.Fatalf("paged candidate set has %d names, want the %d distinct callers in canonical order",
			len(all), len(fx.srcNames))
	}
}

// TestFindCallersUnresolvedSpellingsStayInVariableBudget drives the real
// unknown-target path: no symbol is named `pkg.Target`, so the suffix scan
// discovers 1500 unresolved destination spellings that extend it, which the
// pre-P22.34 transport bound into one 1509-argument statement.
func TestFindCallersUnresolvedSpellingsStayInVariableBudget(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	const spellings = 1500
	file, err := insertTestFile(ctx, s, repo.ID, "a/a.go")
	if err != nil {
		t.Fatal(err)
	}
	caller, err := insertTestSymbol(ctx, s, repo.ID, file, "Caller", "a.Caller")
	if err != nil {
		t.Fatal(err)
	}
	for i := range spellings {
		if _, err := insertTestEdge(ctx, s, repo.ID, file, caller, fmt.Sprintf("m%04d.pkg.Target", i)); err != nil {
			t.Fatal(err)
		}
	}
	guard := guardNeighborStatements(s)

	page, err := s.FindCallers(ctx, repo.ID, "pkg.Target", 0, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertNeighborBudget(t, guard)
	if got := qualifiedNames(page); !slices.Equal(got, []string{"a.Caller"}) {
		t.Fatalf("unknown-target page = %v, want [a.Caller]", got)
	}
}

// TestNeighborPagingIsGlobalAcrossBatches is the semantic half of the
// transport change: batching the inputs must not page per batch. Consecutive
// pages, including one straddling a transport batch boundary, must reproduce
// the canonical ordering of the complete candidate set exactly once.
func TestNeighborPagingIsGlobalAcrossBatches(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	const n = 2000
	fx := buildNeighborBudgetFixture(t, s, repo.ID, n)

	page := func(limit, offset int) []graph.Symbol {
		out, err := s.findCalleesResolved(ctx, repo.ID, fx.srcIDs, limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, offset := range []int{0, 449, 450, 890, 899, 900, 1799, 1990} {
		got := qualifiedNames(page(20, offset))
		want := fx.dstNames[offset:min(offset+20, len(fx.dstNames))]
		if !slices.Equal(got, want) {
			t.Fatalf("page(20, %d) = %v, want %v", offset, got, want)
		}
	}
	if got := page(20, n); len(got) != 0 {
		t.Fatalf("page beyond the end returned %d rows, want 0", len(got))
	}
	if got := qualifiedNames(page(20, n-5)); !slices.Equal(got, fx.dstNames[n-5:]) {
		t.Fatalf("final short page = %v, want %v", got, fx.dstNames[n-5:])
	}
	all := readAllNeighborPages(t, page, 250)
	if !slices.Equal(qualifiedNames(all), fx.dstNames) {
		t.Fatal("concatenated pages do not equal the canonical global ordering")
	}
}

// TestNeighborInputOrderIndependence pins that batching is transport: the same
// id set in any order is the same page.
func TestNeighborInputOrderIndependence(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	fx := buildNeighborBudgetFixture(t, s, repo.ID, 2000)

	want, err := s.findCalleesResolved(ctx, repo.ID, fx.srcIDs, 20, 890)
	if err != nil {
		t.Fatal(err)
	}
	orders := map[string][]int64{
		"ascending":  slices.Clone(fx.srcIDs),
		"descending": slices.Clone(fx.srcIDs),
		"shuffled":   slices.Clone(fx.srcIDs),
	}
	slices.Sort(orders["ascending"])
	slices.SortFunc(orders["descending"], func(a, b int64) int { return cmp.Compare(b, a) })
	shuffled := orders["shuffled"]
	for i := range shuffled {
		j := (i * 7919) % len(shuffled)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	for name, ids := range orders {
		got, err := s.findCalleesResolved(ctx, repo.ID, ids, 20, 890)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(qualifiedNames(got), qualifiedNames(want)) {
			t.Fatalf("%s input order changed the page", name)
		}
	}
}

// TestNeighborStagedKeysDoNotLeakBetweenCalls pins the temp-table lifecycle: a
// pooled connection reused by a later call must not answer with the candidates
// an earlier one staged, including after the earlier call was cancelled.
func TestNeighborStagedKeysDoNotLeakBetweenCalls(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	fx := buildNeighborBudgetFixture(t, s, repo.ID, 2000)
	// One connection, so every call is guaranteed to reuse the same temp table.
	s.db.SetMaxOpenConns(1)

	first, err := s.findCalleesResolved(ctx, repo.ID, fx.srcIDs[:1200], 2000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1200 {
		t.Fatalf("first call returned %d candidates, want 1200", len(first))
	}

	// Cancel between the first staging insert and the page statement, so the
	// connection really is handed back carrying half a candidate set. That is
	// the state the defensive clear exists for; cancelling before the call
	// would never reach staging at all.
	cancelled, cancel := context.WithCancel(ctx)
	s.neighborStatementWrap = func(target execQuerier) execQuerier {
		return cancellingQuerier{inner: target, cancel: cancel}
	}
	if _, err := s.findCalleesResolved(cancelled, repo.ID, fx.srcIDs[:1200], 20, 0); err == nil {
		t.Fatal("cancelled call returned no error")
	}
	s.neighborStatementWrap = nil

	second, err := s.findCalleesResolved(ctx, repo.ID, fx.srcIDs[1200:], 2000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(fx.srcIDs)-1200 {
		t.Fatalf("second call returned %d candidates, want %d", len(second), len(fx.srcIDs)-1200)
	}
	wantSecond := make([]string, 0, len(fx.srcIDs)-1200)
	for i := 1200; i < len(fx.srcIDs); i++ {
		wantSecond = append(wantSecond, fmt.Sprintf("a.D%04d", (i*7)%len(fx.srcIDs)))
	}
	slices.Sort(wantSecond)
	if !slices.Equal(qualifiedNames(second), wantSecond) {
		t.Fatal("second call's candidate set is not exactly its own inputs' destinations")
	}
}

// TestNeighborConcurrentQueriesStayIsolated runs both directions concurrently
// on one Store. Staging is connection-local, so nothing here may serialise on
// a shared name.
func TestNeighborConcurrentQueriesStayIsolated(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	fx := buildNeighborBudgetFixture(t, s, repo.ID, 2000)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callees, err := s.findCalleesResolved(ctx, repo.ID, fx.srcIDs, 20, 890)
			if err != nil {
				errs <- err
				return
			}
			if !slices.Equal(qualifiedNames(callees), fx.dstNames[890:910]) {
				errs <- fmt.Errorf("callee page contaminated: %v", qualifiedNames(callees))
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			callers, err := s.findCallersResolved(ctx, repo.ID, "", fx.dstIDs, 20, 890)
			if err != nil {
				errs <- err
				return
			}
			if !slices.Equal(qualifiedNames(callers), fx.srcNames[890:910]) {
				errs <- fmt.Errorf("caller page contaminated: %v", qualifiedNames(callers))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestNeighborStagedTransportWorksReadOnly proves the staged transport is
// usable from the read-only handles the CLI and MCP query surfaces open: a
// TEMP table lives in a separate database that `mode=ro` does not cover.
func TestNeighborStagedTransportWorksReadOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	fx := buildNeighborBudgetFixture(t, s, repo.ID, 2000)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(dbPath, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	guard := guardNeighborStatements(ro)
	page, err := ro.findCalleesResolved(ctx, repo.ID, fx.srcIDs, 20, 890)
	if err != nil {
		t.Fatalf("staged transport on a read-only handle (%s): %v", sqliteDriverName, err)
	}
	assertNeighborBudget(t, guard)
	if !slices.Equal(qualifiedNames(page), fx.dstNames[890:910]) {
		t.Fatalf("read-only page = %v", qualifiedNames(page))
	}
}

// TestNeighborStagedTransportLeavesGraphUnchanged pins that a staged neighbour
// query is query state only: no persistent table gains, loses or changes a row.
func TestNeighborStagedTransportLeavesGraphUnchanged(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	fx := buildNeighborBudgetFixture(t, s, repo.ID, 2000)

	before := persistentGraphSnapshot(t, s)
	if _, err := s.findCalleesResolved(ctx, repo.ID, fx.srcIDs, 20, 890); err != nil {
		t.Fatal(err)
	}
	if _, err := s.findCallersResolved(ctx, repo.ID, "", fx.dstIDs, 20, 890); err != nil {
		t.Fatal(err)
	}
	if after := persistentGraphSnapshot(t, s); after != before {
		t.Fatalf("persistent graph changed:\nbefore %s\nafter  %s", before, after)
	}
}

// TestNeighborStagedPageQueryPlan pins the query plan the CROSS JOIN exists to
// force: the candidate set drives, and symbols is a primary-key seek rather
// than a full scan.
func TestNeighborStagedPageQueryPlan(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	fx := buildNeighborBudgetFixture(t, s, repo.ID, 2000)

	var plan string
	s.neighborStatementWrap = func(target execQuerier) execQuerier {
		return explainingQuerier{inner: target, plan: &plan}
	}
	if _, err := s.findCalleesResolved(ctx, repo.ID, fx.srcIDs, 20, 0); err != nil {
		t.Fatal(err)
	}
	if plan == "" {
		t.Fatal("no page statement was explained")
	}
	t.Logf("EXPLAIN QUERY PLAN:\n%s", plan)
	if !strings.Contains(plan, "SEARCH s USING INTEGER PRIMARY KEY") {
		t.Fatalf("symbols is not a primary-key seek:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN s") {
		t.Fatalf("page plan scans the symbols table:\n%s", plan)
	}
	// The candidate leg must stay index-driven on both sides of the join: the
	// staged set is a primary-key seek, and the edges leg an index seek.
	if !strings.Contains(plan, "SEARCH temp.neighbor_query_keys USING PRIMARY KEY") {
		t.Fatalf("staged candidate set is not a primary-key seek:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN e") {
		t.Fatalf("page plan scans the edges table:\n%s", plan)
	}
}

// cancellingQuerier cancels the query's context once staging has begun.
type cancellingQuerier struct {
	inner  execQuerier
	cancel context.CancelFunc
}

func (c cancellingQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := c.inner.ExecContext(ctx, query, args...)
	if err == nil && strings.HasPrefix(query, insertNeighborKeysSQL) {
		c.cancel()
	}
	return res, err
}

func (c cancellingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.inner.QueryContext(ctx, query, args...)
}

// explainingQuerier records the plan of the neighbour page statement, on the
// same connection that staged the candidates it reads.
type explainingQuerier struct {
	inner execQuerier
	plan  *string
}

func (e explainingQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return e.inner.ExecContext(ctx, query, args...)
}

func (e explainingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "CROSS JOIN symbols s") {
		rows, err := e.inner.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			return nil, err
		}
		var lines []string
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				rows.Close()
				return nil, err
			}
			lines = append(lines, detail)
		}
		rows.Close()
		*e.plan = strings.Join(lines, "\n")
	}
	return e.inner.QueryContext(ctx, query, args...)
}

func persistentGraphSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	var out []string
	for _, table := range []string{"files", "symbols", "edges", "refs", "scans", "repos"} {
		var count int
		err := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&count)
		if err != nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s=%d", table, count))
	}
	var total, unbound int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN dst_symbol_id IS NULL THEN 1 ELSE 0 END), 0) FROM edges`).Scan(&total, &unbound); err != nil {
		t.Fatal(err)
	}
	out = append(out, fmt.Sprintf("edges=%d/unbound=%d", total, unbound))
	// The staging table is ephemeral by construction. Counting the persistent
	// schema catches a `temp.` prefix lost from neighborKeysTable, which would
	// otherwise create a real table this snapshot never looks at.
	var staged int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM main.sqlite_master WHERE name = 'neighbor_query_keys'`).Scan(&staged); err != nil {
		t.Fatal(err)
	}
	out = append(out, fmt.Sprintf("persistent_staging_tables=%d", staged))
	return strings.Join(out, " ")
}

func readAllNeighborPages(t *testing.T, page func(limit, offset int) []graph.Symbol, size int) []graph.Symbol {
	t.Helper()
	var all []graph.Symbol
	for offset := 0; ; offset += size {
		got := page(size, offset)
		if len(got) == 0 {
			return all
		}
		all = append(all, got...)
		if len(got) < size {
			return all
		}
		if offset > 1_000_000 {
			t.Fatal("paging did not terminate")
		}
	}
}
