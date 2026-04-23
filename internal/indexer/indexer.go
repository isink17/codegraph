package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	scanKind := opts.ScanKind
	if scanKind == "" {
		scanKind = "index"
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

	var existing map[string]store.FileRecord
	existingLoadStarted := time.Now()
	if len(candidateSet) > 0 {
		existing, err = i.store.ExistingFilesForPaths(ctx, repo.ID, candidatePaths)
	} else {
		existing, err = i.store.ExistingFiles(ctx, repo.ID)
	}
	summary.ExistingLoadMS = time.Since(existingLoadStarted).Milliseconds()
	if err != nil {
		_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
		return summary, err
	}

	workerCount := runtime.NumCPU()
	if workerCount < 2 {
		workerCount = 2
	}
	if workerCount > 8 {
		workerCount = 8
	}

	ctxRun, cancel := context.WithCancel(ctx)
	defer cancel()

	tasks := make(chan fileTask, workerCount*2)
	results := make(chan fileResult, workerCount*2)
	producerErr := make(chan error, 1)
	walkStart := time.Now()
	missingCandidatePaths := make([]string, 0, 16)

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
				if info.IsDir() {
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

		producerErr <- filepath.WalkDir(opts.RepoRoot, func(path string, d fs.DirEntry, walkErr error) error {
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
			info, err := d.Info()
			if err != nil {
				return err
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
			for task := range tasks {
				res := processFileTask(ctxRun, task, existing[task.rel], opts.Force, repoCfg.MaxFileSizeBytes, opts.Languages, repoCfg.ParseErrorPolicy)
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
		coverage := summary.LanguageCoverage[coverageLanguage]
		coverage.Seen++
		summary.LanguageCoverage[coverageLanguage] = coverage
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
			coverage := summary.LanguageCoverage[coverageLanguage]
			coverage.Skipped++
			summary.LanguageCoverage[coverageLanguage] = coverage
		case "mark_seen":
			writeStart := time.Now()
			markSeenBatch = append(markSeenBatch, res.task.rel)
			coverage := summary.LanguageCoverage[coverageLanguage]
			coverage.Skipped++
			summary.LanguageCoverage[coverageLanguage] = coverage
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
			coverage := summary.LanguageCoverage[coverageLanguage]
			coverage.Skipped++
			summary.LanguageCoverage[coverageLanguage] = coverage
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
			summary.FilesChanged++
			summary.FilesIndexed++
			coverage := summary.LanguageCoverage[coverageLanguage]
			coverage.Indexed++
			summary.LanguageCoverage[coverageLanguage] = coverage
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
			coverage := summary.LanguageCoverage[coverageLanguage]
			coverage.Skipped++
			coverage.ParseFailed++
			summary.LanguageCoverage[coverageLanguage] = coverage
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
	summary.WalkMS = time.Since(walkStart).Milliseconds()
	summary.ProcessWallMS = time.Since(processWallStart).Milliseconds()
	summary.TaskMS = taskDur.Milliseconds()
	summary.TaskOtherMS = taskOtherDur.Milliseconds()
	summary.ParseMS = taskDur.Milliseconds()
	summary.ReadMS = readDur.Milliseconds()
	summary.HashMS = hashDur.Milliseconds()
	summary.AdapterParseMS = adapterParseDur.Milliseconds()
	summary.WriteMS = writeDur.Milliseconds()
	summary.WriteMetadataMS = writeMetadataDur.Milliseconds()
	summary.WriteReplaceMS = writeReplaceDur.Milliseconds()
	summary.EmbedMS = embedDur.Milliseconds()
	if writeStats != (store.WriteStats{}) {
		summary.WriteStats = &writeStats
	}
	if runErr == nil && walkErr != nil && walkErr != context.Canceled {
		runErr = walkErr
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
	resolveStart := time.Now()
	if len(missingCandidatePaths) > 0 {
		deleted, err := i.store.MarkFilesDeletedBatch(ctx, repo.ID, scanID, missingCandidatePaths)
		if err != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
			return summary, err
		}
		summary.FilesDeleted += deleted
	}

	if len(changedPathSet) == 0 {
		summary.ResolveMS = 0
	} else if len(candidateSet) > 0 {
		changedPaths := make([]string, 0, len(changedPathSet))
		for path := range changedPathSet {
			changedPaths = append(changedPaths, path)
		}
		if err := i.store.ResolveEdgesForPaths(ctx, repo.ID, changedPaths); err != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
			return summary, err
		}
	} else {
		if _, resolveErr := i.store.ResolveEdges(ctx, repo.ID); resolveErr != nil {
			_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", resolveErr.Error())
			return summary, resolveErr
		}
	}
	if len(changedPathSet) > 0 {
		summary.ResolveMS = time.Since(resolveStart).Milliseconds()
	}
	summary.DurationMS = time.Since(started).Milliseconds()
	summary.PhaseTimings = []store.ScanPhaseTiming{
		{Phase: "existing_load", MS: summary.ExistingLoadMS},
		{Phase: "walk", MS: summary.WalkMS},
		{Phase: "task", MS: summary.TaskMS},
		{Phase: "task_other", MS: summary.TaskOtherMS},
		{Phase: "read", MS: summary.ReadMS},
		{Phase: "hash", MS: summary.HashMS},
		{Phase: "adapter_parse", MS: summary.AdapterParseMS},
		{Phase: "write", MS: summary.WriteMS},
		{Phase: "write_metadata", MS: summary.WriteMetadataMS},
		{Phase: "write_replace", MS: summary.WriteReplaceMS},
		{Phase: "embed", MS: summary.EmbedMS},
		{Phase: "mark_missing", MS: summary.MarkMissingMS},
		{Phase: "resolve_edges", MS: summary.ResolveMS},
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

func processFileTask(ctx context.Context, task fileTask, prev store.FileRecord, force bool, maxFileSizeBytes int64, allowedLanguages []string, parseErrorPolicy string) fileResult {
	result := fileResult{task: task}
	started := time.Now()
	defer func() {
		result.processDur = time.Since(started)
	}()
	hasPrev := prev.Path != ""

	if len(allowedLanguages) > 0 && task.language != "" && !slices.Contains(allowedLanguages, task.language) {
		if hasPrev {
			result.action = "mark_seen"
		} else {
			result.action = "skip_only"
		}
		return result
	}
	if hasPrev && !prev.IsDeleted && !force && prev.SizeBytes == task.info.Size() && prev.MtimeUnixNS == task.info.ModTime().UnixNano() {
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
	if hasPrev && !force && prev.ContentHash == hash {
		result.action = "touch"
		return result
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

func shouldSkipDir(rel string, excludes []string) bool {
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)

	// Always skip hardcoded directories - not overridable.
	for _, skip := range config.HardcodedSkips {
		if base == skip {
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
	if ok, _ := filepath.Match(pattern, path); ok {
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
