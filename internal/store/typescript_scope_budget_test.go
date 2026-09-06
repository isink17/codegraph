package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

type tsFixtureBuilder struct {
	t    *testing.T
	s    *Store
	repo int64
	tx   *sql.Tx
}

func newTSFixtureBuilder(t *testing.T, s *Store, repoID int64) *tsFixtureBuilder {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	return &tsFixtureBuilder{t: t, s: s, repo: repoID, tx: tx}
}

func (b *tsFixtureBuilder) commit() {
	b.t.Helper()
	if err := b.tx.Commit(); err != nil {
		b.t.Fatal(err)
	}
}

func (b *tsFixtureBuilder) file(path string) int64 {
	b.t.Helper()
	res, err := b.tx.Exec(`INSERT INTO files(repo_id,path,language,indexed_at) VALUES(?,?,'typescript','')`, b.repo, path)
	if err != nil {
		b.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		b.t.Fatal(err)
	}
	return id
}

func (b *tsFixtureBuilder) symbol(fileID int64, name string) int64 {
	b.t.Helper()
	res, err := b.tx.Exec(`
		INSERT INTO symbols(repo_id,file_id,language,kind,name,qualified_name,container_name,
			start_line,start_col,end_line,end_col,stable_key,qualified_suffix,dot_tail2,dot_tail3,visibility)
		VALUES(?,?,'typescript','function',?,?,'',1,0,1,0,?,?,'','','public')`,
		b.repo, fileID, name, name, fmt.Sprintf("ts:%d:%s", fileID, name), name)
	if err != nil {
		b.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		b.t.Fatal(err)
	}
	return id
}

func (b *tsFixtureBuilder) importEvidence(fileID int64, source, imported, local, kind string, wildcard, reexport bool) {
	b.t.Helper()
	if _, err := b.tx.Exec(`
		INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard,is_reexport)
		VALUES(?,?,'typescript',?,?,?,?,?,?)`,
		b.repo, fileID, source, imported, local, kind, boolInt(wildcard), boolInt(reexport)); err != nil {
		b.t.Fatal(err)
	}
}

func (b *tsFixtureBuilder) candidate(fileID int64, source, candidatePath string) {
	b.t.Helper()
	if _, err := b.tx.Exec(`
		INSERT INTO scope_module_candidate_evidence(repo_id,source_file_id,source_specifier,candidate_path)
		VALUES(?,?,?,?)`, b.repo, fileID, source, candidatePath); err != nil {
		b.t.Fatal(err)
	}
}

func (b *tsFixtureBuilder) edge(fileID, srcSymbolID int64, dstName string) int64 {
	b.t.Helper()
	res, err := b.tx.Exec(`
		INSERT INTO edges(repo_id,src_symbol_id,dst_symbol_id,dst_name,edge_kind,evidence,file_id,line)
		VALUES(?,?,NULL,?,'call','',?,1)`, b.repo, srcSymbolID, dstName, fileID)
	if err != nil {
		b.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		b.t.Fatal(err)
	}
	return id
}

// tsBudgetFixture is a TypeScript module component wide enough that every
// scoped statement in the resolver would exceed the portable variable budget
// without batching: callerCount importing files, one unresolved edge each,
// all bound to a single shared export.
type tsBudgetFixture struct {
	target     int64
	targetPath string
	edges      map[int64]struct{}
	edgeOrder  []int64
}

func buildTSBudgetFixture(t *testing.T, callerCount int, b *tsFixtureBuilder) tsBudgetFixture {
	t.Helper()
	out := tsBudgetFixture{
		targetPath: "src/target.ts",
		edges:      make(map[int64]struct{}, callerCount),
	}
	targetFile := b.file(out.targetPath)
	out.target = b.symbol(targetFile, "shared")
	for i := range callerCount {
		p := fmt.Sprintf("src/call%04d.ts", i)
		f := b.file(p)
		src := b.symbol(f, fmt.Sprintf("run%04d", i))
		b.importEvidence(f, "./target", "shared", "shared", "named", false, false)
		b.candidate(f, "./target", out.targetPath)
		e := b.edge(f, src, "shared")
		out.edges[e] = struct{}{}
		out.edgeOrder = append(out.edgeOrder, e)
	}
	return out
}

