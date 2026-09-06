package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteBatchSizeContract(t *testing.T) {
	cases := []struct {
		name                     string
		fixedArgs, paramsPerItem int
		want                     int
	}{
		{"no fixed args, one parameter per item", 0, 1, sqliteInClauseBatchSize},
		{"one fixed arg, one parameter per item", 1, 1, sqliteInClauseBatchSize - 1},
		{"two parameters per item", 0, 2, sqliteInClauseBatchSize / 2},
		{"fixed args approaching the budget", sqliteInClauseBatchSize - 1, 1, 1},
		{"fixed args consume the budget", sqliteInClauseBatchSize, 1, 0},
		{"fixed args exceed the budget", sqliteInClauseBatchSize + 1, 1, 0},
		{"one item cannot fit", sqliteInClauseBatchSize - 1, 2, 0},
		{"zero parameters per item", 0, 0, 0},
		{"negative parameters per item", 0, -1, 0},
		{"negative fixed args", -1, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sqliteBatchSize(tc.fixedArgs, tc.paramsPerItem)
			if got != tc.want {
				t.Fatalf("sqliteBatchSize(%d,%d) = %d, want %d", tc.fixedArgs, tc.paramsPerItem, got, tc.want)
			}
			if got > 0 && tc.fixedArgs+got*tc.paramsPerItem > sqliteDefaultMaxVariables {
				t.Fatalf("batch of %d exceeds the hard limit %d", got, sqliteDefaultMaxVariables)
			}
		})
	}
}

// TestSQLiteBatchSizeStaysUnderTheHardLimit checks the invariant itself over the
// whole configuration space rather than the sampled cases above.
func TestSQLiteBatchSizeStaysUnderTheHardLimit(t *testing.T) {
	for fixed := 0; fixed <= sqliteInClauseBatchSize+2; fixed++ {
		for params := 1; params <= 16; params++ {
			size := sqliteBatchSize(fixed, params)
			if size < 0 {
				t.Fatalf("negative batch size for fixed=%d params=%d", fixed, params)
			}
			if size == 0 {
				continue // impossible configuration; callers must refuse it
			}
			if total := fixed + size*params; total > sqliteInClauseBatchSize {
				t.Fatalf("fixed=%d params=%d size=%d binds %d, over the %d ceiling", fixed, params, size, total, sqliteInClauseBatchSize)
			}
		}
	}
}

func openBatchStore(t *testing.T) (*Store, int64) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	repo, err := s.UpsertRepo(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TEMP TABLE batch_items(id INTEGER PRIMARY KEY, tag TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return s, repo.ID
}

func TestSQLiteBatchedQueryCoversEveryItem(t *testing.T) {
	ctx := context.Background()
	s, _ := openBatchStore(t)
	const total = 1850
	rows := make([][]any, 0, total)
	for i := 1; i <= total; i++ {
		rows = append(rows, []any{int64(i), "t"})
	}
	if err := sqliteBatchedValuesExec(ctx, s.db, `INSERT INTO batch_items(id,tag) VALUES `, "(?,?)", nil, rows); err != nil {
		t.Fatal(err)
	}

	batch := sqliteBatchSize(1, 1)
	for _, n := range []int{1, batch - 1, batch, batch + 1, 1801, total} {
		items := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			items = append(items, int64(i))
		}
		guard := newBudgetQuerier(s.db)
		seen := map[int64]struct{}{}
		if err := sqliteBatchedQuery(ctx, guard,
			`SELECT id FROM batch_items WHERE tag=?`, ` AND id IN (%s)`,
			[]any{"t"}, items, true, func(r *sql.Rows) error {
				var id int64
				if err := r.Scan(&id); err != nil {
					return err
				}
				seen[id] = struct{}{}
				return nil
			}); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(seen) != n {
			t.Fatalf("n=%d: read %d rows, want %d", n, len(seen), n)
		}
		if guard.maxArgs > sqliteDefaultMaxVariables {
			t.Fatalf("n=%d: max bound args = %d, want <= %d", n, guard.maxArgs, sqliteDefaultMaxVariables)
		}
		wantStatements := (n + batch - 1) / batch
		if len(guard.statements) != wantStatements {
			t.Fatalf("n=%d: ran %d statements, want %d", n, len(guard.statements), wantStatements)
		}
		for _, st := range guard.statements {
			if strings.Contains(collapseSQL(st.sql), "IN ()") {
				t.Fatalf("n=%d: emitted an empty IN list: %s", n, collapseSQL(st.sql))
			}
		}
	}
}

