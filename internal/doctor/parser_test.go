package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

func seedDB(t *testing.T, rows [][3]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	repo, err := s.UpsertRepo(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	s.Close()

	dsn, err := store.BuildSQLiteDSN(path, store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatalf("BuildSQLiteDSN() error = %v", err)
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	for i, row := range rows {
		callEdges := 0
		if row[2].(bool) {
			callEdges = 1
		}
		res, err := db.Exec(`
			INSERT INTO files(repo_id, path, language, parse_state, parser_profile, parser_call_edges)
			VALUES(?, ?, ?, 'indexed', ?, ?)`,
			repo.ID, filepath.Join("src", string(rune('a'+i))+".src"), row[0], row[1], callEdges)
		if err != nil {
			t.Fatalf("insert file: %v", err)
		}
		fileID, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}
		// Provenance counts files that hold graph evidence, so the fixture has
		// to declare something.
		if _, err := db.Exec(`
			INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, stable_key,
				start_line, start_col, end_line, end_col)
			VALUES(?, ?, ?, 'function', 'f', 'f', ?, 1, 0, 1, 0)`,
			repo.ID, fileID, row[0], fmt.Sprintf("k%d", i)); err != nil {
			t.Fatalf("insert symbol: %v", err)
		}
	}
	return path
}

func langs(entries ...parser.LanguageSupport) []parser.LanguageSupport { return entries }

func TestDoctorReportsDegradedParsers(t *testing.T) {
	info, recs := inspectParser(context.Background(), langs(
		parser.LanguageSupport{Language: "go", ParserProfile: "go-ast:go:v1", CallEdges: true},
		parser.LanguageSupport{Language: "java", ParserProfile: "heuristic:java:v1"},
	), "")
	if strings.Join(info.Degraded, ",") != "java" {
		t.Fatalf("Degraded = %v, want [java]", info.Degraded)
	}
	if info.Provenance != "" {
		t.Fatalf("Provenance = %q, want empty without a DB", info.Provenance)
	}
	if len(recs) != 1 || !strings.Contains(recs[0], "symbols-only") {
		t.Fatalf("recommendations = %v", recs)
	}
}

func TestDoctorParserProvenanceStates(t *testing.T) {
	tsJava := parser.LanguageSupport{Language: "java", ParserProfile: "treesitter:java:v1", CallEdges: true}
	heurJava := parser.LanguageSupport{Language: "java", ParserProfile: "heuristic:java:v1"}

	cases := []struct {
		name    string
		rows    [][3]any
		current []parser.LanguageSupport
		want    string
	}{
		{"current", [][3]any{{"java", "treesitter:java:v1", true}}, langs(tsJava), parserStateCurrent},
		{"upgrade required", [][3]any{{"java", "heuristic:java:v1", false}}, langs(tsJava), parserStateUpgradeRequired},
		{"downgrade refused", [][3]any{{"java", "treesitter:java:v1", true}}, langs(heurJava), parserStateDowngradeRefused},
		{"unknown legacy", [][3]any{{"java", "", false}}, langs(tsJava), parserStateUnknownLegacy},
		{"mixed", [][3]any{
			{"java", "treesitter:java:v1", true},
			{"java", "heuristic:java:v1", false},
		}, langs(tsJava), parserStateMixed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, _ := inspectParser(context.Background(), tc.current, seedDB(t, tc.rows))
			if info.Provenance != tc.want {
				t.Fatalf("Provenance = %q, want %q (languages: %#v)", info.Provenance, tc.want, info.Languages)
			}
		})
	}
}
