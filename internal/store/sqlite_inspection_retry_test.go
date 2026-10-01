package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestRetrySQLiteInspectionRead(t *testing.T) {
	t.Run("transient then success", func(t *testing.T) {
		calls := 0
		transient := errors.New("transient lock")
		want := map[string]sqliteInspectionArtifact{}
		got, err := retrySQLiteInspectionRead(func() (map[string]sqliteInspectionArtifact, error) {
			calls++
			if calls == 1 {
				return nil, transient
			}
			return want, nil
		}, func(err error) bool { return errors.Is(err, transient) })
		if err != nil || calls != 2 || len(got) != 0 {
			t.Fatalf("retry = (%v, %v), calls=%d; want success on second attempt", got, err, calls)
		}
	})

	t.Run("exhaustion keeps last error", func(t *testing.T) {
		want := errors.New("database is locked")
		calls := 0
		_, err := retrySQLiteInspectionRead(func() (map[string]sqliteInspectionArtifact, error) {
			calls++
			return nil, fmt.Errorf("inspection failed: %w", want)
		}, func(err error) bool { return errors.Is(err, want) })
		if !errors.Is(err, want) || calls != 3 {
			t.Fatalf("retry error=%v calls=%d; want wrapped last error after 3 attempts", err, calls)
		}
	})

	t.Run("permanent error is immediate", func(t *testing.T) {
		want := errors.New("permission denied")
		calls := 0
		_, err := retrySQLiteInspectionRead(func() (map[string]sqliteInspectionArtifact, error) {
			calls++
			return nil, want
		}, func(error) bool { return false })
		if err != want || calls != 1 {
			t.Fatalf("retry error=%v calls=%d; want immediate original error", err, calls)
		}
	})
}
