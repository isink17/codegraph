package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestScalaScopeVetoSQLMatchesGoTwin(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	files := map[string]int64{}
	for _, language := range []string{"scala", "java"} {
		id, err := insertTestFileLang(ctx, s, repo.ID, "src."+language, language)
		if err != nil {
			t.Fatal(err)
		}
		files[language] = id
	}
	caller, err := insertTestSymbolLang(ctx, s, repo.ID, files["java"], "caller", "caller", "java")
	if err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"scala", "java"} {
		for _, kind := range []string{EdgeKindCalls, EdgeKindCrossLanguageRef} {
			res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line) VALUES(?, ?, 'f', ?, '', ?, 1)`,
				repo.ID, caller, kind, files[language])
			if err != nil {
				t.Fatal(err)
			}
			id, _ := res.LastInsertId()
			var sqlOwned bool
			if err := s.db.QueryRowContext(ctx, `SELECT NOT (`+scalaScopeVetoSQL+`) FROM edges JOIN files f ON f.id = edges.file_id WHERE edges.id = ?`, id).Scan(&sqlOwned); err != nil {
				t.Fatal(err)
			}
			goOwned := scalaScopeOwned(edgeTarget{srcLanguage: language, edgeKind: kind})
			if want := language == "scala" && kind == EdgeKindCalls; sqlOwned != want || goOwned != want {
				t.Fatalf("%s/%s: SQL owned=%v, Go owned=%v, want %v", language, kind, sqlOwned, goOwned, want)
			}
		}
	}
}

// Every resolver entry point binds a proven Scala call only to the local def
// at its evidence position: never to a same-named local at another position,
// a member of the same file, a member elsewhere or a Lua local at that
// position; an unproven call and a stale position stay unresolved.
func TestScalaOwnershipAllEntrypoints(t *testing.T) {
	ctx := context.Background()
	for _, entry := range []struct {
		name string
		run  func(*Store, int64) error
	}{
		{"pass", func(s *Store, repo int64) error { _, err := resolveScalaScope(ctx, s.db, repo, nil); return err }},
		{"repo-wide", func(s *Store, repo int64) error { _, err := s.ResolveEdges(ctx, repo); return err }},
		{"names", func(s *Store, repo int64) error {
			_, err := s.ResolveEdgesForNames(ctx, repo, []string{"f", "g", "h"})
			return err
		}},
		{"paths", func(s *Store, repo int64) error { return s.ResolveEdgesForPaths(ctx, repo, []string{"M.scala"}) }},
		{"paths-and-names", func(s *Store, repo int64) error {
			_, err := s.ResolveEdgesForPathsAndNames(ctx, repo, []string{"M.scala"}, []string{"f", "g", "h"})
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			s, repo := openBudgetStore(t)
			file, err := insertTestFileLang(ctx, s, repo.ID, "M.scala", "scala")
			if err != nil {
				t.Fatal(err)
			}
			other, err := insertTestFileLang(ctx, s, repo.ID, "Other.scala", "scala")
			if err != nil {
				t.Fatal(err)
			}
			luaFile, err := insertTestFileLang(ctx, s, repo.ID, "m.lua", "lua")
			if err != nil {
				t.Fatal(err)
			}
			symbol := func(fileID int64, language, name, key string, line int) int64 {
				res, err := s.db.ExecContext(ctx, `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, ?, 'function', ?, ?, ?, 3, ?, 9, ?)`,
					repo.ID, fileID, language, name, name, line, line, key)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				return id
			}
			member := symbol(file, "scala", "m", "func:scala:O$.m", 1)
			local := symbol(file, "scala", "f", "func:scala:local:O.m.f:2:3", 2)
			symbol(file, "scala", "f", "func:scala:local:O.n.f:5:3", 5)
			symbol(file, "scala", "h", "func:scala:O$.h", 6)
			symbol(other, "scala", "g", "func:scala:P$.g", 1)
			symbol(other, "scala", "f", "func:scala:P$.f", 1)
			symbol(luaFile, "lua", "f", "func:lua:local:f:7:3", 7)
			edge := func(name, evidence string) int64 {
				res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line, start_col) VALUES(?, ?, ?, 'calls', ?, ?, 3, 5)`,
					repo.ID, member, name, evidence, file)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				return id
			}
			proven := edge("f", graph.ScalaLocalFunctionEvidence+"2:3")
			unproven := edge("g", "")
			memberCall := edge("h", "")
			stale := edge("f", graph.ScalaLocalFunctionEvidence+"4:3")
			luaPosition := edge("f", graph.ScalaLocalFunctionEvidence+"7:3")
			luaEvidence := edge("f", graph.LuaLocalFunctionEvidence+"2:3")
			if err := entry.run(s, repo.ID); err != nil {
				t.Fatal(err)
			}
			state := func(id int64) string {
				var dst *int64
				var strategy string
				if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id, resolution_strategy FROM edges WHERE id = ?`, id).Scan(&dst, &strategy); err != nil {
					t.Fatal(err)
				}
				switch {
				case dst == nil:
					return "-"
				case *dst == local:
					return "local:" + strategy
				}
				return "other:" + strategy
			}
			got := ""
			for _, id := range []int64{proven, unproven, memberCall, stale, luaPosition, luaEvidence} {
				got += state(id) + ","
			}
			if want := "local:" + ResolutionStrategyScalaLocalFunction + ",-,-,-,-,-,"; got != want {
				t.Fatalf("bindings = %s, want %s", got, want)
			}
		})
	}
}

// A Lua or Scala local function whose calls were not proven is not dead code:
// FindDeadCode lists the unreferenced member and neither local.
func TestFindDeadCodeSkipsLocalFunctions(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	for _, seed := range []struct{ path, language, name, key string }{
		{"M.scala", "scala", "member", "func:scala:O$.member"},
		{"M.scala", "scala", "helper", "func:scala:local:O.m.helper:3:5"},
		{"m.lua", "lua", "helper", "func:lua:local:helper:1:1"},
	} {
		var fileID int64
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM files WHERE repo_id = ? AND path = ?`, repo.ID, seed.path).Scan(&fileID); err != nil {
			if fileID, err = insertTestFileLang(ctx, s, repo.ID, seed.path, seed.language); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, ?, 'function', ?, ?, 1, 1, 2, 2, ?)`,
			repo.ID, fileID, seed.language, seed.name, seed.name, seed.key); err != nil {
			t.Fatal(err)
		}
	}
	dead, err := s.FindDeadCode(ctx, repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dead {
		names = append(names, fmt.Sprint(d["name"]))
	}
	if strings.Join(names, ",") != "member" {
		t.Fatalf("dead code = %v, want only the member", names)
	}
}
