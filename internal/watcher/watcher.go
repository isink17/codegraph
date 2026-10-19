package watcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/platform"
	"github.com/isink17/codegraph/internal/store"
)

type Watcher struct {
	store   *store.Store
	indexer *indexer.Indexer

	eventsSeen         atomic.Int64
	eventsIgnored      atomic.Int64
	eventsIgnoredDir   atomic.Int64
	eventsIgnoredChmod atomic.Int64

	flushSignals     atomic.Int64
	coalescedSignals atomic.Int64
	flushRuns        atomic.Int64
	flushErrors      atomic.Int64
	flushNoop        atomic.Int64

	queueExecs   atomic.Int64
	queueSkipped atomic.Int64
	queueErrors  atomic.Int64

	drainRuns   atomic.Int64
	drainPaths  atomic.Int64
	updateRuns  atomic.Int64
	updatePaths atomic.Int64

	// profileConverges counts the full passes this watcher ran because a
	// path-scoped flush could not safely convert a language's parser profile.
	profileConverges atomic.Int64

	overflows atomic.Int64
}

type WatchStats struct {
	EventsSeen         int64 `json:"events_seen"`
	EventsIgnored      int64 `json:"events_ignored"`
	EventsIgnoredDir   int64 `json:"events_ignored_dir"`
	EventsIgnoredChmod int64 `json:"events_ignored_chmod"`

	FlushSignals     int64 `json:"flush_signals"`
	CoalescedSignals int64 `json:"coalesced_signals"`
	FlushRuns        int64 `json:"flush_runs"`
	FlushErrors      int64 `json:"flush_errors"`
	FlushNoop        int64 `json:"flush_noop"`

	DrainRuns   int64 `json:"drain_runs"`
	DrainPaths  int64 `json:"drain_paths"`
	UpdateRuns  int64 `json:"update_runs"`
	UpdatePaths int64 `json:"update_paths"`

	// ProfileConverges counts the full passes forced by a parser-profile
	// transition a path-scoped flush could not safely make.
	ProfileConverges int64 `json:"profile_converges,omitempty"`

	// Overflows counts event-queue overflows, each answered by re-registering
	// the watch tree and a full reconciliation update.
	Overflows int64 `json:"overflows,omitempty"`

	QueueExecs   int64 `json:"queue_execs"`
	QueueSkipped int64 `json:"queue_skipped"`
	QueueErrors  int64 `json:"queue_errors"`
}

func New(s *store.Store, idx *indexer.Indexer) *Watcher {
	return &Watcher{store: s, indexer: idx}
}

func (w *Watcher) Stats() WatchStats {
	return WatchStats{
		EventsSeen:         w.eventsSeen.Load(),
		EventsIgnored:      w.eventsIgnored.Load(),
		EventsIgnoredDir:   w.eventsIgnoredDir.Load(),
		EventsIgnoredChmod: w.eventsIgnoredChmod.Load(),

		FlushSignals:     w.flushSignals.Load(),
		CoalescedSignals: w.coalescedSignals.Load(),
		FlushRuns:        w.flushRuns.Load(),
		FlushErrors:      w.flushErrors.Load(),
		FlushNoop:        w.flushNoop.Load(),

		DrainRuns:   w.drainRuns.Load(),
		DrainPaths:  w.drainPaths.Load(),
		UpdateRuns:  w.updateRuns.Load(),
		UpdatePaths: w.updatePaths.Load(),

		ProfileConverges: w.profileConverges.Load(),
		Overflows:        w.overflows.Load(),

		QueueExecs:   w.queueExecs.Load(),
		QueueSkipped: w.queueSkipped.Load(),
		QueueErrors:  w.queueErrors.Load(),
	}
}

func isRelPathWithinRepo(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, "../")
}

func isWatcherConfigPath(rel string) bool {
	rel = strings.TrimPrefix(rel, "./")
	if strings.EqualFold(rel, ".codegraphignore") {
		return true
	}
	if strings.EqualFold(rel, pathpkg.Join(config.RepoArtifactsDir, "config.json")) {
		return true
	}
	return false
}

func (w *Watcher) Run(ctx context.Context, repoRoot string, repoID int64, debounce time.Duration) error {
	return w.run(ctx, repoRoot, repoID, debounce, nil)
}

// RunWithReady behaves exactly like Run and additionally calls ready once,
// synchronously, after config loading, fsnotify creation, the initial
// recursive watch registration, any startup dirty-queue flush, and the flush
// loop start have all succeeded. It is not called when setup fails or ctx is
// canceled first. Readiness means only that watcher setup succeeded; it does
// not claim that every OS event will be delivered or that the index is fresh.
func (w *Watcher) RunWithReady(ctx context.Context, repoRoot string, repoID int64, debounce time.Duration, ready func()) error {
	return w.run(ctx, repoRoot, repoID, debounce, ready)
}

