package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/isink17/codegraph/internal/platform"
)

const fileName = "config.json"
const ignoreFileName = ".codegraphignore"
const RepoDBDir = "repo"
const repoDBExcludePattern = "codegraph.sqlite*"

var HardcodedSkips = []string{".git", ".codegraph", "node_modules", ".next", ".nuxt", ".svelte-kit", ".turbo", ".pnpm-store", ".yarn", ".parcel-cache"}
var DefaultExcludes = []string{".codegraph/**", ".codegraph-home/**", ".codegraph-home2/**", ".gocache/**", ".gomodcache/**", ".tmp/**", "vendor/**", "dist/**", "build/**", "coverage/**", "out/**", ".cache/**", repoDBExcludePattern}

type Config struct {
	DefaultLogLevel      string        `json:"default_log_level"`
	DefaultExcludes      []string      `json:"default_excludes"`
	DefaultLanguages     []string      `json:"default_languages"`
	WatchDebounce        time.Duration `json:"watch_debounce"`
	DBDir                string        `json:"db_dir"`
	CacheDir             string        `json:"cache_dir"`
	DBPerformanceProfile string        `json:"db_performance_profile"`
}

type EmbeddingConfig struct {
	Enabled    bool   `json:"enabled"`
	Provider   string `json:"provider"`   // "ollama" (default when enabled)
	Model      string `json:"model"`      // default: nomic-embed-text
	BaseURL    string `json:"base_url"`   // default: http://localhost:11434
	Dimensions int    `json:"dimensions"` // default: 768
}

type AgentConfig struct {
	Enabled bool   `json:"enabled"`
	Model   string `json:"model"`    // default: llama3.2
	BaseURL string `json:"base_url"` // default: http://localhost:11434
}

type RepoConfig struct {
	Include          []string        `json:"include"`
	Exclude          []string        `json:"exclude"`
	Languages        []string        `json:"languages"`
	WatchDebounce    time.Duration   `json:"watch_debounce"`
	SemanticMaxTerms int             `json:"semantic_max_terms"`
	MaxFileSizeBytes int64           `json:"max_file_size_bytes"`
	ParseErrorPolicy string          `json:"parse_error_policy"`
	Embedding        EmbeddingConfig `json:"embedding"`
	Agent            AgentConfig     `json:"agent"`
}

func Default() (Config, error) {
	paths, err := platform.DefaultPaths()
	if err != nil {
		return Config{}, err
	}
	return Config{
		DefaultLogLevel:      "info",
		DefaultExcludes:      DefaultExcludes,
		DefaultLanguages:     []string{"go"},
		WatchDebounce:        750 * time.Millisecond,
		DBDir:                RepoDBDir,
		CacheDir:             paths.CacheDir,
		DBPerformanceProfile: "balanced",
	}, nil
}

func ConfigPath() (string, error) {
	paths, err := platform.DefaultPaths()
	if err != nil {
		return "", err
	}
	return filepath.Join(paths.ConfigDir, fileName), nil
}

func RepoConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".codegraph", "config.json")
}

func Load() (Config, error) {
	defaults, err := Default()
	if err != nil {
		return Config{}, err
	}
	paths, err := platform.DefaultPaths()
	if err != nil {
		return Config{}, err
	}
	path, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaults, nil
	}
	if err != nil {
		return Config{}, err
	}
	cfg := defaults
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.DBDir == "" {
		cfg.DBDir = defaults.DBDir
	}
	if isLegacyGlobalDBDir(cfg.DBDir, paths) {
		cfg.DBDir = RepoDBDir
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = defaults.CacheDir
	}
	if cfg.DefaultLogLevel == "" {
		cfg.DefaultLogLevel = defaults.DefaultLogLevel
	}
	if len(cfg.DefaultExcludes) == 0 {
		cfg.DefaultExcludes = defaults.DefaultExcludes
	}
	if len(cfg.DefaultLanguages) == 0 {
		cfg.DefaultLanguages = defaults.DefaultLanguages
	}
	if cfg.WatchDebounce == 0 {
		cfg.WatchDebounce = defaults.WatchDebounce
	}
	if cfg.DBPerformanceProfile == "" {
		cfg.DBPerformanceProfile = defaults.DBPerformanceProfile
	}
	return cfg, nil
}

func IsRepoDBDir(dbDir string) bool {
	return strings.EqualFold(strings.TrimSpace(dbDir), RepoDBDir)
}

func RepoDBExcludePattern() string {
	return repoDBExcludePattern
}

func isLegacyGlobalDBDir(dbDir string, paths platform.Paths) bool {
	cleaned := strings.TrimSpace(dbDir)
	if cleaned == "" {
		return false
	}
	legacy := filepath.Join(paths.DataDir, "db")
	return strings.EqualFold(filepath.Clean(cleaned), filepath.Clean(legacy))
}

func SaveIfMissing(cfg Config) (string, bool, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, err
	}
	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func LoadRepo(repoRoot string) (RepoConfig, error) {
	cfg := RepoConfig{
		SemanticMaxTerms: 8,
		MaxFileSizeBytes: 8 * 1024 * 1024,
		ParseErrorPolicy: "fail_fast",
	}
	path := RepoConfigPath(repoRoot)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return RepoConfig{}, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return RepoConfig{}, err
		}
	}
	if cfg.SemanticMaxTerms == 0 {
		cfg.SemanticMaxTerms = 8
	}
	if cfg.MaxFileSizeBytes == 0 {
		cfg.MaxFileSizeBytes = 8 * 1024 * 1024
	}
	switch strings.ToLower(strings.TrimSpace(cfg.ParseErrorPolicy)) {
	case "", "fail_fast":
		cfg.ParseErrorPolicy = "fail_fast"
	case "best_effort":
		cfg.ParseErrorPolicy = "best_effort"
	default:
		cfg.ParseErrorPolicy = "fail_fast"
	}
	ignorePatterns, err := loadIgnorePatterns(repoRoot)
	if err != nil {
		return RepoConfig{}, err
	}
	if len(ignorePatterns) > 0 {
		cfg.Exclude = append(cfg.Exclude, ignorePatterns...)
	}
	return cfg, nil
}

func loadIgnorePatterns(repoRoot string) ([]string, error) {
	path := filepath.Join(repoRoot, ignoreFileName)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var patterns []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return patterns, nil
}
