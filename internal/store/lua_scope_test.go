package store

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestLuaScopeVetoSQLMatchesGoTwin(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	files := map[string]int64{}
	for _, language := range []string{"lua", "go"} {
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
	for _, language := range []string{"lua", "go"} {
		for _, kind := range []string{EdgeKindCalls, EdgeKindCrossLanguageRef} {
			res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line) VALUES(?, ?, 'f', ?, '', ?, 1)`,
				repo.ID, caller, kind, files[language])
			if err != nil {
				t.Fatal(err)
			}
			id, _ := res.LastInsertId()
			var sqlOwned bool
			if err := s.db.QueryRowContext(ctx, `SELECT NOT (`+luaScopeVetoSQL+`) FROM edges JOIN files f ON f.id = edges.file_id WHERE edges.id = ?`, id).Scan(&sqlOwned); err != nil {
				t.Fatal(err)
			}
			goOwned := luaScopeOwned(edgeTarget{srcLanguage: language, edgeKind: kind})
			if want := language == "lua" && kind == EdgeKindCalls; sqlOwned != want || goOwned != want {
				t.Fatalf("%s/%s: SQL owned=%v, Go owned=%v, want %v", language, kind, sqlOwned, goOwned, want)
			}
		}
	}
}

// The pass binds only the symbol at the evidence position: a same-named local
// at another position, a global function, or a missing declaration leaves the
// edge unresolved.
func TestResolveLuaScopeBindsOnlyTheDeclarationAtTheEvidencePosition(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	file, err := insertTestFileLang(ctx, s, repo.ID, "m.lua", "lua")
	if err != nil {
		t.Fatal(err)
	}
	symbol := func(key string, line int) int64 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, 'lua', 'function', 'f', 'f', ?, 1, ?, 9, ?)`,
			repo.ID, file, line, line, key)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	outer := symbol("func:lua:local:f:1:1", 1)
	symbol("func:lua:local:f:2:1", 2)
	symbol("func:lua:global:f:3:1", 3)
	edge := func(evidence string) int64 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line, start_col) VALUES(?, ?, 'f', 'calls', ?, ?, 9, 1)`,
			repo.ID, outer, evidence, file)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	first := edge(graph.LuaLocalFunctionEvidence + "1:1")
	global := edge(graph.LuaLocalFunctionEvidence + "3:1")
	missing := edge(graph.LuaLocalFunctionEvidence + "4:1")
	plain := edge("")
	n, err := resolveLuaScope(ctx, s.db, repo.ID, nil)
	if err != nil || n != 1 {
		t.Fatalf("bound %d, err %v; want exactly one", n, err)
	}
	var got []string
	for _, id := range []int64{first, global, missing, plain} {
		var dst *int64
		var strategy string
		if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id, resolution_strategy FROM edges WHERE id = ?`, id).Scan(&dst, &strategy); err != nil {
			t.Fatal(err)
		}
		if dst == nil {
			got = append(got, "-")
		} else if *dst == outer {
			got = append(got, "outer:"+strategy)
		} else {
			got = append(got, "other")
		}
	}
	if strings.Join(got, ",") != "outer:"+ResolutionStrategyLuaLocalFunction+",-,-,-" {
		t.Fatalf("bindings = %v", got)
	}
}

// Every resolver entry point leaves a Lua call to the Lua pass: a same-named
// global in another file never answers it, on any path.
func TestLuaOwnershipAllEntrypoints(t *testing.T) {
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
		{"paths", func(s *Store, repo int64) error { return s.ResolveEdgesForPaths(ctx, repo, []string{"m.lua"}) }},
		{"paths-and-names", func(s *Store, repo int64) error {
			_, err := s.ResolveEdgesForPathsAndNames(ctx, repo, []string{"m.lua"}, []string{"f", "g"})
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			s, repo := openBudgetStore(t)
			file, err := insertTestFileLang(ctx, s, repo.ID, "m.lua", "lua")
			if err != nil {
				t.Fatal(err)
			}
			other, err := insertTestFileLang(ctx, s, repo.ID, "other.lua", "lua")
			if err != nil {
				t.Fatal(err)
			}
			symbol := func(fileID int64, name, key string) int64 {
				res, err := s.db.ExecContext(ctx, `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, 'lua', 'function', ?, ?, 1, 1, 3, 4, ?)`,
					repo.ID, fileID, name, name, key)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				return id
			}
			local := symbol(file, "f", "func:lua:local:f:1:1")
			symbol(other, "f", "func:lua:global:f:1:1")
			symbol(other, "g", "func:lua:global:g:1:1")
			edge := func(name, evidence string) int64 {
				res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line, start_col) VALUES(?, ?, ?, 'calls', ?, ?, 2, 1)`,
					repo.ID, local, name, evidence, file)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				return id
			}
			proven := edge("f", graph.LuaLocalFunctionEvidence+"1:1")
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
			if got := state(proven) + "," + state(unproven); got != "local:"+ResolutionStrategyLuaLocalFunction+",-" {
				t.Fatalf("bindings = %s", got)
			}
		})
	}
}
