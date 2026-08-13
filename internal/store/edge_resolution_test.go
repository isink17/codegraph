package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Coverage for P4 edge resolution provenance and confidence.
//
// Two properties are under test:
//
//  1. Every bind persists the strategy that actually performed it, and the
//     confidence tier that strategy maps to. Every unbind clears both.
//  2. P4 is descriptive only: the same edges resolve to the same destinations
//     as before. The scenarios below therefore assert the destination *and* the
//     metadata, so a change that improved the explanation by moving the edge
//     would fail here rather than pass quietly.

// resolutionMetadata reads the persisted explanation for one edge.
func (f *gateFixture) resolutionMetadata(t *testing.T, edgeID int64) (strategy, confidence string) {
	t.Helper()
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT resolution_strategy, resolution_confidence FROM edges WHERE id = ?`, edgeID,
	).Scan(&strategy, &confidence); err != nil {
		t.Fatalf("QueryRow(resolution metadata) error = %v", err)
	}
	return strategy, confidence
}

// symbolWithContainer inserts a symbol with an explicit kind and container, so a
// strategy that keys on the container alone can be isolated from the bare-name
// strategy (which only considers callable/type kinds).
func (f *gateFixture) symbolWithContainer(t *testing.T, fileID int64, name, qualified, kind, container, language string) int64 {
	t.Helper()
	id, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, fileID, name, qualified, kind, container, language)
	if err != nil {
		t.Fatalf("insertTestSymbolKind(%s, %q, %q) error = %v", qualified, kind, language, err)
	}
	return id
}

// assertResolvedWith asserts an edge bound to wantDst and explains itself with
// wantStrategy at that strategy's registered confidence.
func (f *gateFixture) assertResolvedWith(t *testing.T, edgeID, wantDst int64, wantStrategy string) {
	t.Helper()
	gotDst, ok := f.dstSymbolID(t, edgeID)
	if !ok {
		t.Fatalf("edge %d unresolved, want dst=%d via %s", edgeID, wantDst, wantStrategy)
	}
	if gotDst != wantDst {
		t.Fatalf("edge %d dst_symbol_id = %d, want %d", edgeID, gotDst, wantDst)
	}
	strategy, confidence := f.resolutionMetadata(t, edgeID)
	if strategy != wantStrategy {
		t.Fatalf("edge %d resolution_strategy = %q, want %q", edgeID, strategy, wantStrategy)
	}
	if want := resolutionConfidenceFor(wantStrategy); confidence != want {
		t.Fatalf("edge %d resolution_confidence = %q, want %q", edgeID, confidence, want)
	}
}

// assertUnresolvedWithoutMetadata asserts an edge is unbound and carries no
// leftover explanation. An unresolved edge that still names a strategy would be
// claiming evidence for a destination it does not have.
func (f *gateFixture) assertUnresolvedWithoutMetadata(t *testing.T, edgeID int64) {
	t.Helper()
	if dst, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("edge %d resolved to %d, want unresolved", edgeID, dst)
	}
	strategy, confidence := f.resolutionMetadata(t, edgeID)
	if strategy != "" || confidence != "" {
		t.Fatalf("edge %d unresolved but carries metadata (%q, %q), want both empty", edgeID, strategy, confidence)
	}
}

// TestConfidenceMappingCoversEveryStrategy fails if a strategy constant ships
// without a registered confidence tier. resolutionConfidenceFor panics on an
// unregistered value, so without this test the first failure would be a panic
// mid-index rather than a red build.
func TestConfidenceMappingCoversEveryStrategy(t *testing.T) {
	all := []string{
		ResolutionStrategyExactQualified,
		ResolutionStrategyExactName,
		ResolutionStrategyReceiverMethod,
		ResolutionStrategySlashSuffix,
		ResolutionStrategyDotTail2,
		ResolutionStrategyDotTail3,
		ResolutionStrategyDotSuffix,
		ResolutionStrategyBareTail,
		ResolutionStrategyCrossLanguageSharedName,
		ResolutionStrategyCrossLanguageImportPath,
	}
	if len(all) != len(resolutionConfidenceByStrategy) {
		t.Fatalf("strategy constants = %d, registered confidences = %d; every strategy needs exactly one tier", len(all), len(resolutionConfidenceByStrategy))
	}
	valid := map[string]bool{
		ResolutionConfidenceHigh:   true,
		ResolutionConfidenceMedium: true,
		ResolutionConfidenceLow:    true,
	}
	// Both value sets are interpolated into statement text as single-quoted
	// literals (resolverSetResolvedSQL, binderSetResolvedSQL). They are package
	// constants, never caller input, but that is a claim a comment cannot
	// enforce -- so require the shape that makes the interpolation safe.
	safe := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, strategy := range all {
		if !safe.MatchString(strategy) {
			t.Fatalf("strategy %q is not a bare lowercase identifier; it is interpolated into SQL as a literal", strategy)
		}
		confidence := resolutionConfidenceFor(strategy)
		if !valid[confidence] {
			t.Fatalf("strategy %q maps to %q, which is not one of the three tiers", strategy, confidence)
		}
		if !safe.MatchString(confidence) {
			t.Fatalf("confidence %q is not a bare lowercase identifier; it is interpolated into SQL as a literal", confidence)
		}
	}
	for _, strategy := range binderStrategies {
		if _, ok := resolutionConfidenceByStrategy[strategy]; !ok {
			t.Fatalf("binder strategy %q has no registered confidence", strategy)
		}
	}
}

// TestBinderDecodesStrategyPerRow guards the Go-side binder's compact
// `strategy_rank` encoding: one resolve call must be able to bind edges at
// different evidence levels and give each its own provenance. A regression that
// hoisted the rank out of the per-row loop -- deriving it once per batch -- would
// pass every single-edge case and fail only here.
func TestBinderDecodesStrategyPerRow(t *testing.T) {
	f := newGateFixture(t)
	defs := f.file(t, "src/py/defs.py", "python")
	qualifiedDst := f.symbol(t, defs, "load_config", "config.load_config", "python")
	tailDst := f.symbol(t, defs, "render_report", "some.other.path.render_report", "python")

	caller := f.file(t, "src/py/caller.py", "python")
	src := f.symbol(t, caller, "caller", "caller", "python")
	// Matches a full qualified_name -> rank of exact_qualified.
	qualifiedEdge := f.edge(t, caller, src, "config.load_config")
	// Matches nothing qualified; only its dot-tail names a symbol -> rank of
	// bare_tail.
	tailEdge := f.edge(t, caller, src, "widgets.render_report")

	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/py/caller.py"}); err != nil {
		t.Fatalf("ResolveEdgesForPaths() error = %v", err)
	}

	f.assertResolvedWith(t, qualifiedEdge, qualifiedDst, ResolutionStrategyExactQualified)
	f.assertResolvedWith(t, tailEdge, tailDst, ResolutionStrategyBareTail)
}

// TestResolveEdgesRecordsStrategyPerEvidenceLevel pins the provenance the
// repo-wide resolver reports for each of its strategies, on graphs where only
// that strategy can bind the edge.
func TestResolveEdgesRecordsStrategyPerEvidenceLevel(t *testing.T) {
	cases := []struct {
		name         string
		build        func(t *testing.T, f *gateFixture) (edgeID, wantDst int64)
		wantStrategy string
	}{
		{
			// The call names the destination's full qualified_name. Nothing is
			// discarded to make the match.
			name: "exact_qualified",
			build: func(t *testing.T, f *gateFixture) (int64, int64) {
				defs := f.file(t, "src/py/pkg_a.py", "python")
				want := f.symbol(t, defs, "foo", "pkg_a.foo", "python")
				other := f.file(t, "src/py/pkg_b.py", "python")
				f.symbol(t, other, "foo", "pkg_b.foo", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "pkg_a.foo"), want
			},
			wantStrategy: ResolutionStrategyExactQualified,
		},
		{
			// The call names a bare name that exactly one same-language symbol
			// of a callable kind owns.
			name: "exact_name",
			build: func(t *testing.T, f *gateFixture) (int64, int64) {
				defs := f.file(t, "src/py/config.py", "python")
				want := f.symbol(t, defs, "load_config", "config.load_config", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "load_config"), want
			},
			wantStrategy: ResolutionStrategyExactName,
		},
		{
			// The only symbol carrying this bare name is a container member of a
			// kind the bare-name strategy does not consider (resolverBareNameKindsSQL
			// covers callables and types, not properties), so the receiver
			// strategy is the one that binds it. Its container came from the
			// destination -- the call site never named a receiver -- which is why
			// receiver_method sits a tier below exact_name.
			name: "receiver_method",
			build: func(t *testing.T, f *gateFixture) (int64, int64) {
				defs := f.file(t, "src/py/service.py", "python")
				want := f.symbolWithContainer(t, defs, "handle", "Service.handle", "property", "Service", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "handle"), want
			},
			wantStrategy: ResolutionStrategyReceiverMethod,
		},
		{
			// `pkg.Format` is not any symbol's name or qualified_name; it is the
			// after-last-slash portion of exactly one Go symbol's qualified name.
			name: "slash_suffix",
			build: func(t *testing.T, f *gateFixture) (int64, int64) {
				defs := f.file(t, "src/go/pkg/util.go", "go")
				want := f.symbol(t, defs, "Format", "github.com/org/repo/pkg.Format", "go")
				other := f.file(t, "src/go/other/util.go", "go")
				f.symbol(t, other, "Render", "github.com/org/repo/other.Render", "go")

				caller := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, caller, "Caller", "Caller", "go")
				return f.edge(t, caller, src, "pkg.Format"), want
			},
			wantStrategy: ResolutionStrategySlashSuffix,
		},
		{
			// One dot, no slash, and no slash-suffix candidate: the only match is
			// the destination's last two dot segments.
			name: "dot_tail2",
			build: func(t *testing.T, f *gateFixture) (int64, int64) {
				defs := f.file(t, "src/py/deep.py", "python")
				want := f.symbol(t, defs, "run", "a.b.mod.run", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "mod.run"), want
			},
			wantStrategy: ResolutionStrategyDotTail2,
		},
		{
			// Two dots, no slash: the schema-backed dot_tail3 prelude of the
			// dot-suffix strategy claims it before the LIKE fallback runs.
			name: "dot_tail3",
			build: func(t *testing.T, f *gateFixture) (int64, int64) {
				defs := f.file(t, "src/py/deep.py", "python")
				want := f.symbol(t, defs, "run", "a.pkg.mod.run", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "pkg.mod.run"), want
			},
			wantStrategy: ResolutionStrategyDotTail3,
		},
		{
			// Three dots in dst_name puts it past dot_tail3's exactly-two-dot
			// window, so only the LIKE fallback can match. That predicate is the
			// resolver's loosest, hence the low tier.
			name: "dot_suffix",
			build: func(t *testing.T, f *gateFixture) (int64, int64) {
				defs := f.file(t, "src/py/deep.py", "python")
				want := f.symbol(t, defs, "run", "root.a.pkg.mod.run", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "a.pkg.mod.run"), want
			},
			wantStrategy: ResolutionStrategyDotSuffix,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			edgeID, wantDst := tc.build(t, f)
			if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
				t.Fatalf("ResolveEdges() error = %v", err)
			}
			f.assertResolvedWith(t, edgeID, wantDst, tc.wantStrategy)
		})
	}
}

// TestResolveEdgesLeavesUnresolvedEdgesWithoutMetadata covers the P2/P3
// invariants from the metadata side: an edge that must stay unresolved must also
// stay unexplained. A stale strategy on an unresolved edge would be a claim of
// evidence for a destination that was deliberately refused.
func TestResolveEdgesLeavesUnresolvedEdgesWithoutMetadata(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, f *gateFixture) int64
	}{
		{
			// P3: two same-language definitions of one bare name.
			name: "ambiguous_bare_name",
			build: func(t *testing.T, f *gateFixture) int64 {
				first := f.file(t, "src/py/config.py", "python")
				f.symbol(t, first, "load_config", "config.load_config", "python")
				second := f.file(t, "src/py/config_alt.py", "python")
				f.symbol(t, second, "load_config", "config_alt.load_config", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "load_config")
			},
		},
		{
			// P2: the only candidate is in another language family, so no
			// implicit edge may cross to it.
			name: "cross_language_candidate",
			build: func(t *testing.T, f *gateFixture) int64 {
				defs := f.file(t, "src/py/config.py", "python")
				f.symbol(t, defs, "load_config", "config.load_config", "python")

				caller := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, caller, "Caller", "Caller", "go")
				return f.edge(t, caller, src, "load_config")
			},
		},
		{
			// No candidate at all.
			name: "no_candidate",
			build: func(t *testing.T, f *gateFixture) int64 {
				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "nothing_defines_this")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			edgeID := tc.build(t, f)
			if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
				t.Fatalf("ResolveEdges() error = %v", err)
			}
			f.assertUnresolvedWithoutMetadata(t, edgeID)
		})
	}
}

// TestScopedResolversRecordStrategy covers the path-scoped and name-targeted
// entrypoints, which share the Go-side binder rather than the repo-wide SQL
// strategies.
//
// The binder has two evidence levels and reports them separately. Its bare-tail
// fallback is deliberately not reported as exact_name: it applies no symbol-kind
// restriction, so it is weaker than the repo-wide bare-name strategy even when
// dst_name has no dot. See edge_resolution.go.
func TestScopedResolversRecordStrategy(t *testing.T) {
	cases := []struct {
		name         string
		dstName      string
		qualified    string
		wantStrategy string
	}{
		{
			name:         "qualified_match",
			dstName:      "config.load_config",
			qualified:    "config.load_config",
			wantStrategy: ResolutionStrategyExactQualified,
		},
		{
			name:         "bare_tail_fallback",
			dstName:      "config.load_config",
			qualified:    "some.other.path.load_config",
			wantStrategy: ResolutionStrategyBareTail,
		},
		{
			name:         "bare_name_uses_bare_tail",
			dstName:      "load_config",
			qualified:    "config.load_config",
			wantStrategy: ResolutionStrategyBareTail,
		},
	}

	for _, tc := range cases {
		for _, entrypoint := range []string{"paths", "names"} {
			t.Run(tc.name+"_"+entrypoint, func(t *testing.T) {
				f := newGateFixture(t)
				defs := f.file(t, "src/py/config.py", "python")
				wantDst := f.symbol(t, defs, "load_config", tc.qualified, "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				edgeID := f.edge(t, caller, src, tc.dstName)

				var err error
				switch entrypoint {
				case "paths":
					err = f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/py/caller.py"})
				case "names":
					_, err = f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{"load_config"})
				}
				if err != nil {
					t.Fatalf("resolve via %s error = %v", entrypoint, err)
				}
				f.assertResolvedWith(t, edgeID, wantDst, tc.wantStrategy)
			})
		}
	}
}

// TestScopedResolversLeaveRefusedEdgesUnexplained is the scoped-entrypoint twin
// of the repo-wide unresolved case: the Go-side binder must not write metadata
// for a target it refused.
func TestScopedResolversLeaveRefusedEdgesUnexplained(t *testing.T) {
	f := newGateFixture(t)
	first := f.file(t, "src/py/config.py", "python")
	f.symbol(t, first, "load_config", "config.load_config", "python")
	second := f.file(t, "src/py/config_alt.py", "python")
	f.symbol(t, second, "load_config", "config_alt.load_config", "python")

	caller := f.file(t, "src/py/caller.py", "python")
	src := f.symbol(t, caller, "caller", "caller", "python")
	edgeID := f.edge(t, caller, src, "load_config")

	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/py/caller.py"}); err != nil {
		t.Fatalf("ResolveEdgesForPaths() error = %v", err)
	}
	if _, err := f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{"load_config"}); err != nil {
		t.Fatalf("ResolveEdgesForNames() error = %v", err)
	}
	f.assertUnresolvedWithoutMetadata(t, edgeID)
}

// TestPurgeClearsResolutionMetadata covers the unbind path: when the destination
// file is deleted and its symbols are purged, the edge loses its destination and
// must lose the explanation with it.
func TestPurgeClearsResolutionMetadata(t *testing.T) {
	f := newGateFixture(t)
	defs := f.file(t, "src/py/config.py", "python")
	wantDst := f.symbol(t, defs, "load_config", "config.load_config", "python")

	caller := f.file(t, "src/py/caller.py", "python")
	src := f.symbol(t, caller, "caller", "caller", "python")
	edgeID := f.edge(t, caller, src, "load_config")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	f.assertResolvedWith(t, edgeID, wantDst, ResolutionStrategyExactName)

	const scanID = 1
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE files SET is_deleted = 1, last_scan_id = ? WHERE id = ?`, scanID, defs); err != nil {
		t.Fatalf("mark file deleted error = %v", err)
	}
	purged, err := f.store.PurgeDeletedFileGraphsForScan(f.ctx, f.repoID, scanID)
	if err != nil {
		t.Fatalf("PurgeDeletedFileGraphsForScan() error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeDeletedFileGraphsForScan() purged = %d, want 1", purged)
	}

	f.assertUnresolvedWithoutMetadata(t, edgeID)
}

