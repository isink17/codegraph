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

// markLuaDebugFree records the parser's evidence that a Lua file cannot reach
// the debug library; without it no Lua call in the repository binds.
func markLuaDebugFree(t *testing.T, s *Store, repoID, fileID int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO file_scope_evidence(repo_id, file_id, language) VALUES(?, ?, 'lua')`, repoID, fileID); err != nil {
		t.Fatal(err)
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
	markLuaDebugFree(t, s, repo.ID, file)
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
	n, err := resolveLuaScope(ctx, s.db, repo.ID)
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
			markLuaDebugFree(t, s, repo.ID, file)
			markLuaDebugFree(t, s, repo.ID, other)
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

// One Lua file without debug-free evidence withdraws every binding in the
// repository, including bindings the pass made before; restoring the evidence
// restores them.
func TestResolveLuaScopeWithdrawnByAnyFileWithoutDebugEvidence(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	file, err := insertTestFileLang(ctx, s, repo.ID, "m.lua", "lua")
	if err != nil {
		t.Fatal(err)
	}
	other, err := insertTestFileLang(ctx, s, repo.ID, "other.lua", "lua")
	if err != nil {
		t.Fatal(err)
	}
	markLuaDebugFree(t, s, repo.ID, file)
	res, err := s.db.ExecContext(ctx, `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, 'lua', 'function', 'f', 'f', 1, 1, 1, 9, 'func:lua:local:f:1:1')`, repo.ID, file)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := res.LastInsertId()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line, start_col) VALUES(?, ?, 'f', 'calls', ?, ?, 2, 1)`,
		repo.ID, f, graph.LuaLocalFunctionEvidence+"1:1", file); err != nil {
		t.Fatal(err)
	}
	bound := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE dst_symbol_id IS NOT NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n, err := resolveLuaScope(ctx, s.db, repo.ID); err != nil || n != 0 || bound() != 0 {
		t.Fatalf("other.lua unproven: bound %d (stored %d), err %v; want none", n, bound(), err)
	}
	markLuaDebugFree(t, s, repo.ID, other)
	if n, err := resolveLuaScope(ctx, s.db, repo.ID); err != nil || n != 1 || bound() != 1 {
		t.Fatalf("all proven: bound %d (stored %d), err %v; want one", n, bound(), err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM file_scope_evidence WHERE file_id = ?`, other); err != nil {
		t.Fatal(err)
	}
	if n, err := resolveLuaScope(ctx, s.db, repo.ID); err != nil || n != 0 || bound() != 0 {
		t.Fatalf("evidence withdrawn: bound %d (stored %d), err %v; want the earlier binding cleared", n, bound(), err)
	}
	// A deleted file's tombstone proves nothing either way.
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, other); err != nil {
		t.Fatal(err)
	}
	if n, err := resolveLuaScope(ctx, s.db, repo.ID); err != nil || n != 1 {
		t.Fatalf("unproven file deleted: bound %d, err %v; want one", n, err)
	}
}

// luaOneProvenCall stores m.lua, debug-free, with one proven call to its
// local function f, and returns a counter of bound edges.
func luaOneProvenCall(t *testing.T) (*Store, int64, func() int) {
	t.Helper()
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	file, err := insertTestFileLang(ctx, s, repo.ID, "m.lua", "lua")
	if err != nil {
		t.Fatal(err)
	}
	markLuaDebugFree(t, s, repo.ID, file)
	res, err := s.db.ExecContext(ctx, `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, 'lua', 'function', 'f', 'f', 1, 1, 1, 9, 'func:lua:local:f:1:1')`, repo.ID, file)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := res.LastInsertId()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line, start_col) VALUES(?, ?, 'f', 'calls', ?, ?, 2, 1)`,
		repo.ID, f, graph.LuaLocalFunctionEvidence+"1:1", file); err != nil {
		t.Fatal(err)
	}
	return s, repo.ID, func() int {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE dst_symbol_id IS NOT NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

// Only a Lua row is debug-free evidence, and the store writes a Lua row only
// for that proof: a row of another kind, or a Lua file carrying some other
// scope fact without the proof, leaves the repository unproven.
func TestLuaDebugEvidenceRequiresTheDedicatedRow(t *testing.T) {
	ctx := context.Background()
	t.Run("row of another language", func(t *testing.T) {
		s, repoID, bound := luaOneProvenCall(t)
		other, err := insertTestFileLang(ctx, s, repoID, "other.lua", "lua")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO file_scope_evidence(repo_id, file_id, language, package_name) VALUES(?, ?, 'hcl', 'x')`, repoID, other); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveLuaScope(ctx, s.db, repoID); err != nil || bound() != 0 {
			t.Fatalf("bound %d, err %v; want none", bound(), err)
		}
	})
	t.Run("other scope fact without the proof", func(t *testing.T) {
		s, repoID, bound := luaOneProvenCall(t)
		parsed := graph.ParsedFile{Language: "lua", Scope: graph.ScopeEvidence{Package: "x", ModulePath: "y"}}
		if err := s.ReplaceFileGraph(ctx, repoID, 1, "other.lua", "lua", 1, 1, "other", parsed); err != nil {
			t.Fatal(err)
		}
		var rows int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_scope_evidence fs JOIN files f ON f.id = fs.file_id WHERE f.path = 'other.lua'`).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("evidence rows for other.lua = %d, err %v; want none", rows, err)
		}
		if _, err := resolveLuaScope(ctx, s.db, repoID); err != nil || bound() != 0 {
			t.Fatalf("bound %d, err %v; want none", bound(), err)
		}
		parsed.Scope.LuaDebugFree = true
		if err := s.ReplaceFileGraph(ctx, repoID, 1, "other.lua", "lua", 1, 1, "other2", parsed); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveLuaScope(ctx, s.db, repoID); err != nil || bound() != 1 {
			t.Fatalf("with the proof: bound %d, err %v; want one", bound(), err)
		}
	})
}

// A .lua path puts Lua in an update's scope even when it has no file row left
// and the same update edits another language's file.
func TestLuaPathWithoutFileRowSchedulesTheLuaPass(t *testing.T) {
	ctx := context.Background()
	s, repoID, bound := luaOneProvenCall(t)
	if _, err := insertTestFileLang(ctx, s, repoID, "main.go", "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEdgesForPathsAndNames(ctx, repoID, []string{"gone.lua", "main.go"}, nil); err != nil {
		t.Fatal(err)
	}
	if bound() != 1 {
		t.Fatalf("bound %d; want the Lua pass to run and bind one", bound())
	}
}
