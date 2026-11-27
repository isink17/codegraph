package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/store"
)

type watchRun struct {
	cancel context.CancelFunc
	done   chan error
	state  chan string
}

// startWatch runs `watch --jsonl` for root in this process and reports each
// run_state it prints.
func startWatch(t *testing.T, cfg config.Config, root string) *watchRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	r := &watchRun{cancel: cancel, done: make(chan error, 1), state: make(chan string, 16)}
	go func() {
		err := watchRepo(ctx, cfg, pw, []string{"--jsonl", root})
		_ = pw.Close()
		r.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			var ev struct {
				Type     string `json:"type"`
				RunState string `json:"run_state"`
			}
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "watch_status" {
				r.state <- ev.RunState
			}
		}
		close(r.state)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("watch did not stop after cancel")
		}
	})
	return r
}

func (r *watchRun) waitState(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got, ok := <-r.state:
			if !ok {
				t.Fatalf("watch output ended before run_state %s", want)
			}
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("watch never reported run_state %s", want)
		}
	}
}

func (r *watchRun) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not stop after cancel")
		return nil
	}
}

type rebuildFixture struct {
	cfg      config.Config
	root     string
	dbPath   string
	removals *int
}

func newRebuildFixture(t *testing.T, cfg config.Config) *rebuildFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc Original() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runIndex(context.Background(), cfg, io.Discard, "index", []string{root}, false); err != nil {
		t.Fatal(err)
	}
	canonical, err := store.CanonicalRepoPath(root)
	if err != nil {
		t.Fatal(err)
	}
	dbPath, err := dbPathForRepo(cfg, root, canonical)
	if err != nil {
		t.Fatal(err)
	}
	f := &rebuildFixture{cfg: cfg, root: root, dbPath: dbPath, removals: new(int)}
	remove := repoDBRemove
	repoDBRemove = func(path string) error { *f.removals++; return remove(path) }
	t.Cleanup(func() { repoDBRemove = remove })
	return f
}

func (f *rebuildFixture) rebuild() error {
	return runIndex(context.Background(), f.cfg, io.Discard, "index", []string{f.root, "--rebuild"}, false)
}

