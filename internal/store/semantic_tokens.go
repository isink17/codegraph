package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/isink17/codegraph/internal/texttoken"
)

// The rows in `symbol_tokens` are a tokenization of four `symbols` columns
// under one set of rules, and `SemanticSearch` tokenizes its query under the
// same rules. When the rules change, every row written under the old ones
// stops being comparable with a query tokenized under the new ones -- not
// subtly: an index whose `RealConnectionPool` is one token and a query that
// asks for `pool` do not disagree about ranking, they fail to intersect at
// all, and the result is an empty-looking repository rather than a visibly
// wrong order.
//
// `repos.semantic_token_version` records the rules every live row in
// `symbol_tokens` was written under. A read path that finds a version other
// than texttoken.SemanticTokenVersion refuses rather than serving the mix,
// and names the one command that fixes it.
//
// The fix is cheap and non-destructive, which is the whole reason this is a
// version column and not a reindex. `symbol_tokens` is derived from
// `symbols.name`, `qualified_name`, `signature` and `doc_summary` and from
// nothing else: no file is re-read, no file is re-parsed, no edge is
// re-resolved, and no other table is touched. RefreshSemanticTokens rewrites
// the table from the symbols already in the database.

// ErrSemanticTokensStale is returned by the retrieval read paths when
// `symbol_tokens` was written under tokenization rules this binary does not
// score against.
var ErrSemanticTokensStale = errors.New("semantic token index is stale")

// SemanticTokensStaleError carries the versions involved so a caller can say
// what it found as well as what to do about it.
type SemanticTokensStaleError struct {
	RepoID int64
	Stored int
	Want   int
}

func (e *SemanticTokensStaleError) Error() string {
	stored := fmt.Sprintf("version %d", e.Stored)
	if e.Stored == 0 {
		stored = "an index written before semantic token versions existed"
	}
	return fmt.Sprintf(
		"semantic token index is stale: symbol tokens hold %s, this binary scores version %d; "+
			"run `codegraph update` in the repository to rebuild them (no reparse, no graph rebuild)",
		stored, e.Want)
}

func (e *SemanticTokensStaleError) Is(target error) bool { return target == ErrSemanticTokensStale }

// semanticTokenVersion reads the rules the repository's symbol tokens were
// written under. A repository with no symbols at all is reported as current:
// there are no rows to be stale, and refusing an empty graph would turn a
// freshly created database into an error.
func (s *Store) semanticTokenVersion(ctx context.Context, repoID int64) (int, error) {
	// A read-only handle does not migrate, so an index written before
	// migration 013 is opened with no such column at all. That is not an
	// error to report as SQL: it is the oldest legacy state there is, and it
	// has to reach the caller as the refusal that names the fix.
	has, err := s.hasSemanticTokenVersionColumn(ctx)
	if err != nil {
		return 0, err
	}
	if !has {
		return 0, nil
	}
	var version int
	err = s.parserSemanticQueryer(ctx).QueryRowContext(ctx,
		`SELECT semantic_token_version FROM repos WHERE id = ?`, repoID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return texttoken.SemanticTokenVersion, nil
	}
	if err != nil {
		return 0, err
	}
	if version != texttoken.SemanticTokenVersion {
		var any int
		if err := s.parserSemanticQueryer(ctx).QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM symbols WHERE repo_id = ? LIMIT 1)`, repoID).Scan(&any); err != nil {
			return 0, err
		}
		if any == 0 {
			return texttoken.SemanticTokenVersion, nil
		}
	}
	return version, nil
}

// hasSemanticTokenVersionColumn reports whether migration 013 has been
// applied to this database. Migrations run during Open, before any query, so
// the answer cannot change under a live Store and is resolved once.
func (s *Store) hasSemanticTokenVersionColumn(ctx context.Context) (bool, error) {
	s.semanticTokenColumn.once.Do(func() {
		var n int
		s.semanticTokenColumn.err = s.parserSemanticQueryer(ctx).QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('repos') WHERE name = 'semantic_token_version'`).Scan(&n)
		s.semanticTokenColumn.present = n > 0
	})
	return s.semanticTokenColumn.present, s.semanticTokenColumn.err
}