// TestCrossLanguageLinksCarryOwnProvenance covers the explicit cross-language
// entrypoint, which creates already-bound rows rather than binding existing
// ones. Its links come from name coincidence across languages, not from a call
// site, so they get their own strategy values and the lowest tier -- they must
// never be reported under an implicit-resolver strategy.
//
// This is separate from the P2 rule that no *implicit* edge may cross a language
// boundary; that rule is unchanged and covered by the gating tests.
func TestCrossLanguageLinksCarryOwnProvenance(t *testing.T) {
	f := newGateFixture(t)

	// Shared-name strategy: same bare name, different languages.
	pyService := f.file(t, "src/py/service.py", "python")
	f.symbol(t, pyService, "RenderReport", "service.RenderReport", "python")
	tsService := f.file(t, "src/ts/service.ts", "typescript")
	f.symbol(t, tsService, "RenderReport", "service.RenderReport", "typescript")

	// Import-path strategy: a TS file importing a path whose extension-stripped
	// form matches a Python file. The symbol names differ, so only the
	// import-path branch can link these two.
	pyModel := f.file(t, "src/shared/model.py", "python")
	f.symbol(t, pyModel, "BuildPayload", "model.BuildPayload", "python")
	tsClient := f.file(t, "src/ts/client.ts", "typescript")
	f.symbol(t, tsClient, "SendPayload", "client.SendPayload", "typescript")
	if _, err := f.store.db.ExecContext(f.ctx,
		`INSERT INTO file_imports(repo_id, file_id, import_path) VALUES(?, ?, 'src/shared/model.ts')`,
		f.repoID, tsClient); err != nil {
		t.Fatalf("insert file_import error = %v", err)
	}

	created, err := f.store.ResolveCrossLanguageLinks(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("ResolveCrossLanguageLinks() error = %v", err)
	}
	if created == 0 {
		t.Fatal("ResolveCrossLanguageLinks() created no links; fixture no longer exercises either strategy")
	}

	// Scope each assertion by the evidence prefix the creating branch writes, so
	// a row landing under the wrong strategy is a failure rather than a silently
	// re-labelled pass.
	wantByEvidencePrefix := map[string]string{
		"shared_name:": ResolutionStrategyCrossLanguageSharedName,
		"import_path:": ResolutionStrategyCrossLanguageImportPath,
	}
	seenByStrategy := map[string]int{}

	rows, err := f.store.db.QueryContext(f.ctx, `
		SELECT evidence, resolution_strategy, resolution_confidence
		FROM edges
		WHERE repo_id = ? AND edge_kind = 'cross_language_ref'
	`, f.repoID)
	if err != nil {
		t.Fatalf("query cross-language edges error = %v", err)
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var evidence, strategy, confidence string
		if err := rows.Scan(&evidence, &strategy, &confidence); err != nil {
			t.Fatalf("scan cross-language edge error = %v", err)
		}
		total++
		want := ""
		for prefix, strategyForPrefix := range wantByEvidencePrefix {
			if strings.HasPrefix(evidence, prefix) {
				want = strategyForPrefix
			}
		}
		if want == "" {
			t.Fatalf("cross_language_ref edge has unrecognised evidence %q", evidence)
		}
		if strategy != want {
			t.Fatalf("cross_language_ref (evidence %q) resolution_strategy = %q, want %q", evidence, strategy, want)
		}
		if confidence != ResolutionConfidenceLow {
			t.Fatalf("cross_language_ref (evidence %q) resolution_confidence = %q, want %q", evidence, confidence, ResolutionConfidenceLow)
		}
		seenByStrategy[strategy]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("cross-language rows error = %v", err)
	}
	if total != created {
		t.Fatalf("read %d cross_language_ref rows, ResolveCrossLanguageLinks reported %d", total, created)
	}
	for _, strategy := range []string{ResolutionStrategyCrossLanguageSharedName, ResolutionStrategyCrossLanguageImportPath} {
		if seenByStrategy[strategy] == 0 {
			t.Fatalf("no cross_language_ref row carried %q; the fixture no longer exercises that branch", strategy)
		}
	}
}

