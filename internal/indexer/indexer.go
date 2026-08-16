package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/embedding"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

type Options struct {
	RepoRoot  string
	Force     bool
	Include   []string
	Exclude   []string
	Languages []string
	GitBase   string
	Paths     []string
	ScanKind  string
}

type Indexer struct {
	store    *store.Store
	registry *parser.Registry
	embedder embedding.Embedder
}

type fileTask struct {
	path     string
	rel      string
	info     fs.FileInfo
	adapter  parser.Adapter
	language string
}

type fileResult struct {
	task       fileTask
	action     string
	hash       string
	parsed     graph.ParsedFile
	err        error
	parseErr   string
	processDur time.Duration
	readDur    time.Duration
	hashDur    time.Duration
	parseDur   time.Duration
}

func New(s *store.Store, registry *parser.Registry, embedder embedding.Embedder) *Indexer {
	if embedder == nil {
		embedder = embedding.NewNoop()
	}
	return &Indexer{store: s, registry: registry, embedder: embedder}
}

func (i *Indexer) SupportedLanguages() []parser.LanguageSupport {
	return i.registry.SupportedLanguages()
}

// cppProbeSource is parsed once per full run to answer one question: does this
// binary's C/C++ adapter produce call edges? A capability flag on the Adapter
// interface would be a second place for the answer to be wrong; asking the
// adapter is the answer itself.
const cppProbeSource = "void cg_probe_callee() {}\nvoid cg_probe_caller() { cg_probe_callee(); }\n"

// cppAdapterEmitsCalls reports whether the registry's C/C++ adapter builds a
// call graph. The tree-sitter adapter (cgo) does; the heuristic fallback used
// by the non-cgo build emits symbols only. P22.11's upgrade reparses C/C++
// files, which rebuilds their call edges under the first and would delete them
// under the second.
func cppAdapterEmitsCalls(ctx context.Context, registry *parser.Registry) bool {
	if registry == nil {
		return false
	}
	adapter := registry.AdapterFor("cg_probe.cpp")
	if adapter == nil {
		return false
	}
	probe, err := adapter.Parse(ctx, "cg_probe.cpp", []byte(cppProbeSource))
	if err != nil {
		return false
	}
	for _, edge := range probe.Edges {
		if edge.Kind == "calls" {
			return true
		}
	}
	return false
}

// languageAllowed reports whether a run's `languages` allowlist admits a
// language. An empty allowlist admits everything, matching processFileTask.
func languageAllowed(allowed []string, language string) bool {
	return len(allowed) == 0 || slices.Contains(allowed, language)
}

func (i *Indexer) Index(ctx context.Context, opts Options) (store.ScanSummary, error) {
	return i.run(ctx, opts)
}

func (i *Indexer) Update(ctx context.Context, opts Options) (store.ScanSummary, error) {
	if opts.ScanKind == "" {
		opts.ScanKind = "update"
	}
	return i.run(ctx, opts)
}

