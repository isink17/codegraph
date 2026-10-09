package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/isink17/codegraph/internal/parser"
)

var (
	ErrParserSemanticNewer           = errors.New("parser semantic state newer than this binary")
	ErrParserSemanticMalformed       = errors.New("parser semantic marker malformed")
	ErrParserSemanticIncomplete      = errors.New("parser semantic upgrade incomplete")
	ErrParserSemanticUpgradeRequired = errors.New("parser semantic upgrade required")
)

type ParserSemanticError struct {
	Reason           error
	Language, Stored string
	Current          int
}

func (e *ParserSemanticError) Error() string {
	switch {
	case errors.Is(e.Reason, ErrParserSemanticNewer):
		return fmt.Sprintf("refusing to update: %s parser semantic generation %s is newer than this binary's %d", e.Language, e.Stored, e.Current)
	case errors.Is(e.Reason, ErrParserSemanticIncomplete):
		return fmt.Sprintf("refusing to certify %s parser semantic generation %d while older parser evidence remains", e.Language, e.Current)
	case errors.Is(e.Reason, ErrParserSemanticUpgradeRequired):
		return fmt.Sprintf("refusing to resolve %s edges: include this language to upgrade its parser semantics", e.Language)
	default:
		return fmt.Sprintf("refusing to update: %s parser semantic marker %q is malformed", e.Language, e.Stored)
	}
}
func (e *ParserSemanticError) Unwrap() error { return e.Reason }

func parserSemanticKey(repoID int64, language string) string {
	return "parser.semantic." + strconv.FormatInt(repoID, 10) + "." + language
}

func parserSemanticPendingKey(repoID int64, language string) string {
	return "parser.semantic.pending." + strconv.FormatInt(repoID, 10) + "." + language
}

func (s *Store) HasParserSemanticTransitionPending(ctx context.Context, repoID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settings WHERE key LIKE ?)`, "parser.semantic.pending."+strconv.FormatInt(repoID, 10)+".%").Scan(&exists)
	return exists, err
}

func (s *Store) ParserSemanticTransitionLanguages(ctx context.Context, repoID int64) (map[string]struct{}, error) {
	prefix := "parser.semantic.pending." + strconv.FormatInt(repoID, 10) + "."
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM settings WHERE key LIKE ?`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	languages := make(map[string]struct{})
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		languages[strings.TrimPrefix(key, prefix)] = struct{}{}
	}
	return languages, rows.Err()
}

func parseParserSemanticPending(raw string) (from, to int, err error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, 0, errors.New("invalid pending marker")
	}
	from, err = strconv.Atoi(parts[1])
	if err != nil || from < 0 || strconv.Itoa(from) != parts[1] {
		return 0, 0, errors.New("invalid pending source epoch")
	}
	to, err = strconv.Atoi(parts[2])
	if err != nil || to < 1 || strconv.Itoa(to) != parts[2] || to < from {
		return 0, 0, errors.New("invalid pending target epoch")
	}
	return from, to, nil
}

