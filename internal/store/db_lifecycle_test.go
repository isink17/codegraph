package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestOpen_AppliesConnectionPragmasAcrossPool(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	s.db.SetMaxOpenConns(4)
	s.db.SetMaxIdleConns(4)

	conn1, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn() (1) error = %v", err)
	}
	defer conn1.Close()
	conn2, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn() (2) error = %v", err)
	}
	defer conn2.Close()

	var fk1, fk2 int64
	if err := conn1.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk1); err != nil {
		t.Fatalf("PRAGMA foreign_keys (1) error = %v", err)
	}
	if err := conn2.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk2); err != nil {
		t.Fatalf("PRAGMA foreign_keys (2) error = %v", err)
	}
	if fk1 != 1 || fk2 != 1 {
		t.Fatalf("expected foreign_keys=1 on both conns, got %d and %d", fk1, fk2)
	}
}

func TestOpen_ConcurrentMigrateIsSafe(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)

	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			s, err := Open(dbPath)
			if err == nil {
				_ = s.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open() error = %v", err)
		}
	}
}