func (i *Indexer) run(ctx context.Context, opts Options) (store.ScanSummary, error) {
	repoCfg, err := config.LoadRepo(opts.RepoRoot)
	if err != nil {
		return store.ScanSummary{}, err
	}
	if len(opts.Include) == 0 {
		opts.Include = repoCfg.Include
	}
	if len(opts.Exclude) == 0 {
		opts.Exclude = repoCfg.Exclude
	}
	if len(opts.Languages) == 0 {
		opts.Languages = repoCfg.Languages
	}
	repo, err := i.store.UpsertRepo(ctx, opts.RepoRoot)
	if err != nil {
		return store.ScanSummary{}, err
	}
	// Asked before pass 1 writes anything, because that is the only moment an
	// empty graph still means "this repository has never been indexed". It gates
	// the one-time resolver repairs below: a first index produces the current
	// rules' answer by construction, so running a repair resolve against it
	// would only duplicate its own resolve pass.
	hadExistingGraph, err := i.store.RepoHasExistingGraph(ctx, repo.ID)
	if err != nil {
		return store.ScanSummary{}, err
	}
	scanKind := opts.ScanKind
	if scanKind == "" {
		scanKind = "index"
	}
	// P22.11: a repository indexed by an older release holds C/C++ call edges
	// whose destination lost its receiver (`v.size()` persisted as `size`), and
	// the wrong binding that produced. Unlike every earlier resolver upgrade
	// this cannot be repaired by clearing bindings -- the persisted fact itself
	// is lossy -- so the files have to reach the parser again. Marking them here
	// rather than next to the other repair in Pass 2 is deliberate: change
	// detection reads the file metadata below, so a marker written after that
	// point would not take effect until the *next* run, leaving a known-wrong
	// edge class live for one more update.
	//
	// Three shapes are skipped, each because the mark could not be honoured by
	// the run that writes it:
	//
	//   - a path-scoped run walks only the caller's paths, so most marked files
	//     would never be visited. The watcher drains this way on every ordinary
	//     flush (watcher.go sets Options.Paths), so a repository that is only
	//     ever watched keeps the old edges until someone runs `codegraph index`
	//     or `codegraph update` -- a forced rescan is enough, an incremental
	//     flush is not.
	//   - a `languages` allowlist that excludes cpp makes processFileTask stop
	//     at the allowlist check, before it reads the file, so the cleared
	//     metadata would never be replaced and `file_state` would report the
	//     sentinel mtime for the life of the index.
	//   - no C/C++ adapter that produces call edges at all: the non-cgo build
	//     falls back to the heuristic adapter, which emits none, so reparsing
	//     there would delete a C++ call graph instead of rebuilding it.
	//
	// The pending check runs before the capability probe so a repository that is
	// already upgraded costs one indexed SELECT rather than a parse.
	// See store/cpp_receiver_upgrade.go.
	if len(opts.Paths) == 0 && languageAllowed(opts.Languages, "cpp") {
		pending, err := i.store.CppReceiverUpgradePending(ctx, repo.ID)
		if err != nil {
			return store.ScanSummary{}, err
		}
		if pending && cppAdapterEmitsCalls(ctx, i.registry) {
			if err := i.store.MarkCppFilesForReparseOnce(ctx, repo.ID); err != nil {
				return store.ScanSummary{}, err
			}
		}
	}
	scanID, started, err := i.store.BeginScan(ctx, repo.ID, scanKind)
	if err != nil {
		return store.ScanSummary{}, err
	}
	summary := store.ScanSummary{RepoID: repo.ID, ScanID: scanID, ParseSamples: make([]string, 0, 20)}
	summary.LanguageCoverage = map[string]store.LanguageCounts{}
	candidateSet := make(map[string]struct{}, len(opts.Paths))
	if len(opts.Paths) > 0 {
		for _, path := range opts.Paths {
			rel := path
			if filepath.IsAbs(path) {
				if v, err := filepath.Rel(opts.RepoRoot, path); err == nil {
					rel = v
				}
			}
			candidateSet[filepath.Clean(rel)] = struct{}{}
		}
	}
	candidatePaths := make([]string, 0, len(candidateSet))
	for rel := range candidateSet {
		candidatePaths = append(candidatePaths, rel)
	}
	pathScoped := len(candidateSet) > 0

	// Run the existing-files load concurrently with the filesystem walk so
	// the floor cost of a no-op repo-wide update is roughly max(loadMS, walkMS)
	// instead of loadMS+walkMS. Workers block on `existingReady` before
	// reading the resulting map; the FS-walk producer fills the (now
	// larger) `tasks` buffer in parallel. On load failure a monitor goroutine
	// cancels `ctxRun` so the producer/workers drain cleanly.
	var (
		existing       map[string]store.ExistingFileMeta
		loadErr        error
		existingLoadMS int64
	)
	existingLoadStarted := time.Now()
	existingReady := make(chan struct{})
	go func() {
		defer close(existingReady)
		var (
			m   map[string]store.ExistingFileMeta
			err error
		)
		if pathScoped {
			m, err = i.store.ExistingFilesForPaths(ctx, repo.ID, candidatePaths)
		} else {
			m, err = i.store.ExistingFiles(ctx, repo.ID)
		}
		existingLoadMS = time.Since(existingLoadStarted).Milliseconds()
		existing, loadErr = m, err
	}()

	workerCount := runtime.NumCPU()
	if workerCount < 2 {
		workerCount = 2
	}
	if workerCount > 8 {
		workerCount = 8
	}

	ctxRun, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-existingReady
		if loadErr != nil {
			cancel()
		}
	}()

	// `tasks` is sized so the FS-walk producer can keep filling while the
	// existing-files load is still in flight. With cap=workerCount*2 the
	// producer would stall on a full channel after ~16 dirents, defeating
	// the overlap on large repos. workerCount*64 (≤512 entries × ~200B
	// per fileTask ≈ ~100KB) keeps peak buffer trivial.
	tasks := make(chan fileTask, workerCount*64)
	results := make(chan fileResult, workerCount*2)
	producerErr := make(chan error, 1)
	walkStart := time.Now()
	missingCap := len(candidatePaths)
	if missingCap > 128 {
		missingCap = 128
	}
	missingCandidatePaths := make([]string, 0, missingCap)

	go func() {
		defer close(tasks)
		if len(candidateSet) > 0 {
			for _, rel := range candidatePaths {
				rel = filepath.Clean(rel)
				if shouldIgnorePath(rel, opts.Exclude) {
					continue
				}
				if shouldSkipFile(rel, opts.Include, opts.Exclude) {
					continue
				}
				abs := filepath.Join(opts.RepoRoot, rel)
				info, err := os.Stat(abs)
				if err != nil {
					if os.IsNotExist(err) {
						missingCandidatePaths = append(missingCandidatePaths, rel)
						continue
					}
					producerErr <- err
					return
				}
				// Same rule as entryFileInfo below: only regular files are
				// repository content. os.Stat resolves a link first, so a
				// candidate that is (or points at) a directory, FIFO, socket,
				// or device is skipped rather than read.
				if !info.Mode().IsRegular() {
					continue
				}
				adapter := i.registry.AdapterFor(rel)
				language := ""
				if adapter != nil {
					language = adapter.Language()
				}
				task := fileTask{
					path:     abs,
					rel:      rel,
					info:     info,
					adapter:  adapter,
					language: language,
				}
				select {
				case tasks <- task:
				case <-ctxRun.Done():
					producerErr <- ctxRun.Err()
					return
				}
			}
			producerErr <- nil
			return
		}

		producerErr <- walkDir(opts.RepoRoot, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == opts.RepoRoot {
				return nil
			}
			rel, err := filepath.Rel(opts.RepoRoot, path)
			if err != nil {
				return err
			}
			rel = filepath.Clean(rel)
			if d.IsDir() && shouldSkipDir(rel, opts.Exclude) {
				return filepath.SkipDir
			}
			if d.IsDir() {
				return nil
			}
			if shouldSkipFile(rel, opts.Include, opts.Exclude) {
				return nil
			}
			info, err := entryFileInfo(path, d)
			if err != nil {
				return err
			}
			if info == nil {
				return nil
			}
			adapter := i.registry.AdapterFor(rel)
			language := ""
			if adapter != nil {
				language = adapter.Language()
			}
			task := fileTask{
				path:     path,
				rel:      rel,
				info:     info,
				adapter:  adapter,
				language: language,
			}
			select {
			case tasks <- task:
				return nil
			case <-ctxRun.Done():
				return ctxRun.Err()
			}
		})
	}()

	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-existingReady
			if loadErr != nil {
				// Drain producer-buffered tasks so the producer's
				// send-side select can observe ctxRun cancellation and exit.
				for range tasks {
				}
				return
			}
			hashLookup := func(rel string) (string, bool, error) {
				return i.store.LookupFileContentHash(ctxRun, repo.ID, rel)
			}
			for task := range tasks {
				prev, hasPrev := existing[task.rel]
				res := processFileTask(ctxRun, task, prev, hasPrev, opts.Force, repoCfg.MaxFileSizeBytes, opts.Languages, repoCfg.ParseErrorPolicy, hashLookup)
				select {
				case results <- res:
				case <-ctxRun.Done():
					return
				}
				if res.err != nil {
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	const metadataBatchSize = 300
	const replaceBatchSize = 20
	markSeenBatch := make([]string, 0, metadataBatchSize)
	touchBatch := make([]store.FileMetadataUpdate, 0, metadataBatchSize)
	parseFailedBatch := make([]store.FileMetadataUpdate, 0, metadataBatchSize)
	replaceBatch := make([]store.ReplaceFileGraphInput, 0, replaceBatchSize)
	changedPathSet := make(map[string]struct{}, 64)
	changedSymbolNameSet := make(map[string]struct{}, 256)
	// Names the repository STOPS declaring in this run (P22.12). Uniqueness is a
	// repo-wide property, so removing a declaration can make an edge in an
	// untouched file decidable, exactly as adding one can make it undecidable.
	// The new parse cannot mention a vanished name, so it is read off the store
	// before the write that drops it.
	//
	// Only collected for the incremental dispatch below: a full index resolves
	// repo-wide and re-decides everything anyway, so the extra read would be
	// pure cost.
	removedSymbolNameSet := make(map[string]struct{}, 64)
	incrementalResolve := pathScoped || scanKind == "update"
	collectRemovedNames := func(paths []string) error {
		if !incrementalResolve || len(paths) == 0 {
			return nil
		}
		names, err := i.store.PreviousSymbolNamesForPaths(ctx, repo.ID, paths)
		if err != nil {
			return err
		}
		for _, name := range names {
			removedSymbolNameSet[name] = struct{}{}
		}
		return nil
	}

	var writeMetadataDur time.Duration
	var writeReplaceDur time.Duration
	var embedDur time.Duration
	var writeStats store.WriteStats

	flushMarkSeen := func() error {
		if len(markSeenBatch) == 0 {
			return nil
		}
		if pathScoped {
			summary.WriteMarkSeenSkipped += len(markSeenBatch)
			markSeenBatch = markSeenBatch[:0]
			return nil
		}
		started := time.Now()
		if err := i.store.MarkFilesSeenBatch(ctx, repo.ID, scanID, markSeenBatch); err != nil {
			return err
		}
		summary.WriteMarkSeenFlushes++
		writeMetadataDur += time.Since(started)
		markSeenBatch = markSeenBatch[:0]
		return nil
	}
	flushTouch := func() error {
		if len(touchBatch) == 0 {
			return nil
		}
		started := time.Now()
		if err := i.store.TouchFilesMetadataBatch(ctx, repo.ID, scanID, touchBatch); err != nil {
			return err
		}
		summary.WriteTouchFlushes++
		writeMetadataDur += time.Since(started)
		touchBatch = touchBatch[:0]
		return nil
	}
	flushParseFailed := func() error {
		if len(parseFailedBatch) == 0 {
			return nil
		}
		started := time.Now()
		if err := i.store.MarkFilesParseFailedBatch(ctx, repo.ID, scanID, parseFailedBatch); err != nil {
			return err
		}
		summary.WriteParseFailedFlushes++
		writeMetadataDur += time.Since(started)
		parseFailedBatch = parseFailedBatch[:0]
		return nil
	}
	flushReplace := func() error {
		if len(replaceBatch) == 0 {
			return nil
		}
		// Before the replace, not after: ReplaceFileGraphsBatchWithStats drops the
		// old symbol rows inside its own transaction. One query per batch, so no
		// per-file lookup. A failure here aborts the run before anything is
		// written, and a failure in the write below leaves the collected names
		// unused -- neither can half-apply.
		replacedPaths := make([]string, 0, len(replaceBatch))
		for _, input := range replaceBatch {
			replacedPaths = append(replacedPaths, input.Path)
		}
		if err := collectRemovedNames(replacedPaths); err != nil {
			return err
		}
		started := time.Now()
		fileIDs, err := i.store.ReplaceFileGraphsBatchWithStats(ctx, repo.ID, scanID, replaceBatch, &writeStats)
		if err != nil {
			return err
		}
		summary.WriteReplaceFlushes++
		writeReplaceDur += time.Since(started)
		if !embedding.IsNoop(i.embedder) {
			embedStarted := time.Now()
			i.embedReplaceBatch(ctx, repo.ID, fileIDs, replaceBatch)
			embedDur += time.Since(embedStarted)
		}
		replaceBatch = replaceBatch[:0]
		return nil
	}

	var runErr error
	var taskDur time.Duration
	var taskOtherDur time.Duration
	var readDur time.Duration
	var hashDur time.Duration
	var adapterParseDur time.Duration
	var writeDur time.Duration
	processWallStart := time.Now()
	for res := range results {
		taskDur += res.processDur
		readDur += res.readDur
		hashDur += res.hashDur
		adapterParseDur += res.parseDur
		other := res.processDur - res.readDur - res.hashDur - res.parseDur
		if other > 0 {
			taskOtherDur += other
		}
		summary.FilesSeen++
		coverageLanguage := coverageKey(res.task.language)
		updateLanguageCoverage(summary.LanguageCoverage, coverageLanguage, res.task.rel, store.LanguageCounts{Seen: 1})
		if res.err != nil {
			if runErr == nil {
				runErr = fmt.Errorf("%s: %w", res.task.rel, res.err)
				cancel()
			}
			continue
		}
		if runErr != nil {
			continue
		}
		switch res.action {
		case "skip_only":
			summary.FilesSkipped++
			updateLanguageCoverage(summary.LanguageCoverage, coverageLanguage, res.task.rel, store.LanguageCounts{Skipped: 1})
		case "mark_seen":
			writeStart := time.Now()
			markSeenBatch = append(markSeenBatch, res.task.rel)
			updateLanguageCoverage(summary.LanguageCoverage, coverageLanguage, res.task.rel, store.LanguageCounts{Skipped: 1})
			if len(markSeenBatch) < metadataBatchSize {
				if pathScoped {
					summary.WriteMarkSeenSkipped++
					markSeenBatch = markSeenBatch[:0]
				}
				writeDur += time.Since(writeStart)
				summary.FilesSkipped++
				continue
			}
			if err := flushMarkSeen(); err != nil {
				runErr = err
				cancel()
			}
			writeDur += time.Since(writeStart)
			summary.FilesSkipped++
		case "touch":
			writeStart := time.Now()
			touchBatch = append(touchBatch, store.FileMetadataUpdate{
				Path:        res.task.rel,
				Language:    res.task.language,
				SizeBytes:   res.task.info.Size(),
				MtimeUnixNS: res.task.info.ModTime().UnixNano(),
				ContentHash: res.hash,
			})
			updateLanguageCoverage(summary.LanguageCoverage, coverageLanguage, res.task.rel, store.LanguageCounts{Skipped: 1})
			if len(touchBatch) < metadataBatchSize {
				writeDur += time.Since(writeStart)
				summary.FilesSkipped++
				continue
			}
			if err := flushTouch(); err != nil {
				runErr = err
				cancel()
			}
			writeDur += time.Since(writeStart)
			summary.FilesSkipped++
		case "replace":
			writeStart := time.Now()
			replaceBatch = append(replaceBatch, store.ReplaceFileGraphInput{
				Path:        res.task.rel,
				Language:    res.parsed.Language,
				SizeBytes:   res.task.info.Size(),
				MtimeUnixNS: res.task.info.ModTime().UnixNano(),
				ContentHash: res.hash,
				Parsed:      res.parsed,
			})
			changedPathSet[res.task.rel] = struct{}{}
			for _, sym := range res.parsed.Symbols {
				if sym.Name == "" {
					continue
				}
				changedSymbolNameSet[sym.Name] = struct{}{}
			}
			summary.FilesChanged++
			summary.FilesIndexed++
			updateLanguageCoverage(summary.LanguageCoverage, coverageLanguage, res.task.rel, store.LanguageCounts{Indexed: 1})
			if len(replaceBatch) >= replaceBatchSize {
				if err := flushReplace(); err != nil {
					runErr = err
					cancel()
				}
			}
			writeDur += time.Since(writeStart)
		case "parse_failed":
			writeStart := time.Now()
			parseFailedBatch = append(parseFailedBatch, store.FileMetadataUpdate{
				Path:        res.task.rel,
				Language:    res.task.language,
				SizeBytes:   res.task.info.Size(),
				MtimeUnixNS: res.task.info.ModTime().UnixNano(),
				ContentHash: res.hash,
			})
			summary.ParseErrors++
			if len(summary.ParseSamples) < 20 {
				summary.ParseSamples = append(summary.ParseSamples, fmt.Sprintf("%s: %s", res.task.rel, res.parseErr))
			}
			if len(parseFailedBatch) >= metadataBatchSize {
				if err := flushParseFailed(); err != nil {
					runErr = err
					cancel()
				}
			}
			writeDur += time.Since(writeStart)
			summary.FilesSkipped++
			updateLanguageCoverage(summary.LanguageCoverage, coverageLanguage, res.task.rel, store.LanguageCounts{Skipped: 1, ParseFailed: 1})
		}
	}

	if runErr == nil {
		writeStart := time.Now()
		if err := flushMarkSeen(); err != nil {
			runErr = err
			cancel()
		}
		writeDur += time.Since(writeStart)
	}
	if runErr == nil {
		writeStart := time.Now()
		if err := flushReplace(); err != nil {
			runErr = err
			cancel()
		}
		writeDur += time.Since(writeStart)
	}
	if runErr == nil {
		writeStart := time.Now()
		if err := flushTouch(); err != nil {
			runErr = err
			cancel()
		}
		writeDur += time.Since(writeStart)
	}
	if runErr == nil {
		writeStart := time.Now()
		if err := flushParseFailed(); err != nil {
			runErr = err
			cancel()
		}
		writeDur += time.Since(writeStart)
	}

	walkErr := <-producerErr
	// Safe to read existingLoadMS here: the workers' `<-existingReady` happens-
	// before any workers exit, and wg.Wait() (closing `results`) joined them
	// before this consumer-loop returned.
	summary.ExistingLoadMS = existingLoadMS
	summary.WalkMS = time.Since(walkStart).Milliseconds()
	summary.ProcessWallMS = time.Since(processWallStart).Milliseconds()
	summary.TaskMS = taskDur.Milliseconds()
	summary.TaskOtherMS = taskOtherDur.Milliseconds()
	// ParseMS is the time spent parsing file content (adapter parse), excluding read/hash/DB writes.
	summary.ParseMS = adapterParseDur.Milliseconds()
	summary.ReadMS = readDur.Milliseconds()
	summary.HashMS = hashDur.Milliseconds()
	summary.WriteMS = writeDur.Milliseconds()
	summary.WriteMetadataMS = writeMetadataDur.Milliseconds()
	summary.WriteReplaceMS = writeReplaceDur.Milliseconds()
	summary.EmbedMS = embedDur.Milliseconds()
	if writeStats != (store.WriteStats{}) {
		summary.WriteStats = &writeStats
	}
	if (runErr == nil || errors.Is(runErr, context.Canceled)) && walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		runErr = walkErr
	}
	if (runErr == nil || errors.Is(runErr, context.Canceled)) && loadErr != nil {
		runErr = loadErr
	}
	if runErr != nil {
		_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", runErr.Error())
		return summary, runErr
	}

	if len(candidateSet) == 0 {
		missingStarted := time.Now()
		deleted, err := i.store.MarkMissingDeleted(ctx, repo.ID, scanID)
		if err != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
			return summary, err
		}
		summary.MarkMissingMS = time.Since(missingStarted).Milliseconds()
		summary.FilesDeleted = deleted
	}
	if len(missingCandidatePaths) > 0 {
		deleted, err := i.store.MarkFilesDeletedBatch(ctx, repo.ID, scanID, missingCandidatePaths)
		if err != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
			return summary, err
		}
		summary.FilesDeleted += deleted
	}

	if summary.FilesDeleted > 0 {
		// The purge below removes these files' symbols. Read the names they
		// declared while the rows are still there: a deleted declaration changes
		// uniqueness for edges in files this scan never looked at, and the only
		// key those edges can be found by is the name.
		//
		// Unconditional, unlike the per-batch collection above: this is one
		// query, and a run whose ONLY change is a deletion has no changed path
		// to dispatch on, so without it the resolve below would be skipped
		// entirely -- on a full index as well as on an update.
		removed, err := i.store.PreviousSymbolNamesForDeletedInScan(ctx, repo.ID, scanID)
		if err != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
			return summary, err
		}
		for _, name := range removed {
			removedSymbolNameSet[name] = struct{}{}
		}
		if _, err := i.store.PurgeDeletedFileGraphsForScan(ctx, repo.ID, scanID); err != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
			return summary, err
		}
		// A deleted file leaves the import index, so the changed-path passes
		// below cannot find the files that used to see through it -- deleting a
		// package barrel would otherwise strand every binding it re-exported.
		// See store/resolver_type_scope.go.
		if err := i.store.RevalidateTypeScopeAfterDeletion(ctx, repo.ID); err != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
			return summary, err
		}
	}

	// ---------------------------------------------------------------------
	// PASS 2 -- relationship resolution.
	//
	// Everything above this line is Pass 1: discover, parse, extract, and
	// persist repository facts (files, symbols, references, imports, unresolved
	// edges, unbound test links), plus deletion/purge of files that went away.
	// Nothing above binds a relationship to a target in another file.
	//
	// Pass 2 therefore runs against a symbol/file table that already contains
	// every file participating in this scan, which is what makes the resulting
	// graph independent of filesystem traversal order, worker completion order,
	// and intra-batch write order. Both relationship kinds are resolved here and
	// scoped identically: repo-wide for a full index, changed-paths plus the
	// changed batch's introduced symbols for an incremental run.
	// ---------------------------------------------------------------------
	resolveStart := time.Now()

	// Runs before the dispatch below, not inside it: a repository indexed by an
	// older release holds bindings the current resolver refuses, no strategy
	// reconsiders an already-bound edge, and a run over an unchanged tree
	// resolves nothing at all -- so an upgrade would otherwise never converge.
	// Guarded by a per-repository settings key, so it costs one indexed SELECT
	// on every run after the first. See store/resolver_type_scope.go.
	// `repairResolvedRepoWide` is what keeps the first scan after an upgrade from
	// resolving the whole repository twice: a repair that had to clear bindings
	// re-resolves inside its own transaction, and the repo-wide dispatch below
	// would then repeat exactly that work.
	repairResolvedRepoWide := false
	if hadExistingGraph {
		repairResolvedRepoWide, err = i.store.RepairResolverBindingsOnce(ctx, repo.ID)
	} else {
		// Nothing an older release could have written. Record the repairs as
		// done so no later update pays for a repo-wide resolve that can only
		// reproduce what this run already decided.
		err = i.store.MarkResolverBindingsRepaired(ctx, repo.ID)
	}
	if err != nil {
		_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
		return summary, err
	}

	if len(changedPathSet) == 0 && len(removedSymbolNameSet) == 0 {
		summary.ResolveMS = 0
		summary.ResolveMode = "none"
	} else if incrementalResolve {
		// For path-scoped updates (Options.Paths) and incremental update runs, limit edge
		// resolution to the changed files. Full index runs still do repo-wide resolution.
		changedPaths := make([]string, 0, len(changedPathSet))
		for path := range changedPathSet {
			changedPaths = append(changedPaths, path)
		}
		// Correctness: partial runs can introduce symbols that should resolve previously-unresolved
		// edges in other files. Share own-module discovery across path and name passes.
		var stats store.ResolveEdgesForNamesStats
		if len(changedSymbolNameSet) > 0 || len(removedSymbolNameSet) > 0 {
			// OLD union NEW. A declaration arriving under a name and a
			// declaration leaving it are the same event to the resolver -- the
			// name's candidate population changed -- and a rename is both at
			// once, so feeding only one side leaves half of every rename stale.
			names := make([]string, 0, len(changedSymbolNameSet)+len(removedSymbolNameSet))
			for name := range changedSymbolNameSet {
				names = append(names, name)
			}
			for name := range removedSymbolNameSet {
				if _, alsoNew := changedSymbolNameSet[name]; alsoNew {
					continue
				}
				names = append(names, name)
			}
			crossStart := time.Now()
			stats, err = i.store.ResolveEdgesForPathsAndNames(ctx, repo.ID, changedPaths, names)
			if err != nil {
				_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
				return summary, err
			}
			summary.ResolveMode = "paths+names"
			summary.ResolveCrossFileMS = time.Since(crossStart).Milliseconds()
		} else {
			if _, err := i.store.ResolveEdgesForPathsAndNames(ctx, repo.ID, changedPaths, nil); err != nil {
				_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
				return summary, err
			}
			summary.ResolveMode = "paths"
		}
		if stats != (store.ResolveEdgesForNamesStats{}) {
			summary.ResolveCrossFile = &stats
			summary.ResolveCrossFileTargets = stats.TargetsSelected
		}
	} else if repairResolvedRepoWide {
		// The repair above already ran this exact pass over the same graph:
		// every file is persisted before Pass 2 starts, so a second repo-wide
		// resolve could only reach the same answer.
		summary.ResolveMode = "repo"
	} else {
		if _, resolveErr := i.store.ResolveEdges(ctx, repo.ID); resolveErr != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", resolveErr.Error())
			return summary, resolveErr
		}
		summary.ResolveMode = "repo"
	}
	// Test links: one canonical repo-wide pass (P22.2). Unlike edge resolution
	// it is not scoped to the changed batch, because the canonical pass is what
	// guarantees full-index/update parity (a candidate added or deleted outside
	// the batch can change a binding anywhere), and it costs O(test_links) in a
	// fixed number of set-based statements. It also runs for deletion-only
	// updates (removing a production file must re-derive the file-level targets
	// its tests were bound to) and unconditionally on a full `index` run, so an
	// upgraded database gets its legacy rows cleaned and re-derived without
	// needing any file to change first. Only a no-op incremental update skips
	// it, keeping watch loops cheap.
	if scanKind != "update" || len(changedPathSet) > 0 || summary.FilesDeleted > 0 {
		testLinkStart := time.Now()
		testLinkStats, resolveErr := i.store.ResolveTestLinks(ctx, repo.ID)
		if resolveErr != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", resolveErr.Error())
			return summary, resolveErr
		}
		summary.ResolveTestLinksBound = testLinkStats.SymbolsBound
		summary.ResolveTestLinksFileBound = testLinkStats.FilesBound
		summary.ResolveTestLinksMS = time.Since(testLinkStart).Milliseconds()
		summary.ResolveMS = time.Since(resolveStart).Milliseconds()
		if len(changedPathSet) == 0 && len(removedSymbolNameSet) == 0 {
			// Edge resolution did not run; the timings above are the test-link
			// pass alone, and the mode should say so instead of "none". The
			// removed-name condition matches the dispatch above: since P22.12 a
			// deletion-only run DOES resolve, and reporting `test_links` there
			// would deny work that happened.
			summary.ResolveMode = "test_links"
		}
	}
	summary.DurationMS = time.Since(started).Milliseconds()
	summary.PhaseTimings = []store.ScanPhaseTiming{
		{Phase: "existing_load", MS: summary.ExistingLoadMS},
		{Phase: "walk", MS: summary.WalkMS},
		{Phase: "task", MS: summary.TaskMS},
		{Phase: "task_other", MS: summary.TaskOtherMS},
		{Phase: "read", MS: summary.ReadMS},
		{Phase: "hash", MS: summary.HashMS},
		{Phase: "parse", MS: summary.ParseMS},
		{Phase: "write", MS: summary.WriteMS},
		{Phase: "write_metadata", MS: summary.WriteMetadataMS},
		{Phase: "write_replace", MS: summary.WriteReplaceMS},
		{Phase: "embed", MS: summary.EmbedMS},
		{Phase: "mark_missing", MS: summary.MarkMissingMS},
		{Phase: "resolve_edges", MS: summary.ResolveMS - summary.ResolveTestLinksMS},
		{Phase: "resolve_test_links", MS: summary.ResolveTestLinksMS},
		{Phase: "total", MS: summary.DurationMS},
	}
	summary.FilesTotal = summary.FilesSeen + summary.FilesDeleted
	if summary.FilesTotal > 0 {
		summary.FilesDeletedPct = (float64(summary.FilesDeleted) / float64(summary.FilesTotal)) * 100
	}
	if err := i.store.CompleteScan(ctx, scanID, summary, started, "completed", ""); err != nil {
		return summary, err
	}
	return summary, nil
}

