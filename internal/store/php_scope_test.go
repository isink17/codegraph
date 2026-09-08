package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// phpFixture builds PHP graphs directly in the Store with the P22.43 fact
// shapes: dotted semantic qnames, method visibility/staticness, php_type
// imports owned by an exact namespace, and `::`/`->` call spellings.
type phpFixture struct {
	*parityFixture
}

func newPHPFixture(t *testing.T) *phpFixture {
	return &phpFixture{newParityFixture(t, "")}
}

func (f *phpFixture) phpFile(t *testing.T, path string) int64 {
	return f.file(t, path, "php")
}

func phpParent(qname string) (string, string) {
	if dot := strings.LastIndexByte(qname, '.'); dot >= 0 {
		return qname[:dot], qname[dot+1:]
	}
	return "", qname
}

func (f *phpFixture) insert(t *testing.T, fileID int64, kind, qname, vis string, static sql.NullInt64) int64 {
	t.Helper()
	container, name := phpParent(qname)
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name, visibility, is_static,
			start_line, start_col, end_line, end_col, stable_key, qualified_suffix, dot_tail2, dot_tail3)
		VALUES(?, ?, 'php', ?, ?, ?, ?, ?, ?, 1, 1, 1, 1, ?, ?, ?, ?)`,
		f.repoID, fileID, kind, name, qname, container, vis, static,
		kind+":php:"+qname+":"+strconv.FormatInt(fileID, 10), qualifiedSuffix(qname), dotTail2(qname), dotTail3(qname))
	if err != nil {
		t.Fatalf("insert %s %s: %v", kind, qname, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// typ declares a PHP class-like type; its container is its namespace.
func (f *phpFixture) typ(t *testing.T, fileID int64, qname string) int64 {
	return f.insert(t, fileID, "type", qname, "public", sql.NullInt64{})
}

// method declares a method with syntax-proven visibility and staticness.
func (f *phpFixture) method(t *testing.T, fileID int64, qname, vis string, static bool) int64 {
	v := int64(0)
	if static {
		v = 1
	}
	return f.insert(t, fileID, "function", qname, vis, sql.NullInt64{Int64: v, Valid: true})
}

// fn declares a top-level function: NULL staticness, namespace as container.
func (f *phpFixture) fn(t *testing.T, fileID int64, qname string) int64 {
	return f.insert(t, fileID, "function", qname, "public", sql.NullInt64{})
}

func (f *phpFixture) use(t *testing.T, fileID int64, source, local, kind, owner string) {
	t.Helper()
	_, name := phpParent(source)
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO scope_import_evidence(repo_id, file_id, language, source_specifier, imported_name, local_name, import_kind, wildcard, is_static, owner_module)
		VALUES(?, ?, 'php', ?, ?, ?, ?, 0, 0, ?)`, f.repoID, fileID, source, name, local, kind, owner); err != nil {
		t.Fatalf("insert use %s: %v", source, err)
	}
}

func (f *phpFixture) call(t *testing.T, fileID int64, src sql.NullInt64, dst string, line int) int64 {
	t.Helper()
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, NULL, ?, 'calls', ?, ?, ?)`, f.repoID, src, dst, dst, fileID, line)
	if err != nil {
		t.Fatalf("insert edge %s: %v", dst, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *phpFixture) reference(t *testing.T, fileID int64, name string, line int) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO references_tbl(repo_id, file_id, ref_kind, name, qualified_name, start_line, start_col, end_line, end_col)
		VALUES (?, ?, 'call', ?, ?, ?, 1, ?, 1)`, f.repoID, fileID, name, name, line, line); err != nil {
		t.Fatal(err)
	}
}

func srcOf(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: true} }

var phpEntryPoints = []string{"full", "paths", "names", "paths+names"}