func (w *Watcher) run(ctx context.Context, repoRoot string, repoID int64, debounce time.Duration, ready func()) error {
	w.eventsSeen.Store(0)
	w.eventsIgnored.Store(0)
	w.eventsIgnoredDir.Store(0)
	w.eventsIgnoredChmod.Store(0)
	w.flushSignals.Store(0)
	w.coalescedSignals.Store(0)
	w.flushRuns.Store(0)
	w.flushErrors.Store(0)
	w.flushNoop.Store(0)
	w.queueExecs.Store(0)
	w.queueSkipped.Store(0)
	w.queueErrors.Store(0)
	w.drainRuns.Store(0)
	w.drainPaths.Store(0)
	w.updateRuns.Store(0)
	w.updatePaths.Store(0)
	w.profileConverges.Store(0)
	w.overflows.Store(0)
	if debounce <= 0 {
		debounce = 750 * time.Millisecond
	}

	repoCfg, err := config.LoadRepo(repoRoot)
	if err != nil {
		return err
	}
	includes := repoCfg.Include
	excludes := repoCfg.Exclude

	var forceFullScan atomic.Bool

	newFSWatcher := func() (*fsnotify.Watcher, error) {
		fw, err := fsnotify.NewWatcher()
		if err == nil && watcherCreatedHook != nil {
			watcherCreatedHook(fw)
		}
		return fw, err
	}
	fsw, err := newFSWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = fsw.Close() }()
	eventsCh := fsw.Events
	errorsCh := fsw.Errors

	// watchedDirs holds every registered directory's native path. Only the
	// event goroutine touches it.
	watchedDirs := map[string]struct{}{}
	// addWatchTreeTo registers root and its directories and passes each
	// indexable file under it to found, when non-nil. Entries removed during
	// the walk are skipped, nothing is left in them to watch; a missing
	// repository root is still an error.
	addWatchTreeTo := func(target *fsnotify.Watcher, root string, found func(rel string)) error {
		addDir := func(path string) error {
			if err := target.Add(path); err != nil {
				if errors.Is(err, fs.ErrNotExist) && path != repoRoot {
					return filepath.SkipDir
				}
				return err
			}
			watchedDirs[path] = struct{}{}
			return nil
		}
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) && path != repoRoot {
				return nil
			}
			if err != nil {
				return err
			}
			logicalRel := ""
			if path != repoRoot {
				rel, relErr := filepath.Rel(repoRoot, path)
				if relErr != nil {
					return relErr
				}
				nativeRel := filepath.Clean(rel)
				var logicalErr error
				logicalRel, logicalErr = platform.NativeRelativeToLogical(nativeRel)
				if logicalErr != nil {
					return logicalErr
				}
				if d.IsDir() && indexer.ShouldSkipDir(logicalRel, excludes) {
					if pathpkg.Base(logicalRel) == config.RepoArtifactsDir {
						// Keep watching repo-local config updates even though artifacts are
						// never indexable.
						if addErr := addDir(path); addErr != nil && addErr != filepath.SkipDir {
							return addErr
						}
					}
					return filepath.SkipDir
				}
			}
			if d.IsDir() {
				return addDir(path)
			}
			if found != nil && !indexer.ShouldIgnorePath(logicalRel, excludes) && !indexer.ShouldSkipFile(logicalRel, includes, excludes) {
				found(logicalRel)
			}
			return nil
		})
	}
	addWatchTree := func(root string) error { return addWatchTreeTo(fsw, root, nil) }

	// resetWatcher rebuilds the watcher so include/exclude changes take effect
	// (especially for directories that were previously skipped and not
	// watched). By default the new watcher is fully populated before the old
	// one closes, so events on already-watched paths keep flowing during the
	// rebuild. closeFirst closes the old one first instead, so the tree is
	// never registered twice (inotify watches and kqueue descriptors are
	// limited); the caller must cover the gap with a full scan.
	resetWatcher := func(closeFirst bool) error {
		if closeFirst {
			_ = fsw.Close()
		}
		next, err := newFSWatcher()
		if err != nil {
			return err
		}
		clear(watchedDirs)
		if err := addWatchTreeTo(next, repoRoot, nil); err != nil {
			_ = next.Close()
			return err
		}
		old := fsw
		fsw = next
		eventsCh = next.Events
		errorsCh = next.Errors
		if !closeFirst {
			_ = old.Close()
		}
		return nil
	}

	if err := addWatchTree(repoRoot); err != nil {
		return err
	}

	flush := func() error {
		w.drainRuns.Add(1)
		force := forceFullScan.Swap(false)
		claimedAt := time.Now().UTC().Format(time.RFC3339Nano)
		paths, err := w.store.ClaimDirtyFiles(ctx, repoID, claimedAt, "watch_inflight")
		if err != nil {
			return err
		}
		if len(paths) == 0 && !force {
			w.flushNoop.Add(1)
			return nil
		}
		w.drainPaths.Add(int64(len(paths)))
		opts := indexer.Options{
			RepoRoot: repoRoot,
			ScanKind: "watch",
		}
		if force {
			opts.ScanKind = "watch_config"
		} else {
			opts.Paths = paths
		}
		if err := w.updateConverging(ctx, repoRoot, opts); err != nil {
			return err
		}
		if len(paths) > 0 {
			deleteCtx := context.WithoutCancel(ctx)
			if err := w.store.DeleteClaimedDirtyFiles(deleteCtx, repoID, paths, claimedAt); err != nil {
				return err
			}
		}
		w.updateRuns.Add(1)
		if !force {
			w.updatePaths.Add(int64(len(paths)))
		}
		return err
	}

	if hasDirty, err := w.store.HasDirtyFiles(ctx, repoID); err != nil {
		return err
	} else if hasDirty {
		// Ensure any queued work from previous runs is processed even if no new
		// fsnotify events occur.
		w.flushRuns.Add(1)
		if err := flush(); err != nil {
			w.flushErrors.Add(1)
			return err
		}
	}

	flushSignalCh := make(chan struct{}, 1)
	signalFlush := func() {
		select {
		case flushSignalCh <- struct{}{}:
			w.flushSignals.Add(1)
		default:
			w.coalescedSignals.Add(1)
		}
	}
	flushErrCh := make(chan error, 1)

	// Coalesce repeated fsnotify events for the same file between flushes to avoid
	// redundant SQLite upserts under bursty save patterns.
	var seenMu sync.Mutex
	seenSinceFlush := make(map[string]struct{})

	// The flush loop is owned by this call: it stops and is awaited before Run
	// returns on any path, because the caller closes the store and indexer
	// right after.
	loopCtx, stopLoop := context.WithCancel(ctx)
	var loopDone sync.WaitGroup
	defer func() {
		stopLoop()
		loopDone.Wait()
	}()
	loopDone.Go(func() {
		timer := time.NewTimer(debounce)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		pending := false
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-flushSignalCh:
				pending = true
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounce)
			case <-timer.C:
				if !pending {
					continue
				}
				pending = false
				seenMu.Lock()
				clear(seenSinceFlush)
				seenMu.Unlock()
				w.flushRuns.Add(1)
				if err := flush(); err != nil {
					w.flushErrors.Add(1)
					select {
					case flushErrCh <- err:
					default:
					}
					return
				}
			}
		}
	})

	if ready != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-flushErrCh:
			return err
		case event, ok := <-eventsCh:
			if !ok {
				return nil
			}
			w.eventsSeen.Add(1)

			// Avoid churn from chmod-only events (common on some editors).
			if event.Op == fsnotify.Chmod {
				w.eventsIgnored.Add(1)
				w.eventsIgnoredChmod.Add(1)
				continue
			}
			rel, err := filepath.Rel(repoRoot, event.Name)
			if err != nil {
				w.eventsIgnored.Add(1)
				continue
			}
			rel, err = platform.NativeRelativeToLogical(rel)
			if err != nil {
				w.eventsIgnored.Add(1)
				continue
			}
			if !isRelPathWithinRepo(rel) {
				w.eventsIgnored.Add(1)
				continue
			}

			// Config changes must be handled before ignore filtering since repo-local
			// config lives under ignored paths (e.g. `.codegraph/**`).
			if isWatcherConfigPath(rel) {
				nextCfg, loadErr := config.LoadRepo(repoRoot)
				if loadErr == nil {
					includes = nextCfg.Include
					excludes = nextCfg.Exclude
					forceFullScan.Store(true)
					if err := resetWatcher(false); err != nil {
						return err
					}
					signalFlush()
				}
				w.eventsIgnored.Add(1)
				continue
			}
			if indexer.ShouldIgnorePath(rel, excludes) {
				w.eventsIgnored.Add(1)
				continue
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
					if createdDirHook != nil {
						createdDirHook(event.Name)
					}
					// A directory moved or copied in brings files that raise no
					// events of their own, and files written before its watch
					// was added raised none either.
					var files []string
					if err := addWatchTreeTo(fsw, event.Name, func(rel string) { files = append(files, rel) }); err != nil {
						return err
					}
					w.eventsIgnored.Add(1)
					w.eventsIgnoredDir.Add(1)
					if len(files) > 0 {
						if err := w.queueDirtyPathsWithRetry(ctx, repoID, files, "create"); err != nil {
							w.queueErrors.Add(1)
							return err
						}
						w.queueExecs.Add(1)
						signalFlush()
					}
					continue
				}
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				if _, wasDir := watchedDirs[event.Name]; wasDir {
					if _, statErr := os.Lstat(event.Name); errors.Is(statErr, fs.ErrNotExist) {
						// Its files raise no events when it is moved out.
						prefix := event.Name + string(filepath.Separator)
						for dir := range watchedDirs {
							if dir == event.Name || strings.HasPrefix(dir, prefix) {
								_ = fsw.Remove(dir)
								delete(watchedDirs, dir)
							}
						}
						// Descendants were dropped from watchedDirs above, so
						// their own events do not repeat this query.
						gone, err := w.store.LiveFilePathsUnder(ctx, repoID, rel)
						if err != nil {
							return err
						}
						if len(gone) > 0 {
							if err := w.queueDirtyPathsWithRetry(ctx, repoID, gone, "remove"); err != nil {
								w.queueErrors.Add(1)
								return err
							}
							w.queueExecs.Add(1)
							signalFlush()
						}
						w.eventsIgnored.Add(1)
						w.eventsIgnoredDir.Add(1)
						continue
					}
				}
			}
			if indexer.ShouldSkipFile(rel, includes, excludes) {
				w.eventsIgnored.Add(1)
				continue
			}

			seenMu.Lock()
			_, alreadyQueued := seenSinceFlush[rel]
			if !alreadyQueued {
				seenSinceFlush[rel] = struct{}{}
			}
			seenMu.Unlock()

			if !alreadyQueued {
				w.queueExecs.Add(1)
				if err := w.queueDirtyWithRetry(ctx, repoID, rel, event.Op.String()); err != nil {
					w.queueErrors.Add(1)
					return err
				}
			} else {
				w.queueSkipped.Add(1)
			}
			signalFlush()
		case err, ok := <-errorsCh:
			if !ok {
				return nil
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				// Dropped events cannot be replayed: directories created
				// meanwhile are unwatched and files changed meanwhile are
				// unqueued. Re-register the tree, releasing the old watches
				// first, then reconcile everything through the incremental
				// full update, which skips files whose size and mtime are
				// unchanged and covers events lost during the re-registration.
				w.overflows.Add(1)
				if err := resetWatcher(true); err != nil {
					return err
				}
				forceFullScan.Store(true)
				signalFlush()
				continue
			}
			return err
		}
	}
}

