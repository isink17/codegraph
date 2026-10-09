package store

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
)

func scanStatus(t *testing.T, s *Store, id int64) (status, errText string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT status, COALESCE(error_text, '') FROM scans WHERE id = ?`, id).Scan(&status, &errText); err != nil {
		t.Fatal(err)
	}
	return status, errText
}

func openSameStore(t *testing.T, s *Store) *Store {
	t.Helper()
	other, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	return other
}

func TestRecoverAbandonedScansRefusesLiveScan(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	running := scan(t, s, repoID, "update", "running", "")
	ids, err := openSameStore(t, s).RecoverAbandonedScans(ctx)
	if !errors.Is(err, ErrScanActive) || ids != nil {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if status, _ := scanStatus(t, s, running); status != "running" {
		t.Fatalf("live scan touched: %s", status)
	}
	// The live scan still finishes normally.
	if err := s.CompleteScan(ctx, running, ScanSummary{}, time.Now(), "completed", ""); err != nil {
		t.Fatal(err)
	}
	if ids, err := openSameStore(t, s).RecoverAbandonedScans(ctx); err != nil || ids != nil {
		t.Fatalf("after completion ids=%v err=%v", ids, err)
	}
}

func TestRecoverAbandonedScansAfterOwnerGone(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	orphan := scan(t, s, repoID, "update", "running", "")
	s.releaseAllScanLocks() // what the OS does when the owning process dies

	ids, err := openSameStore(t, s).RecoverAbandonedScans(ctx)
	if err != nil || !slices.Equal(ids, []int64{orphan}) {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if status, text := scanStatus(t, s, orphan); status != "failed" || text != AbandonedScanError {
		t.Fatalf("status=%s text=%q", status, text)
	}
	f := freshness(t, s, repoID)
	if f.RunningScans.Count != 0 || f.State == graph.FreshnessNoKnownStaleness {
		t.Fatalf("recovery certified the graph: %+v", f)
	}
	if ids, err := s.RecoverAbandonedScans(ctx); err != nil || ids != nil {
		t.Fatalf("second recovery ids=%v err=%v", ids, err)
	}
	next := scan(t, s, repoID, "index", "completed", "")
	if status, _ := scanStatus(t, s, next); status != "completed" {
		t.Fatalf("scan after recovery: %s", status)
	}
}

func TestRecoverAbandonedScansLegacyRow(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	res, err := s.db.Exec(`INSERT INTO scans(repo_id, scan_kind, started_at, status) VALUES(?, 'update', ?, 'running')`,
		repoID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	ids, err := s.RecoverAbandonedScans(context.Background())
	if err != nil || !slices.Equal(ids, []int64{id}) {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

func TestRecoverAbandonedScansConcurrent(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	orphan := scan(t, s, repoID, "update", "running", "")
	s.releaseAllScanLocks()
	stores := []*Store{openSameStore(t, s), openSameStore(t, s)}
	var mu sync.Mutex
	var recovered []int64
	var wg sync.WaitGroup
	for _, st := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids, err := st.RecoverAbandonedScans(ctx)
			if err != nil && !errors.Is(err, ErrScanActive) {
				t.Error(err)
			}
			mu.Lock()
			recovered = append(recovered, ids...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if !slices.Equal(recovered, []int64{orphan}) {
		t.Fatalf("recovered %v, want exactly [%d]", recovered, orphan)
	}
}

func TestScanWaitsWhileRecoveryHoldsLock(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	f, err := openScanLock(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if ok, err := lockFile(f, true); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := s.BeginScan(ctx, repoID, "update"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scan began under recovery: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM scans`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

// TestRecoverAbandonedScansAfterProcessKill runs a real scan owner in a child
// process and kills it, which is the crash recovery exists for.
func TestRecoverAbandonedScansAfterProcessKill(t *testing.T) {
	if db := os.Getenv("CODEGRAPH_SCAN_LOCK_CHILD_DB"); db != "" {
		s, err := Open(db)
		if err != nil {
			os.Exit(2)
		}
		repo, err := s.UpsertRepo(context.Background(), filepath.Dir(db))
		if err != nil {
			os.Exit(2)
		}
		id, _, err := s.BeginScan(context.Background(), repo.ID, "update")
		if err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString(strconv.FormatInt(id, 10) + "\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	db := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecoverAbandonedScansAfterProcessKill$")
	cmd.Env = append(os.Environ(), "CODEGRAPH_SCAN_LOCK_CHILD_DB="+db)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	id, err := strconv.ParseInt(line[:len(line)-1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RecoverAbandonedScans(context.Background()); !errors.Is(err, ErrScanActive) {
		t.Fatalf("live child: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ids, err := s.RecoverAbandonedScans(context.Background())
	if err != nil || !slices.Equal(ids, []int64{id}) {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}