// TestPHPScopeVetoSurvivesEveryGenericStrategy crafts repository symbols so
// that every generic strategy has one tempting candidate for each owned PHP
// spelling -- a symbol whose qualified name IS the spelling -- and requires the
// edge to stay unresolved on every resolver entry point, while a bare PHP call
// in the same file keeps its generic exact_name answer.
func TestPHPScopeVetoSurvivesEveryGenericStrategy(t *testing.T) {
	f := newPHPFixture(t)
	callerFile := f.phpFile(t, "src/Caller.php")
	baitFile := f.phpFile(t, "src/Bait.php")
	f.typ(t, callerFile, "App.Caller")
	caller := f.method(t, callerFile, "App.Caller.f", "public", false)
	spellings := []string{"Service::run", "static::run", "parent::run", "self::run", "$obj->run", "$obj?->run", `\Vendor\Service::run`, "Service::$m"}
	edges := map[string]int64{}
	for i, dst := range spellings {
		// Bait: exact_qualified would bind dst_name == qualified_name; its bare
		// name is unique for exact_name/receiver_method; dot tails are absent.
		f.insert(t, baitFile, "function", dst, "public", sql.NullInt64{Int64: 1, Valid: true})
		edges[dst] = f.call(t, callerFile, srcOf(caller), dst, i+1)
	}
	// The one generic answer that must survive: a bare PHP call to a unique
	// function is not owned by the PHP scope pass.
	f.fn(t, baitFile, "App.helper")
	bare := f.call(t, callerFile, srcOf(caller), "helper", 50)
	for _, entry := range phpEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"src/Caller.php", "src/Bait.php"}, append(append([]string{}, spellings...), "run", "helper"))
		for dst, id := range edges {
			if got := f.binding(t, id); got != "<unresolved>" {
				t.Fatalf("%s: %q bound to %s", entry, dst, got)
			}
		}
		if got := f.binding(t, bare); got != "App.helper|exact_name|high" {
			t.Fatalf("%s: bare PHP call = %s", entry, got)
		}
	}
}

// A PHP scoped call whose source symbol is not a PHP type or function (the
// indexer drops edges with no containing symbol at all; edges.src_symbol_id is
// NOT NULL) cannot prove its lexical namespace, so it abstains -- and it is
// still owned, so the generic strategies cannot answer it either, even for an
// absolute spelling whose destination exists.
func TestPHPScopeUntrustedSourceOwnedEdgeAbstainsAndStaysVetoed(t *testing.T) {
	f := newPHPFixture(t)
	vendor := f.phpFile(t, "src/Vendor.php")
	f.typ(t, vendor, "Vendor.Service")
	f.method(t, vendor, "Vendor.Service.run", "public", true)
	callerFile := f.phpFile(t, "src/Caller.php")
	f.insert(t, f.phpFile(t, "src/Bait.php"), "function", `\Vendor\Service::run`, "public", sql.NullInt64{Int64: 1, Valid: true})
	odd := f.insert(t, callerFile, "variable", "App.config", "", sql.NullInt64{})
	absolute := f.call(t, callerFile, srcOf(odd), `\Vendor\Service::run`, 1)
	for _, entry := range phpEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"src/Caller.php"}, []string{"run", "Service", `\Vendor\Service::run`})
		if got := f.binding(t, absolute); got != "<unresolved>" {
			t.Fatalf("%s: source-less edge bound to %s", entry, got)
		}
	}
}

