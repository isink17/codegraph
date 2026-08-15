package store

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// crossLangSpec describes a semantic cross-language fixture independently of the
// order its rows reach SQLite. Building the same spec under a different
// insertion order must produce the same cross-language links: row ids are an
// artefact of insertion, never evidence.
type crossLangSpec struct {
	files   []crossLangFile
	imports []crossLangImport
}

type crossLangFile struct {
	path     string
	language string
	symbols  []crossLangSymbol
}

type crossLangSymbol struct {
	name      string
	qualified string
	kind      string
}

type crossLangImport struct {
	fromPath string
	path     string
}

// build inserts the spec. order picks the insertion order of files and of the
// symbols inside each file: +1 forward, -1 reverse, 0 an interleave that is
// neither (odd indices first), so equivalent graphs get genuinely different
// symbol/file ids.
func (spec crossLangSpec) build(t *testing.T, f *gateFixture, order int) map[string]int64 {
	t.Helper()
	fileIDs := map[string]int64{}
	for _, idx := range crossLangOrder(len(spec.files), order) {
		file := spec.files[idx]
		id := f.file(t, file.path, file.language)
		fileIDs[file.path] = id
		for _, symIdx := range crossLangOrder(len(file.symbols), order) {
			sym := file.symbols[symIdx]
			kind := sym.kind
			if kind == "" {
				kind = "function"
			}
			f.symbolKind(t, id, sym.name, sym.qualified, kind, file.language)
		}
	}
	for _, idx := range crossLangOrder(len(spec.imports), order) {
		imp := spec.imports[idx]
		fileID, ok := fileIDs[imp.fromPath]
		if !ok {
			t.Fatalf("import from unknown file %q", imp.fromPath)
		}
		if _, err := f.store.db.ExecContext(f.ctx,
			`INSERT INTO file_imports(repo_id, file_id, import_path) VALUES(?, ?, ?)`,
			f.repoID, fileID, imp.path); err != nil {
			t.Fatalf("insert file_import(%s -> %s) error = %v", imp.fromPath, imp.path, err)
		}
	}
	return fileIDs
}

func crossLangOrder(n, order int) []int {
	idx := make([]int, 0, n)
	switch {
	case order > 0:
		for i := 0; i < n; i++ {
			idx = append(idx, i)
		}
	case order < 0:
		for i := n - 1; i >= 0; i-- {
			idx = append(idx, i)
		}
	default:
		for i := 1; i < n; i += 2 {
			idx = append(idx, i)
		}
		for i := 0; i < n; i += 2 {
			idx = append(idx, i)
		}
	}
	return idx
}

// crossLangLinks renders every cross_language_ref edge by semantic identity:
// source file and qualified name, destination file and qualified name, then the
// provenance the row carries. No row id appears, so two databases that hold the
// same semantic graph compare equal even though their ids differ.
func crossLangLinks(t *testing.T, f *gateFixture) []string {
	t.Helper()
	rows, err := f.store.db.QueryContext(f.ctx, `
		SELECT sf.path, src.qualified_name, src.language,
		       COALESCE(df.path, ''), COALESCE(dst.qualified_name, ''), COALESCE(dst.language, ''),
		       e.dst_name, e.evidence, e.resolution_strategy, e.resolution_confidence, ef.path
		FROM edges e
		JOIN symbols src ON src.id = e.src_symbol_id
		JOIN files sf ON sf.id = src.file_id
		JOIN files ef ON ef.id = e.file_id
		LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id
		LEFT JOIN files df ON df.id = dst.file_id
		WHERE e.repo_id = ? AND e.edge_kind = 'cross_language_ref'
	`, f.repoID)
	if err != nil {
		t.Fatalf("query cross-language links error = %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var srcPath, srcQName, srcLang, dstPath, dstQName, dstLang string
		var dstName, evidence, strategy, confidence, edgeFile string
		if err := rows.Scan(&srcPath, &srcQName, &srcLang, &dstPath, &dstQName, &dstLang,
			&dstName, &evidence, &strategy, &confidence, &edgeFile); err != nil {
			t.Fatalf("scan cross-language link error = %v", err)
		}
		out = append(out, fmt.Sprintf("%s:%s(%s) -> %s:%s(%s) name=%s evidence=%s strategy=%s confidence=%s edge_file=%s",
			srcPath, srcQName, srcLang, dstPath, dstQName, dstLang,
			dstName, evidence, strategy, confidence, edgeFile))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("cross-language rows error = %v", err)
	}
	sort.Strings(out)
	return out
}

func (f *gateFixture) resolveCrossLanguage(t *testing.T) int {
	t.Helper()
	created, err := f.store.ResolveCrossLanguageLinks(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("ResolveCrossLanguageLinks() error = %v", err)
	}
	return created
}

// bridgedSpec is the canonical post-fix positive control: a TypeScript file
// whose import path names a Python file, where each source symbol has exactly
// one same-named counterpart in the imported file. n such pairs must produce
// exactly n links -- no cap, no cross product.
func bridgedSpec(n int) crossLangSpec {
	tsFile := crossLangFile{path: "src/ts/client.ts", language: "typescript"}
	pyFile := crossLangFile{path: "src/shared/model.py", language: "python"}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Payload%03d", i)
		tsFile.symbols = append(tsFile.symbols, crossLangSymbol{name: name, qualified: "client." + name})
		pyFile.symbols = append(pyFile.symbols, crossLangSymbol{name: name, qualified: "model." + name})
	}
	return crossLangSpec{
		files:   []crossLangFile{tsFile, pyFile},
		imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "src/shared/model.ts"}},
	}
}