// TestExportEdgeExposesResolutionMetadata is the JSON/export contract: the two
// fields ride on the edge rows the export surface already emits, and stay absent
// (omitempty) for unresolved edges so the highest-volume output does not grow
// for rows that have nothing to explain.
func TestExportEdgeExposesResolutionMetadata(t *testing.T) {
	f := newGateFixture(t)
	defs := f.file(t, "src/py/config.py", "python")
	f.symbol(t, defs, "load_config", "config.load_config", "python")

	caller := f.file(t, "src/py/caller.py", "python")
	src := f.symbol(t, caller, "caller", "caller", "python")
	resolvedEdge := f.edge(t, caller, src, "load_config")
	unresolvedEdge := f.edge(t, caller, src, "nothing_defines_this")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}

	edges, err := f.store.ExportEdgesPage(f.ctx, f.repoID, 100, 0)
	if err != nil {
		t.Fatalf("ExportEdgesPage() error = %v", err)
	}
	byID := map[int64]ExportEdge{}
	for _, edge := range edges {
		byID[edge.ID] = edge
	}

	resolved, ok := byID[resolvedEdge]
	if !ok {
		t.Fatalf("ExportEdgesPage() missing edge %d", resolvedEdge)
	}
	if resolved.ResolutionStrategy != ResolutionStrategyExactName {
		t.Fatalf("resolved edge ResolutionStrategy = %q, want %q", resolved.ResolutionStrategy, ResolutionStrategyExactName)
	}
	if resolved.ResolutionConfidence != ResolutionConfidenceHigh {
		t.Fatalf("resolved edge ResolutionConfidence = %q, want %q", resolved.ResolutionConfidence, ResolutionConfidenceHigh)
	}

	unresolved, ok := byID[unresolvedEdge]
	if !ok {
		t.Fatalf("ExportEdgesPage() missing edge %d", unresolvedEdge)
	}
	if unresolved.ResolutionStrategy != "" || unresolved.ResolutionConfidence != "" {
		t.Fatalf("unresolved edge carries metadata (%q, %q), want both empty",
			unresolved.ResolutionStrategy, unresolved.ResolutionConfidence)
	}

	// The snapshot loader is a second SQL statement scanned by the same
	// positional scanner, so it has to be checked separately.
	_, snapshotEdges, err := f.store.GraphSnapshot(f.ctx, f.repoID, "", 0)
	if err != nil {
		t.Fatalf("GraphSnapshot() error = %v", err)
	}
	for _, edge := range snapshotEdges {
		if edge.ID != resolvedEdge {
			continue
		}
		if edge.ResolutionStrategy != ResolutionStrategyExactName {
			t.Fatalf("GraphSnapshot edge ResolutionStrategy = %q, want %q", edge.ResolutionStrategy, ResolutionStrategyExactName)
		}
	}
}

