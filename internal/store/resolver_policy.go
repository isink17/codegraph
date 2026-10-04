package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Resolver policies version the semantic decisions a language's resolver makes
// over facts that have not changed. A parser profile versions what is extracted
// from a file; nothing versions what is decided about an unchanged file's edges,
// so a resolver-only fix would leave every existing graph on its old bindings
// until some source byte changed.
//
// A policy is a per-repository, per-language, monotonically increasing integer
// kept in `settings`. It is bumped by hand, in this table, in the same change
// that alters a language's resolution rules over unchanged facts. A binary
// build identity is deliberately not a policy: most builds do not change any
// decision, and a version that moves with them would force work on every upgrade.
//
// Reading the marker decides whether the language's existing edges are
// trusted. A missing or lower marker means they were decided by an older
// resolver and are decided again, once, by RedecideResolverPolicies. A marker
// above the registry means a newer binary has decided them; this one cannot
// reproduce that decision and refuses before anything is written.
var resolverPolicyRegistry = map[string]int{
	// 1: a glob re-export passes on only what the re-exporting module sees.
	"rust": 1,
}

var (
	// ErrResolverPolicyNewer: the persisted marker is above this binary's policy
	// for the language.
	ErrResolverPolicyNewer = errors.New("resolver policy newer than this binary")
	// ErrResolverPolicyUnreadable: the persisted marker is not a policy version
	// this binary can interpret (malformed, or a language it has no policy for).
	ErrResolverPolicyUnreadable = errors.New("resolver policy marker unreadable")
)

// ResolverPolicyError names the blocked language and what was found.
type ResolverPolicyError struct {
	Reason   error
	Language string
	Stored   string
	Current  int
}

func (e *ResolverPolicyError) Unwrap() error { return e.Reason }

func (e *ResolverPolicyError) Error() string {
	if errors.Is(e.Reason, ErrResolverPolicyNewer) {
		return fmt.Sprintf("refusing to update: %s edges were decided by resolver policy %s, newer than this binary's %d; run this scan with the newer build", e.Language, e.Stored, e.Current)
	}
	return fmt.Sprintf("refusing to update: resolver policy marker for %s is %q, which this binary cannot interpret", e.Language, e.Stored)
}

// SetResolverPolicies replaces the policy registry this store plans against.
// Production code never calls it; tests use it to stage a policy change in a
// language that has none registered.
func (s *Store) SetResolverPolicies(policies map[string]int) {
	s.resolverPolicies = policies
}

func (s *Store) policies() map[string]int {
	if s.resolverPolicies != nil {
		return s.resolverPolicies
	}
	return resolverPolicyRegistry
}

func resolverPolicyKeyPrefix(repoID int64) string {
	return "resolver.policy." + strconv.FormatInt(repoID, 10) + "."
}

// PlanResolverPolicies returns, sorted, the affected languages whose edges were
// not decided by the current policy. It refuses an affected language whose
// marker this binary cannot honour. It reads only, so a refusal precedes every
// write of the scan.
func (s *Store) PlanResolverPolicies(ctx context.Context, repoID int64, affected func(language string) bool) ([]string, error) {
	prefix := resolverPolicyKeyPrefix(repoID)
	rows, err := s.db.QueryContext(ctx, `SELECT key, COALESCE(value,'') FROM settings WHERE substr(key,1,?)=?`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	stored := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			_ = rows.Close()
			return nil, err
		}
		stored[strings.TrimPrefix(key, prefix)] = value
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	current := s.policies()
	for language, raw := range stored {
		if !affected(language) {
			continue
		}
		version, ok := parseResolverPolicyVersion(raw)
		want, known := current[language]
		switch {
		case !ok || !known:
			return nil, &ResolverPolicyError{Reason: ErrResolverPolicyUnreadable, Language: language, Stored: raw, Current: want}
		case version > want:
			return nil, &ResolverPolicyError{Reason: ErrResolverPolicyNewer, Language: language, Stored: raw, Current: want}
		}
	}
	var stale []string
	for language, want := range current {
		if !affected(language) {
			continue
		}
		raw, has := stored[language]
		if version, _ := parseResolverPolicyVersion(raw); !has || version < want {
			stale = append(stale, language)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

// parseResolverPolicyVersion accepts only the canonical decimal form this
// package writes.
func parseResolverPolicyVersion(raw string) (int, bool) {
	version, err := strconv.Atoi(raw)
	if err != nil || version < 1 || strconv.Itoa(version) != raw {
		return 0, false
	}
	return version, true
}

func (s *Store) stampResolverPolicies(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, repoID int64, languages []string) error {
	current := s.policies()
	for _, language := range languages {
		version, ok := current[language]
		if !ok {
			return fmt.Errorf("no resolver policy registered for %q", language)
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			resolverPolicyKeyPrefix(repoID)+language, strconv.Itoa(version)); err != nil {
			return err
		}
	}
	return nil
}

// StampResolverPolicies records languages as decided by the current policy. The
// caller must have just resolved every one of their edges with this binary.
func (s *Store) StampResolverPolicies(ctx context.Context, repoID int64, languages []string) error {
	if len(languages) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.stampResolverPolicies(ctx, tx, repoID, languages); err != nil {
		return err
	}
	return tx.Commit()
}

// RedecideResolverPolicies decides again every edge whose source file is in
// languages, with the canonical resolvers, and records the policy. Clearing,
// rebinding, reference-identity reconciliation and the marker are one
// transaction: a failure leaves the old bindings and the old marker. Source is
// not parsed; other languages' bindings are not cleared. A language with no
// live file has nothing to decide and is only recorded.
func (s *Store) RedecideResolverPolicies(ctx context.Context, repoID int64, languages []string) error {
	if len(languages) == 0 {
		return nil
	}
	present, err := s.languagesWithLiveFiles(ctx, repoID, languages)
	if err != nil {
		return err
	}
	if len(present) == 0 {
		return s.StampResolverPolicies(ctx, repoID, languages)
	}
	_, err = s.resolveEdgesRepoWide(ctx, repoID, present, func(tx *sql.Tx) error {
		if err := reconcileReferenceIdentitiesForLanguages(ctx, tx, repoID, present); err != nil {
			return err
		}
		return s.stampResolverPolicies(ctx, tx, repoID, languages)
	})
	return err
}

func (s *Store) languagesWithLiveFiles(ctx context.Context, repoID int64, languages []string) ([]string, error) {
	args := []any{repoID}
	for _, language := range languages {
		args = append(args, language)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT language FROM files WHERE repo_id=? AND is_deleted=0 AND language IN (`+placeholders(len(languages))+`) ORDER BY language`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var language string
		if err := rows.Scan(&language); err != nil {
			return nil, err
		}
		out = append(out, language)
	}
	return out, rows.Err()
}