// TestCrossLanguageLinksInsertionOrderIndependent is the P10 principle applied
// to cross-language linking: the same semantic graph inserted forward, in
// reverse, and interleaved must yield the same links.
func TestCrossLanguageLinksInsertionOrderIndependent(t *testing.T) {
	spec := bridgedSpec(64)
	var want []string
	for _, tc := range []struct {
		name  string
		order int
	}{{"forward", 1}, {"reverse", -1}, {"interleaved", 0}} {
		f := newGateFixture(t)
		spec.build(t, f, tc.order)
		f.resolveCrossLanguage(t)
		got := crossLangLinks(t, f)
		if want == nil {
			want = got
			continue
		}
		if len(got) != len(want) {
			t.Fatalf("%s insertion order produced %d links, forward produced %d", tc.name, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s insertion order diverged at link %d:\n got  %s\n want %s", tc.name, i, got[i], want[i])
			}
		}
	}
}

// TestCrossLanguageLinksNoGlobalTruncation pins the count that the old per-pair
// `LIMIT 50` silently swallowed.
func TestCrossLanguageLinksNoGlobalTruncation(t *testing.T) {
	const n = 137
	f := newGateFixture(t)
	bridgedSpec(n).build(t, f, 1)
	created := f.resolveCrossLanguage(t)
	links := crossLangLinks(t, f)
	if created != n || len(links) != n {
		t.Fatalf("resolved %d links (reported %d), want %d for %d eligible facts", len(links), created, n, n)
	}
}

// TestCrossLanguageLinksEligibleCounts walks the old cap from below to well
// above it, so a reintroduced bound is caught at whichever size it appears.
func TestCrossLanguageLinksEligibleCounts(t *testing.T) {
	for _, n := range []int{0, 1, 10, 49, 50, 51, 100, 101} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			f := newGateFixture(t)
			bridgedSpec(n).build(t, f, 1)
			created := f.resolveCrossLanguage(t)
			if created != n {
				t.Fatalf("ResolveCrossLanguageLinks() created %d links, want %d", created, n)
			}
			if got := len(crossLangLinks(t, f)); got != n {
				t.Fatalf("stored %d cross_language_ref rows, want %d", got, n)
			}
		})
	}
}

// TestCrossLanguageLinksIdempotent runs the production entrypoint repeatedly
// against an unchanged graph: the link set, its provenance and its row count
// must not move, and a rerun must report no new links.
func TestCrossLanguageLinksIdempotent(t *testing.T) {
	f := newGateFixture(t)
	bridgedSpec(60).build(t, f, 1)

	first := f.resolveCrossLanguage(t)
	if first != 60 {
		t.Fatalf("first run created %d links, want 60", first)
	}
	want := crossLangLinks(t, f)
	for run := 2; run <= 5; run++ {
		created := f.resolveCrossLanguage(t)
		if created != 0 {
			t.Fatalf("run %d created %d links, want 0 against an unchanged graph", run, created)
		}
		got := crossLangLinks(t, f)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("run %d changed the link set:\n got  %d links\n want %d links", run, len(got), len(want))
		}
	}
}

