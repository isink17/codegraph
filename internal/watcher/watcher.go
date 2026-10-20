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
	"syscall"
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

	transientErrors atomic.Int64
	recoveries      atomic.Int64
	reconciles      atomic.Int64

	health health
}

// Watcher states reported in WatchStats.State.
const (
	// StateReconciling: the start-up reconciliation has not completed, so
	// changes made while no watcher ran may not be in the graph yet.
	StateReconciling = "reconciling"
	// StateWatching: the watch tree is registered and a reconciliation pass
	// completed after the last failure; later changes arrive as events.
	StateWatching = "watching"
	// StateRecovering: a retryable failure left registration or queued work
	// unverified; it ends with a re-registration and a reconciliation pass.
	StateRecovering = "recovering"
)

// health is the watcher state shared by the event and flush goroutines. One
// lock keeps a reconciliation that finishes concurrently with a new failure
// from reporting watching.
type health struct {
	mu            sync.Mutex
	state         string
	registered    bool
	lastTransient string
	// failures counts degradations, so a pass that started before the latest
	// one cannot clear it.
	failures uint64
	// lock is held whenever state is not watching, so other processes read
	// reconcile_pending until this watcher reconciles. lockMu serializes
	// syncLock, which can wait out status reads, outside mu.
	lockMu sync.Mutex
	lock   *store.WatcherLock
}

// reset takes both watcher locks, the unverified one first; see
// store.WatcherLock.
func (h *health) reset(lock *store.WatcherLock) error {
	h.mu.Lock()
	h.state, h.registered, h.lastTransient, h.failures = StateReconciling, false, "", 0
	h.lock = lock
	h.mu.Unlock()
	h.lockMu.Lock()
	defer h.lockMu.Unlock()
	if err := lock.Hold(); err != nil {
		return err
	}
	return lock.HoldLive()
}

func (h *health) degrade(kind string, unregistered bool) {
	h.mu.Lock()
	h.state, h.lastTransient = StateRecovering, kind
	h.failures++
	if unregistered {
		h.registered = false
	}
	h.mu.Unlock()
	h.syncLock()
}

// syncLock makes the lock follow the state as it reads once the previous call
// is done, so the last call leaves them agreeing. A hold is best effort, like
// the reconcile_pending marker written with it: one that fails leaves only
// the marker, which an external full scan clears.
func (h *health) syncLock() {
	h.lockMu.Lock()
	defer h.lockMu.Unlock()
	h.mu.Lock()
	watching := h.state == StateWatching
	h.mu.Unlock()
	if watching {
		h.lock.Release()
	} else {
		_ = h.lock.Hold()
	}
}

func (h *health) setRegistered() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.registered = true
}

func (h *health) failureCount() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failures
}

// reconciled records a reconciliation pass that began when failureCount read
// since.
func (h *health) reconciled(since uint64) {
	h.mu.Lock()
	ok := h.registered && h.failures == since
	if ok {
		h.state = StateWatching
	}
	h.mu.Unlock()
	if ok {
		h.syncLock()
	}
}

func (h *health) snapshot() (state, lastTransient string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state, h.lastTransient
}

// Retry delays after a retryable failure double from retryBaseDelay up to
// retryMaxDelay.
var (
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 30 * time.Second
)

// maxConfigRetries bounds the scheduled recovery attempts that find the
// config still malformed; with the default delays the watcher stops about 8 s
// after it first read it.
const maxConfigRetries = 5

func nextDelay(d time.Duration) time.Duration {
	return min(d*2, retryMaxDelay)
}

