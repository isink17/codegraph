package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// resolveLuaScopeReference is the per-edge formulation the set-based pass
// replaced, kept to pin that both decide every edge identically.
func resolveLuaScopeReference(ctx context.Context, q execQuerier, repoID int64) (int, error) {
	if _, err := q.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+`
		WHERE repo_id = ? AND edge_kind = '`+EdgeKindCalls+`' AND (dst_symbol_id IS NOT NULL OR COALESCE(resolution_strategy, '') <> '' OR COALESCE(resolution_confidence, '') <> '') AND file_id IN (SELECT id FROM files WHERE repo_id = ? AND language = 'lua')`, repoID, repoID); err != nil {
		return 0, err
	}
	var reachable bool
	if err := sqliteScanRows(ctx, q, luaDebugReachableSQL, []any{repoID}, func(rows *sql.Rows) error {
		return rows.Scan(&reachable)
	}); err != nil || reachable {
		return 0, err
	}
	targets := map[int64][]int64{}
	if err := sqliteScanRows(ctx, q, `SELECT e.id, s.id
FROM edges e
JOIN files f ON f.id = e.file_id
JOIN symbols s ON s.repo_id = e.repo_id AND s.file_id = e.file_id
 AND s.language = 'lua' AND s.kind = 'function' AND s.name = e.dst_name
 AND substr(s.stable_key, 1, 15) = 'func:lua:local:'
 AND e.evidence = '`+graph.LuaLocalFunctionEvidence+`' || s.start_line || ':' || s.start_col
WHERE e.repo_id = ? AND f.language = 'lua' AND e.edge_kind = '`+EdgeKindCalls+`' AND e.dst_symbol_id IS NULL`, []any{repoID},
		func(rows *sql.Rows) error {
			var edgeID, symbolID int64
			if err := rows.Scan(&edgeID, &symbolID); err != nil {
				return err
			}
			targets[edgeID] = append(targets[edgeID], symbolID)
			return nil
		}); err != nil {
		return 0, err
	}
	confidence := resolutionConfidenceFor(ResolutionStrategyLuaLocalFunction)
	bound := 0
	for _, edgeID := range slices.Sorted(maps.Keys(targets)) {
		// The position names one declaration; anything else is a stale or
		// inconsistent store and stays unresolved.
		if len(targets[edgeID]) != 1 {
			continue
		}
		// ponytail: one UPDATE per bound edge; batch through a temp table if
		// Lua-heavy repositories make this measurable.
		if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ? WHERE id = ?`,
			targets[edgeID][0], ResolutionStrategyLuaLocalFunction, confidence, edgeID); err != nil {
			return 0, err
		}
		bound++
	}
	return bound, nil
}

// luaSyntheticRepo fills a store with files Lua files of funcs local
// functions each. Every function calls the next one three times on one line
// (distinct columns), a shadowing redeclaration of the same name at another
// position, a reassigned name carrying no evidence, and a same-named global.
// dup adds an inconsistent second symbol at a proven position. Unless
// debugFree, the last file lacks debug-free evidence.
func luaSyntheticRepo(tb testing.TB, files, funcs int, dup, debugFree bool) (*Store, int64) {
	tb.Helper()
	ctx := context.Background()
	s, err := Open(filepath.Join(tb.TempDir(), "graph.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	repo, err := s.UpsertRepo(ctx, tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	exec := func(query string, args ...any) int64 {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			tb.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	symbol := func(file int64, name, scope string, line int) int64 {
		return exec(`INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, 'lua', 'function', ?, ?, ?, 1, ?, 9, ?)`,
			repo.ID, file, name, name, line, line, fmt.Sprintf("func:lua:%s:%s:%d:1", scope, name, line))
	}
	edge := func(file, src int64, name, evidence string, line, col int) {
		exec(`INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line, start_col) VALUES(?, ?, ?, 'calls', ?, ?, ?, ?)`,
			repo.ID, src, name, evidence, file, line, col)
	}
	for fi := 0; fi < files; fi++ {
		file := exec(`INSERT INTO files(repo_id, path, language, indexed_at) VALUES(?, ?, 'lua', '')`, repo.ID, fmt.Sprintf("m%d.lua", fi))
		if debugFree || fi != files-1 {
			exec(`INSERT INTO file_scope_evidence(repo_id, file_id, language) VALUES(?, ?, 'lua')`, repo.ID, file)
		}
		ids := make([]int64, funcs)
		for k := range funcs {
			ids[k] = symbol(file, fmt.Sprintf("f%d", k), "local", 10*k+1)
		}
		symbol(file, "f0", "local", 10*funcs+1) // shadowing redeclaration
		symbol(file, "f1", "global", 10*funcs+2)
		if dup {
			symbol(file, "f2", "local", 21)
		}
		for k := range funcs {
			next := (k + 1) % funcs
			name := fmt.Sprintf("f%d", next)
			for col := 1; col <= 3; col++ {
				edge(file, ids[k], name, fmt.Sprintf("%s%d:1", graph.LuaLocalFunctionEvidence, 10*next+1), 10*k+2, col)
			}
			edge(file, ids[k], "f0", fmt.Sprintf("%s%d:1", graph.LuaLocalFunctionEvidence, 10*funcs+1), 10*k+3, 1)
			edge(file, ids[k], "reassigned", "", 10*k+4, 1)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return s, repo.ID
}

func luaEdgeSnapshot(tb testing.TB, s *Store) string {
	tb.Helper()
	var out strings.Builder
	rows, err := s.db.Query(`SELECT id, src_symbol_id, COALESCE(dst_symbol_id, 0), evidence, COALESCE(resolution_strategy, ''), COALESCE(resolution_confidence, '') FROM edges ORDER BY id`)
	if err != nil {
		tb.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, src, dst int64
		var evidence, strategy, confidence string
		if err := rows.Scan(&id, &src, &dst, &evidence, &strategy, &confidence); err != nil {
			tb.Fatal(err)
		}
		fmt.Fprintf(&out, "%d %d %d %s %s %s\n", id, src, dst, evidence, strategy, confidence)
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	return out.String()
}

type luaPass func(context.Context, execQuerier, int64) (int, error)

func runLuaPass(tb testing.TB, s *Store, repoID int64, pass luaPass) int {
	tb.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := pass(context.Background(), tx, repoID)
	if err != nil {
		tb.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return n
}

func TestResolveLuaScopeMatchesReference(t *testing.T) {
	for _, tc := range []struct {
		name           string
		dup, debugFree bool
	}{
		{"consistent", false, true},
		{"duplicate symbol", true, true},
		{"debug reachable", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snaps := map[string]string{}
			counts := map[string]int{}
			for name, pass := range map[string]luaPass{"reference": resolveLuaScopeReference, "set": resolveLuaScope} {
				s, repo := luaSyntheticRepo(t, 3, 5, tc.dup, tc.debugFree)
				// A stale binding from an earlier run must be cleared first.
				if _, err := s.db.Exec(`UPDATE edges SET dst_symbol_id = src_symbol_id, resolution_strategy = 'stale', resolution_confidence = 'low' WHERE evidence = ''`); err != nil {
					t.Fatal(err)
				}
				counts[name] = runLuaPass(t, s, repo, pass)
				if again := runLuaPass(t, s, repo, pass); again != counts[name] {
					t.Fatalf("%s: rerun bound %d, first run %d", name, again, counts[name])
				}
				snaps[name] = luaEdgeSnapshot(t, s)
			}
			if counts["reference"] != counts["set"] || snaps["reference"] != snaps["set"] {
				t.Fatalf("bound %d vs %d\nreference:\n%s\nset:\n%s", counts["reference"], counts["set"], snaps["reference"], snaps["set"])
			}
			want := 3 * 5 * 4 // every evidence-carrying call
			switch {
			case !tc.debugFree:
				want = 0
			case tc.dup:
				want -= 3 * 3 // f2 is called three times from f1 in each file
			}
			if counts["set"] != want {
				t.Fatalf("bound %d, want %d", counts["set"], want)
			}
		})
	}
}

// BenchmarkResolveLuaScope reports updates/op: the UPDATE statements one pass
// issues (the reference issues one per bound edge plus the clear).
func BenchmarkResolveLuaScope(b *testing.B) {
	for _, impl := range []struct {
		name    string
		pass    luaPass
		updates func(bound int) int
	}{
		{"reference", resolveLuaScopeReference, func(n int) int { return 1 + n }},
		{"set", resolveLuaScope, func(int) int { return 2 }},
	} {
		b.Run(impl.name, func(b *testing.B) {
			s, repo := luaSyntheticRepo(b, 200, 25, false, true)
			bound := 0
			b.ResetTimer()
			for b.Loop() {
				bound = runLuaPass(b, s, repo, impl.pass)
			}
			b.ReportMetric(float64(bound), "bound/op")
			b.ReportMetric(float64(impl.updates(bound)), "updates/op")
		})
	}
}