// TestCrossLanguageLinksRequireImportEvidence is the bare-name defect: before
// the fix these two unrelated symbols were wired together because their names
// happened to be equal.
func TestCrossLanguageLinksRequireImportEvidence(t *testing.T) {
	f := newGateFixture(t)
	crossLangSpec{files: []crossLangFile{
		{path: "src/py/service.py", language: "python", symbols: []crossLangSymbol{
			{name: "RenderReport", qualified: "service.RenderReport"},
		}},
		{path: "src/ts/other.ts", language: "typescript", symbols: []crossLangSymbol{
			{name: "RenderReport", qualified: "other.RenderReport"},
		}},
	}}.build(t, f, 1)

	if created := f.resolveCrossLanguage(t); created != 0 {
		t.Fatalf("a shared bare name with no import bridge created %d links, want 0", created)
	}
	if links := crossLangLinks(t, f); len(links) != 0 {
		t.Fatalf("stored %d links from name coincidence alone:\n%s", len(links), strings.Join(links, "\n"))
	}
}

// TestCrossLanguageLinksAbstainOnAmbiguity covers every shape where the
// evidence stops short of naming one target. Each must write nothing rather
// than pick a winner.
func TestCrossLanguageLinksAbstainOnAmbiguity(t *testing.T) {
	tests := []struct {
		name string
		spec crossLangSpec
	}{
		{
			// Two same-named candidates inside the imported file.
			name: "two same-named targets in the imported file",
			spec: crossLangSpec{
				files: []crossLangFile{
					{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "client.Encode"},
					}},
					{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "model.Encode", kind: "function"},
						{name: "Encode", qualified: "model.Codec.Encode", kind: "method"},
					}},
				},
				imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "src/shared/model"}},
			},
		},
		{
			// No name correspondence and more than one candidate: the bridge is
			// a file-level fact and cannot choose between them.
			name: "no name match and several candidates",
			spec: crossLangSpec{
				files: []crossLangFile{
					{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{
						{name: "SendPayload", qualified: "client.SendPayload"},
					}},
					{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{
						{name: "BuildPayload", qualified: "model.BuildPayload"},
						{name: "ParsePayload", qualified: "model.ParsePayload"},
					}},
				},
				imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "src/shared/model"}},
			},
		},
		{
			// One specifier, two foreign files answering it.
			name: "specifier resolves to two foreign files",
			spec: crossLangSpec{
				files: []crossLangFile{
					{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "client.Encode"},
					}},
					{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "model.Encode"},
					}},
					{path: "src/shared/model.rb", language: "ruby", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "Model.Encode"},
					}},
				},
				imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "src/shared/model"}},
			},
		},
		{
			// A same-language file answers the specifier, so the cross-language
			// claim contradicts the evidence: the native resolver owns it.
			name: "same-language file answers the specifier",
			spec: crossLangSpec{
				files: []crossLangFile{
					{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "client.Encode"},
					}},
					{path: "src/shared/model.ts", language: "typescript", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "model.Encode"},
					}},
					{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "model.Encode"},
					}},
				},
				imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "src/shared/model"}},
			},
		},
		{
			// A dotted module specifier is not a file path: reading its dots as
			// an extension used to match a root-level app.ts.
			name: "dotted module specifier is not a path",
			spec: crossLangSpec{
				files: []crossLangFile{
					{path: "app.py", language: "python", symbols: []crossLangSymbol{
						{name: "Serve", qualified: "app.Serve"},
					}},
					{path: "app.ts", language: "typescript", symbols: []crossLangSymbol{
						{name: "Serve", qualified: "app.Serve"},
					}},
				},
				imports: []crossLangImport{{fromPath: "app.py", path: "app.models"}},
			},
		},
		{
			// Relative specifiers resolve against the importing file's
			// directory, so this one names src/ts/model.*, which does not exist.
			name: "relative specifier resolves outside the target directory",
			spec: crossLangSpec{
				files: []crossLangFile{
					{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "client.Encode"},
					}},
					{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "model.Encode"},
					}},
				},
				imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "./model"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			tc.spec.build(t, f, 1)
			if created := f.resolveCrossLanguage(t); created != 0 {
				t.Fatalf("created %d links, want 0 (abstain)", created)
			}
			if links := crossLangLinks(t, f); len(links) != 0 {
				t.Fatalf("stored %d links, want none:\n%s", len(links), strings.Join(links, "\n"))
			}
		})
	}
}

