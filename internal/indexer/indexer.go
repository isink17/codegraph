package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	existing, err := i.store.ExistingFiles(ctx, repo.ID)
	if err != nil {
		return summary, err
	}

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

	err = filepath.WalkDir(opts.RepoRoot, func(path string, d fs.DirEntry, walkErr error) error {
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
		summary.FilesSeen++
		info, err := d.Info()
		if err != nil {
			return err
		}
		adapter := i.registry.AdapterFor(rel)
		language := ""
		if adapter != nil {
			language = adapter.Language()
		}
		if len(opts.Languages) > 0 && language != "" && !slices.Contains(opts.Languages, language) {
			return i.store.MarkFileSeen(ctx, repo.ID, scanID, rel)
		}
		prev, exists := existing[rel]
		if exists && !opts.Force && prev.SizeBytes == info.Size() && prev.MtimeUnixNS == info.ModTime().UnixNano() {
			summary.FilesSkipped++
			return i.store.MarkFileSeen(ctx, repo.ID, scanID, rel)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hash := hashContent(content)
		if exists && !opts.Force && prev.ContentHash == hash {
			summary.FilesSkipped++
			return i.store.TouchFileMetadata(ctx, repo.ID, scanID, rel, language, info.Size(), info.ModTime().UnixNano(), hash)
		}
		parsed := graph.ParsedFile{Language: language, FileTokens: map[string]float64{}}
		if adapter != nil {
			parsed, err = adapter.Parse(ctx, path, content)
			if err != nil {
				return err
			}
			if parsed.Language == "" {
				parsed.Language = language
			}
		}
		if err := i.store.ReplaceFileGraph(ctx, repo.ID, scanID, rel, parsed.Language, info.Size(), info.ModTime().UnixNano(), hash, parsed); err != nil {
			return err
		}
		summary.FilesChanged++
		summary.FilesIndexed++
		return nil
	})
	if err != nil {
		_ = i.store.CompleteScan(ctx, scanID, summary, started, "failed", err.Error())
		return summary, err
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

func hashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
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
