//go:build unix

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/config"
)

// An interrupted or terminated watch stops in order: the run ends with the
// terminal events, the exit status names the signal, and dirty work queued but
// not yet flushed stays queued for the next run.
func TestWatchStopsGracefullyOnSignal(t *testing.T) {
	if repoRoot := os.Getenv("CODEGRAPH_WATCH_SIGNAL_CHILD"); repoRoot != "" {
		err := Run(context.Background(), []string{"watch", "--jsonl", repoRoot}, os.Stdout, os.Stderr)
		var exitErr *ExitError
		switch {
		case errors.As(err, &exitErr):
			os.Exit(exitErr.Code)
		case err != nil:
			os.Exit(1)
		}
		os.Exit(0)
	}
	for _, tc := range []struct {
		sig  syscall.Signal
		name string
	}{
		{syscall.SIGINT, "interrupt"},
		{syscall.SIGTERM, "terminated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := setupWatchRepo(t)
			cfgPath, err := config.ConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
				t.Fatal(err)
			}
			// A debounce longer than the test keeps the queued change unflushed.
			if err := os.WriteFile(cfgPath, []byte(`{"watch_debounce": 3600000000000}`), 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestWatchStopsGracefullyOnSignal$")
			cmd.Env = append(os.Environ(), "CODEGRAPH_WATCH_SIGNAL_CHILD="+repoRoot)
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

			events := make(chan map[string]any, 64)
			go func() {
				defer close(events)
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					var event map[string]any
					if json.Unmarshal(scanner.Bytes(), &event) == nil {
						events <- event
					}
				}
			}()
			var seen []map[string]any
			deadline := time.After(60 * time.Second)
			for watching := false; !watching; {
				select {
				case event, ok := <-events:
					if !ok {
						t.Fatalf("watch ended before watching: %v", seen)
					}
					seen = append(seen, event)
					watching = event["type"] == "watch_status" && event["run_state"] == "watching"
				case <-deadline:
					t.Fatalf("watch never reported watching: %v", seen)
				}
			}

			if err := os.WriteFile(filepath.Join(repoRoot, "extra.go"), []byte("package main\n\nfunc Extra() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			waitForDirtyFiles(t, repoRoot)

			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			for event := range events {
				seen = append(seen, event)
			}
			waitErr := cmd.Wait()
			finished = true
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 128+int(tc.sig) {
				t.Fatalf("exit = %v, want status %d", waitErr, 128+int(tc.sig))
			}
			if len(seen) < 2 {
				t.Fatalf("events = %v, want terminal events", seen)
			}
			terminal, stopped := seen[len(seen)-2], seen[len(seen)-1]
			if terminal["type"] != "watch_status" || terminal["run_state"] != "stopped" || terminal["reason"] != "signal" || terminal["signal"] != tc.name {
				t.Fatalf("terminal status = %v, want stopped by signal %s", terminal, tc.name)
			}
			if stopped["type"] != "watch_stopped" || stopped["stats"] == nil {
				t.Fatalf("last event = %v, want watch_stopped with stats", stopped)
			}

			opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), repoRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			dirty, err := opened.Store.HasDirtyFiles(context.Background(), opened.Repo.ID)
			if err != nil || !dirty {
				t.Fatalf("dirty queue after stop = %v, %v; want the unflushed change kept", dirty, err)
			}
		})
	}
}

func waitForDirtyFiles(t *testing.T, repoRoot string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), repoRoot)
		if err == nil {
			dirty, dirtyErr := opened.Store.HasDirtyFiles(context.Background(), opened.Repo.ID)
			opened.Close()
			if dirtyErr == nil && dirty {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the change was never queued: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
