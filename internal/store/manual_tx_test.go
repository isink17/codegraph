package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// failOnce reports injected for the first statement containing query after it
// is armed. With runFirst the statement takes effect before the failure is
// reported.
type failOnce struct {
	query    string
	runFirst bool
	injected error
	armed    atomic.Bool
}

func (f *failOnce) hook(ctx context.Context, query string, exec func() error) error {
	if !strings.Contains(query, f.query) || !f.armed.CompareAndSwap(true, false) {
		return exec()
	}
	if f.runFirst {
		if err := exec(); err != nil {
			return err
		}
	}
	return f.injected
}

// A cancel that lands after the last write reported success but before COMMIT
// rolls the transaction back and hands the connection back outside it.
func TestDirtyQueueCancelBeforeCommitRollsBack(t *testing.T) {
	for _, tc := range dirtyQueueTransactions {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var armed atomic.Bool
			s, repoID := dirtyQueueStore(t, tc.noReturning, func(_ context.Context, query string, exec func() error) error {
				if !strings.Contains(query, tc.lastWrite) || !armed.CompareAndSwap(true, false) {
					return exec()
				}
				err := exec()
				cancel()
				return err
			})
			armed.Store(true)
			if _, err := tc.run(ctx, s, repoID); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if armed.Load() {
				t.Fatal("fault was never injected")
			}
			requireConnectionKept(t, s.db)
			if got := dirtyRows(t, s, repoID); !slices.Equal(got, []string{"a.go:event"}) {
				t.Fatalf("dirty rows = %q, want untouched", got)
			}
		})
	}
}

// A statement that took effect but reported failure is rolled back, its error
// reaches the caller, and the next attempt on the released connection succeeds.
func TestDirtyQueueStatementFailureRollsBackAndRetries(t *testing.T) {
	for _, tc := range dirtyQueueTransactions {
		failAt := []string{tc.lastWrite}
		if tc.noReturning {
			failAt = append(failAt, "SELECT path FROM dirty_files")
		}
		for _, stmt := range failAt {
			t.Run(tc.name+"/"+strings.Fields(stmt)[0], func(t *testing.T) {
				bg := context.Background()
				fault := &failOnce{query: stmt, runFirst: true, injected: errors.New("injected statement failure")}
				s, repoID := dirtyQueueStore(t, tc.noReturning, fault.hook)
				fault.armed.Store(true)
				if _, err := tc.run(bg, s, repoID); !errors.Is(err, fault.injected) {
					t.Fatalf("err = %v, want the injected failure", err)
				}
				if fault.armed.Load() {
					t.Fatal("fault was never injected")
				}
				requireFreeConnection(t, s.db)
				if got := dirtyRows(t, s, repoID); !slices.Equal(got, []string{"a.go:event"}) {
					t.Fatalf("dirty rows = %q, want untouched", got)
				}
				paths, err := tc.run(bg, s, repoID)
				if err != nil || !slices.Equal(paths, []string{"a.go"}) {
					t.Fatalf("retry = %q, %v", paths, err)
				}
				if got := dirtyRows(t, s, repoID); !slices.Equal(got, tc.committed) {
					t.Fatalf("dirty rows after retry = %q, want %q", got, tc.committed)
				}
			})
		}
	}
}

// When ROLLBACK itself fails the transaction may still be open, so the
// connection must not go back to the pool.
func TestDirtyQueueRollbackFailureDiscardsConnection(t *testing.T) {
	for _, tc := range dirtyQueueTransactions {
		t.Run(tc.name, func(t *testing.T) {
			bg := context.Background()
			stmtFault := &failOnce{query: tc.lastWrite, runFirst: true, injected: errors.New("injected statement failure")}
			rollbackFault := &failOnce{query: `ROLLBACK`, injected: errors.New("injected rollback failure")}
			s, repoID := dirtyQueueStore(t, tc.noReturning, func(ctx context.Context, query string, exec func() error) error {
				return stmtFault.hook(ctx, query, func() error { return rollbackFault.hook(ctx, query, exec) })
			})
			stmtFault.armed.Store(true)
			rollbackFault.armed.Store(true)
			if _, err := tc.run(bg, s, repoID); !errors.Is(err, stmtFault.injected) {
				t.Fatalf("err = %v, want the injected statement failure", err)
			}
			if rollbackFault.armed.Load() {
				t.Fatal("rollback fault was never injected")
			}
			requireFreeConnection(t, s.db)
			if got := dirtyRows(t, s, repoID); !slices.Equal(got, []string{"a.go:event"}) {
				t.Fatalf("dirty rows = %q, want untouched", got)
			}
			if paths, err := tc.run(bg, s, repoID); err != nil || !slices.Equal(paths, []string{"a.go"}) {
				t.Fatalf("retry = %q, %v", paths, err)
			}
		})
	}
}

