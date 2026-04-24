package watcher

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/indexer"
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

		QueueExecs:   w.queueExecs.Load(),
		QueueSkipped: w.queueSkipped.Load(),
		QueueErrors:  w.queueErrors.Load(),
	}
}

func (w *Watcher) Run(ctx context.Context, repoRoot string, repoID int64, debounce time.Duration) error {
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
	if debounce <= 0 {
		debounce = 750 * time.Millisecond
	}

	repoCfg, err := config.LoadRepo(repoRoot)
	if err != nil {
		return err
	}
	includes := repoCfg.Include
	excludes := repoCfg.Exclude

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()

	addWatchTree := func(root string) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path != repoRoot {
				rel, relErr := filepath.Rel(repoRoot, path)
				if relErr != nil {
					return relErr
				}
				rel = filepath.Clean(rel)
				if d.IsDir() && indexer.ShouldSkipDir(rel, excludes) {
					return filepath.SkipDir
				}
			}
			if d.IsDir() {
				return fsw.Add(path)
			}
			return nil
		})
	}

	if err := addWatchTree(repoRoot); err != nil {
		return err
	}

	flush := func() error {
		w.drainRuns.Add(1)
		paths, err := w.store.DrainDirtyFiles(ctx, repoID)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			w.flushNoop.Add(1)
			return nil
		}
		w.drainPaths.Add(int64(len(paths)))
		_, err = w.indexer.Update(ctx, indexer.Options{
			RepoRoot: repoRoot,
			Paths:    paths,
			ScanKind: "watch",
		})
		if err != nil {
			// `DrainDirtyFiles` is destructive; re-queue the drained paths on failure so
			// work isn't silently dropped.
			for _, path := range paths {
				_ = w.store.QueueDirtyFile(ctx, repoID, path, "watch_retry")
			}
			return err
		}
		w.updateRuns.Add(1)
		w.updatePaths.Add(int64(len(paths)))
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
	flushErrCh := make(chan error, 1)

	// Coalesce repeated fsnotify events for the same file between flushes to avoid
	// redundant SQLite upserts under bursty save patterns.
	var seenMu sync.Mutex
	seenSinceFlush := make(map[string]struct{})

	go func() {
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
			case <-ctx.Done():
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
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-flushErrCh:
			return err
		case event, ok := <-fsw.Events:
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
			rel = filepath.Clean(rel)
			if indexer.ShouldIgnorePath(rel, excludes) {
				w.eventsIgnored.Add(1)
				continue
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
					if err := addWatchTree(event.Name); err != nil {
						return err
					}
					// Directory creates are not indexable file updates.
					w.eventsIgnored.Add(1)
					w.eventsIgnoredDir.Add(1)
					continue
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
			select {
			case flushSignalCh <- struct{}{}:
				w.flushSignals.Add(1)
			default:
				w.coalescedSignals.Add(1)
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			return err
		}
	}
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
