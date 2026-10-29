//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

func TestSolidityReferencesStayUnresolvedAcrossIncrementalLifecycle(t *testing.T) {
	caller := `pragma solidity ^0.8.20;
import "./Provider.sol";
contract Consumer {
    function run(uint x) external { target(x); Provider.target(x); }
    function target(uint) internal {}
}
`
	provider := `pragma solidity ^0.8.20;
contract Provider { function target(uint) external {} }
`
	r := newSolidityLifecycleRepo(t, tree{
		"contracts/Caller.sol":   caller,
		"contracts/Provider.sol": provider,
	})
	r.assertSolidityParity(t, "initial")
	r.assertSolidityUnresolved(t, "initial", 2)
	initialProviderKey := solidityIndexedKey(t, r, "Provider.target")

	providerChanged := `pragma solidity ^0.8.20;
contract Provider { function target(address) external {} }
`
	r.write(t, "contracts/Provider.sol", providerChanged)
	r.update(t, "contracts/Provider.sol")
	r.assertSolidityParity(t, "source modification")
	r.assertSolidityUnresolved(t, "source modification", 2)
	if got := solidityIndexedKey(t, r, "Provider.target"); got == initialProviderKey {
		t.Fatalf("signature change retained provider identity %q", got)
	}

	added := "pragma solidity ^0.8.20; contract Additional { function target(uint) external {} }\n"
	r.write(t, "contracts/Additional.sol", added)
	r.update(t, "contracts/Additional.sol")
	r.assertSolidityParity(t, "file addition")
	r.assertSolidityUnresolved(t, "file addition", 2)

	r.remove(t, "contracts/Provider.sol")
	r.update(t)
	r.assertSolidityParity(t, "provider deletion")
	r.assertSolidityUnresolved(t, "provider deletion", 2)

	r.write(t, "contracts/Provider.sol", providerChanged)
	r.update(t, "contracts/Provider.sol")
	r.assertSolidityParity(t, "provider restoration")
	r.assertSolidityUnresolved(t, "provider restoration", 2)
	if got := solidityIndexedKey(t, r, "Provider.target"); got == initialProviderKey {
		t.Fatalf("restored changed signature unexpectedly has old identity %q", got)
	}

	if err := os.Rename(filepath.Join(r.root, "contracts/Provider.sol"), filepath.Join(r.root, "contracts/Moved.sol")); err != nil {
		t.Fatal(err)
	}
	r.update(t)
	r.assertSolidityParity(t, "file rename")
	r.assertSolidityUnresolved(t, "file rename", 2)

	caller = stringsReplaceImport(caller, "./Provider.sol", "./Moved.sol")
	r.write(t, "contracts/Caller.sol", caller)
	r.update(t, "contracts/Caller.sol")
	r.assertSolidityParity(t, "import change")
	r.assertSolidityUnresolved(t, "import change", 2)

	r.update(t)
	r.assertSolidityParity(t, "no-op update")
	r.assertSolidityUnresolved(t, "no-op update", 2)
}

func newSolidityLifecycleRepo(t *testing.T, files tree) *lifecycleRepo {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "codegraph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: dbPath, store: s, idx: New(s, parser.NewRegistry(tsparser.NewSolidity()), nil)}
	for rel, content := range files {
		r.write(t, rel, content)
	}
	if _, err := r.idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}
	r.repoID = repo.ID
	return r
}

func (r *lifecycleRepo) assertSolidityParity(t *testing.T, step string) {
	t.Helper()
	fresh := newSolidityLifecycleRepo(t, r.currentTree(t))
	got, want := solidityGraphProjection(t, r), solidityGraphProjection(t, fresh)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s fresh/incremental graph mismatch:\n got %v\nwant %v", step, got, want)
	}
}

func (r *lifecycleRepo) assertSolidityUnresolved(t *testing.T, step string, wantCalls int) {
	t.Helper()
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("%s emitted resolved or unresolved edges without proof: %+v", step, edges)
	}
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var calls, unbound, imports int
	err = db.QueryRowContext(r.ctx, `SELECT
		SUM(CASE WHEN ref_kind = 'call' THEN 1 ELSE 0 END),
		SUM(CASE WHEN ref_kind = 'call' AND symbol_id IS NULL AND context_symbol_id IS NULL THEN 1 ELSE 0 END),
		(SELECT COUNT(*) FROM file_imports fi JOIN files f ON f.id = fi.file_id WHERE f.repo_id = ? AND f.is_deleted = 0)
		FROM references_tbl r JOIN files f ON f.id = r.file_id WHERE f.repo_id = ? AND f.is_deleted = 0`, r.repoID, r.repoID).Scan(&calls, &unbound, &imports)
	if err != nil {
		t.Fatal(err)
	}
	if calls != wantCalls || unbound != wantCalls || imports != 0 {
		t.Fatalf("%s references calls/unbound/file-imports = %d/%d/%d, want %d/%d/0", step, calls, unbound, imports, wantCalls, wantCalls)
	}
}

func solidityGraphProjection(t *testing.T, r *lifecycleRepo) []string {
	t.Helper()
	symbols, err := r.store.ExportSymbolsPage(r.ctx, r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		out = append(out, fmt.Sprintf("symbol|%s|%s|%s|%s|%d:%d-%d:%d", filepath.ToSlash(symbol.FilePath), symbol.Kind, symbol.QualifiedName, symbol.StableKey, symbol.Range.StartLine, symbol.Range.StartCol, symbol.Range.EndLine, symbol.Range.EndCol))
	}
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(r.ctx, `SELECT f.path, r.ref_kind, r.name, r.qualified_name, r.start_line, r.start_col, r.end_line, r.end_col, r.symbol_id, r.context_symbol_id
		FROM references_tbl r JOIN files f ON f.id = r.file_id WHERE f.repo_id = ? AND f.is_deleted = 0 ORDER BY f.path, r.start_line, r.start_col, r.ref_kind, r.qualified_name`, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var path, kind, name, qualified string
		var sl, sc, el, ec int
		var symbolID, contextID sql.NullInt64
		if err := rows.Scan(&path, &kind, &name, &qualified, &sl, &sc, &el, &ec, &symbolID, &contextID); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("reference|%s|%s|%s|%s|%d:%d-%d:%d|%v|%v", filepath.ToSlash(path), kind, name, qualified, sl, sc, el, ec, symbolID, contextID))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func solidityIndexedKey(t *testing.T, r *lifecycleRepo, qualified string) string {
	t.Helper()
	symbols, err := r.store.ExportSymbolsPage(r.ctx, r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, symbol := range symbols {
		if symbol.QualifiedName == qualified {
			return symbol.StableKey
		}
	}
	t.Fatalf("symbol %q missing from index", qualified)
	return ""
}

func stringsReplaceImport(source, from, to string) string {
	return strings.Replace(source, from, to, 1)
}