// A BEGIN that failed without taking effect leaves nothing to roll back; the
// harmless "no transaction is active" ROLLBACK error keeps the connection.
func TestDirtyQueueFailedBeginKeepsConnection(t *testing.T) {
	for _, tc := range dirtyQueueTransactions {
		t.Run(tc.name, func(t *testing.T) {
			fault := &failOnce{query: `BEGIN IMMEDIATE`, injected: errors.New("injected begin failure")}
			s, repoID := dirtyQueueStore(t, tc.noReturning, fault.hook)
			fault.armed.Store(true)
			if _, err := tc.run(context.Background(), s, repoID); !errors.Is(err, fault.injected) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			requireConnectionKept(t, s.db)
		})
	}
}

// Without RETURNING support DrainDirtyFiles falls back to SELECT then DELETE
// and returns the same queue order.
func TestDrainDirtyFilesWithoutReturningMatchesReturning(t *testing.T) {
	for _, noReturning := range []bool{false, true} {
		t.Run(map[bool]string{false: "returning", true: "fallback"}[noReturning], func(t *testing.T) {
			bg := context.Background()
			var mu sync.Mutex
			var queries []string
			s, repoID := dirtyQueueStore(t, noReturning, func(_ context.Context, query string, exec func() error) error {
				mu.Lock()
				queries = append(queries, query)
				mu.Unlock()
				return exec()
			})
			for _, row := range [][2]string{{"c.go", "2026-01-03T00:00:00Z"}, {"b.go", "2026-01-01T00:00:00Z"}, {"a.go", "2026-01-02T00:00:00Z"}} {
				if _, err := s.db.ExecContext(bg, `INSERT INTO dirty_files(repo_id, path, reason, queued_at) VALUES(?, ?, 'event', ?)
					ON CONFLICT(repo_id, path) DO UPDATE SET queued_at=excluded.queued_at`, repoID, row[0], row[1]); err != nil {
					t.Fatal(err)
				}
			}
			paths, err := s.DrainDirtyFiles(bg, repoID)
			if want := []string{"b.go", "a.go", "c.go"}; err != nil || !slices.Equal(paths, want) {
				t.Fatalf("drained %q, %v; want %q", paths, err, want)
			}
			if got := dirtyRows(t, s, repoID); len(got) != 0 {
				t.Fatalf("dirty rows after drain = %q, want none", got)
			}
			mu.Lock()
			fellBack := slices.ContainsFunc(queries, func(q string) bool { return strings.Contains(q, "SELECT path FROM dirty_files") })
			mu.Unlock()
			if fellBack != noReturning {
				t.Fatalf("fallback used = %v, want %v", fellBack, noReturning)
			}
			requireFreeConnection(t, s.db)
		})
	}
}

func TestReferenceIdentityRollbackFailureDiscardsConnection(t *testing.T) {
	ctx := context.Background()
	stmtFault := &failOnce{query: `INSERT INTO tmp_reference_scope`, runFirst: true, injected: errors.New("injected statement failure")}
	rollbackFault := &failOnce{query: `ROLLBACK`, injected: errors.New("injected rollback failure")}
	s, repoID := hookedStore(t, func(ctx context.Context, query string, exec func() error) error {
		return stmtFault.hook(ctx, query, func() error { return rollbackFault.hook(ctx, query, exec) })
	})
	s.db.SetMaxOpenConns(1)
	stmtFault.armed.Store(true)
	rollbackFault.armed.Store(true)
	if err := s.ReconcileReferenceIdentities(ctx, repoID); !errors.Is(err, stmtFault.injected) {
		t.Fatalf("err = %v, want the injected statement failure", err)
	}
	if rollbackFault.armed.Load() {
		t.Fatal("rollback fault was never injected")
	}
	requireFreeConnection(t, s.db)
	if err := s.ReconcileReferenceIdentities(ctx, repoID); err != nil {
		t.Fatalf("retry: %v", err)
	}
}