// TestCrossLanguageLinksPositiveControls pins the two strategies that must keep
// working, each with the provenance that states which evidence bound it.
func TestCrossLanguageLinksPositiveControls(t *testing.T) {
	tests := []struct {
		name string
		spec crossLangSpec
		want string
	}{
		{
			// The bridge names the file, the unique shared name names the symbol
			// -- even though an unrelated same-named foreign symbol exists
			// elsewhere, and even though the target file holds other symbols.
			name: "unique shared name inside the bridge",
			spec: crossLangSpec{
				files: []crossLangFile{
					{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "client.Encode"},
					}},
					{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "model.Encode"},
						{name: "Decode", qualified: "model.Decode"},
					}},
					{path: "src/rb/decoy.rb", language: "ruby", symbols: []crossLangSymbol{
						{name: "Encode", qualified: "Decoy.Encode"},
					}},
				},
				imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "src/shared/model.ts"}},
			},
			want: "src/ts/client.ts:client.Encode(typescript) -> src/shared/model.py:model.Encode(python) " +
				"name=Encode evidence=shared_name:typescript→python strategy=cross_language_shared_name " +
				"confidence=low edge_file=src/ts/client.ts",
		},
		{
			// No name correspondence, but the imported file holds exactly one
			// eligible symbol, so the bridge alone identifies the target.
			name: "single-symbol target file",
			spec: crossLangSpec{
				files: []crossLangFile{
					{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{
						{name: "SendPayload", qualified: "client.SendPayload"},
					}},
					{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{
						{name: "BuildPayload", qualified: "model.BuildPayload"},
					}},
				},
				imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "./../shared/model.ts"}},
			},
			want: "src/ts/client.ts:client.SendPayload(typescript) -> src/shared/model.py:model.BuildPayload(python) " +
				"name=BuildPayload evidence=import_path:typescript→python strategy=cross_language_import_path " +
				"confidence=low edge_file=src/ts/client.ts",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			tc.spec.build(t, f, 1)
			if created := f.resolveCrossLanguage(t); created != 1 {
				t.Fatalf("created %d links, want 1", created)
			}
			links := crossLangLinks(t, f)
			if len(links) != 1 || links[0] != tc.want {
				t.Fatalf("link set:\n got  %s\n want %s", strings.Join(links, "\n"), tc.want)
			}
		})
	}
}

