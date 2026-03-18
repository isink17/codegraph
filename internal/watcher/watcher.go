package watcher

import (
	"context"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/store"
)

type Watcher struct {
	store   *store.Store
	indexer *indexer.Indexer
}

func New(s *store.Store, idx *indexer.Indexer) *Watcher {
	return &Watcher{store: s, indexer: idx}
}

func (w *Watcher) Run(ctx context.Context, repoRoot string, repoID int64, debounce time.Duration) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()

	if err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return fsw.Add(path)
		}
		return nil
	}); err != nil {
		return err
	}

	timer := time.NewTimer(debounce)
	if !timer.Stop() {
		<-timer.C
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, event.Name)
			if err != nil {
				continue
			}
			_ = w.store.QueueDirtyFile(ctx, repoID, filepath.Clean(rel), event.Op.String())
			timer.Reset(debounce)
		case <-timer.C:
			if err := flush(); err != nil {
				return err
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			return err
		}
	}
}
