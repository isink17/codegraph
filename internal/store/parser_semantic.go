package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"

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

// PlanParserSemanticEpochs refuses unknown future meaning before scan writes,
// and returns languages whose existing graph must be reparsed to establish the
// current contract. A missing marker is an old/unknown generation.
func (s *Store) PlanParserSemanticEpochs(ctx context.Context, repoID int64, current map[string]int, affected func(string) bool) ([]string, error) {
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
	return tx.Commit()
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