// TestCrossLanguageLinksRetireLostEvidence covers the lifecycle: a link exists
// only while the evidence does. Each step reruns the production entrypoint,
// which is the only thing that maintains these edges.
func TestCrossLanguageLinksRetireLostEvidence(t *testing.T) {
	t.Run("import evidence removed", func(t *testing.T) {
		f := newGateFixture(t)
		bridgedSpec(4).build(t, f, 1)
		if created := f.resolveCrossLanguage(t); created != 4 {
			t.Fatalf("created %d links, want 4", created)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM file_imports WHERE repo_id = ?`, f.repoID); err != nil {
			t.Fatalf("delete file_imports error = %v", err)
		}
		if created := f.resolveCrossLanguage(t); created != 0 {
			t.Fatalf("created %d links after the bridge was removed, want 0", created)
		}
		if links := crossLangLinks(t, f); len(links) != 0 {
			t.Fatalf("%d links outlived their import evidence:\n%s", len(links), strings.Join(links, "\n"))
		}
	})

	t.Run("target becomes ambiguous", func(t *testing.T) {
		f := newGateFixture(t)
		ids := bridgedSpec(1).build(t, f, 1)
		if created := f.resolveCrossLanguage(t); created != 1 {
			t.Fatalf("created %d links, want 1", created)
		}
		// A second same-named symbol appears in the imported file: the link that
		// was unique is now a guess, so it must go.
		f.symbolKind(t, ids["src/shared/model.py"], "Payload000", "model.Codec.Payload000", "method", "python")
		if created := f.resolveCrossLanguage(t); created != 0 {
			t.Fatalf("created %d links after the target became ambiguous, want 0", created)
		}
		if links := crossLangLinks(t, f); len(links) != 0 {
			t.Fatalf("%d links survived an ambiguous target:\n%s", len(links), strings.Join(links, "\n"))
		}
	})

	t.Run("ambiguity resolved by deleting the duplicate", func(t *testing.T) {
		f := newGateFixture(t)
		ids := bridgedSpec(1).build(t, f, 1)
		dup := f.symbolKind(t, ids["src/shared/model.py"], "Payload000", "model.Codec.Payload000", "method", "python")
		if created := f.resolveCrossLanguage(t); created != 0 {
			t.Fatalf("created %d links while ambiguous, want 0", created)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id = ?`, dup); err != nil {
			t.Fatalf("delete duplicate symbol error = %v", err)
		}
		if created := f.resolveCrossLanguage(t); created != 1 {
			t.Fatalf("created %d links once the ambiguity was gone, want 1", created)
		}
	})

	t.Run("target file reindexed leaves no dangling binding", func(t *testing.T) {
		f := newGateFixture(t)
		bridgedSpec(3).build(t, f, 1)
		if created := f.resolveCrossLanguage(t); created != 3 {
			t.Fatalf("created %d links, want 3", created)
		}
		// Reindexing the destination file runs the same graph-delete path an
		// incremental update uses (deleteFileGraphsBatch), which is where an
		// inbound binding is invalidated.
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 0, "src/shared/model.py", "python",
			0, 0, "reindexed", graph.ParsedFile{}); err != nil {
			t.Fatalf("ReplaceFileGraph() error = %v", err)
		}
		var dangling, unbound int
		if err := f.store.db.QueryRowContext(f.ctx, `
			SELECT
				COALESCE(SUM(CASE WHEN e.dst_symbol_id IS NOT NULL
					AND NOT EXISTS (SELECT 1 FROM symbols s WHERE s.id = e.dst_symbol_id) THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN e.dst_symbol_id IS NULL THEN 1 ELSE 0 END), 0)
			FROM edges e WHERE e.repo_id = ? AND e.edge_kind = 'cross_language_ref'
		`, f.repoID).Scan(&dangling, &unbound); err != nil {
			t.Fatalf("query cross-language edge state error = %v", err)
		}
		if dangling != 0 {
			t.Fatalf("%d cross_language_ref edges point at deleted symbols", dangling)
		}
		if unbound != 0 {
			t.Fatalf("%d unbound cross_language_ref edges survived; the implicit resolver would rebind them", unbound)
		}
	})
}

// TestCrossLanguageLinksMetadataCoherent is the P4 invariant on this write path:
// a bound row carries both a cross-language strategy and its confidence, and no
// row carries one without the other.
func TestCrossLanguageLinksMetadataCoherent(t *testing.T) {
	f := newGateFixture(t)
	bridgedSpec(8).build(t, f, 1)
	f.resolveCrossLanguage(t)

	rows, err := f.store.db.QueryContext(f.ctx, `
		SELECT dst_symbol_id IS NOT NULL, dst_name, resolution_strategy, resolution_confidence, line
		FROM edges WHERE repo_id = ? AND edge_kind = 'cross_language_ref'
	`, f.repoID)
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var bound bool
		var dstName, strategy, confidence string
		var line int
		if err := rows.Scan(&bound, &dstName, &strategy, &confidence, &line); err != nil {
			t.Fatalf("scan error = %v", err)
		}
		seen++
		if !bound {
			t.Fatal("cross_language_ref row is unbound")
		}
		if dstName == "" {
			t.Fatal("bound cross_language_ref row has an empty dst_name")
		}
		switch strategy {
		case ResolutionStrategyCrossLanguageSharedName, ResolutionStrategyCrossLanguageImportPath:
		default:
			t.Fatalf("cross_language_ref row carries strategy %q", strategy)
		}
		if confidence != ResolutionConfidenceLow {
			t.Fatalf("strategy %q carries confidence %q, want %q", strategy, confidence, ResolutionConfidenceLow)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error = %v", err)
	}
	if seen != 8 {
		t.Fatalf("inspected %d rows, want 8", seen)
	}
}

