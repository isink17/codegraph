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
type DiffDocument struct {
	Schema    string         `json:"schema"`
	Summary   map[string]int `json:"summary"`
	Files     ChangeSection  `json:"files"`
	Symbols   SymbolSection  `json:"symbols"`
	Edges     EdgeSection    `json:"edges"`
	TestLinks SymbolSection  `json:"test_links"`
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
	if semanticOK {
		compareDeclarations(&changes, base.GraphData, head.GraphData)
	}
	compatible := relationshipCompatible(base, head)
	if compatible {
		compareEdges(&changes, base.GraphData.Edges, head.GraphData.Edges)
		compareTestLinks(&changes, base.GraphData.TestLinks, head.GraphData.TestLinks)
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
	doc := DiffDocument{Schema: Schema, Summary: summary, Files: ChangeSection{Added: []Change{}, Removed: []Change{}, Modified: []Change{}}, Symbols: SymbolSection{Added: []Change{}, Removed: []Change{}, Changed: []Change{}}, Edges: EdgeSection{Added: []Change{}, Removed: []Change{}, Retargeted: []Change{}, ResolutionChanged: []Change{}, EvidenceChanged: []Change{}}, TestLinks: SymbolSection{Added: []Change{}, Removed: []Change{}, Changed: []Change{}}, Total: len(all), Offset: offset, Limit: limit, Truncated: offset+len(changes) < len(all)}
	for _, c := range changes {
		addToSections(&doc, c)
	}
	base.Graph = metadata(base.GraphData, base.IndexPolicy)
	head.Graph = metadata(head.GraphData, head.IndexPolicy)
	return Result{Schema: "codegraph.change/v1", From: base, To: head, Comparison: Comparison{Status: status, Limitations: lims}, Diff: doc}, nil
}

func semanticCompatible(a, b Revision) bool {
	return a.GraphData.SchemaVersion == b.GraphData.SchemaVersion && sameJSON(a.IndexPolicy, b.IndexPolicy) && sameJSON(a.GraphData.State.ResolverPolicies, b.GraphData.State.ResolverPolicies) && sameJSON(a.GraphData.State.ParserSemantics, b.GraphData.State.ParserSemantics) && sameJSON(a.GraphData.State.Capability, b.GraphData.State.Capability) && sameJSON(a.GraphData.State.JVMScope, b.GraphData.State.JVMScope) && sameFileParserSemantics(a.GraphData.Files, b.GraphData.Files) && a.GraphData.State.Pending == "" && b.GraphData.State.Pending == "" && a.GraphData.State.DirtyFiles == 0 && b.GraphData.State.DirtyFiles == 0
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

func compareDeclarations(out *[]Change, a, b store.SemanticGraph) {
	am, bm := group(a.Declarations, declIdentity), group(b.Declarations, declIdentity)
	incomplete := map[string]bool{}
	for _, g := range []store.SemanticGraph{a, b} {
		for _, f := range g.Files {
			if !store.ParseStateDescribesCurrentBytes(f.ParseState) {
				incomplete[f.Path] = true
			}
		}
	}
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

func compareEdges(out *[]Change, a, b []store.SemanticEdge) {
	am, bm := group(a, edgeSite), group(b, edgeSite)
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

func compareTestLinks(out *[]Change, a, b []store.SemanticTestLink) {
	am, bm := group(a, testIdentity), group(b, testIdentity)
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
func edgeSite(v store.SemanticEdge) string {
	return canonical([]any{declIdentity(store.SemanticDeclaration{Path: v.Source.Path, Language: v.Source.Language, Kind: v.Source.Kind, QualifiedName: v.Source.QualifiedName, Signature: v.Source.Signature, StableKey: v.Source.StableKey}), v.Path, v.Line, v.Kind})
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
