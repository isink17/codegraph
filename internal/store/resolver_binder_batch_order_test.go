package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// The Go binder narrows its batch in place, once per language pass. A batch
// in which every pass drops something, with the generically bound edges in
// every position, must decide each edge the same way whatever the order: an
// in-place filter that overwrote an entry before reading it would lose or
// repeat an edge here. The resolved count covers what the language passes
// bound as well as what the generic lookups bound.
func TestResolveEdgeTargetsBatchOrderInvariant(t *testing.T) {
	ctx := context.Background()
	type spec struct {
		key, language, dst, evidence string
		want                         string // qualified name it must bind, "" for unresolved
	}
	specs := []spec{
		{key: "go-a", language: "go", dst: "pkg.Target", want: "pkg.Target"},
		{key: "swift", language: "swift", dst: "Thing.run", evidence: "swift:call"},
		{key: "rust", language: "rust", dst: "helper"},
		{key: "cpp", language: "cpp", dst: "ns::foo", evidence: "static_qualified:ns::foo"},
		{key: "go-b", language: "go", dst: "pkg.Other", want: "pkg.Other"},
		{key: "ts", language: "typescript", dst: "mod.fn"},
		{key: "php", language: "php", dst: "A::run"},
		{key: "ruby", language: "ruby", dst: "helper"},
		{key: "go-c", language: "go", dst: "pkg.Third", want: "pkg.Third"},
		{key: "py", language: "python", dst: "helper", want: "pymod.helper"},
		{key: "py-miss", language: "python", dst: "missing"},
	}
	var orders [][]int
	base := make([]int, len(specs))
	for i := range base {
		base[i] = i
	}
	for r := range len(base) {
		rotated := append(slices.Clone(base[r:]), base[:r]...)
		orders = append(orders, rotated)
		reversed := slices.Clone(rotated)
		slices.Reverse(reversed)
		orders = append(orders, reversed)
	}
	const wantUnresolved = 4
	for _, order := range orders {
		s, repo := openBudgetStore(t)
		files := map[string]int64{}
		callers := map[string]int64{}
		for _, language := range []string{"go", "swift", "rust", "cpp", "typescript", "php", "ruby", "python"} {
			file, err := insertTestFileLang(ctx, s, repo.ID, "src/"+language+".x", language)
			if err != nil {
				t.Fatal(err)
			}
			files[language] = file
			caller, err := insertTestSymbolLang(ctx, s, repo.ID, file, "caller", language+".caller", language)
			if err != nil {
				t.Fatal(err)
			}
			callers[language] = caller
		}
		for _, qualified := range []string{"pkg.Target", "pkg.Other", "pkg.Third"} {
			if _, err := insertTestSymbolLang(ctx, s, repo.ID, files["go"], qualified[len("pkg."):], qualified, "go"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := insertTestSymbolLang(ctx, s, repo.ID, files["python"], "helper", "pymod.helper", "python"); err != nil {
			t.Fatal(err)
		}
		edgeIDs := map[string]int64{}
		var targets []edgeTarget
		for _, i := range order {
			sp := specs[i]
			res, err := s.db.ExecContext(ctx, `
				INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line)
				VALUES(?, ?, ?, ?, ?, ?, 1)`, repo.ID, callers[sp.language], sp.dst, EdgeKindCalls, sp.evidence, files[sp.language])
			if err != nil {
				t.Fatal(err)
			}
			id, _ := res.LastInsertId()
			edgeIDs[sp.key] = id
			targets = append(targets, edgeTarget{edgeID: id, srcLanguage: sp.language, srcFileID: files[sp.language],
				dstName: sp.dst, evidence: sp.evidence, edgeKind: EdgeKindCalls})
		}
		outcome, err := s.resolveEdgeTargets(ctx, repo.ID, targets, map[int64]struct{}{}, newImportScopeCache(s, repo.ID))
		if err != nil {
			t.Fatal(err)
		}
		label := fmt.Sprint(order)
		for _, sp := range specs {
			var got string
			if err := s.db.QueryRowContext(ctx, `
				SELECT COALESCE((SELECT qualified_name FROM symbols WHERE id = e.dst_symbol_id), '')
				FROM edges e WHERE e.id = ?`, edgeIDs[sp.key]).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != sp.want {
				t.Errorf("order %s: %s bound %q, want %q", label, sp.key, got, sp.want)
			}
		}
		// Unresolved: swift, cpp, ruby and the generic miss. The Rust,
		// TypeScript and PHP passes leave their unbound edges uncounted, so
		// the total is pinned rather than derived from the batch size.
		if outcome.resolved != 4 || outcome.unresolved != wantUnresolved {
			t.Errorf("order %s: resolved/unresolved = %d/%d, want 4/%d", label, outcome.resolved, outcome.unresolved, wantUnresolved)
		}
	}
}