// TestCrossLanguageLinksRepoIsolation: one repository's evidence never links
// another's symbols, and resolving one repository leaves the other untouched.
func TestCrossLanguageLinksRepoIsolation(t *testing.T) {
	f := newGateFixture(t)
	bridgedSpec(3).build(t, f, 1)

	other, err := f.store.UpsertRepo(f.ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	neighbour := &gateFixture{ctx: f.ctx, store: f.store, repoID: other.ID}
	bridgedSpec(3).build(t, neighbour, -1)

	if created := f.resolveCrossLanguage(t); created != 3 {
		t.Fatalf("created %d links in the first repository, want 3", created)
	}
	if got := len(crossLangLinks(t, neighbour)); got != 0 {
		t.Fatalf("resolving one repository wrote %d links into the other", got)
	}
	// Both repositories hold the same paths and names, so a boundary crossing
	// would be invisible in the rendered link text: compare the endpoints'
	// repo_id columns instead.
	var crossed int
	if err := f.store.db.QueryRowContext(f.ctx, `
		SELECT COUNT(*) FROM edges e
		JOIN symbols src ON src.id = e.src_symbol_id
		JOIN symbols dst ON dst.id = e.dst_symbol_id
		WHERE e.edge_kind = 'cross_language_ref'
		AND (src.repo_id <> e.repo_id OR dst.repo_id <> e.repo_id OR src.repo_id <> dst.repo_id)
	`).Scan(&crossed); err != nil {
		t.Fatalf("count cross-repository links error = %v", err)
	}
	if crossed != 0 {
		t.Fatalf("%d cross_language_ref edges join symbols of different repositories", crossed)
	}
	if created := neighbour.resolveCrossLanguage(t); created != 3 {
		t.Fatalf("created %d links in the second repository, want 3", created)
	}
	if got := len(crossLangLinks(t, f)); got != 3 {
		t.Fatalf("resolving the second repository changed the first to %d links, want 3", got)
	}
}

// TestCrossLanguageLinksRollbackOnWriteFailure: the pass is one transaction, so
// a failing write leaves the graph as it was rather than half of a new link set.
// The seam is a trigger that aborts one row of the second insert batch, after
// the first has already been written inside the transaction.
func TestCrossLanguageLinksRollbackOnWriteFailure(t *testing.T) {
	const n = crossLangEdgeValuesBatchRows + 22
	f := newGateFixture(t)
	bridgedSpec(n).build(t, f, 1)

	victim := fmt.Sprintf("Payload%03d", n-1)
	if _, err := f.store.db.ExecContext(f.ctx, `
		CREATE TRIGGER xlang_inject_failure BEFORE INSERT ON edges
		WHEN NEW.edge_kind = 'cross_language_ref' AND NEW.dst_name = '`+victim+`'
		BEGIN SELECT RAISE(ABORT, 'injected cross-language write failure'); END
	`); err != nil {
		t.Fatalf("create failure trigger error = %v", err)
	}

	created, err := f.store.ResolveCrossLanguageLinks(f.ctx, f.repoID)
	if err == nil {
		t.Fatal("ResolveCrossLanguageLinks() succeeded with an aborting trigger installed")
	}
	if created != 0 {
		t.Fatalf("a failed pass reported %d links created, want 0", created)
	}
	if links := crossLangLinks(t, f); len(links) != 0 {
		t.Fatalf("a failed pass left %d links behind, want none (rollback)", len(links))
	}

	// With the seam removed the same pass writes the complete set, so the
	// rollback above cost nothing but the failed attempt.
	if _, err := f.store.db.ExecContext(f.ctx, `DROP TRIGGER xlang_inject_failure`); err != nil {
		t.Fatalf("drop failure trigger error = %v", err)
	}
	if created := f.resolveCrossLanguage(t); created != n {
		t.Fatalf("after the failure was removed the pass created %d links, want %d", created, n)
	}
}

// TestCrossLanguageLinksAuditClean runs the production audit checks over a graph
// this pass produced: valid explicit links must trip nothing, including the
// dangling and metadata checks the write path could break.
func TestCrossLanguageLinksAuditClean(t *testing.T) {
	f := newGateFixture(t)
	bridgedSpec(12).build(t, f, 1)
	if created := f.resolveCrossLanguage(t); created != 12 {
		t.Fatalf("created %d links, want 12", created)
	}
	caps, err := f.store.GraphAuditCapabilitiesFor(f.ctx)
	if err != nil {
		t.Fatalf("GraphAuditCapabilitiesFor() error = %v", err)
	}
	for _, check := range []EdgeAuditCheck{
		EdgeCheckImplicitCrossLanguage,
		EdgeCheckCrossLanguageImplicitStrategy,
		EdgeCheckInvalidResolutionMetadata,
		EdgeCheckDanglingSource,
		EdgeCheckDanglingTarget,
	} {
		res, err := f.store.RunEdgeAuditCheck(f.ctx, f.repoID, check, caps, 5)
		if err != nil {
			t.Fatalf("RunEdgeAuditCheck(%v) error = %v", check, err)
		}
		if res.Count != 0 {
			t.Fatalf("check %v flagged %d rows of a valid cross-language link set", check, res.Count)
		}
	}
}

// TestCrossLanguageLinksAuditFlagsRebind is the audit half of the reindex
// lifecycle: the state the audit reports as a defect -- a cross-language row
// rebound by an implicit same-language strategy -- is exactly what an unbound
// cross-language row decays into, so the write path must not leave one behind.
func TestCrossLanguageLinksAuditFlagsRebind(t *testing.T) {
	f := newGateFixture(t)
	bridgedSpec(2).build(t, f, 1)
	f.resolveCrossLanguage(t)
	caps, err := f.store.GraphAuditCapabilitiesFor(f.ctx)
	if err != nil {
		t.Fatalf("GraphAuditCapabilitiesFor() error = %v", err)
	}

	// Hand-build the decayed row the old unbind path produced, and confirm the
	// audit still reports it. This is the guard on the check itself: it must not
	// be weakened to make the rest of this file pass.
	var src, dst int64
	if err := f.store.db.QueryRowContext(f.ctx, `
		SELECT s.id, d.id FROM symbols s, symbols d
		WHERE s.repo_id = ? AND s.language = 'typescript' AND d.language = 'typescript' AND s.id <> d.id
		LIMIT 1
	`, f.repoID).Scan(&src, &dst); err != nil {
		t.Fatalf("pick same-language pair error = %v", err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
		                  resolution_strategy, resolution_confidence)
		SELECT ?, ?, ?, 'Payload000', 'cross_language_ref', 'shared_name:typescript→python', file_id, 0, ?, ?
		FROM symbols WHERE id = ?
	`, f.repoID, src, dst, ResolutionStrategyExactName, ResolutionConfidenceHigh, src); err != nil {
		t.Fatalf("insert decayed row error = %v", err)
	}
	res, err := f.store.RunEdgeAuditCheck(f.ctx, f.repoID, EdgeCheckCrossLanguageImplicitStrategy, caps, 5)
	if err != nil {
		t.Fatalf("RunEdgeAuditCheck() error = %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("audit flagged %d rebound cross-language rows, want 1", res.Count)
	}

	// The pass owns this edge kind, so its rebuild also retires the decayed row.
	if _, err := f.store.ResolveCrossLanguageLinks(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveCrossLanguageLinks() error = %v", err)
	}
	res, err = f.store.RunEdgeAuditCheck(f.ctx, f.repoID, EdgeCheckCrossLanguageImplicitStrategy, caps, 5)
	if err != nil {
		t.Fatalf("RunEdgeAuditCheck() error = %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("%d rebound cross-language rows survived the rebuild", res.Count)
	}
}

// TestCrossLanguageLinksRepeatedRunsStable is the determinism repetition: the
// same fixture, built and resolved 20 times, must produce one link set.
func TestCrossLanguageLinksRepeatedRunsStable(t *testing.T) {
	spec := bridgedSpec(51)
	var want string
	for run := 0; run < 20; run++ {
		f := newGateFixture(t)
		spec.build(t, f, run%3-1)
		f.resolveCrossLanguage(t)
		got := strings.Join(crossLangLinks(t, f), "\n")
		if run == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("run %d produced a different link set", run)
		}
	}
}

// TestCrossLanguageLinksImportPathNeedsBothEnds: a file-level import does not
// say which of several source symbols uses the imported file, so the
// import-path strategy binds only when each side has one eligible symbol.
// Otherwise one import fact would fan out into an edge per source symbol.
func TestCrossLanguageLinksImportPathNeedsBothEnds(t *testing.T) {
	tsFile := crossLangFile{path: "src/ts/client.ts", language: "typescript"}
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("Send%03d", i)
		tsFile.symbols = append(tsFile.symbols, crossLangSymbol{name: name, qualified: "client." + name})
	}
	f := newGateFixture(t)
	crossLangSpec{
		files: []crossLangFile{tsFile, {
			path: "src/shared/model.py", language: "python",
			symbols: []crossLangSymbol{{name: "OnlyOne", qualified: "model.OnlyOne"}},
		}},
		imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "src/shared/model"}},
	}.build(t, f, 1)

	if created := f.resolveCrossLanguage(t); created != 0 {
		t.Fatalf("one import fact created %d links from 25 source symbols, want 0", created)
	}
}

// TestCrossLanguageLinksAbstainOnFanIn: two imports each answering the same
// source symbol's name are two candidates for one claim, and arriving through
// different imports does not stop that from being ambiguous.
func TestCrossLanguageLinksAbstainOnFanIn(t *testing.T) {
	f := newGateFixture(t)
	crossLangSpec{
		files: []crossLangFile{
			{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{
				{name: "Encode", qualified: "client.Encode"},
				{name: "Unrelated", qualified: "client.Unrelated"},
			}},
			{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{
				{name: "Encode", qualified: "model.Encode"},
			}},
			{path: "src/rb/codec.rb", language: "ruby", symbols: []crossLangSymbol{
				{name: "Encode", qualified: "Codec.Encode"},
			}},
		},
		imports: []crossLangImport{
			{fromPath: "src/ts/client.ts", path: "src/shared/model"},
			{fromPath: "src/ts/client.ts", path: "src/rb/codec"},
		},
	}.build(t, f, 1)

	if created := f.resolveCrossLanguage(t); created != 0 {
		t.Fatalf("a source symbol answered by two bridges created %d links, want 0", created)
	}
}

// TestCrossLanguageLinksReconcileDuplicatePair: a database written before this
// pass became a rebuild can hold the same pair twice. The reconciliation must
// retire the copy, or every rerun would report a change forever.
func TestCrossLanguageLinksReconcileDuplicatePair(t *testing.T) {
	f := newGateFixture(t)
	bridgedSpec(2).build(t, f, 1)
	if created := f.resolveCrossLanguage(t); created != 2 {
		t.Fatalf("created %d links, want 2", created)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
		                  resolution_strategy, resolution_confidence)
		SELECT repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
		       resolution_strategy, resolution_confidence
		FROM edges WHERE repo_id = ? AND edge_kind = 'cross_language_ref'
	`, f.repoID); err != nil {
		t.Fatalf("clone rows error = %v", err)
	}
	if got := len(crossLangLinks(t, f)); got != 4 {
		t.Fatalf("fixture holds %d rows, want 4 before reconciliation", got)
	}
	if created := f.resolveCrossLanguage(t); created != 0 {
		t.Fatalf("reconciling duplicates reported %d new links, want 0", created)
	}
	if got := len(crossLangLinks(t, f)); got != 2 {
		t.Fatalf("after reconciliation %d rows remain, want 2", got)
	}
	if created := f.resolveCrossLanguage(t); created != 0 {
		t.Fatalf("the rerun after reconciliation created %d links, want 0", created)
	}
}

// TestCrossLanguageLinksTempTableDeletePath covers the other graph-delete
// implementation: above sqliteInClauseBatchSize files the delete runs from a
// temp table, and that copy of the cross-language DELETE has its own SQL text.
func TestCrossLanguageLinksTempTableDeletePath(t *testing.T) {
	f := newGateFixture(t)
	ids := bridgedSpec(2).build(t, f, 1)
	if created := f.resolveCrossLanguage(t); created != 2 {
		t.Fatalf("created %d links, want 2", created)
	}
	fileIDs := []int64{ids["src/shared/model.py"]}
	for i := 0; i <= sqliteInClauseBatchSize; i++ {
		fileIDs = append(fileIDs, f.file(t, fmt.Sprintf("src/filler/f%04d.go", i), "go"))
	}
	if len(fileIDs) <= sqliteInClauseBatchSize {
		t.Fatalf("fixture has %d file ids, need more than %d to reach the temp-table path",
			len(fileIDs), sqliteInClauseBatchSize)
	}

	tx, err := f.store.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := deleteFileGraphsBatch(f.ctx, tx, f.repoID, fileIDs, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("deleteFileGraphsBatch() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	var total, unbound int
	if err := f.store.db.QueryRowContext(f.ctx, `
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN dst_symbol_id IS NULL THEN 1 ELSE 0 END), 0)
		FROM edges WHERE repo_id = ? AND edge_kind = 'cross_language_ref'
	`, f.repoID).Scan(&total, &unbound); err != nil {
		t.Fatalf("count cross-language rows error = %v", err)
	}
	if total != 0 || unbound != 0 {
		t.Fatalf("temp-table delete left %d cross-language rows (%d unbound), want none", total, unbound)
	}
}
