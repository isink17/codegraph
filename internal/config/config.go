package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/isink17/codegraph/internal/platform"
)

const fileName = "config.json"

type Config struct {
	DefaultLogLevel      string        `json:"default_log_level"`
	DefaultExcludes      []string      `json:"default_excludes"`
	DefaultLanguages     []string      `json:"default_languages"`
	WatchDebounce        time.Duration `json:"watch_debounce"`
	DBDir                string        `json:"db_dir"`
	CacheDir             string        `json:"cache_dir"`
	DBPerformanceProfile string        `json:"db_performance_profile"`
}

type RepoConfig struct {
	Include          []string      `json:"include"`
	Exclude          []string      `json:"exclude"`
	Languages        []string      `json:"languages"`
	WatchDebounce    time.Duration `json:"watch_debounce"`
	SemanticMaxTerms int           `json:"semantic_max_terms"`
}

func Default() (Config, error) {
	paths, err := platform.DefaultPaths()
	if err != nil {
		return Config{}, err
	}
	return Config{
		DefaultLogLevel:      "info",
		DefaultExcludes:      []string{".git/**", ".codegraph/**", ".codegraph-home/**", ".codegraph-home2/**", ".gocache/**", ".gomodcache/**", ".tmp/**", "node_modules/**", "vendor/**", "dist/**", "build/**"},
		DefaultLanguages:     []string{"go"},
		WatchDebounce:        750 * time.Millisecond,
		DBDir:                filepath.Join(paths.DataDir, "db"),
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
	cfg := RepoConfig{SemanticMaxTerms: 8}
	path := RepoConfigPath(repoRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return RepoConfig{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return RepoConfig{}, err
	}
	if cfg.SemanticMaxTerms == 0 {
		cfg.SemanticMaxTerms = 8
	}
	return cfg, nil
}