func processFileTask(ctx context.Context, task fileTask, prev store.ExistingFileMeta, hasPrev bool, force bool, maxFileSizeBytes int64, allowedLanguages []string, parseErrorPolicy string, hashLookup func(rel string) (string, bool, error)) fileResult {
	result := fileResult{task: task}
	started := time.Now()
	defer func() {
		result.processDur = time.Since(started)
	}()

	if shouldSkipIndexByExtension(task.rel) {
		result.action = "skip_only"
		return result
	}

	if len(allowedLanguages) > 0 && task.language != "" && !slices.Contains(allowedLanguages, task.language) {
		if hasPrev {
			result.action = "mark_seen"
		} else {
			result.action = "skip_only"
		}
		return result
	}
	if hasPrev && !force && prev.SizeBytes == task.info.Size() && prev.MtimeUnixNS == task.info.ModTime().UnixNano() {
		result.action = "mark_seen"
		return result
	}
	if maxFileSizeBytes > 0 && task.info.Size() > maxFileSizeBytes {
		result.action = "touch"
		return result
	}

	hash := ""
	var content []byte
	var err error
	if task.adapter == nil {
		hashStarted := time.Now()
		hash, err = hashFile(task.path)
		if err != nil {
			result.err = err
			return result
		}
		result.hashDur = time.Since(hashStarted)
	} else {
		readStarted := time.Now()
		content, err = os.ReadFile(task.path)
		if err != nil {
			result.err = err
			return result
		}
		result.readDur = time.Since(readStarted)
		hashStarted := time.Now()
		hash = hashContent(content)
		result.hashDur = time.Since(hashStarted)
	}
	result.hash = hash
	if hasPrev && !force && hashLookup != nil {
		prevHash, ok, err := hashLookup(task.rel)
		if err != nil {
			result.err = err
			return result
		}
		if ok && prevHash == hash {
			result.action = "touch"
			return result
		}
	}

	parsed := graph.ParsedFile{Language: task.language}
	if task.adapter != nil {
		parseStarted := time.Now()
		parsed, err = task.adapter.Parse(ctx, task.path, content)
		result.parseDur = time.Since(parseStarted)
		if err != nil {
			if parseErrorPolicy == "best_effort" {
				result.action = "parse_failed"
				result.parseErr = err.Error()
				return result
			}
			result.err = err
			return result
		}
	}
	if parsed.Language == "" {
		parsed.Language = task.language
	}
	result.parsed = parsed
	result.action = "replace"
	return result
}