// transientKind classifies err as a failure a later retry can clear: the OS
// out of watches or descriptors, or SQLite busy. Everything else, including
// cancellation, is permanent and stops the watcher.
func transientKind(err error) (string, bool) {
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", false
	case errors.Is(err, syscall.ENOSPC):
		return "enospc", true
	case errors.Is(err, syscall.EMFILE):
		return "emfile", true
	case errors.Is(err, syscall.ENFILE):
		return "enfile", true
	case store.IsBusy(err):
		return "sqlite_busy", true
	}
	return "", false
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

	// State is StateReconciling, StateWatching or StateRecovering. Only
	// watching means a reconciliation pass covered every change made before
	// the last failure or start.
	State string `json:"state"`
	// TransientErrors counts retryable failures; LastTransient names the
	// latest (enospc, emfile, enfile, sqlite_busy).
	TransientErrors int64  `json:"transient_errors,omitempty"`
	LastTransient   string `json:"last_transient,omitempty"`
	// Recoveries counts re-registrations of the watch tree after a failure.
	Recoveries int64 `json:"recoveries,omitempty"`
	// Reconciles counts completed full reconciliation passes.
	Reconciles int64 `json:"reconciles"`

	QueueExecs   int64 `json:"queue_execs"`
	QueueSkipped int64 `json:"queue_skipped"`
	QueueErrors  int64 `json:"queue_errors"`
}

func New(s *store.Store, idx *indexer.Indexer) *Watcher {
	return &Watcher{store: s, indexer: idx}
}