// BeginParserSemanticTransitions durably records transition intent before
// parser-owned graph rows are changed. Repeating it after interruption is safe.
func (s *Store) BeginParserSemanticTransitions(ctx context.Context, repoID int64, current map[string]int, languages []string) error {
	if len(languages) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, language := range languages {
		want := current[language]
		if want < 1 {
			return &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: language, Current: want}
		}
		key := parserSemanticPendingKey(repoID, language)
		var pending string
		err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&pending)
		if err == nil {
			from, to, parseErr := parseParserSemanticPending(pending)
			if parseErr != nil || to > want {
				return &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: language, Stored: pending, Current: want}
			}
			if to < want {
				if _, err := tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, fmt.Sprintf("v1:%d:%d", from, want), key); err != nil {
					return err
				}
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		from := 0
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, parserSemanticKey(repoID, language)).Scan(&raw)
		if err == nil {
			from, err = strconv.Atoi(raw)
			if err != nil || from < 1 || strconv.Itoa(from) != raw {
				return &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: language, Stored: raw, Current: want}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if from > want {
			return &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: language, Stored: raw, Current: want}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)`, key, fmt.Sprintf("v1:%d:%d", from, want)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PlanParserSemanticEpochs refuses unknown future meaning before scan writes,
// and returns languages whose existing graph must be reparsed to establish the
// current contract. A missing marker is an old/unknown generation.
func (s *Store) PlanParserSemanticEpochs(ctx context.Context, repoID int64, current map[string]int, affected func(string) bool) ([]string, error) {
	prefix := "parser.semantic.pending." + strconv.FormatInt(repoID, 10) + "."
	pendingRows, err := s.db.QueryContext(ctx, `SELECT key,value FROM settings WHERE key LIKE ?`, prefix+"%")
	if err != nil {
		return nil, err
	}
	for pendingRows.Next() {
		var key, raw string
		if err := pendingRows.Scan(&key, &raw); err != nil {
			pendingRows.Close()
			return nil, err
		}
		language := strings.TrimPrefix(key, prefix)
		_, to, parseErr := parseParserSemanticPending(raw)
		want, supported := current[language]
		if parseErr != nil || language == "" {
			pendingRows.Close()
			return nil, &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: language, Stored: raw, Current: want}
		}
		if !supported || to > want {
			pendingRows.Close()
			return nil, &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: language, Stored: raw, Current: want}
		}
		if !affected(language) {
			pendingRows.Close()
			return nil, &ParserSemanticError{Reason: ErrParserSemanticUpgradeRequired, Language: language, Current: want}
		}
	}
	if err := pendingRows.Err(); err != nil {
		pendingRows.Close()
		return nil, err
	}
	if err := pendingRows.Close(); err != nil {
		return nil, err
	}
	langs := make([]string, 0, len(current))
	for lang := range current {
		if affected(lang) {
			langs = append(langs, lang)
		}
	}
	sort.Strings(langs)
	var stale []string
	for _, lang := range langs {
		want := current[lang]
		if want < 1 {
			return nil, &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: lang, Current: want}
		}
		var raw string
		err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, parserSemanticKey(repoID, lang)).Scan(&raw)
		markerStale := false
		if errors.Is(err, sql.ErrNoRows) {
			markerStale = true
		} else if err != nil {
			return nil, err
		} else {
			got, parseErr := strconv.Atoi(raw)
			if parseErr != nil || got < 1 || strconv.Itoa(got) != raw {
				return nil, &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: lang, Stored: raw, Current: want}
			}
			if got > want {
				return nil, &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: lang, Stored: raw, Current: want}
			}
			markerStale = got < want
		}
		var pending string
		err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, parserSemanticPendingKey(repoID, lang)).Scan(&pending)
		if err == nil {
			_, to, parseErr := parseParserSemanticPending(pending)
			if parseErr != nil {
				return nil, &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: lang, Stored: pending, Current: want}
			}
			if to > want {
				return nil, &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: lang, Stored: pending, Current: want}
			}
			markerStale = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var newer, older int
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(parser_semantic_epoch),0), COALESCE(SUM(CASE WHEN parser_semantic_epoch < ? THEN 1 ELSE 0 END),0) FROM files WHERE repo_id=? AND language=? AND is_deleted=0 AND parse_state='indexed'`, want, repoID, lang).Scan(&newer, &older); err != nil {
			return nil, err
		}
		if newer > want {
			return nil, &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: lang, Stored: strconv.Itoa(newer), Current: want}
		}
		if markerStale || older > 0 {
			stale = append(stale, lang)
		}
	}
	return stale, nil
}

// StampParserSemanticEpochs advances markers only after the scan has completed
// parser and resolver work. It never lowers a marker.
func (s *Store) StampParserSemanticEpochs(ctx context.Context, repoID int64, current map[string]int, languages []string) error {
	if len(languages) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := stampParserSemanticEpochsTx(ctx, tx, repoID, current, languages); err != nil {
		return err
	}
	return tx.Commit()
}

