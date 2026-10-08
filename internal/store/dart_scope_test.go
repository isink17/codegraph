package store

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestDartScopeVetoSQLMatchesGoTwin(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	files := map[string]int64{}
	for _, language := range []string{"dart", "go"} {
		id, err := insertTestFileLang(ctx, s, repo.ID, "src."+language, language)
		if err != nil {
			t.Fatal(err)
		}
		files[language] = id
	}
	caller, err := insertTestSymbolLang(ctx, s, repo.ID, files["go"], "caller", "caller", "go")
	if err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"dart", "go"} {
		for _, kind := range []string{EdgeKindCalls, EdgeKindCrossLanguageRef} {
			res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line) VALUES(?, ?, 'f', ?, '', ?, 1)`,
				repo.ID, caller, kind, files[language])
			if err != nil {
				t.Fatal(err)
			}
			id, _ := res.LastInsertId()
			var sqlOwned bool
			if err := s.db.QueryRowContext(ctx, `SELECT NOT (`+dartScopeVetoSQL+`) FROM edges JOIN files f ON f.id = edges.file_id WHERE edges.id = ?`, id).Scan(&sqlOwned); err != nil {
				t.Fatal(err)
			}
			goOwned := dartScopeOwned(edgeTarget{srcLanguage: language, edgeKind: kind})
			if want := language == "dart" && kind == EdgeKindCalls; sqlOwned != want || goOwned != want {
				t.Fatalf("%s/%s: SQL owned=%v, Go owned=%v, want %v", language, kind, sqlOwned, goOwned, want)
			}
		}
	}
}

// The pass binds only the function symbol at the evidence position: a
// same-named function elsewhere, a type at that position, a Lua-shaped
// evidence or a missing declaration leaves the edge unresolved.
func TestResolveDartScopeBindsOnlyTheFunctionAtTheEvidencePosition(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	file, err := insertTestFileLang(ctx, s, repo.ID, "m.dart", "dart")
	if err != nil {
		t.Fatal(err)
	}
	symbol := func(kind, key string, line int) int64 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, 'dart', ?, 'f', 'f', ?, 1, ?, 9, ?)`,
			repo.ID, file, kind, line, line, key)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	top := symbol("function", "func:dart:f", 1)
	local := symbol("function", "func:dart:local:f:2:1", 2)
	symbol("class", "type:dart:f", 3)
	edge := func(evidence string) int64 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line, start_col) VALUES(?, ?, 'f', 'calls', ?, ?, 9, 1)`,
			repo.ID, top, evidence, file)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	toTop := edge(graph.DartLexicalFunctionEvidence + "1:1")
	toLocal := edge(graph.DartLexicalFunctionEvidence + "2:1")
	toType := edge(graph.DartLexicalFunctionEvidence + "3:1")
	lua := edge(graph.LuaLocalFunctionEvidence + "1:1")
	missing := edge(graph.DartLexicalFunctionEvidence + "4:1")
	plain := edge("")
	n, err := resolveDartScope(ctx, s.db, repo.ID, nil)
	if err != nil || n != 2 {
		t.Fatalf("bound %d, err %v; want two", n, err)
	}
	var got []string
	for _, id := range []int64{toTop, toLocal, toType, lua, missing, plain} {
		var dst *int64
		var strategy string
		if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id, resolution_strategy FROM edges WHERE id = ?`, id).Scan(&dst, &strategy); err != nil {
			t.Fatal(err)
		}
		switch {
		case dst == nil:
			got = append(got, "-")
		case *dst == top:
			got = append(got, "top:"+strategy)
		case *dst == local:
			got = append(got, "local:"+strategy)
		default:
			got = append(got, "other")
		}
	}
	want := "top:" + ResolutionStrategyDartLexicalFunction + ",local:" + ResolutionStrategyDartLexicalFunction + ",-,-,-,-"
	if strings.Join(got, ",") != want {
		t.Fatalf("bindings = %v, want %s", got, want)
	}
}

// Every resolver entry point leaves a Dart call to the Dart pass: a
// same-named function in another file never answers it, on any path.
func TestDartOwnershipAllEntrypoints(t *testing.T) {
	ctx := context.Background()
	for _, entry := range []struct {
		name string
		run  func(*Store, int64) error
	}{
		{"repo-wide", func(s *Store, repo int64) error { _, err := s.ResolveEdges(ctx, repo); return err }},
		{"names", func(s *Store, repo int64) error {
			_, err := s.ResolveEdgesForNames(ctx, repo, []string{"f", "g"})
			return err
		}},
		{"paths", func(s *Store, repo int64) error { return s.ResolveEdgesForPaths(ctx, repo, []string{"m.dart"}) }},
		{"paths-and-names", func(s *Store, repo int64) error {
			_, err := s.ResolveEdgesForPathsAndNames(ctx, repo, []string{"m.dart"}, []string{"f", "g"})
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			s, repo := openBudgetStore(t)
			file, err := insertTestFileLang(ctx, s, repo.ID, "m.dart", "dart")
			if err != nil {
				t.Fatal(err)
			}
			other, err := insertTestFileLang(ctx, s, repo.ID, "other.dart", "dart")
			if err != nil {
				t.Fatal(err)
			}
			symbol := func(fileID int64, name, key string) int64 {
				res, err := s.db.ExecContext(ctx, `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, 'dart', 'function', ?, ?, 1, 1, 3, 4, ?)`,
					repo.ID, fileID, name, name, key)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				return id
			}
			local := symbol(file, "f", "func:dart:f")
			symbol(other, "f", "func:dart:f")
			symbol(other, "g", "func:dart:g")
			edge := func(name, evidence string) int64 {
				res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line, start_col) VALUES(?, ?, ?, 'calls', ?, ?, 2, 1)`,
					repo.ID, local, name, evidence, file)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				return id
			}
			proven := edge("f", graph.DartLexicalFunctionEvidence+"1:1")
			unproven := edge("g", "")
			if err := entry.run(s, repo.ID); err != nil {
				t.Fatal(err)
			}
			state := func(id int64) string {
				var dst *int64
				var strategy string
				if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id, resolution_strategy FROM edges WHERE id = ?`, id).Scan(&dst, &strategy); err != nil {
					t.Fatal(err)
				}
				if dst == nil {
					return "-"
				}
				if *dst == local {
					return "local:" + strategy
				}
				return "other:" + strategy
			}
			if got := state(proven) + "," + state(unproven); got != "local:"+ResolutionStrategyDartLexicalFunction+",-" {
				t.Fatalf("bindings = %s", got)
			}
		})
	}
}