// walkDir is filepath.WalkDir, indirected so tests can inject a deterministic
// traversal failure. A failure here means the scan cannot know what the
// repository contains, so it must reach the caller as an error rather than a
// shorter successful scan.
//
// Production never assigns to it. Tests that do must not run in parallel with
// any other test in this package that indexes.
var walkDir = filepath.WalkDir

// entryFileInfo returns the FileInfo the file pipeline should use for entry
// `d`, or (nil, nil) when the entry is one the scan deliberately skips.
//
// Only regular files are repository content. Everything else is skipped by an
// explicit rule rather than carried into the pipeline, because the pipeline
// reads whatever it is handed: a directory made hashFile return EISDIR
// ("read <path>: is a directory"), and a FIFO would make it block forever.
// A deliberate skip is not a traversal failure — the walk keeps visiting the
// remaining entries either way.
//
// Symlinks are never followed as traversal targets. WalkDir reports a link
// with fs.ModeSymlink and does not descend into it, so before this check a
// link to a directory reached the file pipeline and failed the entire scan on
// EISDIR — one link truncated the repository. Not following also keeps
// traversal inside the repository, makes a self-referential link harmless, and
// keeps one physical file from being indexed under two logical paths. A link
// that cannot be classified at all (broken, looping, unreadable target) is
// skipped for the same reason: under a no-follow policy its target is not
// repository content.
//
// A link whose target is a regular file is still indexed, under its own
// repo-relative path and never the target's — including a target outside the
// repository, whose content then enters the graph at an in-repo logical path.
// That is the pre-existing file-symlink contract and is left as it stands; the
// no-escape rule this phase adds is about traversal, not about content reached
// through a single explicit link. Such an entry is described by the target's
// metadata: d.Info() reports the link's own size and mtime, which silently
// defeats both the max-file-size cap and the size/mtime change check for the
// bytes os.ReadFile actually reads.
//
// A regular file that vanishes between readdir and the lazy lstat behind
// d.Info() is likewise not a traversal failure: it is gone, so the scan is not
// missing anything it should have seen, and MarkMissingDeleted retires the row.
func entryFileInfo(path string, d fs.DirEntry) (fs.FileInfo, error) {
	switch {
	case d.Type().IsRegular():
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return info, nil
	case d.Type()&fs.ModeSymlink != 0:
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, nil
		}
		return info, nil
	default:
		return nil, nil
	}
}