// TestMigrationAddsResolutionMetadataToExistingDB replays every migration up to
// the one before 019 into a fresh database, writes a resolved edge the way the
// pre-P4 resolver would have, then opens the store normally so migration 019
// runs against real prior-version data.
//
// Two things are asserted: the migration applies without touching existing rows,
// and the historical edge keeps its destination with empty provenance. There is
// deliberately no backfill -- the strategy that bound a historical edge is not
// recoverable from graph shape, so guessing one would fabricate evidence.
func TestMigrationAddsResolutionMetadataToExistingDB(t *testing.T) {
	const edgeResolutionMigrationVersion = 19

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")

	priorVersions := applyMigrationsBelow(t, ctx, dbPath, edgeResolutionMigrationVersion)
	if len(priorVersions) == 0 {
		t.Fatal("no migrations below the edge-resolution migration; version constant is wrong")
	}

	// Write a resolved edge with the pre-019 column set, exactly as the old
	// binary would have left it.
	legacyDB, err := sql.Open(sqliteDriverName, dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	var repoID int64
	if res, err := legacyDB.ExecContext(ctx,
		`INSERT INTO repos(root_path, canonical_path, created_at, updated_at) VALUES('/tmp/legacy', '/tmp/legacy', '', '')`); err != nil {
		t.Fatalf("insert repo error = %v", err)
	} else if repoID, err = res.LastInsertId(); err != nil {
		t.Fatalf("repo LastInsertId() error = %v", err)
	}
	if _, err := legacyDB.ExecContext(ctx,
		`INSERT INTO files(id, repo_id, path, language, indexed_at) VALUES(1, ?, 'a.py', 'python', '')`, repoID); err != nil {
		t.Fatalf("insert file error = %v", err)
	}
	if _, err := legacyDB.ExecContext(ctx, `
		INSERT INTO symbols(id, repo_id, file_id, language, kind, name, qualified_name, container_name,
			start_line, start_col, end_line, end_col, stable_key)
		VALUES(1, ?, 1, 'python', 'function', 'load_config', 'config.load_config', '', 1, 1, 1, 1, 'k1'),
		      (2, ?, 1, 'python', 'function', 'caller', 'caller', '', 1, 1, 1, 1, 'k2')
	`, repoID, repoID); err != nil {
		t.Fatalf("insert symbols error = %v", err)
	}
	if _, err := legacyDB.ExecContext(ctx, `
		INSERT INTO edges(id, repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(1, ?, 2, 1, 'load_config', 'call', '', 1, 7)
	`, repoID); err != nil {
		t.Fatalf("insert legacy edge error = %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db error = %v", err)
	}

	// Open() runs Migrate(), which must apply 019 and nothing destructive.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() on pre-019 database error = %v", err)
	}
	defer s.Close()

	applied, err := hasMigration(s.db, edgeResolutionMigrationVersion)
	if err != nil {
		t.Fatalf("hasMigration() error = %v", err)
	}
	if !applied {
		t.Fatalf("migration %d not recorded after Open()", edgeResolutionMigrationVersion)
	}

	var dstID sql.NullInt64
	var line int
	var strategy, confidence string
	if err := s.db.QueryRowContext(ctx, `
		SELECT dst_symbol_id, line, resolution_strategy, resolution_confidence FROM edges WHERE id = 1
	`).Scan(&dstID, &line, &strategy, &confidence); err != nil {
		t.Fatalf("read migrated edge error = %v", err)
	}
	if !dstID.Valid || dstID.Int64 != 1 {
		t.Fatalf("migrated edge dst_symbol_id = %v, want 1 (migration must not unbind existing edges)", dstID)
	}
	if line != 7 {
		t.Fatalf("migrated edge line = %d, want 7 (migration must not rewrite existing columns)", line)
	}
	if strategy != "" || confidence != "" {
		t.Fatalf("migrated edge metadata = (%q, %q), want both empty; historical provenance must not be inferred", strategy, confidence)
	}

	// A fresh database gets the columns from the same migration, and a resolve
	// on top of migrated data fills them in normally.
	if _, err := s.ResolveEdges(ctx, repoID); err != nil {
		t.Fatalf("ResolveEdges() on migrated database error = %v", err)
	}
}

// applyMigrationsBelow replays every embedded migration with a version below
// maxVersion into dbPath, recording each in schema_migrations the same way
// Migrate() does. It returns the versions applied.
func applyMigrationsBelow(t *testing.T, ctx context.Context, dbPath string, maxVersion int) []int {
	t.Helper()
	entries, err := fs.ReadDir(migrationFS, "schema")
	if err != nil {
		t.Fatalf("read migration dir error = %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	db, err := sql.Open(sqliteDriverName, dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	var applied []int
	for _, name := range names {
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			continue
		}
		if version >= maxVersion {
			continue
		}
		body, err := migrationFS.ReadFile("schema/" + name)
		if err != nil {
			t.Fatalf("read migration %s error = %v", name, err)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply migration %s error = %v", name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?, '')`, version); err != nil {
			t.Fatalf("record migration %d error = %v", version, err)
		}
		applied = append(applied, version)
	}
	return applied
}
