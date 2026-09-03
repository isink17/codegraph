package store

import (
	"testing"

	"github.com/isink17/codegraph/internal/classify"
)

// P8 controls at the store boundary. internal/classify owns the rules and tests
// them directly; what is verified here is the part only the store can get wrong:
// that the batched evidence reaching the classifier is the right file's imports,
// the right persisted language, and the current binding state -- and that the
// value moves correctly when an edge is bound or unbound.

// classifyFixture wires the shared resolver-gate fixture to the export path.
type classifyFixture struct {
	*gateFixture
}

func newClassifyFixture(t *testing.T) *classifyFixture {
	t.Helper()
	return &classifyFixture{gateFixture: newGateFixture(t)}
}

func (f *classifyFixture) imports(t *testing.T, fileID int64, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := f.store.db.ExecContext(f.ctx,
			`INSERT INTO file_imports(repo_id, file_id, import_path) VALUES(?, ?, ?)`,
			f.repoID, fileID, path); err != nil {
			t.Fatalf("insert file_import(%q) error = %v", path, err)
		}
	}
}

// edgeWithCallSite inserts an unresolved edge that carries recorded call-site
// text in `edges.evidence`, which is what the non-cgo Python adapter stores.
func (f *classifyFixture) edgeWithCallSite(t *testing.T, fileID, srcSymbolID int64, dstName, callSite string) int64 {
	t.Helper()
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, NULL, ?, 'call', ?, ?, 1)
	`, f.repoID, srcSymbolID, dstName, callSite, fileID)
	if err != nil {
		t.Fatalf("insert edge(%q) error = %v", dstName, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}
	return id
}

// classificationByDstName exports the whole repo and indexes the result by
// dst_name, so assertions read through the real production surface rather than
// calling the classifier directly.
func (f *classifyFixture) classificationByDstName(t *testing.T) map[string]ExportEdge {
	t.Helper()
	edges, err := f.store.ExportEdgesPage(f.ctx, f.repoID, 1000, 0)
	if err != nil {
		t.Fatalf("ExportEdgesPage() error = %v", err)
	}
	out := map[string]ExportEdge{}
	for _, edge := range edges {
		out[edge.DstName] = edge
	}
	return out
}

func (f *classifyFixture) assertClassification(t *testing.T, dstName, want string) ExportEdge {
	t.Helper()
	edge, ok := f.classificationByDstName(t)[dstName]
	if !ok {
		t.Fatalf("no exported edge with dst_name %q", dstName)
	}
	if edge.TargetClassification != want {
		t.Fatalf("dst_name %q: target_classification = %q, want %q (resolved=%v)",
			dstName, edge.TargetClassification, want, edge.DstSymbolID != nil)
	}
	return edge
}

func TestTargetClassification_TypeScriptIntrinsicShadowIsBatchedAndLanguageScoped(t *testing.T) {
	f := newClassifyFixture(t)

	tsFile := f.file(t, "src/ts/caller.ts", "typescript")
	tsSymbol := f.symbol(t, tsFile, "run", "caller.run", "typescript")
	f.edge(t, tsFile, tsSymbol, "Array.isArray")
	f.edge(t, tsFile, tsSymbol, "JSON.stringify")

	// A same-spelled symbol in another language is not shadow evidence.
	goFile := f.file(t, "src/go/array.go", "go")
	f.symbol(t, goFile, "Array", "array.Array", "go")
	if got := f.assertClassification(t, "Array.isArray", classify.Builtin).TargetClassification; got != classify.Builtin {
		t.Fatalf("cross-language Array shadow = %q", got)
	}
	f.assertClassification(t, "JSON.stringify", classify.Builtin)

	shadowFile := f.file(t, "src/ts/shadow.ts", "typescript")
	f.symbol(t, shadowFile, "Array", "shadow.Array", "typescript")
	f.assertClassification(t, "Array.isArray", classify.Unknown)
}

// TestTargetClassification_PythonBuiltinSurvivesCrossLanguageNameCollision is
// the adversarial audit's case A at the store level: Python calls the builtin
// `sorted`, and the only project definition of that name is an unrelated Swift
// function.
//
// All three properties must hold together. The edge must stay unresolved (P2),
// it must never point at the Swift symbol, and it must classify as `builtin` --
// the Swift definition is not evidence that the project shadows Python's
// builtin.
func TestTargetClassification_PythonBuiltinSurvivesCrossLanguageNameCollision(t *testing.T) {
	f := newClassifyFixture(t)

	caller := f.file(t, "src/py/caller_a.py", "python")
	srcSymbol := f.symbol(t, caller, "run_a", "caller_a.run_a", "python")
	edgeID := f.edge(t, caller, srcSymbol, "sorted")

	swift := f.file(t, "src/swift/SortKitA.swift", "swift")
	swiftSymbol := f.symbol(t, swift, "sorted", "SortKitA.sorted", "swift")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if dst, resolved := f.dstSymbolID(t, edgeID); resolved {
		t.Fatalf("edge bound dst_symbol_id = %d (swift symbol is %d); P2 gate regressed", dst, swiftSymbol)
	}

	edge := f.assertClassification(t, "sorted", classify.Builtin)
	if edge.DstSymbolID != nil {
		t.Fatalf("builtin edge carries dst_symbol_id = %d, want NULL", *edge.DstSymbolID)
	}
	if edge.ResolutionStrategy != "" || edge.ResolutionConfidence != "" {
		t.Fatalf("unresolved edge carries P4 metadata (%q/%q), want empty",
			edge.ResolutionStrategy, edge.ResolutionConfidence)
	}
}

// TestTargetClassification_GoBuiltin is the second builtin language, and pins
// that builtins need no import evidence at all.
func TestTargetClassification_GoBuiltin(t *testing.T) {
	f := newClassifyFixture(t)

	caller := f.file(t, "src/go/caller.go", "go")
	srcSymbol := f.symbol(t, caller, "Run", "main.Run", "go")
	f.edge(t, caller, srcSymbol, "len")
	f.edge(t, caller, srcSymbol, "append")
	f.imports(t, caller, "fmt")

	f.assertClassification(t, "len", classify.Builtin)
	f.assertClassification(t, "append", classify.Builtin)
}

// TestTargetClassification_BuiltinAbstainsForAmbiguousProjectName is the P3
// interaction. Two same-language definitions of `sorted` make the name ambiguous;
// the resolver refuses to bind, and classification must report `unknown` rather
// than reading the unresolved state as proof of a builtin.
//
// The Swift definition from case A is kept in the fixture to show the guard is
// language-scoped: it is the Python definitions that cause the abstention.
func TestTargetClassification_BuiltinAbstainsForAmbiguousProjectName(t *testing.T) {
	f := newClassifyFixture(t)

	caller := f.file(t, "src/py/caller.py", "python")
	srcSymbol := f.symbol(t, caller, "run", "caller.run", "python")
	edgeID := f.edge(t, caller, srcSymbol, "sorted")

	first := f.file(t, "src/py/a.py", "python")
	f.symbol(t, first, "sorted", "a.sorted", "python")
	second := f.file(t, "src/py/b.py", "python")
	f.symbol(t, second, "sorted", "b.sorted", "python")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if dst, resolved := f.dstSymbolID(t, edgeID); resolved {
		t.Fatalf("ambiguous name bound dst_symbol_id = %d; P3 refusal regressed", dst)
	}
	f.assertClassification(t, "sorted", classify.Unknown)
}

// TestTargetClassification_StdlibAndExternalUseOnlyTheOwningFilesImports is the
// linkage control. Four languages with genuine stdlib evidence and one with
// genuine external evidence share a repo, and each file's imports must classify
// only that file's edges.
func TestTargetClassification_StdlibAndExternalUseOnlyTheOwningFilesImports(t *testing.T) {
	f := newClassifyFixture(t)

	goFile := f.file(t, "src/go/main.go", "go")
	goSymbol := f.symbol(t, goFile, "Run", "main.Run", "go")
	f.imports(t, goFile, "fmt", "net/http", "github.com/gin-gonic/gin")
	f.edge(t, goFile, goSymbol, "fmt.Println")
	f.edge(t, goFile, goSymbol, "net/http.NewRequest")
	// A dependency path and this repo's own module path are the same shape, and
	// go.mod is never read: unknown, not external.
	f.edge(t, goFile, goSymbol, "github.com/gin-gonic/gin.New")

	pyFile := f.file(t, "src/py/main.py", "python")
	pySymbol := f.symbol(t, pyFile, "run", "main.run", "python")
	f.imports(t, pyFile, "os", "requests")
	f.edge(t, pyFile, pySymbol, "os.getcwd")
	f.edge(t, pyFile, pySymbol, "requests.get")

	javaFile := f.file(t, "src/java/Main.java", "java")
	javaSymbol := f.symbol(t, javaFile, "run", "Main.run", "java")
	f.imports(t, javaFile, "java.util.List", "com.acme.Widget")
	f.edge(t, javaFile, javaSymbol, "List.of")
	f.edge(t, javaFile, javaSymbol, "Widget.build")

	rustFile := f.file(t, "src/rust/main.rs", "rust")
	rustSymbol := f.symbol(t, rustFile, "run", "main.run", "rust")
	f.imports(t, rustFile, "std::collections::{HashMap, HashSet}", "serde::Serialize")
	f.edge(t, rustFile, rustSymbol, "HashMap::new")
	f.edge(t, rustFile, rustSymbol, "Serialize::serialize")

	tsFile := f.file(t, "src/ts/main.ts", "typescript")
	tsSymbol := f.symbol(t, tsFile, "run", "main.run", "typescript")
	f.imports(t, tsFile, "express", "node:fs", "./helpers")
	f.edge(t, tsFile, tsSymbol, "express.Router")
	f.edge(t, tsFile, tsSymbol, "fs.readFileSync")
	f.edge(t, tsFile, tsSymbol, "helpers.run")

	// A file with no imports of its own must not borrow another file's, however
	// familiar the name looks.
	bareFile := f.file(t, "src/go/bare.go", "go")
	bareSymbol := f.symbol(t, bareFile, "Bare", "main.Bare", "go")
	f.edge(t, bareFile, bareSymbol, "fmt.Println2")

	for dstName, want := range map[string]string{
		"fmt.Println":                  classify.Stdlib,
		"net/http.NewRequest":          classify.Stdlib,
		"github.com/gin-gonic/gin.New": classify.Unknown,
		"os.getcwd":                    classify.Stdlib,
		"requests.get":                 classify.Unknown,
		"List.of":                      classify.Stdlib,
		"Widget.build":                 classify.Unknown,
		"HashMap::new":                 classify.Stdlib,
		"Serialize::serialize":         classify.Unknown,
		"express.Router":               classify.External,
		"fs.readFileSync":              classify.Stdlib,
		"helpers.run":                  classify.Unknown,
		"fmt.Println2":                 classify.Unknown,
	} {
		f.assertClassification(t, dstName, want)
	}
}

// TestTargetClassification_ReceiverTruncatedMethodCallIsNotABuiltin is the
// end-to-end guard for the worst available false positive.
//
// The non-cgo Python adapter matches calls with a regex that keeps only the
// identifier before `(`, so `Model.objects.filter(x)` persists as dst_name
// `filter` -- which is also a Python builtin, with no project symbol claiming it.
// `edges.evidence` still holds the call text, so the classifier can see the
// receiver the parser dropped and abstain.
//
// Without this, the single most common ORM call in a Django repository would be
// reported as a language builtin, and the same repository would classify
// differently under the cgo build, whose adapter keeps the receiver.
func TestTargetClassification_ReceiverTruncatedMethodCallIsNotABuiltin(t *testing.T) {
	f := newClassifyFixture(t)

	caller := f.file(t, "src/py/views.py", "python")
	srcSymbol := f.symbol(t, caller, "list_active", "views.list_active", "python")

	f.edgeWithCallSite(t, caller, srcSymbol, "filter", "return Model.objects.filter(active=True)")
	f.edgeWithCallSite(t, caller, srcSymbol, "format", "label = template.format(name)")
	// A genuine unqualified builtin in the same file must still be reported.
	f.edgeWithCallSite(t, caller, srcSymbol, "sorted", "return sorted(items)")

	f.assertClassification(t, "filter", classify.Unknown)
	f.assertClassification(t, "format", classify.Unknown)
	f.assertClassification(t, "sorted", classify.Builtin)
}

// TestTargetClassification_BuiltinAbstentionSeesSoftDeletedSymbols pins the
// abstention set as a SUPERSET of the resolver's candidate set.
//
// Between MarkFilesDeletedBatch and PurgeDeletedFileGraphsForScan a file is
// flagged deleted but its symbol rows still exist, and resolveSymbolCandidates
// filters on neither `is_deleted` nor kind -- so those rows can still be the P3
// ambiguity that left the edge unresolved. If the classifier hid them it would
// report `builtin` for an ambiguity.
func TestTargetClassification_BuiltinAbstentionSeesSoftDeletedSymbols(t *testing.T) {
	f := newClassifyFixture(t)

	caller := f.file(t, "src/py/caller.py", "python")
	srcSymbol := f.symbol(t, caller, "run", "caller.run", "python")
	f.edge(t, caller, srcSymbol, "sorted")

	shadow := f.file(t, "src/py/shadow.py", "python")
	f.symbol(t, shadow, "sorted", "shadow.sorted", "python")
	other := f.file(t, "src/py/other.py", "python")
	f.symbol(t, other, "sorted", "other.sorted", "python")

	// Soft-delete both definers, as a scan does before purging.
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE files SET is_deleted = 1 WHERE id IN (?, ?)`, shadow, other); err != nil {
		t.Fatalf("soft delete error = %v", err)
	}

	f.assertClassification(t, "sorted", classify.Unknown)
}