// TestTypeScriptScopeScopedPassStaysInVariableBudget is the load-bearing
// regression: a scoped pass over more than a thousand edges, caller files,
// frontier files, component symbols and resolution rows must never bind more
// than sqliteDefaultMaxVariables arguments in a single statement.
func TestTypeScriptScopeScopedPassStaysInVariableBudget(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	b := newTSFixtureBuilder(t, s, repo.ID)
	fixture := buildTSBudgetFixture(t, 1200, b)
	b.commit()

	guard := newBudgetQuerier(s.db)
	n, err := resolveTypeScriptScope(ctx, guard, repo.ID, fixture.edges)
	if err != nil {
		t.Fatalf("scoped resolve: %v", err)
	}
	if n != len(fixture.edges) {
		t.Fatalf("resolved %d edges, want %d", n, len(fixture.edges))
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	// One-column IN batches must also respect the conservative chunk size with
	// room left for the fixed repo_id parameter.
	for _, st := range guard.matching(" IN (?") {
		if len(st.args) > sqliteInClauseBatchSize+4 {
			t.Fatalf("IN statement bound %d args: %s", len(st.args), collapseSQL(st.sql))
		}
	}
	for _, id := range fixture.edgeOrder {
		var got sql.NullInt64
		if err := s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !got.Valid || got.Int64 != fixture.target {
			t.Fatalf("edge %d bound to %v, want %d", id, got, fixture.target)
		}
	}
}

// TestTypeScriptScopeBatchCountIsProportionalToBatches pins the batching shape:
// N scoped items must cost O(ceil(N/batch)) statements at a stage, not O(N).
func TestTypeScriptScopeBatchCountIsProportionalToBatches(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	b := newTSFixtureBuilder(t, s, repo.ID)
	fixture := buildTSBudgetFixture(t, 2000, b)
	b.commit()

	guard := newBudgetQuerier(s.db)
	if _, err := resolveTypeScriptScope(ctx, guard, repo.ID, fixture.edges); err != nil {
		t.Fatalf("scoped resolve: %v", err)
	}
	edgeSelect := guard.matching("FROM edges e JOIN files f", "e.dst_symbol_id IS NULL")
	want := (len(fixture.edges) + sqliteInClauseBatchSize - 1) / sqliteInClauseBatchSize
	if len(edgeSelect) != want {
		t.Fatalf("edge selection ran %d statements, want %d", len(edgeSelect), want)
	}
	if len(guard.statements) > 64 {
		t.Fatalf("scoped pass ran %d statements, expected a batch-proportional count", len(guard.statements))
	}
}

// TestTypeScriptScopeVariantExpansionIsBatched covers the subtle case where the
// logical candidate set fits the budget but its persisted path spellings do not.
func TestTypeScriptScopeVariantExpansionIsBatched(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	b := newTSFixtureBuilder(t, s, repo.ID)
	const callers = 300
	only := make(map[int64]struct{}, callers)
	targets := make(map[int64]int64, callers)
	for i := range callers {
		mod := b.file(fmt.Sprintf("src/mod%04d.ts", i))
		target := b.symbol(mod, fmt.Sprintf("shared%04d", i))
		f := b.file(fmt.Sprintf("src/call%04d.ts", i))
		src := b.symbol(f, fmt.Sprintf("run%04d", i))
		// Extensionless specifiers expand to two logical candidates each, and
		// every candidate expands again into its persisted path spellings.
		b.importEvidence(f, fmt.Sprintf("./mod%04d", i), fmt.Sprintf("shared%04d", i), fmt.Sprintf("shared%04d", i), "named", false, false)
		e := b.edge(f, src, fmt.Sprintf("shared%04d", i))
		only[e] = struct{}{}
		targets[e] = target
	}
	b.commit()

	guard := newBudgetQuerier(s.db)
	if _, err := resolveTypeScriptScope(ctx, guard, repo.ID, only); err != nil {
		t.Fatalf("scoped resolve: %v", err)
	}
	pathLookup := guard.matching("SELECT id,path FROM files", "path IN (")
	bound := 0
	for _, st := range pathLookup {
		bound += len(st.args) - 1
	}
	if bound <= sqliteDefaultMaxVariables {
		t.Fatalf("path lookup bound %d variants; fixture must exceed %d", bound, sqliteDefaultMaxVariables)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	for edge, target := range targets {
		var got sql.NullInt64
		if err := s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, edge).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !got.Valid || got.Int64 != target {
			t.Fatalf("edge %d bound to %v, want %d", edge, got, target)
		}
	}
}

// TestTypeScriptScopeSymbolAmbiguityAcrossBatchBoundary proves batching never
// turns a second candidate into a unique one: two star re-export sources are
// placed in different symbol batches and the edge must stay unresolved.
func TestTypeScriptScopeSymbolAmbiguityAcrossBatchBoundary(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	b := newTSFixtureBuilder(t, s, repo.ID)

	// A low file id: the first re-export source lands in the first symbol batch.
	pFile := b.file("src/p.ts")
	b.symbol(pFile, "dup")

	fixture := buildTSBudgetFixture(t, 1200, b)

	// High file ids: the competing source lands in a later symbol batch.
	qFile := b.file("src/q.ts")
	b.symbol(qFile, "dup")
	mFile := b.file("src/m.ts")
	b.importEvidence(mFile, "./p", "", "", "named", true, true)
	b.importEvidence(mFile, "./q", "", "", "named", true, true)

	use := b.file("src/sym-use.ts")
	src := b.symbol(use, "runSym")
	b.importEvidence(use, "./m", "dup", "dup", "named", false, false)
	edge := b.edge(use, src, "dup")
	b.commit()

	only := map[int64]struct{}{edge: {}}
	for id := range fixture.edges {
		only[id] = struct{}{}
	}
	guard := newBudgetQuerier(s.db)
	if _, err := resolveTypeScriptScope(ctx, guard, repo.ID, only); err != nil {
		t.Fatalf("scoped resolve: %v", err)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	symbolLoads := guard.matching("FROM symbols s JOIN files f")
	pBatch := statementBinding(symbolLoads, pFile)
	qBatch := statementBinding(symbolLoads, qFile)
	if pBatch < 0 || qBatch < 0 || pBatch == qBatch {
		t.Fatalf("competing sources must load in different batches, got %d and %d of %d", pBatch, qBatch, len(symbolLoads))
	}
	var got sql.NullInt64
	if err := s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, edge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("ambiguous edge bound to %d across a batch boundary", got.Int64)
	}
}

// TestTypeScriptScopeFileAmbiguityAcrossBatchBoundary places the two
// extensionless spellings of one module in different path-lookup batches. Both
// must still be read, so the module stays ambiguous and the edge unresolved.
func TestTypeScriptScopeFileAmbiguityAcrossBatchBoundary(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	b := newTSFixtureBuilder(t, s, repo.ID)

	b.file("src/amb.ts")
	b.file("src/amb.tsx")
	use := b.file("src/use.ts")
	src := b.symbol(use, "run")
	// Candidate paths are looked up sorted, and every "src/aaa*" spelling sorts
	// before "src/amb.ts". Padding is sized so "src/amb.ts" is the last entry of
	// the first batch and "src/amb.tsx" the first entry of the second.
	before := sqliteBatchSize(1, 1) - 1
	for i := range before / 2 {
		b.importEvidence(use, fmt.Sprintf("./aaa%04d", i), "x", fmt.Sprintf("x%04d", i), "named", false, false)
	}
	if before%2 == 1 {
		// One explicit extension contributes a single candidate, so the parity
		// of the padding always lands "src/amb.ts" on the batch boundary.
		b.importEvidence(use, "./aab.ts", "x", "xodd", "named", false, false)
	}
	b.importEvidence(use, "./amb", "shared", "shared", "named", false, false)
	edge := b.edge(use, src, "shared")
	b.commit()

	guard := newBudgetQuerier(s.db)
	if _, err := resolveTypeScriptScope(ctx, guard, repo.ID, map[int64]struct{}{edge: {}}); err != nil {
		t.Fatalf("scoped resolve: %v", err)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	pathLookup := guard.matching("SELECT id,path FROM files", "path IN (")
	tsBatch := statementBinding(pathLookup, "src/amb.ts")
	tsxBatch := statementBinding(pathLookup, "src/amb.tsx")
	if tsBatch < 0 || tsxBatch < 0 || tsBatch == tsxBatch {
		t.Fatalf("competing spellings must land in different batches, got %d and %d of %d", tsBatch, tsxBatch, len(pathLookup))
	}
	var got sql.NullInt64
	if err := s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, edge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("extensionless ambiguity resolved to %d across a batch boundary", got.Int64)
	}
}

// TestTypeScriptScopeFullAndScopedAgree proves batching boundaries do not shift
// results: a scoped pass over the whole edge set must match a full pass.
func TestTypeScriptScopeFullAndScopedAgree(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	b := newTSFixtureBuilder(t, s, repo.ID)
	fixture := buildTSBudgetFixture(t, 1100, b)
	b.commit()

	if _, err := resolveTypeScriptScope(ctx, s.db, repo.ID, nil); err != nil {
		t.Fatalf("full resolve: %v", err)
	}
	full := map[int64]int64{}
	rows, err := s.db.Query(`SELECT id,dst_symbol_id FROM edges WHERE dst_symbol_id IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, dst int64
		if err := rows.Scan(&id, &dst); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		full[id] = dst
	}
	rows.Close()
	if len(full) != len(fixture.edges) {
		t.Fatalf("full pass resolved %d edges, want %d", len(full), len(fixture.edges))
	}
	if _, err := s.db.Exec(`UPDATE edges SET ` + resolverClearResolutionSQL); err != nil {
		t.Fatal(err)
	}
	guard := newBudgetQuerier(s.db)
	if _, err := resolveTypeScriptScope(ctx, guard, repo.ID, fixture.edges); err != nil {
		t.Fatalf("scoped resolve: %v", err)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	scoped := map[int64]int64{}
	rows, err = s.db.Query(`SELECT id,dst_symbol_id FROM edges WHERE dst_symbol_id IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, dst int64
		if err := rows.Scan(&id, &dst); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		scoped[id] = dst
	}
	rows.Close()
	if len(scoped) != len(full) {
		t.Fatalf("scoped pass resolved %d edges, full pass %d", len(scoped), len(full))
	}
	for id, dst := range full {
		if scoped[id] != dst {
			t.Fatalf("edge %d: scoped %d, full %d", id, scoped[id], dst)
		}
	}
}

// TestTypeScriptScopeInvalidationStaysInVariableBudget exercises the reverse
// candidate walk and the affected-edge lookup past the portable budget.
func TestTypeScriptScopeInvalidationStaysInVariableBudget(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	b := newTSFixtureBuilder(t, s, repo.ID)
	fixture := buildTSBudgetFixture(t, 1200, b)
	// An unrelated TypeScript module that no changed path reaches.
	otherFile := b.file("other/untouched.ts")
	otherSym := b.symbol(otherFile, "untouched")
	otherEdge := b.edge(otherFile, otherSym, "untouched")
	b.commit()

	if _, err := resolveTypeScriptScope(ctx, s.db, repo.ID, nil); err != nil {
		t.Fatalf("full resolve: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence='high' WHERE id=?`,
		otherSym, tsScopeStrategy, otherEdge); err != nil {
		t.Fatal(err)
	}

	guard := newBudgetQuerier(s.db)
	names, err := invalidateTypeScriptScopeBindingsQuery(ctx, guard, repo.ID, []string{fixture.targetPath})
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	if len(names) != 1 || names[0] != "shared" {
		t.Fatalf("affected names = %v, want [shared]", names)
	}
	var stale int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM edges WHERE dst_symbol_id IS NOT NULL AND resolution_strategy=?`, tsScopeStrategy).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 1 {
		t.Fatalf("%d bindings survived invalidation, want only the unrelated one", stale)
	}
	var untouched sql.NullInt64
	if err := s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, otherEdge).Scan(&untouched); err != nil {
		t.Fatal(err)
	}
	if !untouched.Valid || untouched.Int64 != otherSym {
		t.Fatalf("unrelated binding was cleared: %v", untouched)
	}
}

// TestTSScopeBatchedQueryEmptySets pins the helper's contract for empty input:
// a filtered pass over an empty set runs no statement at all rather than
// emitting "IN ()", and an unfiltered pass still runs its statement once.
func TestTSScopeBatchedQueryEmptySets(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)

	guard := newBudgetQuerier(s.db)
	if err := sqliteBatchedQuery(ctx, guard, `SELECT id FROM files WHERE repo_id=?`, ` AND id IN (%s)`,
		[]any{repo.ID}, nil, true, func(*sql.Rows) error { return nil }); err != nil {
		t.Fatalf("filtered empty batch: %v", err)
	}
	if len(guard.statements) != 0 {
		t.Fatalf("filtered empty set ran %d statements: %s", len(guard.statements), collapseSQL(guard.statements[0].sql))
	}

	guard = newBudgetQuerier(s.db)
	if err := sqliteBatchedQuery(ctx, guard, `SELECT id FROM files WHERE repo_id=?`, ` AND id IN (%s)`,
		[]any{repo.ID}, nil, false, func(*sql.Rows) error { return nil }); err != nil {
		t.Fatalf("unfiltered pass: %v", err)
	}
	if len(guard.statements) != 1 {
		t.Fatalf("unfiltered pass ran %d statements, want 1", len(guard.statements))
	}
	if strings.Contains(guard.statements[0].sql, "IN (") {
		t.Fatalf("unfiltered pass emitted an IN clause: %s", collapseSQL(guard.statements[0].sql))
	}
}

// TestTypeScriptScopeEmptySetsAreNoOps keeps the resolver's and the
// invalidator's existing no-op behaviour for empty inputs.
func TestTypeScriptScopeEmptySetsAreNoOps(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	guard := newBudgetQuerier(s.db)
	n, err := resolveTypeScriptScope(ctx, guard, repo.ID, map[int64]struct{}{})
	if err != nil {
		t.Fatalf("empty scoped resolve: %v", err)
	}
	if n != 0 {
		t.Fatalf("resolved %d edges on an empty repo", n)
	}
	for _, st := range guard.statements {
		if strings.Contains(collapseSQL(st.sql), "IN ()") {
			t.Fatalf("empty IN clause emitted: %s", collapseSQL(st.sql))
		}
	}
	names, err := invalidateTypeScriptScopeBindingsQuery(ctx, guard, repo.ID, nil)
	if err != nil {
		t.Fatalf("empty invalidate: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("affected names = %v, want none", names)
	}
}