func stampParserSemanticEpochsTx(ctx context.Context, tx *sql.Tx, repoID int64, current map[string]int, languages []string) error {
	for _, lang := range languages {
		want := current[lang]
		if want < 1 {
			return &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: lang, Current: want}
		}
		var newer, older int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(parser_semantic_epoch),0), COALESCE(SUM(CASE WHEN parser_semantic_epoch < ? THEN 1 ELSE 0 END),0) FROM files WHERE repo_id=? AND language=? AND is_deleted=0 AND parse_state='indexed'`, want, repoID, lang).Scan(&newer, &older); err != nil {
			return err
		}
		if newer > want {
			return &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: lang, Stored: strconv.Itoa(newer), Current: want}
		}
		if older > 0 {
			return &ParserSemanticError{Reason: ErrParserSemanticIncomplete, Language: lang, Stored: "older file evidence", Current: want}
		}
		key := parserSemanticKey(repoID, lang)
		var old string
		err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			got, parseErr := strconv.Atoi(old)
			if parseErr != nil || got < 1 || strconv.Itoa(got) != old {
				return &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: lang, Stored: old, Current: want}
			}
			if got > want {
				return &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: lang, Stored: old, Current: want}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE CAST(settings.value AS INTEGER) < CAST(excluded.value AS INTEGER)`, key, strconv.Itoa(want)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, parserSemanticPendingKey(repoID, lang)); err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&raw); err != nil {
			return err
		}
		got, parseErr := strconv.Atoi(raw)
		if parseErr != nil || strconv.Itoa(got) != raw {
			return &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: lang, Stored: raw, Current: want}
		}
		if got > want {
			return &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: lang, Stored: raw, Current: want}
		}
	}
	return nil
}

