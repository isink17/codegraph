package indexer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/githistory"
	"github.com/isink17/codegraph/internal/store"
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
	state, files, changes, symbols, err := i.evaluateHistory(ctx, repoID, root, disabled)
	if err != nil {
		return githistory.State{}, 0, err
	}
	if err := i.store.ReplaceGitHistory(ctx, repoID, state, files, changes, symbols); err != nil {
		return state, 0, err
	}
	return state, time.Since(start).Milliseconds(), nil
}

func (i *Indexer) evaluateHistory(ctx context.Context, repoID int64, root string, disabled bool) (githistory.State, []githistory.FileStats, []string, store.GitSymbolUpdate, error) {
	absent := func(reason string) (githistory.State, []githistory.FileStats, []string, store.GitSymbolUpdate, error) {
		return githistory.Absent(reason), []githistory.FileStats{}, nil, store.GitSymbolUpdate{All: true}, nil
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
		return githistory.State{}, nil, nil, store.GitSymbolUpdate{}, err
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
	var files []githistory.FileStats
	prev, found, err := i.store.GitHistoryState(ctx, repoID)
	reuse := err == nil && found && state.Status == githistory.StatusOK && prev.Status == state.Status &&
		prev.Watermark == state.Watermark && prev.WindowLimit == state.WindowLimit &&
		prev.Algorithm == state.Algorithm && prev.Mailmap == state.Mailmap
	if reuse {
		state = prev
	} else {
		var n int
		files, n, err = githistory.Files(ctx, root, state.Watermark)
		if err != nil {
			return absent(githistory.ReasonOf(err))
		}
		state.WindowCommits = n
	}
	symbols, err := i.symbolHistory(ctx, repoID, root, state, changes, reuse)
	if err != nil {
		var gerr *githistory.Error
		if errors.As(err, &gerr) {
			return absent(gerr.Reason)
		}
		return githistory.State{}, nil, nil, store.GitSymbolUpdate{}, err
	}
	return state, files, changes, symbols, nil
}

// symbolHistory blames the files whose symbol ranges are trusted and whose
// working-tree content equals the watermark; a dirty or untracked file gets
// no symbol rows, because its ranges come from content blame cannot see.
// Rows are a pure function of the reuse key and the file's ranges, so when
// the watermark is reused only files whose range set changed are blamed
// again, and the result equals a fresh evaluation.
func (i *Indexer) symbolHistory(ctx context.Context, repoID int64, root string, state githistory.State, changes []string, reuse bool) (store.GitSymbolUpdate, error) {
	want, err := i.store.GitSymbolRanges(ctx, repoID)
	if err != nil {
		return store.GitSymbolUpdate{}, err
	}
	for _, p := range changes {
		delete(want, p)
	}
	candidates := make([]string, 0, len(want))
	for p := range want {
		candidates = append(candidates, p)
	}
	slices.Sort(candidates)
	filtered, err := githistory.Filtered(ctx, root, candidates)
	if err != nil {
		return store.GitSymbolUpdate{}, err
	}
	for _, p := range filtered {
		delete(want, p)
	}
	update := store.GitSymbolUpdate{All: !reuse, Filtered: filtered}
	var blame []string
	if reuse {
		have, err := i.store.StoredGitSymbolRanges(ctx, repoID)
		if err != nil {
			return store.GitSymbolUpdate{}, err
		}
		for p := range have {
			if _, ok := want[p]; !ok {
				update.Paths = append(update.Paths, p)
			}
		}
		for p, ranges := range want {
			if !slices.Equal(have[p], ranges) {
				update.Paths = append(update.Paths, p)
				blame = append(blame, p)
			}
		}
		slices.Sort(update.Paths)
	} else {
		for p := range want {
			blame = append(blame, p)
		}
	}
	if len(blame) == 0 {
		return update, nil
	}
	slices.Sort(blame)
	boundary, err := githistory.WindowBoundary(ctx, root, state.Watermark, state.WindowCommits)
	if err != nil {
		return store.GitSymbolUpdate{}, err
	}
	shallow := state.Status == githistory.StatusTruncated
	results := make([][]githistory.SymbolStats, len(blame))
	errs := make([]error, len(blame))
	// ponytail: fixed pool of blame processes; Git does the work, so more
	// workers mostly add contention.
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < min(4, len(blame)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range next {
				results[k], errs[k] = githistory.Blame(ctx, root, state.Watermark, boundary, blame[k], shallow, want[blame[k]])
			}
		}()
	}
	for k := range blame {
		next <- k
	}
	close(next)
	wg.Wait()
	for k := range blame {
		if errs[k] != nil {
			return store.GitSymbolUpdate{}, errs[k]
		}
		update.Rows = append(update.Rows, results[k]...)
	}
	return update, nil
}
