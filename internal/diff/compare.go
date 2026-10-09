// Package diff compares bounded, indexed snapshots without using database row IDs.
package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/limits"
	"github.com/isink17/codegraph/internal/store"
)

const Schema = "codegraph.diff/v1"

type Change struct {
	Kind      string          `json:"kind"`
	Identity  string          `json:"identity"`
	Ambiguous bool            `json:"ambiguous,omitempty"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
}

type Revision struct {
	Revision    string              `json:"revision"`
	Commit      string              `json:"commit"`
	Tree        string              `json:"tree"`
	Graph       GraphMetadata       `json:"graph"`
	GraphData   store.SemanticGraph `json:"-"`
	TreeFiles   []SourceFile        `json:"-"`
	IndexPolicy IndexPolicy         `json:"-"`
}

type Result struct {
	Schema     string       `json:"schema"`
	From       Revision     `json:"from"`
	To         Revision     `json:"to"`
	Comparison Comparison   `json:"comparison"`
	Diff       DiffDocument `json:"diff"`
}

type SourceFile struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	Language    string `json:"language,omitempty"`
}
type IndexPolicy struct {
	Include          []string `json:"include"`
	Exclude          []string `json:"exclude"`
	Languages        []string `json:"languages"`
	MaxFileSizeBytes int64    `json:"max_file_size_bytes"`
	ParseErrorPolicy string   `json:"parse_error_policy"`
}
type GraphMetadata struct {
	SchemaMigrations          []int64                `json:"schema_migrations"`
	ParserProfiles            map[string][]string    `json:"parser_profiles"`
	ParserSemanticEpochs      map[string][]int       `json:"parser_semantic_epochs"`
	ResolverPolicies          map[string]string      `json:"resolver_policies"`
	Capabilities              store.GraphCapability  `json:"capabilities"`
	ParseFailures             int                    `json:"parse_failures"`
	PendingSemanticTransition bool                   `json:"pending_semantic_transition"`
	JVMScope                  store.SemanticJVMScope `json:"jvm_scope"`
	IndexPolicy               IndexPolicy            `json:"index_policy"`
}
type Comparison struct {
	Status      string   `json:"status"`
	Limitations []string `json:"limitations"`
}
type ChangeSection struct {
	Added    []Change `json:"added"`
	Removed  []Change `json:"removed"`
	Modified []Change `json:"modified"`
}
type SymbolSection struct {
	Added   []Change `json:"added"`
	Removed []Change `json:"removed"`
	Changed []Change `json:"changed"`
}
type EdgeSection struct {
	Added             []Change `json:"added"`
	Removed           []Change `json:"removed"`
	Retargeted        []Change `json:"retargeted"`
	ResolutionChanged []Change `json:"resolution_changed"`
	EvidenceChanged   []Change `json:"evidence_changed"`
}

// SectionCoverage says how far an empty or short section can be trusted.
// Only "complete" means an absent change is evidence of no change.
type SectionCoverage struct {
	State  string `json:"state"` // complete, partial, or unavailable
	Reason string `json:"reason,omitempty"`
}
type Coverage struct {
	Files     SectionCoverage `json:"files"`
	Symbols   SectionCoverage `json:"symbols"`
	Edges     SectionCoverage `json:"edges"`
	TestLinks SectionCoverage `json:"test_links"`
}

const (
	CoverageComplete    = "complete"
	CoveragePartial     = "partial"
	CoverageUnavailable = "unavailable"
)

// Risk is change-risk evidence derived from the changes above. It carries no
// score: every entry cites the removed declaration and the base-side calls it
// rests on, and Coverage says how far an empty list can be trusted.
type Risk struct {
	Coverage SectionCoverage `json:"coverage"`
	// RemovedDeclarationCallers lists, for each declaration_removed change,
	// the resolved calls to it in the base revision from a declaration that
	// still exists in the head revision.
	RemovedDeclarationCallers []RemovedDeclarationCallers `json:"removed_declaration_callers"`
}

type RemovedDeclarationCallers struct {
	// Declaration is the identity of the declaration_removed change.
	Declaration string     `json:"declaration"`
	Calls       []RiskCall `json:"calls"`
}

// RiskCall is one base-side call into a removed declaration. Site is the
// edge identity used by the edge sections. Head is the edge at the same site
// in the head revision, absent when the call is gone or the site is ambiguous.
type RiskCall struct {
	Site          string              `json:"site"`
	Base          store.SemanticEdge  `json:"base"`
	Head          *store.SemanticEdge `json:"head,omitempty"`
	HeadAmbiguous bool                `json:"head_ambiguous,omitempty"`
}

type DiffDocument struct {
	Schema    string         `json:"schema"`
	Coverage  Coverage       `json:"coverage"`
	Summary   map[string]int `json:"summary"`
	Files     ChangeSection  `json:"files"`
	Symbols   SymbolSection  `json:"symbols"`
	Edges     EdgeSection    `json:"edges"`
	TestLinks SymbolSection  `json:"test_links"`
	Risk      Risk           `json:"risk"`
	Total     int            `json:"total"`
	Offset    int            `json:"offset"`
	Limit     int            `json:"limit"`
	Truncated bool           `json:"truncated"`
}

func Compare(base, head Revision, offset, limit int) (Result, error) {
	if limit == 0 {
		limit = limits.DefaultPage
	}
	if offset < 0 || limit < 1 || limit > limits.MaxPage {
		return Result{}, fmt.Errorf("invalid diff page: offset must be non-negative and limit must be 1..%d", limits.MaxPage)
	}
	changes := make([]Change, 0)
	compareFiles(&changes, revisionFiles(base), revisionFiles(head))
	semanticOK := semanticCompatible(base, head)
	incomplete := incompletePaths(base.GraphData, head.GraphData)
	if semanticOK {
		compareDeclarations(&changes, base.GraphData, head.GraphData, incomplete)
	}
	compatible := relationshipCompatible(base, head)
	// Edges of files without a current parse are dropped before anything,
	// including the column decision, looks at them.
	keepEdge := func(v store.SemanticEdge) bool { return !incomplete[v.Path] && !incomplete[v.Source.Path] }
	baseEdges, headEdges := filter(base.GraphData.Edges, keepEdge), filter(head.GraphData.Edges, keepEdge)
	if compatible {
		compareEdges(&changes, baseEdges, headEdges)
		compareTestLinks(&changes, base.GraphData.TestLinks, head.GraphData.TestLinks, incomplete)
	}
	sort.Slice(changes, func(i, j int) bool {
		if changeOrder(changes[i].Kind) != changeOrder(changes[j].Kind) {
			return changeOrder(changes[i].Kind) < changeOrder(changes[j].Kind)
		}
		if changes[i].Kind != changes[j].Kind {
			return changes[i].Kind < changes[j].Kind
		}
		if changes[i].Identity != changes[j].Identity {
			return changes[i].Identity < changes[j].Identity
		}
		return bytes.Compare(changes[i].Before, changes[j].Before) < 0
	})
	all := changes
	if offset >= len(all) {
		changes = []Change{}
	} else {
		end := offset + limit
		if end > len(all) {
			end = len(all)
		}
		changes = all[offset:end]
	}
	lims := limitations(base.GraphData, head.GraphData)
	if !sameJSON(base.IndexPolicy, head.IndexPolicy) {
		lims = append(lims, "effective indexing policies differ")
	}
	if !semanticOK {
		lims = append(lims, "declaration, edge, and test-link deltas omitted because indexing or graph semantics differ")
	}
	if semanticOK && !compatible {
		lims = append(lims, "edge and test-link deltas omitted because graph state is incomplete")
	}
	sort.Strings(lims)
	lims = compact(lims)
	status := "comparable"
	if !semanticOK {
		status = "incompatible"
	} else if len(lims) > 0 {
		status = "limited"
	}
	summary := make(map[string]int)
	for _, c := range all {
		summary[summaryKey(c.Kind)]++
	}
	coverage := sectionCoverage(base, head, baseEdges, headEdges, semanticOK, compatible, len(incomplete))
	doc := DiffDocument{Schema: Schema, Coverage: coverage, Risk: removedDeclarationRisk(all, coverage, head.GraphData.Declarations, baseEdges, headEdges), Summary: summary, Files: ChangeSection{Added: []Change{}, Removed: []Change{}, Modified: []Change{}}, Symbols: SymbolSection{Added: []Change{}, Removed: []Change{}, Changed: []Change{}}, Edges: EdgeSection{Added: []Change{}, Removed: []Change{}, Retargeted: []Change{}, ResolutionChanged: []Change{}, EvidenceChanged: []Change{}}, TestLinks: SymbolSection{Added: []Change{}, Removed: []Change{}, Changed: []Change{}}, Total: len(all), Offset: offset, Limit: limit, Truncated: offset+len(changes) < len(all)}
	for _, c := range changes {
		addToSections(&doc, c)
	}
	base.Graph = metadata(base.GraphData, base.IndexPolicy)
	head.Graph = metadata(head.GraphData, head.IndexPolicy)
	return Result{Schema: "codegraph.change/v1", From: base, To: head, Comparison: Comparison{Status: status, Limitations: lims}, Diff: doc}, nil
}

func sectionCoverage(base, head Revision, baseEdges, headEdges []store.SemanticEdge, semanticOK, compatible bool, skippedFiles int) Coverage {
	c := Coverage{Files: SectionCoverage{State: CoverageComplete}}
	switch {
	case !semanticOK:
		c.Symbols = SectionCoverage{State: CoverageUnavailable, Reason: "indexing or graph semantics differ between revisions"}
	case skippedFiles > 0:
		c.Symbols = SectionCoverage{State: CoveragePartial, Reason: fmt.Sprintf("%d files without a current parse were not compared", skippedFiles)}
	default:
		c.Symbols = SectionCoverage{State: CoverageComplete}
	}
	switch {
	case !semanticOK:
		c.Edges = c.Symbols
		c.TestLinks = c.Symbols
	case !compatible:
		c.Edges = SectionCoverage{State: CoverageUnavailable, Reason: "graph state is incomplete"}
		c.TestLinks = c.Edges
	default:
		c.Edges = SectionCoverage{State: CoverageComplete}
		c.TestLinks = SectionCoverage{State: CoverageComplete, Reason: "test links are heuristic relations, not proof of test coverage"}
		switch {
		case skippedFiles > 0:
			c.Edges = SectionCoverage{State: CoveragePartial, Reason: c.Symbols.Reason}
			c.TestLinks = SectionCoverage{State: CoveragePartial, Reason: c.Symbols.Reason + "; test links are heuristic relations"}
		case len(base.GraphData.State.Capability.Limitations()) > 0 || len(head.GraphData.State.Capability.Limitations()) > 0:
			c.Edges = SectionCoverage{State: CoveragePartial, Reason: "parser call capability is limited; a missing edge is not evidence of a removed call"}
		case !useEdgeColumns(baseEdges, headEdges) && anyEdgeColumn(baseEdges, headEdges):
			c.Edges = SectionCoverage{State: CoveragePartial, Reason: "call column not recorded for some edges; same-line calls are matched by line only"}
		}
	}
	return c
}

func semanticCompatible(a, b Revision) bool {
	return a.GraphData.SchemaVersion == b.GraphData.SchemaVersion && sameJSON(a.IndexPolicy, b.IndexPolicy) && sameJSON(a.GraphData.State.ResolverPolicies, b.GraphData.State.ResolverPolicies) && sameJSON(a.GraphData.State.ParserSemantics, b.GraphData.State.ParserSemantics) && sameJSON(a.GraphData.State.Capability, b.GraphData.State.Capability) && sameJSON(a.GraphData.State.JVMScope, b.GraphData.State.JVMScope) && sameFileParserSemantics(a.GraphData.Files, b.GraphData.Files) && a.GraphData.State.Pending == "" && b.GraphData.State.Pending == "" && a.GraphData.State.DirtyFiles == 0 && b.GraphData.State.DirtyFiles == 0
}
func hasUnknownLanguageFiles(files []store.SemanticFile) bool {
	for _, f := range files {
		if f.Language == "" {
			return true
		}
	}
	return false
}
func sameFileParserSemantics(a, b []store.SemanticFile) bool {
	am, bm := map[string]store.SemanticFile{}, map[string]store.SemanticFile{}
	for _, f := range a {
		am[f.Path] = f
	}
	for _, f := range b {
		bm[f.Path] = f
	}
	for path, x := range am {
		if y, ok := bm[path]; ok && (x.Language != y.Language || x.ParserProfile != y.ParserProfile || x.ParserSemanticEpoch != y.ParserSemanticEpoch || x.ParserCallEdges != y.ParserCallEdges) {
			return false
		}
	}
	return true
}
func relationshipCompatible(a, b Revision) bool {
	return semanticCompatible(a, b) && a.GraphData.State.DirtyFiles == 0 && b.GraphData.State.DirtyFiles == 0 && a.GraphData.State.ParseFailures == 0 && b.GraphData.State.ParseFailures == 0
}

func revisionFiles(r Revision) []SourceFile {
	if r.TreeFiles != nil {
		return r.TreeFiles
	}
	out := make([]SourceFile, 0, len(r.GraphData.Files))
	for _, f := range r.GraphData.Files {
		out = append(out, SourceFile{Path: f.Path, ContentHash: f.ContentHash, Language: f.Language})
	}
	return out
}
func changeOrder(k string) int {
	switch {
	case strings.HasPrefix(k, "file_"):
		return 0
	case strings.HasPrefix(k, "declaration_"):
		return 1
	case strings.HasPrefix(k, "edge_"):
		return 2
	default:
		return 3
	}
}
func summaryKey(k string) string {
	switch {
	case strings.HasPrefix(k, "file_"):
		return "files_" + strings.TrimPrefix(k, "file_")
	case strings.HasPrefix(k, "declaration_"):
		return "symbols_" + strings.TrimPrefix(k, "declaration_")
	case strings.HasPrefix(k, "edge_"):
		return "edges_" + strings.TrimPrefix(k, "edge_")
	default:
		return "test_links_" + strings.TrimPrefix(k, "heuristic_test_link_")
	}
}
func addToSections(d *DiffDocument, c Change) {
	switch c.Kind {
	case "file_added":
		d.Files.Added = append(d.Files.Added, c)
	case "file_removed":
		d.Files.Removed = append(d.Files.Removed, c)
	case "file_modified":
		d.Files.Modified = append(d.Files.Modified, c)
	case "declaration_added":
		d.Symbols.Added = append(d.Symbols.Added, c)
	case "declaration_removed":
		d.Symbols.Removed = append(d.Symbols.Removed, c)
	case "declaration_changed", "declaration_ambiguous":
		d.Symbols.Changed = append(d.Symbols.Changed, c)
	case "edge_added":
		d.Edges.Added = append(d.Edges.Added, c)
	case "edge_removed":
		d.Edges.Removed = append(d.Edges.Removed, c)
	case "edge_retargeted":
		d.Edges.Retargeted = append(d.Edges.Retargeted, c)
	case "edge_resolution_changed":
		d.Edges.ResolutionChanged = append(d.Edges.ResolutionChanged, c)
	case "edge_changed", "edge_ambiguous":
		d.Edges.EvidenceChanged = append(d.Edges.EvidenceChanged, c)
	case "heuristic_test_link_added":
		d.TestLinks.Added = append(d.TestLinks.Added, c)
	case "heuristic_test_link_removed":
		d.TestLinks.Removed = append(d.TestLinks.Removed, c)
	case "heuristic_test_link_changed", "test_link_ambiguous":
		d.TestLinks.Changed = append(d.TestLinks.Changed, c)
	}
}
func metadata(g store.SemanticGraph, p IndexPolicy) GraphMetadata {
	profiles := map[string][]string{}
	epochs := map[string][]int{}
	for _, f := range g.Files {
		if f.Language == "" {
			continue
		}
		profiles[f.Language] = append(profiles[f.Language], f.ParserProfile)
		epochs[f.Language] = append(epochs[f.Language], f.ParserSemanticEpoch)
	}
	for k := range profiles {
		profiles[k] = uniqueStrings(profiles[k])
	}
	for k := range epochs {
		epochs[k] = uniqueInts(epochs[k])
	}
	migrations := make([]int64, 0, g.SchemaVersion)
	for i := int64(1); i <= g.SchemaVersion; i++ {
		migrations = append(migrations, i)
	}
	policies := make(map[string]string, len(g.State.ResolverPolicies))
	for k, v := range g.State.ResolverPolicies {
		parts := strings.Split(k, ".")
		if len(parts) >= 4 {
			policies[parts[len(parts)-1]] = v
		} else {
			policies[k] = v
		}
	}
	return GraphMetadata{SchemaMigrations: migrations, ParserProfiles: profiles, ParserSemanticEpochs: epochs, ResolverPolicies: policies, Capabilities: g.State.Capability, ParseFailures: g.State.ParseFailures, PendingSemanticTransition: g.State.Pending != "", JVMScope: g.State.JVMScope, IndexPolicy: p}
}
func uniqueStrings(v []string) []string {
	sort.Strings(v)
	if len(v) == 0 {
		return v
	}
	out := v[:1]
	for _, x := range v[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}
func uniqueInts(v []int) []int {
	sort.Ints(v)
	if len(v) == 0 {
		return v
	}
	out := v[:1]
	for _, x := range v[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

func limitations(a, b store.SemanticGraph) []string {
	var out []string
	add := func(v string) {
		if v != "" {
			out = append(out, v)
		}
	}
	for side, g := range map[string]store.SemanticGraph{"base": a, "head": b} {
		if hasUnknownLanguageFiles(g.Files) {
			add(side + ": semantic coverage is incomplete for files without a recognized language")
		}
		if g.State.Pending != "" {
			add(side + ": " + g.State.Pending)
		}
		if g.State.DirtyFiles > 0 {
			add(fmt.Sprintf("%s: %d dirty files", side, g.State.DirtyFiles))
		}
		if g.State.ParseFailures > 0 {
			add(fmt.Sprintf("%s: %d files failed, exceeded size limits, or remain pending", side, g.State.ParseFailures))
		}
		if g.State.JVMScope.State != "complete" {
			add(side + ": JVM compilation scope is unknown or incomplete")
		}
		if len(g.State.Capability.Limitations()) > 0 {
			add(side + ": parser call capability is limited")
		}
	}
	if !sameJSON(a.State.JVMScope, b.State.JVMScope) {
		add("JVM compilation-scope evidence differs")
	}
	if a.SchemaVersion != b.SchemaVersion {
		add("schema versions differ")
	}
	if !sameJSON(a.State.ResolverPolicies, b.State.ResolverPolicies) {
		add("resolver policies differ")
	}
	if !sameJSON(a.State.ParserSemantics, b.State.ParserSemantics) {
		add("parser semantic markers differ")
	}
	if !sameJSON(a.State.Capability, b.State.Capability) {
		add("parser capabilities differ")
	}
	if !sameFileParserSemantics(a.Files, b.Files) {
		add("per-file parser semantics differ")
	}
	sort.Strings(out)
	return compact(out)
}

func compareFiles(out *[]Change, a, b []SourceFile) {
	am, bm := map[string]SourceFile{}, map[string]SourceFile{}
	for _, v := range a {
		am[v.Path] = SourceFile{Path: v.Path, ContentHash: v.ContentHash, Language: v.Language}
	}
	for _, v := range b {
		bm[v.Path] = SourceFile{Path: v.Path, ContentHash: v.ContentHash, Language: v.Language}
	}
	keys := union(am, bm)
	for _, k := range keys {
		x, xok := am[k]
		y, yok := bm[k]
		switch {
		case !xok:
			appendChange(out, "file_added", k, nil, y)
		case !yok:
			appendChange(out, "file_removed", k, x, nil)
		case x.ContentHash != y.ContentHash || x.Language != y.Language:
			appendChange(out, "file_modified", k, x, y)
		}
	}
}

// incompletePaths lists files whose graph rows do not describe the current
// bytes on either side; their declarations, edges and test links are not compared.
func incompletePaths(a, b store.SemanticGraph) map[string]bool {
	incomplete := map[string]bool{}
	for _, g := range []store.SemanticGraph{a, b} {
		for _, f := range g.Files {
			if !store.ParseStateDescribesCurrentBytes(f.ParseState) {
				incomplete[f.Path] = true
			}
		}
	}
	return incomplete
}

// useEdgeColumns reports whether every edge on both sides has a recorded call
// column. Columns join the site identity only then: an edge indexed before
// columns were persisted must still pair with the same call on the other side.
func useEdgeColumns(a, b []store.SemanticEdge) bool {
	for _, side := range [][]store.SemanticEdge{a, b} {
		for _, e := range side {
			if e.Column <= 0 {
				return false
			}
		}
	}
	return true
}

func anyEdgeColumn(a, b []store.SemanticEdge) bool {
	for _, side := range [][]store.SemanticEdge{a, b} {
		for _, e := range side {
			if e.Column > 0 {
				return true
			}
		}
	}
	return false
}

func compareDeclarations(out *[]Change, a, b store.SemanticGraph, incomplete map[string]bool) {
	am, bm := group(a.Declarations, declIdentity), group(b.Declarations, declIdentity)
	for _, k := range union(am, bm) {
		x, y := am[k], bm[k]
		if len(x) > 0 && incomplete[x[0].Path] || len(y) > 0 && incomplete[y[0].Path] {
			continue
		}
		if len(x) > 1 || len(y) > 1 {
			if !sameDeclarationMultiset(x, y) {
				appendChange(out, "declaration_ambiguous", k, x, y)
			}
			continue
		}
		switch {
		case len(x) == 0:
			appendChange(out, "declaration_added", k, nil, y[0])
		case len(y) == 0:
			appendChange(out, "declaration_removed", k, x[0], nil)
		case !sameDeclaration(x[0], y[0]):
			appendChange(out, "declaration_changed", k, x[0], y[0])
		}
	}
}

// compareEdges compares edges already restricted to files with a current parse.
func compareEdges(out *[]Change, a, b []store.SemanticEdge) {
	withColumn := useEdgeColumns(a, b)
	site := func(v store.SemanticEdge) string { return edgeSite(v, withColumn) }
	if !withColumn {
		// A column recorded on one side only is not a source change.
		a, b = withoutColumns(a), withoutColumns(b)
	}
	am, bm := group(a, site), group(b, site)
	for _, k := range union(am, bm) {
		x, y := am[k], bm[k]
		if len(x) > 1 || len(y) > 1 {
			if len(x) != len(y) || !sameJSON(x, y) {
				appendChange(out, "edge_ambiguous", k, map[string]int{"count": len(x)}, map[string]int{"count": len(y)})
			}
			continue
		}
		switch {
		case len(x) == 0:
			appendChange(out, "edge_added", k, nil, y[0])
		case len(y) == 0:
			appendChange(out, "edge_removed", k, x[0], nil)
		case !sameJSON(x[0], y[0]):
			kind := "edge_changed"
			if x[0].Target.State != y[0].Target.State {
				kind = "edge_resolution_changed"
			} else if !sameJSON(x[0].Target, y[0].Target) {
				kind = "edge_retargeted"
			}
			appendChange(out, kind, k, x[0], y[0])
		}
	}
}

func compareTestLinks(out *[]Change, a, b []store.SemanticTestLink, incomplete map[string]bool) {
	keep := func(v store.SemanticTestLink) bool { return !incomplete[v.TestFile] && !incomplete[v.TargetFile] }
	am, bm := group(filter(a, keep), testIdentity), group(filter(b, keep), testIdentity)
	for _, k := range union(am, bm) {
		x, y := am[k], bm[k]
		if len(x) > 1 || len(y) > 1 {
			if len(x) != len(y) || !sameJSON(x, y) {
				appendChange(out, "test_link_ambiguous", k, map[string]int{"count": len(x)}, map[string]int{"count": len(y)})
			}
			continue
		}
		switch {
		case len(x) == 0:
			appendChange(out, "heuristic_test_link_added", k, nil, y[0])
		case len(y) == 0:
			appendChange(out, "heuristic_test_link_removed", k, x[0], nil)
		case !sameJSON(x[0], y[0]):
			appendChange(out, "heuristic_test_link_changed", k, x[0], y[0])
		}
	}
}

func declIdentity(v store.SemanticDeclaration) string {
	return canonical([]any{v.Path, v.Language, v.Kind, v.QualifiedName, v.Signature, v.StableKey})
}
func edgeSite(v store.SemanticEdge, withColumn bool) string {
	src := declIdentity(store.SemanticDeclaration{Path: v.Source.Path, Language: v.Source.Language, Kind: v.Source.Kind, QualifiedName: v.Source.QualifiedName, Signature: v.Source.Signature, StableKey: v.Source.StableKey})
	if withColumn {
		return canonical([]any{src, v.Path, v.Line, v.Column, v.Kind})
	}
	return canonical([]any{src, v.Path, v.Line, v.Kind})
}
func withoutColumns(in []store.SemanticEdge) []store.SemanticEdge {
	out := append([]store.SemanticEdge(nil), in...)
	for i := range out {
		out[i].Column = 0
	}
	return out
}

func endpointIdentity(v store.SemanticEndpoint) string {
	return declIdentity(store.SemanticDeclaration{Path: v.Path, Language: v.Language, Kind: v.Kind, QualifiedName: v.QualifiedName, Signature: v.Signature, StableKey: v.StableKey})
}

// removedDeclarationRisk cites the base-side resolved calls into each removed
// declaration from callers that survive into the head revision. Only resolved
// calls are evidence, so the list is never complete: an unresolved, dynamic or
// cross-language call can reach a removed declaration without appearing here.
func removedDeclarationRisk(changes []Change, cov Coverage, headDecls []store.SemanticDeclaration, baseEdges, headEdges []store.SemanticEdge) Risk {
	r := Risk{RemovedDeclarationCallers: []RemovedDeclarationCallers{}}
	if cov.Symbols.State == CoverageUnavailable || cov.Edges.State == CoverageUnavailable {
		r.Coverage = SectionCoverage{State: CoverageUnavailable, Reason: "removed declarations or edges were not compared"}
		return r
	}
	reason := "only resolved calls are cited; an unresolved, dynamic or cross-language call can still reach a removed declaration"
	if cov.Edges.State != CoverageComplete {
		reason = cov.Edges.Reason + "; " + reason
	}
	r.Coverage = SectionCoverage{State: CoveragePartial, Reason: reason}
	removed := map[string]bool{}
	for _, c := range changes {
		if c.Kind == "declaration_removed" {
			removed[c.Identity] = true
		}
	}
	if len(removed) == 0 {
		return r
	}
	surviving := make(map[string]bool, len(headDecls))
	for _, d := range headDecls {
		surviving[declIdentity(d)] = true
	}
	withColumn := useEdgeColumns(baseEdges, headEdges)
	if !withColumn {
		baseEdges, headEdges = withoutColumns(baseEdges), withoutColumns(headEdges)
	}
	site := func(v store.SemanticEdge) string { return edgeSite(v, withColumn) }
	heads := group(headEdges, site)
	byDecl := map[string][]RiskCall{}
	for _, e := range baseEdges {
		target := endpointIdentity(e.Target)
		if e.Target.State != "resolved" || !removed[target] || !surviving[endpointIdentity(e.Source)] {
			continue
		}
		call := RiskCall{Site: site(e), Base: e}
		switch h := heads[call.Site]; len(h) {
		case 0:
		case 1:
			call.Head = &h[0]
		default:
			call.HeadAmbiguous = true
		}
		byDecl[target] = append(byDecl[target], call)
	}
	for _, decl := range union(byDecl, byDecl) {
		calls := byDecl[decl]
		sort.Slice(calls, func(i, j int) bool {
			if calls[i].Site != calls[j].Site {
				return calls[i].Site < calls[j].Site
			}
			return canonical(calls[i].Base) < canonical(calls[j].Base)
		})
		r.RemovedDeclarationCallers = append(r.RemovedDeclarationCallers, RemovedDeclarationCallers{Declaration: decl, Calls: calls})
	}
	return r
}
func filter[T any](in []T, keep func(T) bool) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}
func testIdentity(v store.SemanticTestLink) string {
	return canonical([]any{v.TestFile, v.Test, v.TargetFile, v.Target, v.Reason})
}

func sameDeclaration(a, b store.SemanticDeclaration) bool {
	return a.Name == b.Name && a.Visibility == b.Visibility && sameJSON(a.IsStatic, b.IsStatic) && sameJSON(a.ArityMin, b.ArityMin) && sameJSON(a.ArityMax, b.ArityMax)
}

func sameDeclarationMultiset(a, b []store.SemanticDeclaration) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]store.SemanticDeclaration(nil), a...)
	bb := append([]store.SemanticDeclaration(nil), b...)
	less := func(v []store.SemanticDeclaration) {
		sort.Slice(v, func(i, j int) bool { return canonical(declarationAttrs(v[i])) < canonical(declarationAttrs(v[j])) })
	}
	less(aa)
	less(bb)
	for i := range aa {
		if !sameJSON(declarationAttrs(aa[i]), declarationAttrs(bb[i])) {
			return false
		}
	}
	return true
}
func declarationAttrs(v store.SemanticDeclaration) any {
	return struct {
		Name, Visibility   string
		Static             *bool
		ArityMin, ArityMax *int
	}{v.Name, v.Visibility, v.IsStatic, v.ArityMin, v.ArityMax}
}
func appendChange(out *[]Change, k, id string, b, a any) {
	var before, after json.RawMessage
	if b != nil {
		before, _ = json.Marshal(b)
	}
	if a != nil {
		after, _ = json.Marshal(a)
	}
	*out = append(*out, Change{Kind: k, Identity: id, Before: before, After: after})
}
func canonical(v any) string { b, _ := json.Marshal(v); return string(b) }
func sameJSON(a, b any) bool { return canonical(a) == canonical(b) }
func group[T any](values []T, key func(T) string) map[string][]T {
	m := make(map[string][]T, len(values))
	for _, v := range values {
		m[key(v)] = append(m[key(v)], v)
	}
	for k, vs := range m {
		sort.Slice(vs, func(i, j int) bool { return canonical(vs[i]) < canonical(vs[j]) })
		m[k] = vs
	}
	return m
}
func union[A any](a, b map[string]A) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func compact(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