// TestTargetClassification_UnknownForBareUnprovableName is the honest negative:
// nothing in the repo defines the name and no import can be linked to it.
func TestTargetClassification_UnknownForBareUnprovableName(t *testing.T) {
	f := newClassifyFixture(t)

	caller := f.file(t, "src/py/caller_f.py", "python")
	srcSymbol := f.symbol(t, caller, "run_f", "caller_f.run_f", "python")
	f.edge(t, caller, srcSymbol, "no_such_symbol_f")
	// Dotted is not external.
	f.edge(t, caller, srcSymbol, "foo.bar")

	f.assertClassification(t, "no_such_symbol_f", classify.Unknown)
	f.assertClassification(t, "foo.bar", classify.Unknown)
}

// TestTargetClassification_ResolvedEdgeIsProject covers the positive control and
// the lifecycle in one pass: a resolvable same-language call classifies as
// `project`, and the value is derived from the binding rather than from the name.
func TestTargetClassification_ResolvedEdgeIsProject(t *testing.T) {
	f := newClassifyFixture(t)

	caller := f.file(t, "src/py/caller_b.py", "python")
	srcSymbol := f.symbol(t, caller, "run_b", "caller_b.run_b", "python")
	edgeID := f.edge(t, caller, srcSymbol, "normalize_b")
	target := f.file(t, "src/py/helpers_b.py", "python")
	targetSymbol := f.symbol(t, target, "normalize_b", "helpers_b.normalize_b", "python")

	// Before resolution the same edge is only an unprovable name.
	f.assertClassification(t, "normalize_b", classify.Unknown)

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	dst, resolved := f.dstSymbolID(t, edgeID)
	if !resolved || dst != targetSymbol {
		t.Fatalf("dst_symbol_id = (%d, %v), want (%d, true)", dst, resolved, targetSymbol)
	}

	edge := f.assertClassification(t, "normalize_b", classify.Project)
	if edge.DstSymbolID == nil || *edge.DstSymbolID != targetSymbol {
		t.Fatalf("project edge dst_symbol_id = %v, want %d", edge.DstSymbolID, targetSymbol)
	}
	if edge.ResolutionStrategy == "" {
		t.Fatal("project edge carries no resolution strategy; P4 provenance regressed")
	}
}

