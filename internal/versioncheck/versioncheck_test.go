package versioncheck

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckerWritesCurrentVersion(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "version-state.json")
	c := Checker{
		StatePath:     statePath,
		Current:       "v1.2.3",
		CheckInterval: 24 * time.Hour,
		Now:           func() time.Time { return time.Date(2026, 3, 18, 10, 0, 0, 0, time.UTC) },
		FetchLatest:   func(context.Context) (string, error) { return "v1.2.3", nil },
	}

	if err := c.Run(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	st := readState(t, statePath)
	if got, want := st.CurrentVersion, "v1.2.3"; got != want {
		t.Fatalf("current_version = %q, want %q", got, want)
	}
}

func TestCheckerPrintsUpdateNoticeWhenLatestIsNewer(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "version-state.json")
	c := Checker{
		StatePath:     statePath,
		Current:       "v1.2.3",
		CheckInterval: 24 * time.Hour,
		Now:           func() time.Time { return time.Date(2026, 3, 18, 10, 0, 0, 0, time.UTC) },
		FetchLatest:   func(context.Context) (string, error) { return "v1.3.0", nil },
	}

	var errOut bytes.Buffer
	if err := c.Run(context.Background(), &errOut); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !strings.Contains(errOut.String(), "update available: v1.3.0 (current: v1.2.3)") {
		t.Fatalf("expected update notice, got: %q", errOut.String())
	}
}

func TestCheckerSkipsFetchWithinInterval(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "version-state.json")
	initial := state{
		CurrentVersion: "v1.2.2",
		LatestVersion:  "v1.2.2",
		LastCheckedAt:  time.Date(2026, 3, 18, 10, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	writeState(t, statePath, initial)

	fetchCalls := 0
	c := Checker{
		StatePath:     statePath,
		Current:       "v1.2.3",
		CheckInterval: 24 * time.Hour,
		Now:           func() time.Time { return time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC) },
		FetchLatest: func(context.Context) (string, error) {
			fetchCalls++
			return "v9.9.9", nil
		},
	}

	if err := c.Run(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if fetchCalls != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetchCalls)
	}
	st := readState(t, statePath)
	if got, want := st.CurrentVersion, "v1.2.3"; got != want {
		t.Fatalf("current_version = %q, want %q", got, want)
	}
}

func readState(t *testing.T, path string) state {
	t.Helper()
	var st state
	data := mustReadFile(t, path)
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	return st
}

func writeState(t *testing.T, path string, st state) {
	t.Helper()
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	mustWriteFile(t, path, data)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	return data
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}