func shouldSkipIndexByExtension(relPath string) bool {
	ext := strings.ToLower(filepath.Ext(relPath))
	if ext == "" {
		return false
	}
	_, denied := deniedIndexExtensions[ext]
	return denied
}

var deniedIndexExtensions = map[string]struct{}{
	// Windows build artifacts and debug symbols.
	".exe": {}, ".dll": {}, ".lib": {}, ".exp": {}, ".pdb": {}, ".ilk": {}, ".obj": {}, ".sbr": {}, ".bsc": {}, ".tlb": {}, ".map": {}, ".dmp": {},

	// Archives.
	".7z": {}, ".zip": {}, ".rar": {},

	// Images and binary assets.
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".bmp": {}, ".tif": {}, ".tiff": {}, ".dds": {}, ".ico": {}, ".cur": {}, ".psd": {}, ".raw": {},

	// Fonts.
	".ttf": {}, ".otf": {}, ".woff": {}, ".woff2": {}, ".eot": {},

	// Audio.
	".mp3": {}, ".wav": {},

	// Video.
	".mp4": {}, ".mkv": {}, ".avi": {}, ".mov": {}, ".wmv": {}, ".webm": {}, ".mpg": {}, ".mpeg": {}, ".m4v": {}, ".flv": {},

	// Game/engine and other binary formats seen in legacy repos.
	".anm": {}, ".dff": {}, ".rpe": {}, ".eff": {}, ".pk": {}, ".dat": {}, ".bin": {}, ".arc": {}, ".bsp": {}, ".bm1": {}, ".pix": {}, ".grp": {},

	// Documents/help.
	".pdf": {}, ".doc": {}, ".pptx": {}, ".chm": {},
}

func hashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func ShouldSkipDir(rel string, excludes []string) bool {
	return shouldSkipDir(rel, excludes)
}

func ShouldSkipFile(rel string, includes, excludes []string) bool {
	return shouldSkipFile(rel, includes, excludes)
}

func ShouldIgnorePath(rel string, excludes []string) bool {
	return shouldIgnorePath(rel, excludes)
}

func shouldSkipDir(rel string, excludes []string) bool {
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)

	// Always skip hardcoded directories - not overridable.
	for _, skip := range config.HardcodedSkips {
		if base == skip || (runtime.GOOS == "windows" && strings.EqualFold(base, skip)) {
			return true
		}
	}

	// Generic hidden directories can be overridden.
	if strings.HasPrefix(base, ".") {
		return !hasNegationWithin(rel, excludes)
	}

	// Configurable ignores.
	if matchesIgnore(rel, excludes) {
		return !hasNegationWithin(rel, excludes)
	}
	return false
}

func shouldSkipFile(rel string, includes, excludes []string) bool {
	if matchPattern(rel, config.RepoDBExcludePattern()) {
		return true
	}
	if matchesIgnore(rel, excludes) {
		return true
	}
	if len(includes) == 0 {
		return false
	}
	return !matchesAny(rel, includes)
}

func shouldIgnorePath(rel string, excludes []string) bool {
	current := filepath.Clean(rel)
	for current != "." && current != "" {
		if shouldSkipDir(current, excludes) {
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

func matchesAny(path string, globs []string) bool {
	path = filepath.ToSlash(path)
	for _, glob := range globs {
		if matchPattern(path, glob) {
			return true
		}
	}
	return false
}

func coverageKey(language string) string {
	normalized := strings.TrimSpace(strings.ToLower(language))
	if normalized == "" {
		return "unknown"
	}
	return normalized
}

func updateLanguageCoverage(coverage map[string]store.LanguageCounts, language string, relPath string, delta store.LanguageCounts) {
	// TODO: per-file callers can precompute extension deltas in a later optimization pass.
	current := coverage[language]
	current.Seen += delta.Seen
	current.Indexed += delta.Indexed
	current.Skipped += delta.Skipped
	current.ParseFailed += delta.ParseFailed

	if language == "unknown" {
		extKey := coverageExtensionKey(relPath)
		if current.Extensions == nil {
			current.Extensions = map[string]store.LanguageCounts{}
		}
		extCounts := current.Extensions[extKey]
		extCounts.Seen += delta.Seen
		extCounts.Indexed += delta.Indexed
		extCounts.Skipped += delta.Skipped
		extCounts.ParseFailed += delta.ParseFailed
		current.Extensions[extKey] = extCounts
	}

	coverage[language] = current
}

func coverageExtensionKey(relPath string) string {
	ext := strings.ToLower(filepath.Ext(relPath))
	if ext == "" {
		return "[no extension]"
	}
	return ext
}

func matchesIgnore(path string, patterns []string) bool {
	path = filepath.ToSlash(path)
	ignored := false
	for _, raw := range patterns {
		pattern := strings.TrimSpace(filepath.ToSlash(raw))
		if pattern == "" {
			continue
		}
		negated := strings.HasPrefix(pattern, "!")
		if negated {
			pattern = strings.TrimPrefix(pattern, "!")
		}
		if !matchPattern(path, pattern) {
			continue
		}
		if negated {
			ignored = false
		} else {
			ignored = true
		}
	}
	return ignored
}

func hasNegationWithin(dir string, patterns []string) bool {
	dir = strings.Trim(filepath.ToSlash(dir), "/")
	if dir == "" || dir == "." {
		return false
	}
	prefix := dir + "/"
	for _, raw := range patterns {
		pattern := strings.TrimSpace(filepath.ToSlash(raw))
		if !strings.HasPrefix(pattern, "!") {
			continue
		}
		pattern = strings.TrimPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "/")
		if pattern == dir || strings.HasPrefix(pattern, prefix) {
			return true
		}
	}
	return false
}

func matchPattern(path, pattern string) bool {
	pattern = strings.TrimSpace(filepath.ToSlash(pattern))
	if pattern == "" {
		return false
	}
	pattern = strings.TrimPrefix(pattern, "/")
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	if ok, _ := pathpkg.Match(pattern, path); ok {
		return true
	}
	if ok, _ := filepath.Match(pattern, filepath.Base(path)); ok {
		return true
	}
	return false
}

// embedFileSymbols computes and stores embeddings for all symbols in a parsed file.
func (i *Indexer) embedFileSymbols(ctx context.Context, repoID int64, relPath string, parsed graph.ParsedFile) {
	if len(parsed.Symbols) == 0 {
		return
	}

	texts := make([]string, len(parsed.Symbols))
	keys := make([]string, len(parsed.Symbols))
	for j, sym := range parsed.Symbols {
		texts[j] = embedding.FormatSymbolText(sym.Kind, sym.QualifiedName, sym.Signature, sym.DocSummary)
		keys[j] = sym.StableKey
	}

	vectors, err := i.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return // best-effort: don't fail indexing on embedding errors
	}

	fileID, err := i.store.FileIDByPath(ctx, repoID, relPath)
	if err != nil || fileID == 0 {
		return
	}

	symbolMap := make(map[string][]float32, len(keys))
	for j, key := range keys {
		if vectors[j] != nil {
			symbolMap[key] = vectors[j]
		}
	}
	if len(symbolMap) > 0 {
		_ = i.store.UpsertSymbolEmbeddings(ctx, repoID, fileID, "", symbolMap)
	}
}

func (i *Indexer) embedFileSymbolsWithFileID(ctx context.Context, repoID int64, fileID int64, parsed graph.ParsedFile) {
	if len(parsed.Symbols) == 0 || fileID == 0 {
		return
	}

	texts := make([]string, len(parsed.Symbols))
	keys := make([]string, len(parsed.Symbols))
	for j, sym := range parsed.Symbols {
		texts[j] = embedding.FormatSymbolText(sym.Kind, sym.QualifiedName, sym.Signature, sym.DocSummary)
		keys[j] = sym.StableKey
	}

	vectors, err := i.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return // best-effort: don't fail indexing on embedding errors
	}

	symbolMap := make(map[string][]float32, len(keys))
	for j, key := range keys {
		if vectors[j] != nil {
			symbolMap[key] = vectors[j]
		}
	}
	if len(symbolMap) > 0 {
		_ = i.store.UpsertSymbolEmbeddings(ctx, repoID, fileID, "", symbolMap)
	}
}