// TestTargetClassification_ProjectDoesNotSurviveAnUnbind is the lifecycle
// invariant a persisted column would have needed a migration and two extra SQL
// clauses to hold. Deriving on read makes a stale `project` unrepresentable: the
// classification is read from the same row as the binding it describes.
func TestTargetClassification_ProjectDoesNotSurviveAnUnbind(t *testing.T) {
	f := newClassifyFixture(t)

	caller := f.file(t, "src/go/caller.go", "go")
	srcSymbol := f.symbol(t, caller, "Caller", "main.Caller", "go")
	edgeID := f.edge(t, caller, srcSymbol, "Target")
	target := f.file(t, "src/go/target.go", "go")
	f.symbol(t, target, "Target", "main.Target", "go")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if _, resolved := f.dstSymbolID(t, edgeID); !resolved {
		t.Fatal("fixture did not resolve; the unbind half of this test would be vacuous")
	}
	f.assertClassification(t, "Target", classify.Project)

	// Unbind exactly as a re-index of the destination's file does.
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE edges SET `+resolverClearResolutionSQL+` WHERE id = ?`, edgeID); err != nil {
		t.Fatalf("unbind error = %v", err)
	}

	edge := f.assertClassification(t, "Target", classify.Unknown)
	if edge.DstSymbolID != nil {
		t.Fatalf("unbound edge still carries dst_symbol_id = %d", *edge.DstSymbolID)
	}
}

// TestTargetClassification_EveryExportedEdgeCarriesAValue: the field is a total
// function of an edge, so no exported edge may be missing it. An empty value
// would serialise as an absent key and read as "not classified" -- a state that
// does not exist.
func TestTargetClassification_EveryExportedEdgeCarriesAValue(t *testing.T) {
	f := newClassifyFixture(t)

	goFile := f.file(t, "src/go/main.go", "go")
	goSymbol := f.symbol(t, goFile, "Run", "main.Run", "go")
	f.imports(t, goFile, "fmt")
	f.edge(t, goFile, goSymbol, "fmt.Println")
	f.edge(t, goFile, goSymbol, "len")
	f.edge(t, goFile, goSymbol, "whatever")

	// An edge whose file row carries no language must fail closed, not panic.
	unknownLang := f.file(t, "vendor/blob.xyz", "")
	unknownSymbol := f.symbol(t, unknownLang, "blob", "blob", "")
	f.edge(t, unknownLang, unknownSymbol, "opaque_call")

	edges, err := f.store.ExportEdgesPage(f.ctx, f.repoID, 1000, 0)
	if err != nil {
		t.Fatalf("ExportEdgesPage() error = %v", err)
	}
	if len(edges) != 4 {
		t.Fatalf("exported %d edges, want 4", len(edges))
	}
	valid := map[string]bool{
		classify.Project: true, classify.Builtin: true, classify.Stdlib: true,
		classify.External: true, classify.Unknown: true,
	}
	for _, edge := range edges {
		if !valid[edge.TargetClassification] {
			t.Errorf("edge %d (dst_name %q): target_classification = %q, not a defined value",
				edge.ID, edge.DstName, edge.TargetClassification)
		}
		if edge.DstSymbolID == nil && edge.TargetClassification == classify.Project {
			t.Errorf("edge %d (dst_name %q) classified project with no dst_symbol_id", edge.ID, edge.DstName)
		}
	}
}

// TestTargetClassification_PaginationDoesNotChangeAnyValue: evidence is batched
// per page, so a page boundary must not alter what an edge classifies as.
func TestTargetClassification_PaginationDoesNotChangeAnyValue(t *testing.T) {
	f := newClassifyFixture(t)

	goFile := f.file(t, "src/go/main.go", "go")
	goSymbol := f.symbol(t, goFile, "Run", "main.Run", "go")
	f.imports(t, goFile, "fmt")
	pyFile := f.file(t, "src/py/main.py", "python")
	pySymbol := f.symbol(t, pyFile, "run", "main.run", "python")
	f.imports(t, pyFile, "os")

	for _, dstName := range []string{"fmt.Println", "len", "whatever"} {
		f.edge(t, goFile, goSymbol, dstName)
	}
	for _, dstName := range []string{"os.getcwd", "sorted", "mystery"} {
		f.edge(t, pyFile, pySymbol, dstName)
	}

	whole, err := f.store.ExportEdgesPage(f.ctx, f.repoID, 1000, 0)
	if err != nil {
		t.Fatalf("ExportEdgesPage(whole) error = %v", err)
	}
	byID := map[int64]string{}
	for _, edge := range whole {
		byID[edge.ID] = edge.TargetClassification
	}

	for offset := 0; offset < len(whole); offset++ {
		page, err := f.store.ExportEdgesPage(f.ctx, f.repoID, 1, offset)
		if err != nil {
			t.Fatalf("ExportEdgesPage(limit=1, offset=%d) error = %v", offset, err)
		}
		if len(page) != 1 {
			t.Fatalf("page at offset %d returned %d edges, want 1", offset, len(page))
		}
		if got, want := page[0].TargetClassification, byID[page[0].ID]; got != want {
			t.Fatalf("edge %d (dst_name %q): paged classification %q != whole-repo %q",
				page[0].ID, page[0].DstName, got, want)
		}
	}
}

// TestTargetClassification_GraphSnapshotAgreesWithTheEdgePage: the second
// ExportEdge producer must not be a second opinion.
func TestTargetClassification_GraphSnapshotAgreesWithTheEdgePage(t *testing.T) {
	f := newClassifyFixture(t)

	goFile := f.file(t, "src/go/main.go", "go")
	goSymbol := f.symbol(t, goFile, "Run", "main.Run", "go")
	f.imports(t, goFile, "fmt")
	f.edge(t, goFile, goSymbol, "fmt.Println")
	f.edge(t, goFile, goSymbol, "len")
	f.edge(t, goFile, goSymbol, "whatever")

	page, err := f.store.ExportEdgesPage(f.ctx, f.repoID, 1000, 0)
	if err != nil {
		t.Fatalf("ExportEdgesPage() error = %v", err)
	}
	want := map[int64]string{}
	for _, edge := range page {
		want[edge.ID] = edge.TargetClassification
	}

	_, snapshotEdges, err := f.store.GraphSnapshot(f.ctx, f.repoID, "", 0)
	if err != nil {
		t.Fatalf("GraphSnapshot() error = %v", err)
	}
	if len(snapshotEdges) != len(want) {
		t.Fatalf("GraphSnapshot returned %d edges, want %d", len(snapshotEdges), len(want))
	}
	for _, edge := range snapshotEdges {
		if got := edge.TargetClassification; got != want[edge.ID] {
			t.Errorf("edge %d (dst_name %q): snapshot %q != page %q", edge.ID, edge.DstName, got, want[edge.ID])
		}
	}
}
