package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// errSQLiteBatchImpossible reports a batch configuration that can never fit a
// single statement: the fixed parameters alone already consume the budget, or
// a single item binds more parameters than remain. Returning an error keeps the
// batching loops from dividing by zero or spinning forever.
var errSQLiteBatchImpossible = errors.New("store: sqlite batch configuration exceeds the parameter budget")

// sqliteBatchSize answers the one question every batched statement in this
// package asks: given fixedArgs parameters that every batch repeats and
// paramsPerItem parameters per dynamic item, how many items may one statement
// carry?
//
// The invariant is
//
//	fixedArgs + items*paramsPerItem <= sqliteInClauseBatchSize <= sqliteDefaultMaxVariables
//
// sqliteInClauseBatchSize is the conservative working ceiling; the hard limit
// is sqliteDefaultMaxVariables. A configuration that cannot fit even one item
// returns 0, which callers must treat as errSQLiteBatchImpossible.
func sqliteBatchSize(fixedArgs, paramsPerItem int) int {
	if paramsPerItem <= 0 || fixedArgs < 0 {
		return 0
	}
	room := sqliteInClauseBatchSize - fixedArgs
	if room < paramsPerItem {
		return 0
	}
	return room / paramsPerItem
}

// sqliteBatchArgs slices items into batches that satisfy sqliteBatchSize, with
// one bind parameter per item. The batches are deterministic slices of the
// caller's slice: order and duplicates are preserved exactly, because batching
// is transport and the caller owns the semantics.
func sqliteBatchArgs(fixedArgs int, items []any) ([][]any, error) {
	size := sqliteBatchSize(fixedArgs, 1)
	if size == 0 {
		return nil, errSQLiteBatchImpossible
	}
	batches := make([][]any, 0, (len(items)+size-1)/size)
	for start := 0; start < len(items); start += size {
		batches = append(batches, items[start:min(start+size, len(items))])
	}
	return batches, nil
}

// sqliteBatchedQuery runs one statement per batch of dynamic parameters so no
// statement ever binds more than the shared budget allows, and feeds every row
// of every batch to scan. base is the statement without its IN clause; clause
// is appended for a filtered pass and must contain a single %s for the
// placeholder list. An unfiltered pass (filtered=false) runs base once with the
// fixed arguments alone, and a filtered pass over an empty set runs nothing at
// all -- neither ever emits "IN ()".
//
// The accumulated rows are exactly those one hypothetical unlimited statement
// would return: batches are deterministic slices of the caller's item slice,
// and every batch is read before the caller acts on the result. A failure in
// any batch is returned immediately; no partial result is reported as success.
func sqliteBatchedQuery(
	ctx context.Context,
	q queryContexter,
	base, clause string,
	fixed, items []any,
	filtered bool,
	scan func(*sql.Rows) error,
) error {
	batches := [][]any{nil}
	if filtered {
		var err error
		if batches, err = sqliteBatchArgs(len(fixed), items); err != nil {
			return err
		}
	}
	for _, batch := range batches {
		query := base
		if filtered {
			query += strings.Replace(clause, "%s", sqlitePlaceholders(len(batch)), 1)
		}
		args := make([]any, 0, len(fixed)+len(batch))
		args = append(args, fixed...)
		args = append(args, batch...)
		if err := sqliteScanRows(ctx, q, query, args, scan); err != nil {
			return err
		}
	}
	return nil
}

// sqliteBatchedIDQuery is the int64 form of sqliteBatchedQuery for the common
// "repo id plus an id set" shape. prefix must end with an open IN clause, which
// this closes.
func sqliteBatchedIDQuery(ctx context.Context, q queryContexter, ids []int64, prefix string, fixed []any, scanRow func(func(...any) error) error) error {
	if len(ids) == 0 {
		return nil
	}
	return sqliteBatchedQuery(ctx, q, prefix, "%s)", fixed, int64SliceToAny(ids), true, func(rows *sql.Rows) error {
		return scanRow(rows.Scan)
	})
}

// sqliteScanRows runs one statement and feeds each row to scan.
func sqliteScanRows(ctx context.Context, q queryContexter, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// sqliteBatchedValuesExec runs a multi-row INSERT in batches sized by the same
// budget arithmetic. prefix is the statement up to and including "VALUES ",
// rowSQL is one row's placeholder tuple such as "(?,?)", and rows carries each
// row's arguments. Every row must bind the same number of parameters as rowSQL
// declares. An empty rows slice executes nothing.
func sqliteBatchedValuesExec(ctx context.Context, q execContexter, prefix, rowSQL string, fixed []any, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	paramsPerRow := strings.Count(rowSQL, "?")
	size := sqliteBatchSize(len(fixed), paramsPerRow)
	if size == 0 {
		return errSQLiteBatchImpossible
	}
	for start := 0; start < len(rows); start += size {
		batch := rows[start:min(start+size, len(rows))]
		args := make([]any, 0, len(fixed)+len(batch)*paramsPerRow)
		args = append(args, fixed...)
		for _, row := range batch {
			if len(row) != paramsPerRow {
				return errSQLiteBatchImpossible
			}
			args = append(args, row...)
		}
		tuples := make([]string, len(batch))
		for i := range tuples {
			tuples[i] = rowSQL
		}
		if _, err := q.ExecContext(ctx, prefix+strings.Join(tuples, ","), args...); err != nil {
			return err
		}
	}
	return nil
}

type execContexter interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func stringSliceToAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
