package versioncheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/isink17/codegraph/internal/platform"
	"github.com/isink17/codegraph/internal/version"
)

const (
	stateFileName       = "version-state.json"
	githubLatestRelease = "https://api.github.com/repos/isink17/codegraph/releases/latest"
)

type state struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version,omitempty"`
	LastCheckedAt  string `json:"last_checked_at,omitempty"`
	ETag           string `json:"etag,omitempty"`
	LastModified   string `json:"last_modified,omitempty"`
}

type fetchResult struct {
	LatestVersion string
	ETag          string
	LastModified  string
	NotModified   bool
}

type Checker struct {
	StatePath     string
	Current       string
	CheckInterval time.Duration
	Now           func() time.Time
	FetchLatest   func(context.Context, state) (fetchResult, error)
}

func NotifyIfOutdated(ctx context.Context, stderr io.Writer) {
	c, err := DefaultChecker()
	if err != nil {
		return
	}
	_ = c.Run(ctx, stderr)
}

func DefaultChecker() (Checker, error) {
	paths, err := platform.DefaultPaths()
	if err != nil {
		return Checker{}, err
	}
	current := version.Current()
	return Checker{
		StatePath:     filepath.Join(paths.ConfigDir, stateFileName),
		Current:       current,
		CheckInterval: 24 * time.Hour,
		Now:           time.Now,
		FetchLatest: func(ctx context.Context, st state) (fetchResult, error) {
			return fetchLatestFromGitHub(ctx, current, st.ETag, st.LastModified)
		},
	}, nil
}

func (c Checker) Run(ctx context.Context, stderr io.Writer) error {
	if c.StatePath == "" {
		return nil
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.FetchLatest == nil {
		c.FetchLatest = func(context.Context, state) (fetchResult, error) { return fetchResult{}, nil }
	}
	if c.CheckInterval <= 0 {
		c.CheckInterval = 24 * time.Hour
	}
	now := c.Now().UTC()
	st, _ := loadState(c.StatePath)
	st.CurrentVersion = c.Current

	lastChecked, _ := time.Parse(time.RFC3339, st.LastCheckedAt)
	if now.Sub(lastChecked) >= c.CheckInterval || st.LastCheckedAt == "" {
		if latest, err := c.FetchLatest(ctx, st); err == nil {
			if latest.ETag != "" {
				st.ETag = latest.ETag
			}
			if latest.LastModified != "" {
				st.LastModified = latest.LastModified
			}
			if !latest.NotModified && latest.LatestVersion != "" {
				st.LatestVersion = latest.LatestVersion
			}
		}
		st.LastCheckedAt = now.Format(time.RFC3339)
	}

	if err := saveState(c.StatePath, st); err != nil {
		return err
	}

	current, currentOK := normalizedSemver(st.CurrentVersion)
	latest, latestOK := normalizedSemver(st.LatestVersion)
	if currentOK && latestOK && semver.Compare(latest, current) > 0 {
		fmt.Fprintf(stderr, "update available: %s (current: %s)\n", st.LatestVersion, st.CurrentVersion)
	}
	return nil
}

func loadState(path string) (state, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return state{}, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return state{}, err
	}
	return st, nil
}

func saveState(path string, st state) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func normalizedSemver(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v, semver.IsValid(v)
}

func fetchLatestFromGitHub(ctx context.Context, current, etag, lastModified string) (fetchResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, githubLatestRelease, nil)
	if err != nil {
		return fetchResult{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "codegraph/"+sanitizeVersion(current))
	if strings.TrimSpace(etag) != "" {
		req.Header.Set("If-None-Match", strings.TrimSpace(etag))
	}
	if strings.TrimSpace(lastModified) != "" {
		req.Header.Set("If-Modified-Since", strings.TrimSpace(lastModified))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fetchResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return fetchResult{
			NotModified:  true,
			ETag:         strings.TrimSpace(resp.Header.Get("ETag")),
			LastModified: strings.TrimSpace(resp.Header.Get("Last-Modified")),
		}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return fetchResult{}, fmt.Errorf("github latest release request failed: %s", resp.Status)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fetchResult{}, err
	}
	return fetchResult{
		LatestVersion: strings.TrimSpace(payload.TagName),
		ETag:          strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified:  strings.TrimSpace(resp.Header.Get("Last-Modified")),
	}, nil
}

func sanitizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "dev"
	}
	var b strings.Builder
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "dev"
	}
	return b.String()
}