// Type identity is decided before the member: two active rows for one type
// qname are ambiguous (no partial types), and two eligible static methods on
// one type are ambiguous. Neither picks a row by insertion order.
func TestPHPScopeDuplicateTypeOrMethodFailsClosed(t *testing.T) {
	f := newPHPFixture(t)
	one := f.phpFile(t, "src/One.php")
	two := f.phpFile(t, "src/Two.php")
	f.typ(t, one, "Vendor.Service")
	f.method(t, one, "Vendor.Service.run", "public", true)
	f.typ(t, two, "Vendor.Service") // duplicate type, no method
	f.typ(t, one, "Vendor.Other")
	f.method(t, one, "Vendor.Other.run", "public", true)
	f.method(t, one, "Vendor.Other.run", "public", true) // duplicate eligible method
	callerFile := f.phpFile(t, "src/Caller.php")
	f.typ(t, callerFile, "App.Caller")
	caller := f.method(t, callerFile, "App.Caller.f", "public", false)
	f.use(t, callerFile, "Vendor.Service", "Service", "php_type", "App")
	f.use(t, callerFile, "Vendor.Other", "Other", "php_type", "App")
	dupType := f.call(t, callerFile, srcOf(caller), "Service::run", 1)
	dupMethod := f.call(t, callerFile, srcOf(caller), "Other::run", 2)
	for _, entry := range phpEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"src/Caller.php"}, []string{"run", "Service", "Other"})
		if got := f.binding(t, dupType); got != "<unresolved>" {
			t.Fatalf("%s: duplicate type bound to %s", entry, got)
		}
		if got := f.binding(t, dupMethod); got != "<unresolved>" {
			t.Fatalf("%s: duplicate method bound to %s", entry, got)
		}
	}
	// Remove the duplicate type row: identity is unique again and binds.
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE file_id = ?`, two); err != nil {
		t.Fatal(err)
	}
	for _, entry := range phpEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"src/Caller.php"}, []string{"run", "Service"})
		if got := f.binding(t, dupType); got != "Vendor.Service.run|php_alias_static|high" {
			t.Fatalf("%s: unique type = %s", entry, got)
		}
	}
}

// Import kinds and owners are exact: php_function/php_const never alias a
// type, and a global-namespace import ("" owner) does not apply inside App.
func TestPHPScopeImportKindAndOwnerAreExact(t *testing.T) {
	f := newPHPFixture(t)
	vendor := f.phpFile(t, "src/Vendor.php")
	f.typ(t, vendor, "Vendor.Service")
	f.method(t, vendor, "Vendor.Service.run", "public", true)
	callerFile := f.phpFile(t, "src/Caller.php")
	f.typ(t, callerFile, "App.Caller")
	caller := f.method(t, callerFile, "App.Caller.f", "public", false)
	f.use(t, callerFile, "Vendor.Service", "F", "php_function", "App")
	f.use(t, callerFile, "Vendor.Service", "C", "php_const", "App")
	f.use(t, callerFile, "Vendor.Service", "G", "php_type", "")
	f.use(t, callerFile, "Vendor.Service", "T", "php_type", "App")
	fnAlias := f.call(t, callerFile, srcOf(caller), "F::run", 1)
	constAlias := f.call(t, callerFile, srcOf(caller), "C::run", 2)
	globalAlias := f.call(t, callerFile, srcOf(caller), "G::run", 3)
	typeAlias := f.call(t, callerFile, srcOf(caller), "T::run", 4)
	f.resolveVia(t, "full", nil, nil)
	for name, id := range map[string]int64{"php_function": fnAlias, "php_const": constAlias, "global owner": globalAlias} {
		if got := f.binding(t, id); got != "<unresolved>" {
			t.Fatalf("%s alias bound to %s", name, got)
		}
	}
	if got := f.binding(t, typeAlias); got != "Vendor.Service.run|php_alias_static|high" {
		t.Fatalf("php_type alias = %s", got)
	}
}

// A method source whose containing type row is ambiguous cannot prove its
// namespace and fails closed; a top-level function source proves it directly.
func TestPHPScopeSourceNamespaceDerivation(t *testing.T) {
	f := newPHPFixture(t)
	file := f.phpFile(t, "src/App.php")
	f.typ(t, file, "App.Service")
	f.method(t, file, "App.Service.run", "public", true)
	f.typ(t, file, "App.Caller")
	f.typ(t, f.phpFile(t, "src/Dup.php"), "App.Caller")
	method := f.method(t, file, "App.Caller.f", "public", false)
	top := f.fn(t, file, "App.g")
	fromMethod := f.call(t, file, srcOf(method), "Service::run", 1)
	fromFunction := f.call(t, file, srcOf(top), "Service::run", 2)
	f.resolveVia(t, "full", nil, nil)
	if got := f.binding(t, fromMethod); got != "<unresolved>" {
		t.Fatalf("ambiguous containing type bound to %s", got)
	}
	if got := f.binding(t, fromFunction); got != "App.Service.run|php_type_scope|high" {
		t.Fatalf("top-level function source = %s", got)
	}
}

// TestPHPScopeUpgradeRepairOldDatabase simulates a repository indexed by a
// P22.43 binary and already carrying every earlier repair marker: PHP facts
// present, one owned edge unresolved, one owned edge bound by a generic
// strategy the PHP pass refuses. The repair must re-decide both, converge the
// derived reference identities despite the existing reference marker, set its
// own marker only after success, and do nothing on a second run.
func TestPHPScopeUpgradeRepairOldDatabase(t *testing.T) {
	f := newPHPFixture(t)
	vendor := f.phpFile(t, "src/Vendor.php")
	f.typ(t, vendor, "Vendor.Service")
	run := f.method(t, vendor, "Vendor.Service.run", "public", true)
	callerFile := f.phpFile(t, "src/Caller.php")
	f.typ(t, callerFile, "App.Caller")
	caller := f.method(t, callerFile, "App.Caller.f", "public", false)
	helper := f.method(t, callerFile, "App.Caller.helper", "private", true)
	f.use(t, callerFile, "Vendor.Service", "S", "php_type", "App")
	provable := f.call(t, callerFile, srcOf(caller), "S::run", 1)
	f.reference(t, callerFile, "S::run", 1)
	late := f.call(t, callerFile, srcOf(caller), "static::helper", 2)
	f.reference(t, callerFile, "static::helper", 2)
	f.setBinding(t, late, helper, ResolutionStrategyExactQualified, ResolutionConfidenceHigh)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id = ?, context_symbol_id = ? WHERE start_line = 2`, helper, caller); err != nil {
		t.Fatal(err)
	}
	for _, repair := range []resolverRepair{typeScopeRepair, bareNameLevelRepair, dotTailAmbiguityRepair, referenceIdentityRepair} {
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}

	// A failed pass must not mark the repository repaired.
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.RepairResolverBindingsOnce(canceled, f.repoID); err == nil {
		t.Fatal("repair under a canceled context succeeded")
	}
	if f.markerSet(t, phpScopeRepairSettingKey) {
		t.Fatal("marker written after a failed repair")
	}
	if got := f.binding(t, late); got != "App.Caller.helper|exact_qualified|high" {
		t.Fatalf("failed repair left half-cleared state: %s", got)
	}

	resolvedRepoWide, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if !resolvedRepoWide {
		t.Fatal("PHP scope repair did not report a repo-wide resolve")
	}
	if got := f.binding(t, provable); got != "Vendor.Service.run|php_alias_static|high" {
		t.Fatalf("provable edge after repair = %s", got)
	}
	if got := f.binding(t, late); got != "<unresolved>" {
		t.Fatalf("static:: edge after repair = %s", got)
	}
	f.assertReference(t, 1, srcOf(run), srcOf(caller))
	f.assertReference(t, 2, sql.NullInt64{}, srcOf(caller))
	if !f.markerSet(t, phpScopeRepairSettingKey) || !f.markerSet(t, referenceIdentityRepairSettingKey) {
		t.Fatal("repair markers not set after success")
	}

	// Second run: nothing runs.
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET resolution_confidence = 'probe' WHERE id = ?`, provable); err != nil {
		t.Fatal(err)
	}
	if resolvedRepoWide, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || resolvedRepoWide {
		t.Fatalf("second repair: resolvedRepoWide=%v err=%v", resolvedRepoWide, err)
	}
	if got := f.binding(t, provable); got != "Vendor.Service.run|php_alias_static|probe" {
		t.Fatalf("second repair rewrote edges: %s", got)
	}
}

func (f *phpFixture) markerSet(t *testing.T, key string) bool {
	t.Helper()
	var value string
	err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key = ?`, key+"."+strconv.FormatInt(f.repoID, 10)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return value == "1"
}

