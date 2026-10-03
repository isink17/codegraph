package indexer

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/githistory"
)

// refreshHistory evaluates file-level Git history after the semantic graph is
// complete. It never reads or writes graph rows (history is not semantic
// evidence), and a Git problem becomes a stored absent reason rather than a
// scan error. Only a store failure is returned.
//
// The window is bounded, so no update ever reads full history. While HEAD,
// the window limit and the algorithm are unchanged, only the worktree change
// set is re-read; otherwise the window is recomputed from the new watermark,
// which also covers a rewrite or reset (the old watermark need not be an
// ancestor) and makes every stored value equal to a fresh computation.
func (i *Indexer) refreshHistory(ctx context.Context, repoID int64, root string, disabled bool) (githistory.State, int64, error) {
	start := time.Now()
	state, files, changes, err := i.evaluateHistory(ctx, repoID, root, disabled)
	if err != nil {
		return githistory.State{}, 0, err
	}
	if err := i.store.ReplaceGitHistory(ctx, repoID, state, files, changes); err != nil {
		return state, 0, err
	}
	return state, time.Since(start).Milliseconds(), nil
}

func (i *Indexer) evaluateHistory(ctx context.Context, repoID int64, root string, disabled bool) (githistory.State, []githistory.FileStats, []string, error) {
	absent := func(reason string) (githistory.State, []githistory.FileStats, []string, error) {
		return githistory.Absent(reason), []githistory.FileStats{}, nil, nil
	}
	if disabled {
		return absent(githistory.ReasonDisabled)
	}
	state, err := githistory.Probe(ctx, root)
	if err != nil {
		return absent(githistory.ReasonOf(err))
	}
	indexed, err := i.store.LiveFilePaths(ctx, repoID)
	if err != nil {
		return githistory.State{}, nil, nil, err
	}
	changes, err := githistory.WorktreeChanges(ctx, root, state.Watermark, indexed)
	if err != nil {
		return absent(githistory.ReasonOf(err))
	}
	// codegraph's own database lives under the root; it is not source and its
	// WAL files come and go, so it must never read as a worktree change.
	changes = slices.DeleteFunc(changes, func(p string) bool {
		return strings.HasPrefix(p, config.RepoArtifactsDir+"/")
	})
	// A shallow clone can be deepened without moving HEAD, so its window is
	// always recomputed. The mailmap fingerprint covers .mailmap edits, which
	// Git reads from the working tree.
	prev, found, err := i.store.GitHistoryState(ctx, repoID)
	if err == nil && found && state.Status == githistory.StatusOK && prev.Status == state.Status &&
		prev.Watermark == state.Watermark && prev.WindowLimit == state.WindowLimit &&
		prev.Algorithm == state.Algorithm && prev.Mailmap == state.Mailmap {
		return prev, nil, changes, nil
	}
	files, n, err := githistory.Files(ctx, root, state.Watermark)
	if err != nil {
		return absent(githistory.ReasonOf(err))
	}
	state.WindowCommits = n
	return state, files, changes, nil
}