func (w *Watcher) queueDirtyPathsWithRetry(ctx context.Context, repoID int64, paths []string, reason string) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if lastErr = w.store.QueueDirtyFiles(ctx, repoID, paths, reason); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt*50) * time.Millisecond):
		}
	}
	return fmt.Errorf("queue %d dirty files: %w", len(paths), lastErr)
}

func (w *Watcher) queueDirtyWithRetry(ctx context.Context, repoID int64, path, reason string) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := w.store.QueueDirtyFile(ctx, repoID, path, reason); err == nil {
			return nil
		} else {
			lastErr = err
		}
		delay := time.Duration(attempt*50) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("queue dirty file %s: %w", path, lastErr)
}

// updateConverging runs one watcher update, upgrading a path-scoped run to a
// full pass when parser-profile safety refuses the incremental one.
//
// Without this, every existing user upgrading past migration 036 would watch
// their watcher die: the first flush against an already-indexed repository is
// path-scoped, the repository's provenance is unknown or stale, and a returned
// error aborts the run for good while the claimed paths stay in flight. The
// scan is asking for exactly one full convergence pass, which the watcher is
// perfectly able to perform -- and the flushed paths are inside the full walk,
// so the caller can still retire them afterwards.
//
// A refused DOWNGRADE is not upgraded here: re-running it as a full pass would
// be refused again, and correctly so. It reaches the caller as an error.
func (w *Watcher) updateConverging(ctx context.Context, repoRoot string, opts indexer.Options) error {
	_, err := w.indexer.Update(ctx, opts)
	if !errors.Is(err, indexer.ErrParserProfileTransitionRequired) {
		return err
	}
	w.profileConverges.Add(1)
	_, err = w.indexer.Update(ctx, indexer.Options{RepoRoot: repoRoot, ScanKind: "watch_config"})
	return err
}

// Test hooks: watcherCreatedHook sees every fsnotify watcher Run creates;
// createdDirHook runs before a newly created directory is registered.
var (
	watcherCreatedHook func(*fsnotify.Watcher)
	createdDirHook     func(path string)
)