func (f *phpFixture) assertReference(t *testing.T, line int, wantSymbol, wantContext sql.NullInt64) {
	t.Helper()
	var symbol, ctxID sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id, context_symbol_id FROM references_tbl WHERE repo_id = ? AND start_line = ?`, f.repoID, line).Scan(&symbol, &ctxID); err != nil {
		t.Fatal(err)
	}
	if symbol != wantSymbol || ctxID != wantContext {
		t.Fatalf("reference line %d = (%v,%v), want (%v,%v)", line, symbol, ctxID, wantSymbol, wantContext)
	}
}

// Every PHP strategy is registered, confidence high, and redecidable
// incrementally.
func TestPHPScopeStrategiesRegistered(t *testing.T) {
	for _, strategy := range phpScopeStrategies {
		if got := resolutionConfidenceFor(strategy); got != ResolutionConfidenceHigh {
			t.Fatalf("%s confidence = %s", strategy, got)
		}
		found := false
		for _, s := range incrementallyRedecidableStrategies {
			found = found || s == strategy
		}
		if !found {
			t.Fatalf("%s is not incrementally redecidable", strategy)
		}
	}
	if !strings.Contains(resolverBindableCandidateSQL, phpScopeVetoSQL) {
		t.Fatal("generic bind gate does not carry the PHP ownership veto")
	}
}

// A repository without PHP has nothing for the PHP repair to re-decide: the
// marker is written, no repo-wide resolve is claimed, and the reference repair
// marker is left alone.
func TestPHPScopeRepairSkipsRepositoriesWithoutPHP(t *testing.T) {
	f := newPHPFixture(t)
	goFile := f.file(t, "main.go", "go")
	f.symbol(t, goFile, "main", "main.main", "function", "go")
	for _, repair := range resolverRepairs {
		if repair.key != phpScopeRepairSettingKey {
			if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
				t.Fatal(err)
			}
		}
	}
	resolvedRepoWide, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedRepoWide {
		t.Fatal("PHP repair claimed a repo-wide resolve on a repository without PHP")
	}
	if !f.markerSet(t, phpScopeRepairSettingKey) || !f.markerSet(t, referenceIdentityRepairSettingKey) {
		t.Fatal("markers after a skipped PHP repair are not all set")
	}
}

// TestPHPScopeBatchBudget drives the pass past the SQLite bound-variable
// ceiling on both dynamic sets it binds -- edge ids (incremental `only`) and
// candidate qualified names -- on every entry point.
func TestPHPScopeBatchBudget(t *testing.T) {
	f := newPHPFixture(t)
	types := f.phpFile(t, "src/Types.php")
	callerFile := f.phpFile(t, "src/Caller.php")
	f.typ(t, callerFile, "App.Caller")
	caller := f.method(t, callerFile, "App.Caller.f", "public", false)
	const n = 1200
	edges := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		q := fmt.Sprintf("App.T%d", i)
		f.typ(t, types, q)
		f.method(t, types, q+".run", "public", true)
		edges = append(edges, f.call(t, callerFile, srcOf(caller), fmt.Sprintf("T%d::run", i), i+1))
	}
	for _, entry := range phpEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"src/Caller.php"}, []string{"run"})
		for i, id := range edges {
			if got, want := f.binding(t, id), fmt.Sprintf("App.T%d.run|php_type_scope|high", i); got != want {
				t.Fatalf("%s: edge %d = %s, want %s", entry, i, got, want)
			}
		}
	}
}