func (w *Watcher) Stats() WatchStats {
	state, lastTransient := w.health.snapshot()
	return WatchStats{
		State:           state,
		TransientErrors: w.transientErrors.Load(),
		LastTransient:   lastTransient,
		Recoveries:      w.recoveries.Load(),
		Reconciles:      w.reconciles.Load(),

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
// recursive watch registration, the start-up reconciliation pass and the flush
// loop start. It is not called when setup fails permanently or ctx is canceled
// first. A retryable failure during setup does not withhold it: Stats().State
// then reads recovering until recovery completes. Readiness never claims that
// every OS event will be delivered or that the index is fresh.
func (w *Watcher) RunWithReady(ctx context.Context, repoRoot string, repoID int64, debounce time.Duration, ready func()) error {
	return w.run(ctx, repoRoot, repoID, debounce, ready)
}

func (w *Watcher) run(ctx context.Context, repoRoot string, repoID int64, debounce time.Duration, ready func()) error {
	for _, c := range []*atomic.Int64{
		&w.eventsSeen, &w.eventsIgnored, &w.eventsIgnoredDir, &w.eventsIgnoredChmod,
		&w.flushSignals, &w.coalescedSignals, &w.flushRuns, &w.flushErrors, &w.flushNoop,
		&w.queueExecs, &w.queueSkipped, &w.queueErrors,
		&w.drainRuns, &w.drainPaths, &w.updateRuns, &w.updatePaths,
		&w.profileConverges, &w.overflows, &w.transientErrors, &w.recoveries, &w.reconciles,
	} {
		c.Store(0)
	}
	lock, err := w.store.OpenWatcherLock(repoID)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := w.health.reset(lock); err != nil {
		return err
	}
	if debounce <= 0 {
		debounce = 750 * time.Millisecond
	}

	repoCfg, err := config.LoadRepo(repoRoot)
	if err != nil {
		return err
	}
	includes := repoCfg.Include
	excludes := repoCfg.Exclude

	// Every watcher starts with a reconciliation pass: changes made while no
	// watcher ran raised no events.
	var forceFullScan atomic.Bool
	forceFullScan.Store(true)

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
			err := error(nil)
			if addWatchHook != nil {
				err = addWatchHook(path)
			}
			if err == nil {
				err = target.Add(path)
			}
			if err != nil {
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
			// A closed watcher's channels are closed; reading them would end
			// Run as if the watcher had been shut down.
			eventsCh, errorsCh = nil, nil
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

	// recoverAt fires the next re-registration attempt; nil when none is
	// pending. Only the event goroutine touches it.
	var recoverAt <-chan time.Time
	var recoverTimer *time.Timer
	defer func() {
		if recoverTimer != nil {
			recoverTimer.Stop()
		}
	}()
	registerDelay := retryBaseDelay
	// markPending is best effort: a database too busy to take it fails the
	// retry too, which marks again.
	markPending := func() { _ = w.store.MarkReconcilePending(context.WithoutCancel(ctx), repoID) }
	// degradeRegistration records a retryable registration failure: the watch
	// tree may be partial, so events can be missing until a re-registration
	// and a reconciliation pass both succeed.
	degradeRegistration := func(kind string) {
		w.transientErrors.Add(1)
		w.health.degrade(kind, true)
		markPending()
		if recoverAt == nil {
			recoverTimer = time.NewTimer(registerDelay)
			recoverAt = recoverTimer.C
			registerDelay = nextDelay(registerDelay)
		}
	}

	// A config file caught mid-write reads as malformed JSON, whether an
	// event, a recovery attempt or a flush reads it first. Flushes pause and
	// the recovery path re-reads it; only its scheduled attempts count toward
	// maxConfigRetries, so a burst of events cannot spend them. Any other
	// config error stops the watcher at once.
	var configPaused atomic.Bool
	configRetries := 0
	pauseForConfig := func(err error) bool {
		if !errors.Is(err, config.ErrMalformedRepoConfig) {
			return false
		}
		configPaused.Store(true)
		degradeRegistration("config_invalid")
		return true
	}

	// Until a reconciliation completes, freshness reports reconcile_pending,
	// so an interrupted start-up pass is never read as consistent.
	if err := w.store.MarkReconcilePending(ctx, repoID); err != nil {
		if _, ok := transientKind(err); !ok {
			return err
		}
		// If the start-up pass then fails too, its retry marks again.
	}
	if err := addWatchTreeTo(fsw, repoRoot, nil); err != nil {
		kind, ok := transientKind(err)
		if !ok {
			return err
		}
		degradeRegistration(kind)
	} else {
		w.health.setRegistered()
	}

	flush := func() error {
		w.drainRuns.Add(1)
		failuresBefore := w.health.failureCount()
		force := forceFullScan.Swap(false)
		var paths []string
		claimedAt := time.Now().UTC().Format(time.RFC3339Nano)
		// A repository that was never indexed accepts no claim; the full pass
		// below indexes it.
		hasDirty, err := w.store.HasDirtyFiles(ctx, repoID)
		if err == nil && hasDirty {
			paths, err = w.store.ClaimDirtyFiles(ctx, repoID, claimedAt, "watch_inflight")
		}
		if err != nil {
			if force {
				forceFullScan.Store(true)
			}
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
			if force {
				forceFullScan.Store(true)
			}
			return err
		}
		if len(paths) > 0 {
			deleteCtx := context.WithoutCancel(ctx)
			if err := w.store.DeleteClaimedDirtyFiles(deleteCtx, repoID, paths, claimedAt); err != nil {
				return err
			}
		}
		w.updateRuns.Add(1)
		if force {
			w.reconciles.Add(1)
			w.health.reconciled(failuresBefore)
		} else {
			w.updatePaths.Add(int64(len(paths)))
		}
		return nil
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
	configErrCh := make(chan error, 1)

	// degradeQueue records a retryable failure that may have lost an event
	// before it reached the dirty queue; a reconciliation pass covers it.
	degradeQueue := func(kind string) {
		w.transientErrors.Add(1)
		w.health.degrade(kind, false)
		markPending()
		forceFullScan.Store(true)
		signalFlush()
	}

	// The start-up pass also drains work queued by earlier runs. A retryable
	// failure leaves it to the flush loop's retry.
	flushDelay := debounce
	w.flushRuns.Add(1)
	if err := flush(); err != nil {
		w.flushErrors.Add(1)
		if pauseForConfig(err) {
			forceFullScan.Store(true)
		} else {
			kind, ok := transientKind(err)
			if !ok {
				return err
			}
			w.transientErrors.Add(1)
			w.health.degrade(kind, false)
			markPending()
			flushDelay = retryBaseDelay
		}
	}

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
		retryDelay := retryBaseDelay
		// A retry waits out its backoff; an event signal does not cut it short.
		retrying := false
		if forceFullScan.Load() {
			pending, retrying = true, true
			timer.Reset(flushDelay)
			retryDelay = nextDelay(retryDelay)
		}
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-flushSignalCh:
				pending = true
				if retrying {
					continue
				}
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
				if configPaused.Load() {
					// Resumed by the signal that follows a valid config.
					retrying = false
					continue
				}
				pending, retrying = false, false
				seenMu.Lock()
				clear(seenSinceFlush)
				seenMu.Unlock()
				w.flushRuns.Add(1)
				err := flush()
				if err == nil {
					retryDelay = retryBaseDelay
					continue
				}
				w.flushErrors.Add(1)
				if errors.Is(err, config.ErrMalformedRepoConfig) && loopCtx.Err() == nil {
					// The indexer re-reads the config; recovery happens on the
					// event goroutine, which owns the retry schedule.
					forceFullScan.Store(true)
					configPaused.Store(true)
					pending = true
					select {
					case configErrCh <- err:
					default:
					}
					continue
				}
				if kind, ok := transientKind(err); ok && loopCtx.Err() == nil {
					// Claimed paths stay in the dirty queue and the next claim
					// takes them again; the retry is a full pass because the
					// failed one may have left the graph partly written.
					w.transientErrors.Add(1)
					w.health.degrade(kind, false)
					markPending()
					forceFullScan.Store(true)
					pending, retrying = true, true
					timer.Reset(retryDelay)
					retryDelay = nextDelay(retryDelay)
					continue
				}
				select {
				case flushErrCh <- err:
				default:
				}
				return
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
		case err := <-configErrCh:
			pauseForConfig(err)
		case <-recoverAt:
			recoverAt, recoverTimer = nil, nil
			// The configuration is read again too: a change to it may have
			// raised no event while the tree was unregistered.
			nextCfg, err := config.LoadRepo(repoRoot)
			if err != nil {
				if _, ok := transientKind(err); !ok {
					if errors.Is(err, config.ErrMalformedRepoConfig) {
						configRetries++
						if configRetries < maxConfigRetries && pauseForConfig(err) {
							continue
						}
					}
					return fmt.Errorf("reload repository config: %w", err)
				}
			} else {
				configRetries = 0
				configPaused.Store(false)
				includes, excludes = nextCfg.Include, nextCfg.Exclude
				err = resetWatcher(true)
			}
			if err != nil {
				kind, ok := transientKind(err)
				if !ok {
					return fmt.Errorf("recover watcher: %w", err)
				}
				degradeRegistration(kind)
				continue
			}
			registerDelay = retryBaseDelay
			w.recoveries.Add(1)
			w.health.setRegistered()
			forceFullScan.Store(true)
			signalFlush()
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
			configChanged := isWatcherConfigPath(rel)
			if rel == config.RepoArtifactsDir && event.Op&fsnotify.Create != 0 {
				// Created after the tree was registered: register it, and read
				// a config file that may already be inside.
				if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
					if err := addWatchTreeTo(fsw, event.Name, nil); err != nil {
						kind, ok := transientKind(err)
						if !ok {
							return err
						}
						degradeRegistration(kind)
					}
					configChanged = true
				}
			}
			if configChanged {
				w.eventsIgnored.Add(1)
				nextCfg, loadErr := config.LoadRepo(repoRoot)
				if loadErr != nil {
					if kind, ok := transientKind(loadErr); ok {
						degradeRegistration(kind)
						continue
					}
					if pauseForConfig(loadErr) {
						continue
					}
					// Carrying on with the old include/exclude rules would
					// index against a configuration the user replaced.
					return fmt.Errorf("reload repository config: %w", loadErr)
				}
				configRetries = 0
				configPaused.Store(false)
				includes = nextCfg.Include
				excludes = nextCfg.Exclude
				markPending()
				forceFullScan.Store(true)
				if err := resetWatcher(false); err != nil {
					kind, ok := transientKind(err)
					if !ok {
						return err
					}
					degradeRegistration(kind)
				} else {
					// The whole tree is registered again, which is what a
					// pending recovery attempt would do.
					if recoverTimer != nil {
						recoverTimer.Stop()
						recoverAt, recoverTimer = nil, nil
					}
					registerDelay = retryBaseDelay
					w.health.setRegistered()
				}
				signalFlush()
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
					w.eventsIgnored.Add(1)
					w.eventsIgnoredDir.Add(1)
					// A directory moved or copied in brings files that raise no
					// events of their own, and files written before its watch
					// was added raised none either.
					var files []string
					if err := addWatchTreeTo(fsw, event.Name, func(rel string) { files = append(files, rel) }); err != nil {
						kind, ok := transientKind(err)
						if !ok {
							return err
						}
						degradeRegistration(kind)
						continue
					}
					if len(files) > 0 {
						if err := w.queueDirtyPathsWithRetry(ctx, repoID, files, "create"); err != nil {
							w.queueErrors.Add(1)
							kind, ok := transientKind(err)
							if !ok {
								return err
							}
							degradeQueue(kind)
							continue
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
						w.eventsIgnored.Add(1)
						w.eventsIgnoredDir.Add(1)
						// Descendants were dropped from watchedDirs above, so
						// their own events do not repeat this query.
						gone, err := w.store.LiveFilePathsUnder(ctx, repoID, rel)
						if err == nil && len(gone) > 0 {
							if err = w.queueDirtyPathsWithRetry(ctx, repoID, gone, "remove"); err != nil {
								w.queueErrors.Add(1)
							} else {
								w.queueExecs.Add(1)
								signalFlush()
							}
						}
						if err != nil {
							kind, ok := transientKind(err)
							if !ok {
								return err
							}
							degradeQueue(kind)
						}
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
					kind, ok := transientKind(err)
					if !ok {
						return err
					}
					seenMu.Lock()
					delete(seenSinceFlush, rel)
					seenMu.Unlock()
					degradeQueue(kind)
					continue
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
				markPending()
				forceFullScan.Store(true)
				if err := resetWatcher(true); err != nil {
					kind, ok := transientKind(err)
					if !ok {
						return err
					}
					degradeRegistration(kind)
					continue
				}
				signalFlush()
				continue
			}
			if kind, ok := transientKind(err); ok {
				degradeRegistration(kind)
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
		if lastErr = queueFailure(); lastErr == nil {
			lastErr = w.store.QueueDirtyFiles(ctx, repoID, paths, reason)
		}
		if lastErr == nil {
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
		if lastErr = queueFailure(); lastErr == nil {
			lastErr = w.store.QueueDirtyFile(ctx, repoID, path, reason)
		}
		if lastErr == nil {
			return nil
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
	if updateHook != nil {
		if err := updateHook(opts); err != nil {
			return err
		}
	}
	_, err := w.indexer.Update(ctx, opts)
	if !errors.Is(err, indexer.ErrParserProfileTransitionRequired) {
		return err
	}
	w.profileConverges.Add(1)
	_, err = w.indexer.Update(ctx, indexer.Options{RepoRoot: repoRoot, ScanKind: "watch_config"})
	return err
}

// Test hooks: watcherCreatedHook sees every fsnotify watcher Run creates;
// createdDirHook runs before a newly created directory is registered;
// addWatchHook, when it returns an error, fails one directory registration;
// updateHook, when it returns an error, fails one index update; queueHook,
// when it returns an error, fails one dirty-queue write attempt.
var (
	watcherCreatedHook func(*fsnotify.Watcher)
	createdDirHook     func(path string)
	addWatchHook       func(path string) error
	updateHook         func(indexer.Options) error
	queueHook          func() error
)

func queueFailure() error {
	if queueHook != nil {
		return queueHook()
	}
	return nil
}