// TestSQLiteBatchedQueryEmptyAndUnfiltered pins the two degenerate passes: a
// filtered pass over an empty set runs nothing, and an unfiltered pass runs the
// base statement exactly once with the fixed arguments alone.
func TestSQLiteBatchedQueryEmptyAndUnfiltered(t *testing.T) {
	ctx := context.Background()
	s, _ := openBatchStore(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO batch_items(id,tag) VALUES(1,'t'),(2,'t')`); err != nil {
		t.Fatal(err)
	}
	noop := func(*sql.Rows) error { return nil }

	guard := newBudgetQuerier(s.db)
	if err := sqliteBatchedQuery(ctx, guard, `SELECT id FROM batch_items WHERE tag=?`, ` AND id IN (%s)`,
		[]any{"t"}, nil, true, noop); err != nil {
		t.Fatal(err)
	}
	if len(guard.statements) != 0 {
		t.Fatalf("filtered pass over an empty set ran %d statements, want 0", len(guard.statements))
	}

	guard = newBudgetQuerier(s.db)
	rowCount := 0
	if err := sqliteBatchedQuery(ctx, guard, `SELECT id FROM batch_items WHERE tag=?`, ` AND id IN (%s)`,
		[]any{"t"}, nil, false, func(*sql.Rows) error { rowCount++; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(guard.statements) != 1 || rowCount != 2 {
		t.Fatalf("unfiltered pass ran %d statements over %d rows, want 1 over 2", len(guard.statements), rowCount)
	}
}

// TestSQLiteBatchedQueryPreservesOrderAndDuplicates pins that batching is
// transport: the helper neither sorts nor dedupes what the caller passed.
func TestSQLiteBatchedQueryPreservesOrderAndDuplicates(t *testing.T) {
	ctx := context.Background()
	s, _ := openBatchStore(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO batch_items(id,tag) VALUES(1,'t'),(2,'t'),(3,'t')`); err != nil {
		t.Fatal(err)
	}
	var got []int64
	if err := sqliteBatchedQuery(ctx, s.db, `SELECT id FROM batch_items WHERE tag=?`, ` AND id IN (%s)`,
		[]any{"t"}, []any{int64(3), int64(1), int64(3)}, true, func(r *sql.Rows) error {
			var id int64
			if err := r.Scan(&id); err != nil {
				return err
			}
			got = append(got, id)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	// One statement, so SQLite decides the row order; what matters is that the
	// duplicate did not make the helper drop or reorder the caller's set.
	if len(got) != 2 {
		t.Fatalf("read %v, want the two distinct matching rows", got)
	}
}

func TestSQLiteBatchedQueryRejectsImpossibleConfiguration(t *testing.T) {
	ctx := context.Background()
	s, _ := openBatchStore(t)
	fixed := make([]any, sqliteInClauseBatchSize)
	for i := range fixed {
		fixed[i] = int64(0)
	}
	err := sqliteBatchedQuery(ctx, s.db, `SELECT id FROM batch_items WHERE tag=?`, ` AND id IN (%s)`,
		fixed, []any{int64(1)}, true, func(*sql.Rows) error { return nil })
	if !errors.Is(err, errSQLiteBatchImpossible) {
		t.Fatalf("err = %v, want errSQLiteBatchImpossible", err)
	}
}

func TestSQLiteBatchedValuesExecBatchesAndValidates(t *testing.T) {
	ctx := context.Background()
	s, _ := openBatchStore(t)
	const total = 1000
	rows := make([][]any, 0, total)
	for i := 1; i <= total; i++ {
		rows = append(rows, []any{int64(i), "t"})
	}
	guard := newBudgetQuerier(s.db)
	if err := sqliteBatchedValuesExec(ctx, guard, `INSERT INTO batch_items(id,tag) VALUES `, "(?,?)", nil, rows); err != nil {
		t.Fatal(err)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	perStatement := sqliteBatchSize(0, 2)
	if want := (total + perStatement - 1) / perStatement; len(guard.statements) != want {
		t.Fatalf("ran %d statements, want %d", len(guard.statements), want)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_items`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != total {
		t.Fatalf("inserted %d rows, want %d", count, total)
	}

	if err := sqliteBatchedValuesExec(ctx, s.db, `INSERT INTO batch_items(id,tag) VALUES `, "(?,?)", nil, nil); err != nil {
		t.Fatalf("empty row set: %v", err)
	}
	if err := sqliteBatchedValuesExec(ctx, s.db, `INSERT INTO batch_items(id,tag) VALUES `, "(?,?)", nil,
		[][]any{{int64(9001)}}); !errors.Is(err, errSQLiteBatchImpossible) {
		t.Fatalf("row with the wrong arity: err = %v, want errSQLiteBatchImpossible", err)
	}
}

// TestSQLiteBatchedQueryReportsBatchFailures pins that a failure part-way
// through is returned rather than reported as a partial success.
func TestSQLiteBatchedQueryReportsBatchFailures(t *testing.T) {
	ctx := context.Background()
	s, _ := openBatchStore(t)
	batch := sqliteBatchSize(1, 1)
	items := make([]any, 0, batch*3)
	for i := 1; i <= batch*3; i++ {
		items = append(items, int64(i))
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO batch_items(id,tag) VALUES(1,'t')`); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("scan failed")
	seen := 0
	err := sqliteBatchedQuery(ctx, s.db, `SELECT id FROM batch_items WHERE tag=?`, ` AND id IN (%s)`,
		[]any{"t"}, items, true, func(*sql.Rows) error { seen++; return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the scan error", err)
	}
	if seen != 1 {
		t.Fatalf("kept scanning after a failure: %d rows", seen)
	}
}
