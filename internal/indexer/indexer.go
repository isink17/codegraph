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

	"github.com/isink17/codegraph/internal/config"
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
}

type fileTask struct {
	path     string
	rel      string
	info     fs.FileInfo
	adapter  parser.Adapter
	language string
}

type fileResult struct {
	task   fileTask
	action string
	hash   string
	parsed graph.ParsedFile
	err    error
}

func New(s *store.Store, registry *parser.Registry) *Indexer {
	return &Indexer{store: s, registry: registry}
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
	summary := store.ScanSummary{RepoID: repo.ID, ScanID: scanID}
	candidateSet := map[string]struct{}{}
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

	var existing map[string]store.FileRecord
	if len(candidateSet) > 0 {
		paths := make([]string, 0, len(candidateSet))
		for rel := range candidateSet {
			paths = append(paths, rel)
		}
		existing, err = i.store.ExistingFilesForPaths(ctx, repo.ID, paths)
	} else {
		existing, err = i.store.ExistingFiles(ctx, repo.ID)
	}
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

	go func() {
		defer close(tasks)
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
			if len(candidateSet) > 0 {
				if _, ok := candidateSet[rel]; !ok {
					return nil
				}
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
				res := processFileTask(ctxRun, task, existing[task.rel], opts.Force, repoCfg.MaxFileSizeBytes, opts.Languages)
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

	var runErr error
	for res := range results {
		summary.FilesSeen++
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
		case "mark_seen":
			if err := i.store.MarkFileSeen(ctx, repo.ID, scanID, res.task.rel); err != nil {
				runErr = err
				cancel()
			}
			summary.FilesSkipped++
		case "touch":
			if err := i.store.TouchFileMetadata(ctx, repo.ID, scanID, res.task.rel, res.task.language, res.task.info.Size(), res.task.info.ModTime().UnixNano(), res.hash); err != nil {
				runErr = err
				cancel()
			}
			summary.FilesSkipped++
		case "replace":
			if err := i.store.ReplaceFileGraph(ctx, repo.ID, scanID, res.task.rel, res.parsed.Language, res.task.info.Size(), res.task.info.ModTime().UnixNano(), res.hash, res.parsed); err != nil {
				runErr = err
				cancel()
				continue
			}
			summary.FilesChanged++
			summary.FilesIndexed++
		}
	}

	walkErr := <-producerErr
	if runErr == nil && walkErr != nil && walkErr != context.Canceled {
		runErr = walkErr
	}
	if runErr != nil {
		_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", runErr.Error())
		return summary, runErr
	}

	deleted, err := i.store.MarkMissingDeleted(ctx, repo.ID, scanID)
	if err != nil {
		_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
		return summary, err
	}
	summary.FilesDeleted = deleted
	if err := i.store.ResolveEdges(ctx, repo.ID); err != nil {
		_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
		return summary, err
	}
	if err := i.store.CompleteScan(ctx, scanID, summary, started, "completed", ""); err != nil {
		return summary, err
	}
	return summary, nil
}

func processFileTask(ctx context.Context, task fileTask, prev store.FileRecord, force bool, maxFileSizeBytes int64, allowedLanguages []string) fileResult {
	result := fileResult{task: task}
	hasPrev := prev.Path != ""

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
		hash, err = hashFile(task.path)
		if err != nil {
			result.err = err
			return result
		}
	} else {
		content, err = os.ReadFile(task.path)
		if err != nil {
			result.err = err
			return result
		}
		hash = hashContent(content)
	}
	result.hash = hash
	if hasPrev && !force && prev.ContentHash == hash {
		result.action = "touch"
		return result
	}

	parsed := graph.ParsedFile{Language: task.language, FileTokens: map[string]float64{}}
	if task.adapter != nil {
		parsed, err = task.adapter.Parse(ctx, task.path, content)
		if err != nil {
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
	base := filepath.Base(rel)
	if strings.HasPrefix(base, ".") {
		return true
	}
	switch base {
	case "node_modules", "vendor", "dist", "build", "target", "out", "bin":
		return true
	}
	return matchesAny(rel, excludes)
}

func shouldSkipFile(rel string, includes, excludes []string) bool {
	if matchesAny(rel, excludes) {
		return true
	}
	if len(includes) == 0 {
		return false
	}
	return !matchesAny(rel, includes)
}

func matchesAny(path string, globs []string) bool {
	path = filepath.ToSlash(path)
	for _, glob := range globs {
		glob = filepath.ToSlash(glob)
		if strings.HasSuffix(glob, "/**") {
			prefix := strings.TrimSuffix(glob, "/**")
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
		if ok, _ := filepath.Match(glob, path); ok {
			return true
		}
		if ok, _ := filepath.Match(glob, filepath.Base(path)); ok {
			return true
		}
	}
	return false
}