func (f *rebuildFixture) identity(t *testing.T) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// livePaths reads the indexed paths through a store opened now, so it sees
// the file at the database path rather than any file a watcher opened.
func (f *rebuildFixture) livePaths(t *testing.T) []string {
	t.Helper()
	s, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := s.LiveFilePaths(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func (f *rebuildFixture) waitIndexed(t *testing.T, rel string, run *watchRun) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(f.livePaths(t), rel) {
		select {
		case err := <-run.done:
			run.done <- err
			t.Fatalf("watch stopped before indexing %s: %v", rel, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached the database at its path", rel)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func repoLocalConfig() config.Config { return config.Config{DBDir: config.RepoDBDir} }

// A starting watcher is protected at every stage of startup: right after it
// opens its database and right after it takes its watcher lock, both under the
// writer lock, a rebuild is refused as an active scan; once the writer lock is
// released and before the watcher runs, the start lock refuses it. In no stage
// may the file be replaced, and once the watcher runs its writes must reach the
// database at the path and its start lock must be gone.
func TestRebuildCannotReplaceDatabaseOfStartingWatcher(t *testing.T) {
	configs := map[string]func(t *testing.T) config.Config{
		"shared-db-dir": func(t *testing.T) config.Config { return config.Config{DBDir: t.TempDir()} },
		"repo-local":    func(*testing.T) config.Config { return repoLocalConfig() },
	}
	stages := map[string]error{"opened": store.ErrScanActive, "locked": store.ErrScanActive, "run": store.ErrWatcherActive}
	for name, cfg := range configs {
		for stage, want := range stages {
			t.Run(name+"/"+stage, func(t *testing.T) {
				f := newRebuildFixture(t, cfg(t))
				paused, proceed := make(chan struct{}), make(chan struct{})
				watchStartHook = func(s string) {
					if s == stage {
						close(paused)
						<-proceed
					}
				}
				t.Cleanup(func() { watchStartHook = func(string) {} })
				before := f.identity(t)
				run := startWatch(t, f.cfg, f.root)
				select {
				case <-paused:
				case err := <-run.done:
					t.Fatalf("watch stopped before stage %s: %v", stage, err)
				}
				err := f.rebuild()
				close(proceed)
				if !errors.Is(err, want) || *f.removals != 0 || !os.SameFile(before, f.identity(t)) {
					t.Fatalf("rebuild at watch stage %s: err=%v removals=%d, want %v and no removal", stage, err, *f.removals, want)
				}
				run.waitState(t, watchRunWatching)
				f.waitLiveWatcher(t, store.LiveWatcherWatching)
				if err := f.rebuild(); !errors.Is(err, store.ErrWatcherActive) || *f.removals != 0 {
					t.Fatalf("rebuild while watching: err=%v removals=%d", err, *f.removals)
				}
				if err := os.WriteFile(filepath.Join(f.root, "b.go"), []byte("package main\nfunc Added() {}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				f.waitIndexed(t, "b.go", run)
				if err := run.stop(t); err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("watch: %v", err)
				}
				if err := f.rebuild(); err != nil || *f.removals == 0 {
					t.Fatalf("rebuild after watch exited: err=%v removals=%d", err, *f.removals)
				}
				if !slices.Contains(f.livePaths(t), "b.go") {
					t.Fatal("rebuild after watch lost b.go")
				}
			})
		}
	}
}

// waitLiveWatcher waits until a fresh read reports want: a watcher that has
// reconciled must not keep reading as unverified because of its start lock.
func (f *rebuildFixture) waitLiveWatcher(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := store.Open(f.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		repo, err := s.UpsertRepo(context.Background(), f.root)
		if err != nil {
			t.Fatal(err)
		}
		fr, err := s.FreshnessStatus(context.Background(), repo.ID)
		_ = s.Close()
		if err != nil {
			t.Fatal(err)
		}
		if fr.LiveWatcher == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("live watcher %s, want %s (verdict %s)", fr.LiveWatcher, want, fr.Verdict)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *rebuildFixture) verdict(t *testing.T) string {
	t.Helper()
	s, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	fr, err := s.FreshnessStatus(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fr.Verdict
}

// A watch that starts while a rebuild replaces the database refuses before it
// opens anything; once the rebuild has finished it starts on the new file.
func TestWatchStartRefusedWhileRebuildReplacesDatabase(t *testing.T) {
	f := newRebuildFixture(t, config.Config{DBDir: t.TempDir()})
	removing, finish := make(chan struct{}), make(chan struct{})
	remove := repoDBRemove
	var once bool
	repoDBRemove = func(path string) error {
		if !once {
			once = true
			close(removing)
			<-finish
		}
		return remove(path)
	}
	rebuilt := make(chan error, 1)
	go func() { rebuilt <- f.rebuild() }()
	<-removing
	watchStartHook = func(string) { t.Error("watch opened the database during a rebuild") }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := watchRepo(ctx, f.cfg, io.Discard, []string{"--jsonl", f.root})
	cancel()
	watchStartHook = func(string) {}
	close(finish)
	if !errors.Is(err, store.ErrScanActive) {
		t.Fatalf("watch during rebuild: %v", err)
	}
	if err := <-rebuilt; err != nil {
		t.Fatal(err)
	}
	run := startWatch(t, f.cfg, f.root)
	run.waitState(t, watchRunWatching)
	if err := os.WriteFile(filepath.Join(f.root, "b.go"), []byte("package main\nfunc Added() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.waitIndexed(t, "b.go", run)
}

// Watcher locks belong to one database: a watch on one repository does not
// block rebuilding another that shares the database directory.
func TestWatchDoesNotBlockRebuildOfAnotherRepository(t *testing.T) {
	cfg := config.Config{DBDir: t.TempDir()}
	watched := newRebuildFixture(t, cfg)
	other := newRebuildFixture(t, cfg)
	if watched.dbPath == other.dbPath {
		t.Fatal("repositories share a database path")
	}
	run := startWatch(t, cfg, watched.root)
	run.waitState(t, watchRunWatching)
	if err := watched.rebuild(); !errors.Is(err, store.ErrWatcherActive) {
		t.Fatalf("rebuild of the watched repository: %v", err)
	}
	if err := other.rebuild(); err != nil {
		t.Fatalf("rebuild of another repository: %v", err)
	}
}

// A watch cancelled while starting, or one that fails after taking its locks,
// leaves nothing held that would block a later rebuild.
func TestStoppedWatchStartLeavesRebuildFree(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		f := newRebuildFixture(t, config.Config{DBDir: t.TempDir()})
		ctx, cancel := context.WithCancel(context.Background())
		watchStartHook = func(string) { cancel() }
		t.Cleanup(func() { watchStartHook = func(string) {} })
		if err := watchRepo(ctx, f.cfg, io.Discard, []string{"--jsonl", f.root}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled watch: %v", err)
		}
		if err := f.rebuild(); err != nil {
			t.Fatalf("rebuild after a cancelled watch: %v", err)
		}
	})
	t.Run("invalid-config", func(t *testing.T) {
		f := newRebuildFixture(t, config.Config{DBDir: t.TempDir()})
		cfgPath := config.RepoConfigPath(f.root)
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, []byte(`{"exclude": 7}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := watchRepo(context.Background(), f.cfg, io.Discard, []string{"--jsonl", f.root}); err == nil {
			t.Fatal("watch started with an invalid config")
		}
		if err := os.Remove(cfgPath); err != nil {
			t.Fatal(err)
		}
		if err := f.rebuild(); err != nil {
			t.Fatalf("rebuild after a failed watch: %v", err)
		}
	})
}

const watchHelperEnv = "CODEGRAPH_WATCH_REBUILD_HELPER"

// The same exclusion holds between processes: watch runs in a child process
// that pauses after opening its database until the parent says to continue,
// and stops when the parent closes its input.
func TestRebuildExcludesWatchInAnotherProcess(t *testing.T) {
	if spec := os.Getenv(watchHelperEnv); spec != "" {
		var args struct{ DBDir, Root string }
		if err := json.Unmarshal([]byte(spec), &args); err != nil {
			os.Exit(2)
		}
		in := bufio.NewScanner(os.Stdin)
		ctx, cancel := context.WithCancel(context.Background())
		watchStartHook = func(stage string) {
			if stage != "opened" {
				return
			}
			os.Stdout.WriteString("{\"type\":\"helper_opened\"}\n")
			in.Scan()
			go func() { in.Scan(); cancel() }()
		}
		if err := watchRepo(ctx, config.Config{DBDir: args.DBDir}, os.Stdout, []string{"--jsonl", args.Root}); err != nil && !errors.Is(err, context.Canceled) {
			os.Exit(3)
		}
		os.Exit(0)
	}
	f := newRebuildFixture(t, config.Config{DBDir: t.TempDir()})
	spec, _ := json.Marshal(map[string]string{"DBDir": f.cfg.DBDir, "Root": f.root})
	cmd := exec.Command(os.Args[0], "-test.run=^TestRebuildExcludesWatchInAnotherProcess$")
	cmd.Env = append(os.Environ(), watchHelperEnv+"="+string(spec))
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
	events := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var ev struct {
				Type     string `json:"type"`
				RunState string `json:"run_state"`
			}
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				events <- ev.Type + "/" + ev.RunState
			}
		}
		close(events)
	}()
	wait := func(want string) {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case got, ok := <-events:
				if !ok {
					t.Fatalf("helper exited before %s", want)
				}
				if got == want {
					return
				}
			case <-deadline:
				t.Fatalf("helper never reported %s", want)
			}
		}
	}
	wait("helper_opened/")
	before := f.identity(t)
	if err := f.rebuild(); !errors.Is(err, store.ErrScanActive) || *f.removals != 0 || !os.SameFile(before, f.identity(t)) {
		t.Fatalf("rebuild while the other process's watch was starting: err=%v removals=%d", err, *f.removals)
	}
	if _, err := io.WriteString(stdin, "go\n"); err != nil {
		t.Fatal(err)
	}
	wait("watch_status/watching")
	if err := f.rebuild(); !errors.Is(err, store.ErrWatcherActive) || *f.removals != 0 {
		t.Fatalf("rebuild while the other process watches: err=%v removals=%d", err, *f.removals)
	}
	if err := os.WriteFile(filepath.Join(f.root, "b.go"), []byte("package main\nfunc Added() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !slices.Contains(f.livePaths(t), "b.go") {
		if time.Now().After(deadline) {
			t.Fatal("the other process's watch never indexed b.go into the database at its path")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = stdin.Close()
	err = cmd.Wait()
	finished = true
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	if err := f.rebuild(); err != nil || *f.removals == 0 {
		t.Fatalf("rebuild after the other process exited: err=%v removals=%d", err, *f.removals)
	}
	if v := f.verdict(t); v != "consistent_with_last_scan" || !slices.Contains(f.livePaths(t), "b.go") {
		t.Fatalf("after rebuild: verdict %s paths %v", v, f.livePaths(t))
	}
}
