package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/isink17/codegraph/internal/texttoken"
)

// seedSemanticTokenRepo inserts symbols and the tokens a scan would have
// written for them, under whichever rules the caller asks for.
func seedSemanticTokenRepo(t *testing.T, legacy bool) (*Store, int64) {
	t.Helper()
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "src/pool.kt")
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range []struct{ name, qualified string }{
		{"callAcquirePooledConnection", "okhttp3.internal.connection.RealConnectionPool.callAcquirePooledConnection"},
		{"isEligible", "okhttp3.internal.connection.RealConnection.isEligible"},
	} {
		id, err := insertTestSymbol(ctx, s, repoID, fileID, sym.name, sym.qualified)
		if err != nil {
			t.Fatal(err)
		}
		weights := map[string]float64{}
		if legacy {
			texttoken.WeightsStringsInto(weights, sym.name, sym.qualified)
		} else {
			texttoken.SemanticSymbolWeightsInto(weights, sym.name, sym.qualified, "", "")
		}
		for token, weight := range weights {
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?,?,?)`, id, token, weight); err != nil {
				t.Fatal(err)
			}
		}
	}
	version := 0
	if !legacy {
		version = texttoken.SemanticTokenVersion
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE repos SET semantic_token_version = ? WHERE id = ?`, version, repoID); err != nil {
		t.Fatal(err)
	}
	return s, repoID
}

// An index whose tokens were written under the old rules must not be scored
// against a query tokenized under the new ones. The two vocabularies barely
// intersect, so the result would not look wrong, it would look empty.
func TestSemanticSearchRefusesStaleTokenIndex(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedSemanticTokenRepo(t, true)

	_, err := s.SemanticSearch(ctx, repoID, "connection pool", 10, 0)
	if !errors.Is(err, ErrSemanticTokensStale) {
		t.Fatalf("SemanticSearch on a legacy index: err = %v, want ErrSemanticTokensStale", err)
	}
	var stale *SemanticTokensStaleError
	if !errors.As(err, &stale) {
		t.Fatalf("error does not carry the versions: %v", err)
	}
	if stale.Stored != 0 || stale.Want != texttoken.SemanticTokenVersion {
		t.Errorf("stored=%d want=%d", stale.Stored, stale.Want)
	}
	if got := err.Error(); !contains(got, "codegraph update") {
		t.Errorf("refusal does not name the recovery command: %q", got)
	}
}

func TestSemanticSearchServesCurrentTokenIndex(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedSemanticTokenRepo(t, false)
	got, err := s.SemanticSearch(ctx, repoID, "connection pool", 10, 0)
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v", err)
	}
	if len(got) == 0 {
		t.Fatal("decomposed index returned nothing for a component query")
	}
}

// The refresh is the migration path, so it has to land on exactly what a
// fresh index would have written.
func TestRefreshSemanticTokensMatchesFreshIndexing(t *testing.T) {
	ctx := context.Background()
	stale, staleRepo := seedSemanticTokenRepo(t, true)
	fresh, freshRepo := seedSemanticTokenRepo(t, false)

	if _, err := stale.RefreshSemanticTokens(ctx, staleRepo); err != nil {
		t.Fatalf("RefreshSemanticTokens() error = %v", err)
	}
	if got, want := dumpTokens(t, stale, staleRepo), dumpTokens(t, fresh, freshRepo); got != want {
		t.Errorf("refreshed tokens differ from freshly written ones:\n got %s\nwant %s", got, want)
	}
	if err := stale.requireCurrentSemanticTokens(ctx, staleRepo); err != nil {
		t.Errorf("still stale after refresh: %v", err)
	}
}

// Running it twice must not double the weights, which is what a refresh that
// appended instead of replacing would do.
func TestRefreshSemanticTokensIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedSemanticTokenRepo(t, true)
	if _, err := s.RefreshSemanticTokens(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	once := dumpTokens(t, s, repoID)
	if _, err := s.RefreshSemanticTokens(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	if twice := dumpTokens(t, s, repoID); twice != once {
		t.Errorf("second refresh changed the index:\n got %s\nwant %s", twice, once)
	}
}

// Only the named repository is rewritten; a second repository in the same
// database keeps its own tokens and its own version.
func TestRefreshSemanticTokensIsScopedToOneRepository(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedSemanticTokenRepo(t, true)
	other, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := insertTestFile(ctx, s, other.ID, "src/other.go")
	if err != nil {
		t.Fatal(err)
	}
	symbolID, err := insertTestSymbol(ctx, s, other.ID, fileID, "Untouched", "pkg.Untouched")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?, 'sentinel', 7)`, symbolID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshSemanticTokens(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	var weight float64
	if err := s.db.QueryRowContext(ctx,
		`SELECT weight FROM symbol_tokens WHERE symbol_id = ? AND token = 'sentinel'`, symbolID).Scan(&weight); err != nil {
		t.Fatalf("other repository's tokens were touched: %v", err)
	}
	if weight != 7 {
		t.Errorf("sentinel weight = %v, want 7", weight)
	}
	var tokens int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM symbol_tokens WHERE symbol_id = ?`, symbolID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 {
		t.Errorf("other repository has %d token rows, want the 1 it started with", tokens)
	}
}

// A repository with no symbols is not stale: there is nothing written under
// any rules, and refusing it would turn a new database into an error.
func TestEmptyRepositoryIsNotStale(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	stale, err := s.SemanticTokensStale(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Error("an empty repository reported a stale token index")
	}
	if _, err := s.SemanticSearch(ctx, repoID, "anything", 10, 0); err != nil {
		t.Errorf("SemanticSearch on an empty repository: %v", err)
	}
}

func dumpTokens(t *testing.T, s *Store, repoID int64) string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT sym.qualified_name, st.token, st.weight
		FROM symbol_tokens st JOIN symbols sym ON sym.id = st.symbol_id
		WHERE sym.repo_id = ? ORDER BY sym.qualified_name, st.token`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		var qualified, token string
		var weight float64
		if err := rows.Scan(&qualified, &token, &weight); err != nil {
			t.Fatal(err)
		}
		out += fmt.Sprintf("%s|%s|%g\n", qualified, token, weight)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("no tokens")
	}
	return out
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