func (i *Indexer) embedReplaceBatch(ctx context.Context, repoID int64, fileIDs []int64, inputs []store.ReplaceFileGraphInput) {
	type keyRef struct {
		fileID    int64
		stableKey string
	}

	totalSyms := 0
	for idx := range inputs {
		if idx >= len(fileIDs) {
			break
		}
		totalSyms += len(inputs[idx].Parsed.Symbols)
	}
	if totalSyms == 0 {
		return
	}

	texts := make([]string, 0, totalSyms)
	keys := make([]keyRef, 0, totalSyms)
	for idx, input := range inputs {
		if idx >= len(fileIDs) {
			break
		}
		fileID := fileIDs[idx]
		if fileID == 0 || len(input.Parsed.Symbols) == 0 {
			continue
		}
		for _, sym := range input.Parsed.Symbols {
			texts = append(texts, embedding.FormatSymbolText(sym.Kind, sym.QualifiedName, sym.Signature, sym.DocSummary))
			keys = append(keys, keyRef{fileID: fileID, stableKey: sym.StableKey})
		}
	}
	if len(texts) == 0 {
		return
	}

	const embedChunkSize = 128
	upserts := make([]store.SymbolEmbeddingUpsert, 0, len(texts))
	for start := 0; start < len(texts); start += embedChunkSize {
		end := min(start+embedChunkSize, len(texts))
		vectors, err := i.embedder.EmbedBatch(ctx, texts[start:end])
		if err != nil {
			return // best-effort: don't fail indexing on embedding errors
		}
		for j := range vectors {
			vec := vectors[j]
			if vec == nil {
				continue
			}
			ref := keys[start+j]
			if ref.fileID == 0 || ref.stableKey == "" {
				continue
			}
			upserts = append(upserts, store.SymbolEmbeddingUpsert{
				FileID:    ref.fileID,
				StableKey: ref.stableKey,
				Vector:    vec,
			})
		}
	}
	if len(upserts) == 0 {
		return
	}
	_ = i.store.UpsertSymbolEmbeddingsBatch(ctx, repoID, "", upserts)
}