// requireCurrentSemanticTokens is the guard every retrieval read path takes
// before it scores anything.
func (s *Store) requireCurrentSemanticTokens(ctx context.Context, repoID int64) error {
	version, err := s.semanticTokenVersion(ctx, repoID)
	if err != nil {
		return err
	}
	if version != texttoken.SemanticTokenVersion {
		return &SemanticTokensStaleError{RepoID: repoID, Stored: version, Want: texttoken.SemanticTokenVersion}
	}
	return nil
}

// SemanticTokensStale reports whether the repository's symbol tokens need
// rebuilding. It is the question the indexer asks; the read paths ask
// requireCurrentSemanticTokens, which answers with the refusal instead.
func (s *Store) SemanticTokensStale(ctx context.Context, repoID int64) (bool, error) {
	version, err := s.semanticTokenVersion(ctx, repoID)
	if err != nil {
		return false, err
	}
	return version != texttoken.SemanticTokenVersion, nil
}

// MarkSemanticTokensCurrent records that every live row in `symbol_tokens`
// for this repository was written under the current rules.
//
// The indexer calls it at the end of a successful scan that did not need a
// refresh. That is not redundant with a repository being born current: a
// legacy repository row that happened to hold no symbols reads as current
// rather than stale -- there is nothing written under any rules to refuse --
// and without this it would keep its stored 0 the moment a later scan gave
// it symbols, and the read paths would refuse an index that is in fact
// correct.
func (s *Store) MarkSemanticTokensCurrent(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE repos SET semantic_token_version = ? WHERE id = ?`,
		texttoken.SemanticTokenVersion, repoID)
	return err
}

// RefreshSemanticTokens rewrites `symbol_tokens` for one repository from the
// `symbols` rows already in the database, then records the version.
//
// It is the migration path for an index written by an older binary, and it is
// deliberately the only one: no file is read, parsed or hashed, and no table
// but `symbol_tokens` is written. The rewrite and the version stamp are one
// transaction, so an interrupted refresh leaves the version where it was and
// the next run does the whole thing again -- a repository is never recorded
// as current while part of it is not.
//
// Deleted files are not special-cased. Their symbols are deleted with them by
// the scan that noticed, and a symbol that still exists behind an is_deleted
// file keeps tokens here exactly as it does after an ordinary index; the read
// paths join through `files.is_deleted` and skip it either way.
func (s *Store) RefreshSemanticTokens(ctx context.Context, repoID int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM symbol_tokens WHERE symbol_id IN (SELECT id FROM symbols WHERE repo_id = ?)`,
		repoID); err != nil {
		return 0, fmt.Errorf("clear symbol tokens: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, COALESCE(qualified_name, ''), COALESCE(signature, ''), COALESCE(doc_summary, '')
		 FROM symbols WHERE repo_id = ? ORDER BY id`, repoID)
	if err != nil {
		return 0, err
	}

	weights := make(map[string]float64, 64)
	args := make([]any, 0, sqliteTokenValuesBatchRows*3)
	var written int64
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		if err := execTokenTriplesInsert(ctx, tx, "symbol_tokens", "symbol_id", args, nil); err != nil {
			return err
		}
		args = args[:0]
		return nil
	}
	for rows.Next() {
		var id int64
		var name, qualified, signature, doc string
		if err := rows.Scan(&id, &name, &qualified, &signature, &doc); err != nil {
			rows.Close()
			return 0, err
		}
		clear(weights)
		texttoken.SemanticSymbolWeightsInto(weights, name, qualified, signature, doc)
		for token, weight := range weights {
			args = append(args, id, token, weight)
			written++
			if len(args) >= sqliteTokenValuesBatchRows*3 {
				if err := flush(); err != nil {
					rows.Close()
					return 0, err
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if err := flush(); err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE repos SET semantic_token_version = ? WHERE id = ?`,
		texttoken.SemanticTokenVersion, repoID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// The BM25 corpus statistics are keyed by scan generation, which a token
	// refresh does not move. Document length changed under them, so the entry
	// is dropped rather than left to be served until the next index.
	s.tokenStats.forget(repoID)
	return written, nil
}

// semanticTokenColumnState resolves migration 013's presence once per Store.
type semanticTokenColumnState struct {
	once    sync.Once
	present bool
	err     error
}
