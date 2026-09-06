package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// errTooManyTestSQLVariables mimics the failure a SQLite build compiled with
// the common 999-parameter ceiling raises. The locally linked build may accept
// more, so the guard below makes the portable budget observable regardless of
// what this machine's driver tolerates.
var errTooManyTestSQLVariables = errors.New("too many SQL variables")

type budgetStatement struct {
	sql  string
	args []any
}

// budgetQuerier wraps an execQuerier and refuses any statement that binds more
// than sqliteDefaultMaxVariables arguments, recording every statement so tests
// can assert batch shape, batch count, and the maximum argument count reached.
type budgetQuerier struct {
	inner      execQuerier
	limit      int
	statements []budgetStatement
	maxArgs    int
}

func newBudgetQuerier(inner execQuerier) *budgetQuerier {
	return &budgetQuerier{inner: inner, limit: sqliteDefaultMaxVariables}
}

func (b *budgetQuerier) record(query string, args []any) error {
	b.statements = append(b.statements, budgetStatement{sql: query, args: args})
	if len(args) > b.maxArgs {
		b.maxArgs = len(args)
	}
	if len(args) > b.limit {
		return fmt.Errorf("%w: %d bound arguments exceed %d in %s",
			errTooManyTestSQLVariables, len(args), b.limit, collapseSQL(query))
	}
	return nil
}

func (b *budgetQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := b.record(query, args); err != nil {
		return nil, err
	}
	return b.inner.QueryContext(ctx, query, args...)
}

func (b *budgetQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := b.record(query, args); err != nil {
		return nil, err
	}
	return b.inner.ExecContext(ctx, query, args...)
}

// matching returns every recorded statement whose SQL contains all fragments.
func (b *budgetQuerier) matching(fragments ...string) []budgetStatement {
	var out []budgetStatement
	for _, st := range b.statements {
		hit := true
		for _, fragment := range fragments {
			if !strings.Contains(st.sql, fragment) {
				hit = false
				break
			}
		}
		if hit {
			out = append(out, st)
		}
	}
	return out
}

// statementBinding reports the index of the statement in the list that bound
// want, or -1 when no statement did.
func statementBinding[T comparable](statements []budgetStatement, want T) int {
	for i, st := range statements {
		for _, arg := range st.args {
			if v, ok := arg.(T); ok && v == want {
				return i
			}
		}
	}
	return -1
}

func collapseSQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

func openBudgetStore(t *testing.T) (*Store, graph.Repo) {
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
	return s, repo
}
