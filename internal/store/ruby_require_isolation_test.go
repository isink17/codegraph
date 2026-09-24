package store

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

// Ruby `file_imports` rows are raw require strings: they state neither the call
// kind nor the load path, so no generic import consumer may read them. These
// fixtures pair each Ruby importer with the same import written in a language
// whose import semantics the consumer does model, so every abstention is shown
// to be about the Ruby source evidence and not about a fixture that could never
// have produced a link.

func rubyRequireBridgeSpec(importerLanguage, importerPath, specifier, targetPath, targetLanguage string) crossLangSpec {
	return crossLangSpec{
		files: []crossLangFile{
			{path: importerPath, language: importerLanguage, symbols: []crossLangSymbol{
				{name: "render_report", qualified: "App.render_report"},
			}},
			{path: targetPath, language: targetLanguage, symbols: []crossLangSymbol{
				{name: "render_report", qualified: "report.render_report"},
			}},
		},
		imports: []crossLangImport{{fromPath: importerPath, path: specifier}},
	}
}

func TestCrossLanguageRubyRequireRowsAreNotBridges(t *testing.T) {
	tests := []struct {
		name, importer, specifier, target, targetLanguage string
	}{
		{"require_relative extensionless foreign target", "app/caller.rb", "../tools/report", "tools/report.py", "python"},
		{"require_relative explicit .py", "app/caller.rb", "../tools/report.py", "tools/report.py", "python"},
		{"require_relative explicit .ts", "app/caller.rb", "../tools/report.ts", "tools/report.ts", "typescript"},
		// `require "./x"` is working-directory relative in Ruby, never
		// caller-relative, yet the generic resolver would read it as the latter.
		{"ordinary require with ./", "app/caller.rb", "./tools/report", "app/tools/report.py", "python"},
		{"root path-shaped require", "app/caller.rb", "tools/report", "tools/report.py", "python"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Control: the identical bridge from a TypeScript importer links, so
			// the fixture is capable of producing the false Ruby edge.
			control := newGateFixture(t)
			tsImporter := strings.TrimSuffix(tc.importer, ".rb") + ".ts"
			controlTarget := tc.target
			if tc.targetLanguage == "typescript" {
				controlTarget = strings.TrimSuffix(tc.target, ".ts") + ".py"
			}
			controlSpecifier := strings.TrimSuffix(tc.specifier, ".ts")
			rubyRequireBridgeSpec("typescript", tsImporter, controlSpecifier, controlTarget, "python").build(t, control, 1)
			if created := control.resolveCrossLanguage(t); created != 1 {
				t.Fatalf("control TypeScript bridge created %d links, want 1", created)
			}

			f := newGateFixture(t)
			rubyRequireBridgeSpec("ruby", tc.importer, tc.specifier, tc.target, tc.targetLanguage).build(t, f, 1)
			if created := f.resolveCrossLanguage(t); created != 0 {
				t.Fatalf("Ruby require row created %d cross-language links, want 0:\n%s",
					created, strings.Join(crossLangLinks(t, f), "\n"))
			}
			var rows int
			if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM file_imports WHERE repo_id = ?`, f.repoID).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 1 {
				t.Fatalf("Ruby file_imports rows = %d, want the row kept as metadata", rows)
			}
		})
	}
}

// The restriction is on Ruby SOURCE evidence: another language's own import may
// still name a Ruby file as its bridge destination.
func TestCrossLanguageRubyFileRemainsBridgeDestination(t *testing.T) {
	f := newGateFixture(t)
	rubyRequireBridgeSpec("typescript", "src/ts/client.ts", "src/rb/report", "src/rb/report.rb", "ruby").build(t, f, 1)
	if created := f.resolveCrossLanguage(t); created != 1 {
		t.Fatalf("TypeScript -> Ruby bridge created %d links, want 1", created)
	}
}

// importScopeForRepo must take nothing from a Ruby row: not the direct scope
// of the Ruby file itself, and not a re-export hop for a caller that imports it.
func TestImportScopeExcludesRubyRows(t *testing.T) {
	f := newTypeScopeFixture(t)
	tcp := f.file(t, "app/tcp.py", "python")
	other := f.file(t, "app/other.py", "python")
	bridge := f.file(t, "app/pkg.rb", "ruby")
	for _, spec := range []string{"./tcp", "../app/other", ".tcp", "app.tcp", "tcp"} {
		f.importPath(t, bridge, spec)
	}
	caller := f.file(t, "app/caller.py", "python")
	f.importPath(t, caller, "app.pkg")

	scope, err := importScopeForRepo(f.ctx, f.store.db, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if got := scope[bridge]; len(got) != 0 {
		t.Fatalf("Ruby file has generic import scope %v, want none", got)
	}
	if _, ok := scope[caller][bridge]; !ok {
		t.Fatalf("Python import of the Ruby file lost its own direct scope: %v", scope[caller])
	}
	for _, id := range []int64{tcp, other} {
		if _, ok := scope[caller][id]; ok {
			t.Fatalf("Ruby require row acted as a re-export hop: caller scope %v holds %d", scope[caller], id)
		}
	}

	// Control: a Python barrel with the same relative specifier is a hop.
	barrel := f.file(t, "app/pkgpy.py", "python")
	f.importPath(t, barrel, ".tcp")
	pyCaller := f.file(t, "app/pycaller.py", "python")
	f.importPath(t, pyCaller, "app.pkgpy")
	scope, err = importScopeForRepo(f.ctx, f.store.db, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := scope[pyCaller][tcp]; !ok {
		t.Fatalf("Python re-export hop lost: %v", scope[pyCaller])
	}
}

// A Ruby file on the would-be hop grants the bare-name binding nothing.
func TestTypeScopeRubyFileIsNotAHop(t *testing.T) {
	f := newTypeScopeFixture(t)
	tcpFile := f.file(t, "app/tcp.py", "python")
	f.class(t, tcpFile, "Layer", "tcp.Layer", "python")
	bridge := f.file(t, "app/pkg.rb", "ruby")
	f.importPath(t, bridge, "./tcp")
	callFile := f.file(t, "app/caller.py", "python")
	f.importPath(t, callFile, "app.pkg")
	caller := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
	edgeID := f.edge(t, callFile, caller, "Layer")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("Ruby require row granted scope for %s", f.qualifiedNameOf(t, got))
	}
}

// A database written before the Ruby exclusion can hold a binding only the Ruby
// hop allowed, with the v2 repair marker set. One ordinary repair pass must
// re-decide it, reconcile the reference that followed it, and not run again.
func TestTypeScopeRepairV3RetiresRubyHopBinding(t *testing.T) {
	f := newTypeScopeFixture(t)
	tcpFile := f.file(t, "app/tcp.py", "python")
	layer := f.class(t, tcpFile, "Layer", "tcp.Layer", "python")
	bridge := f.file(t, "app/pkg.rb", "ruby")
	f.importPath(t, bridge, "./tcp")
	callFile := f.file(t, "app/caller.py", "python")
	f.importPath(t, callFile, "app.pkg")
	caller := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
	var edgeID int64
	if err := f.store.db.QueryRowContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
		                  resolution_strategy, resolution_confidence)
		VALUES(?, ?, ?, 'Layer', ?, '', ?, 3, ?, ?) RETURNING id`,
		f.repoID, caller, layer, EdgeKindCalls, callFile,
		ResolutionStrategyExactName, resolutionConfidenceFor(ResolutionStrategyExactName)).Scan(&edgeID); err != nil {
		t.Fatal(err)
	}
	var refID int64
	if err := f.store.db.QueryRowContext(f.ctx, `
		INSERT INTO references_tbl(repo_id, file_id, symbol_id, ref_kind, name, qualified_name,
		                           start_line, start_col, end_line, end_col, context_symbol_id)
		VALUES(?, ?, ?, 'call', 'Layer', '', 3, 1, 3, 6, ?) RETURNING id`,
		f.repoID, callFile, layer, caller).Scan(&refID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkResolverBindingsRepaired(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	repo := strconv.FormatInt(f.repoID, 10)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM settings WHERE key = ?`, typeScopeRepairSettingKey+"."+repo); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT OR REPLACE INTO settings(key, value) VALUES(?, '1')`,
		"resolver.type_scope_repaired.v2."+repo); err != nil {
		t.Fatal(err)
	}

	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("v3 repair kept the Ruby-hop binding to %s", f.qualifiedNameOf(t, got))
	}
	var refSymbol sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE id = ?`, refID).Scan(&refSymbol); err != nil {
		t.Fatal(err)
	}
	if refSymbol.Valid {
		t.Fatalf("reference still names symbol %d after its edge was cleared", refSymbol.Int64)
	}
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key = ?`, typeScopeRepairSettingKey+"."+repo).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("v3 marker = %q, %v; want 1", marker, err)
	}
	if typeScopeRepairSettingKey != "resolver.type_scope_repaired.v3" {
		t.Fatalf("type-scope repair key = %q", typeScopeRepairSettingKey)
	}
	if ran, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, typeScopeRepair); err != nil || ran {
		t.Fatalf("second repair ran=%v err=%v, want no rerun", ran, err)
	}
}
