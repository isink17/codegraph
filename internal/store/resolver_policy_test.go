package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestPlanResolverPoliciesDecidesStaleAndRefusesUnsupported(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "g.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetResolverPolicies(map[string]int{"rust": 2, "java": 1})
	all := func(string) bool { return true }
	set := func(repoID int64, language, value string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, resolverPolicyKeyPrefix(repoID)+language, value); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(repoID int64, affected func(string) bool) ([]string, error) {
		return s.PlanResolverPolicies(ctx, repoID, affected)
	}

	// No marker: every registered language is stale. The marker is per
	// repository, so repo 2 is not decided by repo 1's.
	if got, err := plan(1, all); err != nil || len(got) != 2 || got[0] != "java" || got[1] != "rust" {
		t.Fatalf("missing markers: %v %v", got, err)
	}
	set(1, "rust", "1")
	set(1, "java", "1")
	if got, err := plan(1, all); err != nil || len(got) != 1 || got[0] != "rust" {
		t.Fatalf("older rust marker: %v %v", got, err)
	}
	if got, err := plan(2, all); err != nil || len(got) != 2 {
		t.Fatalf("repo 2: %v %v", got, err)
	}
	// A language the scan cannot touch is neither planned nor stamped.
	if got, err := plan(1, func(l string) bool { return l == "java" }); err != nil || len(got) != 0 {
		t.Fatalf("unaffected rust: %v %v", got, err)
	}
	set(1, "rust", "2")
	if got, err := plan(1, all); err != nil || len(got) != 0 {
		t.Fatalf("current: %v %v", got, err)
	}

	for _, tc := range []struct {
		language, value string
		reason          error
	}{
		{"rust", "3", ErrResolverPolicyNewer},
		{"rust", "x", ErrResolverPolicyUnreadable},
		{"rust", "-1", ErrResolverPolicyUnreadable},
		{"rust", "2.0", ErrResolverPolicyUnreadable},
		{"cobol", "1", ErrResolverPolicyUnreadable},
	} {
		set(1, tc.language, tc.value)
		_, err := plan(1, all)
		var pe *ResolverPolicyError
		if !errors.As(err, &pe) || !errors.Is(err, tc.reason) || pe.Language != tc.language {
			t.Fatalf("%s=%q: %v", tc.language, tc.value, err)
		}
		// Refusal is decided only for affected languages.
		if _, err := plan(1, func(l string) bool { return l == "java" }); err != nil {
			t.Fatalf("%s=%q refused an unrelated scan: %v", tc.language, tc.value, err)
		}
		set(1, tc.language, "2")
		if tc.language == "cobol" {
			if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, resolverPolicyKeyPrefix(1)+"cobol"); err != nil {
				t.Fatal(err)
			}
		}
	}

	// A language with no live file is recorded without a resolve.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key LIKE 'resolver.policy.%'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RedecideResolverPolicies(ctx, 1, []string{"rust"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := plan(1, all); len(got) != 1 || got[0] != "java" {
		t.Fatalf("after recording rust: %v", got)
	}
}