// CompleteScanWithParserSemanticEpochs certifies parser convergence and marks
// the scan complete in one transaction, leaving pending state intact on error.
func (s *Store) CompleteScanWithParserSemanticEpochs(ctx context.Context, scanID int64, summary ScanSummary, started time.Time, repoID int64, current map[string]int, languages []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := stampParserSemanticEpochsTx(ctx, tx, repoID, current, languages); err != nil {
		return err
	}
	if err := completeScanTx(ctx, tx, scanID, summary, started, "completed", ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.releaseScanLock(scanID)
	return nil
}

// CheckParserSemanticWrite prevents a scan that began before a newer semantic
// generation was stamped from persisting parser-owned rows afterward. Call it
// inside the same transaction as those rows.
func CheckParserSemanticWrite(ctx context.Context, tx *sql.Tx, repoID int64, language string, current int) error {
	if current < 1 {
		return nil
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, parserSemanticKey(repoID, language)).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		stored, parseErr := strconv.Atoi(raw)
		if parseErr != nil || stored < 1 || strconv.Itoa(stored) != raw {
			return &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: language, Stored: raw, Current: current}
		}
		if stored > current {
			return &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: language, Stored: raw, Current: current}
		}
	}
	var fileEpoch int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(parser_semantic_epoch),0) FROM files WHERE repo_id=? AND language=? AND is_deleted=0 AND parse_state='indexed'`, repoID, language).Scan(&fileEpoch); err != nil {
		return err
	}
	if fileEpoch > current {
		return &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: language, Stored: strconv.Itoa(fileEpoch), Current: current}
	}
	return nil
}

// CheckParserSemanticResolve protects resolver writes whose parser generation
// is not represented by an incoming file row. Empty languages means repo-wide.
func CheckParserSemanticResolve(ctx context.Context, tx *sql.Tx, repoID int64, current map[string]int, languages []string) error {
	selected := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		selected[language] = struct{}{}
	}
	for language, epoch := range current {
		if len(selected) > 0 {
			if _, ok := selected[language]; !ok {
				continue
			}
		}
		if err := CheckParserSemanticWrite(ctx, tx, repoID, language, epoch); err != nil {
			return err
		}
	}
	return nil
}

func checkParserSemanticMarkerSet(ctx context.Context, tx *sql.Tx, repoID int64, current map[string]int, languages []string) error {
	selected := map[string]struct{}{}
	for _, language := range languages {
		selected[language] = struct{}{}
	}
	for language, epoch := range current {
		if len(selected) > 0 {
			if _, ok := selected[language]; !ok {
				continue
			}
		}
		if err := CheckParserSemanticWrite(ctx, tx, repoID, language, epoch); err != nil {
			return err
		}
	}
	return nil
}

func checkParserSemanticMarkers(ctx context.Context, tx *sql.Tx, repoID int64, current map[string]int, languages []string) error {
	selected := map[string]struct{}{}
	for _, language := range languages {
		selected[language] = struct{}{}
	}
	for language, epoch := range current {
		if len(selected) > 0 {
			if _, ok := selected[language]; !ok {
				continue
			}
		}
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, parserSemanticKey(repoID, language)).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		stored, parseErr := strconv.Atoi(raw)
		if parseErr != nil || stored < 1 || strconv.Itoa(stored) != raw {
			return &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: language, Stored: raw, Current: epoch}
		}
		if stored > epoch {
			return &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: language, Stored: raw, Current: epoch}
		}
	}
	return nil
}

func (s *Store) checkParserSemanticAll(ctx context.Context, repoID int64) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return CheckParserSemanticResolve(ctx, tx, repoID, parser.SemanticEpochs(), nil)
}

// CheckParserSemanticGraph rejects relationship answers while any transition
// is pending. The settings lookup is constant-size and shared by graph APIs.
func (s *Store) CheckParserSemanticGraph(ctx context.Context, repoID int64) error {
	rows, err := s.parserSemanticQueryer(ctx).QueryContext(ctx, `SELECT key,value FROM settings WHERE key LIKE ?`, "parser.semantic.pending."+strconv.FormatInt(repoID, 10)+".%")
	if err != nil {
		return err
	}
	defer rows.Close()
	current := parser.SemanticEpochs()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		language := strings.TrimPrefix(key, "parser.semantic.pending."+strconv.FormatInt(repoID, 10)+".")
		_, to, parseErr := parseParserSemanticPending(raw)
		want, supported := current[language]
		if parseErr != nil || language == "" || !supported {
			return &ParserSemanticError{Reason: ErrParserSemanticMalformed, Language: language, Stored: raw}
		}
		if to > want {
			return &ParserSemanticError{Reason: ErrParserSemanticNewer, Language: language, Stored: raw, Current: want}
		}
		return &ParserSemanticError{Reason: ErrParserSemanticIncomplete, Language: language, Stored: raw, Current: want}
	}
	return rows.Err()
}

type parserSemanticGraphQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type parserSemanticGraphQueryerKey struct{}

func (s *Store) parserSemanticQueryer(ctx context.Context) parserSemanticGraphQueryer {
	if q, ok := ctx.Value(parserSemanticGraphQueryerKey{}).(parserSemanticGraphQueryer); ok {
		return q
	}
	return s.db
}

// beginParserSemanticGraphRead pins the validity check and every relationship
// read that follows to one SQLite snapshot. A transition committed after the
// first read cannot make later statements observe its partially replaced graph.
func (s *Store) beginParserSemanticGraphRead(ctx context.Context, repoID int64) (context.Context, *sql.Tx, error) {
	if _, ok := ctx.Value(parserSemanticGraphQueryerKey{}).(parserSemanticGraphQueryer); ok {
		if err := s.CheckParserSemanticGraph(ctx, repoID); err != nil {
			return ctx, nil, err
		}
		return ctx, nil, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ctx, nil, err
	}
	readCtx := context.WithValue(ctx, parserSemanticGraphQueryerKey{}, parserSemanticGraphQueryer(tx))
	if err := s.CheckParserSemanticGraph(readCtx, repoID); err != nil {
		_ = tx.Rollback()
		return ctx, nil, err
	}
	return readCtx, tx, nil
}

func (s *Store) checkParserSemanticLanguages(ctx context.Context, repoID int64, languages []string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return CheckParserSemanticResolve(ctx, tx, repoID, parser.SemanticEpochs(), languages)
}

func (s *Store) IndexedLanguages(ctx context.Context, repoID int64) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT language FROM files WHERE repo_id=? AND is_deleted=0`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	langs := map[string]struct{}{}
	for rows.Next() {
		var language string
		if err := rows.Scan(&language); err != nil {
			return nil, err
		}
		langs[language] = struct{}{}
	}
	return langs, rows.Err()
}

func (s *Store) languagesForPaths(ctx context.Context, repoID int64, paths []string) (map[string]struct{}, error) {
	langs := map[string]struct{}{}
	ids, err := fileIDsByPaths(ctx, s.db, repoID, paths)
	if err != nil || len(ids) == 0 {
		return langs, err
	}
	err = sqliteBatchedIDQuery(ctx, s.db, ids, `SELECT id,language FROM files WHERE repo_id=? AND id IN (`, []any{repoID}, func(scan func(...any) error) error {
		var id int64
		var language string
		if err := scan(&id, &language); err != nil {
			return err
		}
		langs[language] = struct{}{}
		return nil
	})
	return langs, err
}
