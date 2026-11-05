package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

type repositoryScanKey struct{}
type repositoryScanLease struct {
	database, repository string
	released             atomic.Bool
}

// AcquireRepositoryScan admits one index/update per repository, before planning
// reads. The separate shared scan lock continues to own liveness and recovery.
// Contention is refused rather than queueing a plan made from an older tree.
// Readers and scans of other repositories do not take this exclusive lock.
func (s *Store) AcquireRepositoryScan(ctx context.Context, root string) (func(), error) {
	_, release, err := AcquireRepositoryScan(ctx, s.path, root)
	return release, err
}

// AcquireRepositoryScan also covers CLI rebuild, whose database must not be
// opened until after removal. Forward the returned context to the indexer and
// retain ownership until its store closes. Inherited ownership is bound to
// both identities and expires when released; it is not a caller-supplied flag.
func AcquireRepositoryScan(ctx context.Context, dbPath, root string) (context.Context, func(), error) {
	if dbPath == "" {
		return ctx, nil, errors.New("repository scan requires a writable store")
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	canonical, err := CanonicalRepoPath(root)
	if err != nil {
		return ctx, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return ctx, nil, err
	}
	// Resolve the parent even for a not-yet-created database, so ownership
	// survives creation through aliases such as /var and /private/var.
	dir, err := CanonicalRepoPath(filepath.Dir(dbPath))
	if err != nil {
		return ctx, nil, err
	}
	dbPath, err = CanonicalRepoPath(filepath.Join(dir, filepath.Base(dbPath)))
	if err != nil {
		return ctx, nil, err
	}
	if held, ok := ctx.Value(repositoryScanKey{}).(*repositoryScanLease); ok && held.database == dbPath && held.repository == canonical && !held.released.Load() {
		return ctx, func() {}, nil
	}
	name := fmt.Sprintf("%s.writer-%x.lock", dbPath, sha256.Sum256([]byte(canonical)))
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return ctx, nil, fmt.Errorf("open repository scan lock: %w", err)
	}
	ok, err := lockFile(f, true)
	if err != nil {
		_ = f.Close()
		return ctx, nil, fmt.Errorf("lock repository scan: %w", err)
	}
	if !ok {
		_ = f.Close()
		return ctx, nil, fmt.Errorf("%w: index/update already owns repository %q; retry after it finishes", ErrScanActive, canonical)
	}
	lease := &repositoryScanLease{database: dbPath, repository: canonical}
	return context.WithValue(ctx, repositoryScanKey{}, lease), func() {
		if !lease.released.Swap(true) {
			releaseScanLockFile(f)
		}
	}, nil
}
