package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
)

// AcquireRepositoryScan admits one index/update per repository, before planning
// reads. The separate shared scan lock continues to own liveness and recovery.
// Contention is refused rather than queueing a plan made from an older tree.
// Readers and scans of other repositories do not take this exclusive lock.
func (s *Store) AcquireRepositoryScan(ctx context.Context, root string) (func(), error) {
	if s.path == "" {
		return nil, errors.New("repository scan requires a writable store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonical, err := CanonicalRepoPath(root)
	if err != nil {
		return nil, err
	}
	dbPath, err := CanonicalRepoPath(s.path)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s.writer-%x.lock", dbPath, sha256.Sum256([]byte(canonical)))
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open repository scan lock: %w", err)
	}
	ok, err := lockFile(f, true)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock repository scan: %w", err)
	}
	if !ok {
		_ = f.Close()
		return nil, fmt.Errorf("%w: index/update already owns repository %q; retry after it finishes", ErrScanActive, canonical)
	}
	return func() { releaseScanLockFile(f) }, nil
}
