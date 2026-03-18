package watcher

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/store"
)

type Watcher struct {
	store   *store.Store
	indexer *indexer.Indexer

	flushSignals     atomic.Int64
	coalescedSignals atomic.Int64
	flushRuns        atomic.Int64
	flushErrors      atomic.Int64
}

type WatchStats struct {
	FlushSignals     int64 `json:"flush_signals"`
	CoalescedSignals int64 `json:"coalesced_signals"`
	FlushRuns        int64 `json:"flush_runs"`
	FlushErrors      int64 `json:"flush_errors"`
}

func New(s *store.Store, idx *indexer.Indexer) *Watcher {
	return &Watcher{store: s, indexer: idx}
}

func (w *Watcher) Stats() WatchStats {
	return WatchStats{
		FlushSignals:     w.flushSignals.Load(),
		CoalescedSignals: w.coalescedSignals.Load(),
		FlushRuns:        w.flushRuns.Load(),
		FlushErrors:      w.flushErrors.Load(),
	}
}

func (w *Watcher) Run(ctx context.Context, repoRoot string, repoID int64, debounce time.Duration) error {
	w.flushSignals.Store(0)
	w.coalescedSignals.Store(0)
	w.flushRuns.Store(0)
	w.flushErrors.Store(0)
	if debounce <= 0 {
		debounce = 750 * time.Millisecond
	}

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
				if d.IsDir() && indexer.ShouldSkipDir(rel, nil) {
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
		paths, err := w.store.DrainDirtyFiles(ctx, repoID)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			return nil
		}
		_, err = w.indexer.Update(ctx, indexer.Options{
			RepoRoot: repoRoot,
			Paths:    paths,
			ScanKind: "watch",
		})
		return err
	}

	flushSignalCh := make(chan struct{}, 1)
	flushErrCh := make(chan error, 1)
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
			rel, err := filepath.Rel(repoRoot, event.Name)
			if err != nil {
				continue
			}
			rel = filepath.Clean(rel)
			if shouldIgnorePath(rel) {
				continue
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
					if err := addWatchTree(event.Name); err != nil {
						return err
					}
				}
			}
			_ = w.store.QueueDirtyFile(ctx, repoID, rel, event.Op.String())
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

func shouldIgnorePath(rel string) bool {
	current := rel
	for current != "." && current != "" {
		if indexer.ShouldSkipDir(current, nil) {
			return true
		}
		next := filepath.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	return false
}
