//go:build unix

package cli

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/isink17/codegraph/internal/constraints"
	"github.com/isink17/codegraph/internal/editsafety"
	"github.com/isink17/codegraph/internal/graph"
)

// A scan in another process takes the shared scan lock before it inserts its
// running row, and that insert can wait on a busy database. Every freshness
// consumer must see that writer, including the CLI paths that read a snapshot
// of the database, whose rows cannot show it.
func TestFreshnessParityWithScanLockedBeforeRow(t *testing.T) {
	if lockPath := os.Getenv("CODEGRAPH_SCAN_LOCK_HOLDER"); lockPath != "" {
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			os.Exit(2)
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("locked\n")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		// An unreachable f could be finalized during the wait, closing the fd and dropping the lock.
		runtime.KeepAlive(f)
		os.Exit(0)
	}
	root, repo := safeToEditRepo(t, map[string]string{constraints.ConfigFileName: cliConstraintsConfig})
	opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := opened.DBPath
	opened.Close()

	base := readFreshnessSurfaces(t, root)
	if base.freshness.Verdict != graph.FreshnessVerdictConsistentWithLastScan || base.strict == nil || base.strict.Verdict != constraints.VerdictFullCoverageAtHead {
		t.Fatalf("indexed: verdict %s strict %+v", base.freshness.Verdict, base.strict)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestFreshnessParityWithScanLockedBeforeRow$")
	cmd.Env = append(os.Environ(), "CODEGRAPH_SCAN_LOCK_HOLDER="+dbPath+".scan.lock")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("lock holder: %q %v", line, err)
	}

	held := readFreshnessSurfaces(t, root)
	if !bytes.Equal(held.stats, held.mcp) {
		t.Fatalf("stats and graph_stats differ\nstats: %s\nmcp:   %s", held.stats, held.mcp)
	}
	if held.freshness.RunningScans.Liveness != "active" || held.freshness.Verdict != graph.FreshnessVerdictWriterActive {
		t.Fatalf("stats with a pre-insert writer: %+v", held.freshness)
	}
	if held.strict == nil || held.strict.Verdict != constraints.VerdictUnknown || held.strictExit != constraints.ExitFreshnessUnknown {
		t.Fatalf("strict check_constraints with a pre-insert writer: exit %d %+v", held.strictExit, held.strict)
	}
	if held.safe.Verdict != nil || held.safe.Reason != editsafety.ReasonFreshnessUnknown || held.safeExit != editsafety.ExitUnknown {
		t.Fatalf("safe_to_edit with a pre-insert writer: exit %d reason %s", held.safeExit, held.safe.Reason)
	}

	// The writer finishes: the holder exits and a real scan commits.
	_ = stdin.Close()
	finished = true
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock holder: %v", err)
	}
	repo.Write("infra/extra.go", "package infra\n\nfunc Extra() {}\n")
	repo.Commit("", "two")
	if _, _, err := runCLI(t, "index", root); err != nil {
		t.Fatal(err)
	}
	after := readFreshnessSurfaces(t, root)
	if after.freshness.Verdict != graph.FreshnessVerdictConsistentWithLastScan || after.strictExit != base.strictExit || after.strict.Verdict != constraints.VerdictFullCoverageAtHead || after.safeExit != base.safeExit {
		t.Fatalf("after the writer finished: verdict %s strict exit %d %+v safe exit %d (base %d)", after.freshness.Verdict, after.strictExit, after.strict, after.safeExit, base.safeExit)
	}
}
