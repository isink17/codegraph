package diff

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/store"
)

// Risk is an evidence-backed change-risk assessment over the whole change set
// (never the page). Score is an ordinal evidence index, not a failure
// probability, and no assessment proves a change safe: only resolved edges are
// cited, so an unresolved, dynamic or cross-language dependent can still break.
//
// Each component scores one rule over distinct dependent relationships
// (dependent declaration, depended-on declaration); a relationship is cited by
// at most one component. Score is the clamped sum of component points and is
// null when declarations or edges were not compared.
type Risk struct {
	Assessment string          `json:"assessment"`
	Model      string          `json:"model"`
	ScoreKind  string          `json:"score_kind"`
	Score      *int            `json:"score"`
	Level      string          `json:"level"`
	Coverage   SectionCoverage `json:"coverage"`
	Components []RiskComponent `json:"components"`
	// Uncertainty lists what the compared evidence cannot rule out; it adds no
	// points.
	Uncertainty []RiskUncertainty `json:"uncertainty"`
	// RemovedDeclarationDependents lists, for each declaration_removed change,
	// the resolved edges into it (calls or any other edge kind) in the base
	// revision from a declaration that still exists in the head revision.
	RemovedDeclarationDependents []RemovedDeclarationDependents `json:"removed_declaration_dependents"`
}

type RiskComponent struct {
	Rule       string         `json:"rule"`
	PointsEach int            `json:"points_each"`
	MaxPoints  int            `json:"max_points"`
	Count      int            `json:"count"`
	Points     int            `json:"points"`
	Evidence   []RiskEvidence `json:"evidence"`
}

// RiskEvidence is one dependent relationship. Declaration and Dependent are
// declaration identities as used by the symbol sections, Sites the edge
// identities as used by the edge sections, and Change the diff change kind the
// relationship rests on.
type RiskEvidence struct {
	Change      string   `json:"change"`
	Declaration string   `json:"declaration"`
	Dependent   string   `json:"dependent"`
	File        string   `json:"file"`
	Sites       []string `json:"sites"`
	HeadSites   []string `json:"head_sites,omitempty"`
	HeadStates  []string `json:"head_states,omitempty"`
	HeadTargets []string `json:"head_targets,omitempty"`
}

type RiskUncertainty struct {
	Reason   string `json:"reason"`
	Identity string `json:"identity,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type RemovedDeclarationDependents struct {
	// Declaration is the identity of the declaration_removed change.
	Declaration string     `json:"declaration"`
	Edges       []RiskEdge `json:"edges"`
}

// RiskEdge is one base-side resolved edge into a removed declaration. Site is the
// edge identity used by the edge sections. Head is the edge at the same site
// in the head revision, absent when the call is gone or the site is ambiguous.
type RiskEdge struct {
	Site          string              `json:"site"`
	Base          store.SemanticEdge  `json:"base"`
	Head          *store.SemanticEdge `json:"head,omitempty"`
	HeadAmbiguous bool                `json:"head_ambiguous,omitempty"`
}

const (
	RiskModel     = "codegraph.change-risk/v1"
	RiskScoreKind = "evidence_index_not_probability"
	RiskMaxScore  = 100

	// AssessmentRiskEvidence: at least one component cites a dependent relationship.
	AssessmentRiskEvidence = "risk_evidence"
	// AssessmentLimitedEvidence: no component fired, but uncertainty remains.
	AssessmentLimitedEvidence = "limited_evidence"
	// AssessmentNoRiskEvidence: no component fired and no uncertainty row; still
	// not proof of safety.
	AssessmentNoRiskEvidence = "no_risk_evidence"
	// AssessmentInsufficientEvidence: declarations or edges were not compared.
	AssessmentInsufficientEvidence = "insufficient_evidence"

	RiskLevelNone    = "none"
	RiskLevelLow     = "low"
	RiskLevelMedium  = "medium"
	RiskLevelHigh    = "high"
	RiskLevelUnknown = "unknown"

	RuleRemovedDeclarationDependent = "removed_declaration_dependent"
	RuleDependentResolutionLost     = "dependent_resolution_lost"
	RuleDependentRetargeted         = "dependent_retargeted"
	RuleChangedDeclarationDependent = "changed_declaration_dependent"
)

// Level thresholds: low from 1, medium from 25, high from 50.
var riskLevelFloors = []struct {
	level string
	floor int
}{{RiskLevelHigh, 50}, {RiskLevelMedium, 25}, {RiskLevelLow, 1}}

// riskRules is the fixed v1 rule set, in output order. Changing a value
// changes RiskModel.
var riskRules = []struct {
	rule       string
	pointsEach int
	maxPoints  int
}{
	// The declaration is gone and the dependent still references its name
	// without resolving it.
	{RuleRemovedDeclarationDependent, 20, 60},
	// The declaration exists, but the dependent's references no longer resolve.
	{RuleDependentResolutionLost, 10, 30},
	// The dependent's references now resolve to a different declaration.
	{RuleDependentRetargeted, 5, 20},
	// A declaration kept its identity but changed visibility, staticness or
	// arity while other declarations still resolve to it.
	{RuleChangedDeclarationDependent, 5, 20},
}

// RiskLevel maps a score to its documented band.
func RiskLevel(score int) string {
	for _, f := range riskLevelFloors {
		if score >= f.floor {
			return f.level
		}
	}
	return RiskLevelNone
}

func assessRisk(changes []Change, cov Coverage, lims []string, baseDecls, headDecls []store.SemanticDeclaration, baseEdges, headEdges []store.SemanticEdge, pairs DeclarationPairs) Risk {
	r := Risk{Model: RiskModel, ScoreKind: RiskScoreKind, Uncertainty: []RiskUncertainty{}, RemovedDeclarationDependents: []RemovedDeclarationDependents{}}
	found := map[string][]RiskEvidence{}
	r.Uncertainty = append(r.Uncertainty, limitationUncertainty(changes, lims)...)
	if cov.Symbols.State == CoverageUnavailable || cov.Edges.State == CoverageUnavailable {
		r.Assessment, r.Level = AssessmentInsufficientEvidence, RiskLevelUnknown
		r.Coverage = SectionCoverage{State: CoverageUnavailable, Reason: "removed declarations or edges were not compared"}
		r.Components = riskComponents(found)
		r.Uncertainty = sortUncertainty(r.Uncertainty)
		return r
	}
	reason := "only resolved edges are cited; an unresolved, dynamic or cross-language call can still reach a changed declaration"
	if cov.Edges.State != CoverageComplete {
		reason = cov.Edges.Reason + "; " + reason
	}
	r.Coverage = SectionCoverage{State: CoveragePartial, Reason: reason}
	for name, c := range map[string]SectionCoverage{"symbols": cov.Symbols, "edges": cov.Edges} {
		if c.State != CoverageComplete {
			r.Uncertainty = append(r.Uncertainty, RiskUncertainty{Reason: name + "_coverage_partial", Detail: c.Reason})
		}
	}

	removed, changed := map[string]bool{}, map[string]bool{}
	for _, c := range changes {
		switch c.Kind {
		case "declaration_removed":
			removed[c.Identity] = true
		case "declaration_changed":
			changed[c.Identity] = true
		case "declaration_ambiguous", "edge_ambiguous":
			r.Uncertainty = append(r.Uncertainty, RiskUncertainty{Reason: c.Kind, Identity: c.Identity})
		}
	}
	pairedTo := map[string]string{}
	for _, p := range pairs.Pairs {
		pairedTo[declIdentity(p.Before)] = declIdentity(p.After)
	}

	r.RemovedDeclarationDependents = removedDependents(removed, headDecls, baseEdges, headEdges)
	for _, d := range r.RemovedDeclarationDependents {
		for _, e := range d.Edges {
			if e.HeadAmbiguous {
				r.Uncertainty = append(r.Uncertainty, RiskUncertainty{Reason: "dependent_site_ambiguous", Identity: e.Site})
			}
		}
	}

	withColumn := useEdgeColumns(baseEdges, headEdges)
	surviving := make(map[string]bool, len(headDecls))
	for _, d := range headDecls {
		surviving[declIdentity(d)] = true
	}
	// A base relationship is judged by what the same dependent references by
	// the same name in head, not by the call site: a site is a line and column,
	// and an edit that shifts lines can land an unrelated call on it.
	type relationship struct {
		dependent, declaration, file, name string
		sites                              []string
	}
	rels := map[string]*relationship{}
	baseTargets := map[string]map[string]bool{}
	for _, e := range baseEdges {
		dep, decl := endpointIdentity(e.Source), endpointIdentity(e.Target)
		if e.Target.State != "resolved" || dep == decl || !surviving[dep] {
			continue
		}
		key := canonical([]string{decl, dep})
		if rels[key] == nil {
			rels[key] = &relationship{dependent: dep, declaration: decl, file: e.Source.Path, name: referenceName(e.Target.QualifiedName)}
		}
		rels[key].sites = append(rels[key].sites, edgeSite(e, withColumn))
		if baseTargets[dep] == nil {
			baseTargets[dep] = map[string]bool{}
		}
		baseTargets[dep][decl] = true
	}
	headRefs := map[string][]store.SemanticEdge{}
	for _, e := range headEdges {
		headRefs[endpointIdentity(e.Source)] = append(headRefs[endpointIdentity(e.Source)], e)
	}
	cited := map[string]bool{}
	for _, key := range union(rels, rels) {
		rel := rels[key]
		ev := RiskEvidence{Declaration: rel.declaration, Dependent: rel.dependent, File: rel.file, Sites: rel.sites}
		bound, unbound, elsewhere := false, false, false
		for _, h := range headRefs[rel.dependent] {
			target := endpointIdentity(h.Target)
			// A reference resolving to another declaration the dependent
			// already used in base belongs to that relationship.
			if referenceName(h.Target.QualifiedName) != rel.name || h.Target.State == "resolved" && target != rel.declaration && baseTargets[rel.dependent][target] {
				continue
			}
			ev.HeadSites = append(ev.HeadSites, edgeSite(h, withColumn))
			ev.HeadStates = append(ev.HeadStates, h.Target.State)
			switch {
			case h.Target.State != "resolved":
				unbound = true
			case target == rel.declaration || target == pairedTo[rel.declaration]:
				bound = true
			default:
				elsewhere = true
				ev.HeadTargets = append(ev.HeadTargets, target)
			}
		}
		normalizeEvidence(&ev)
		if removed[rel.declaration] {
			cited[rel.declaration] = true
		}
		switch {
		case removed[rel.declaration] && unbound:
			ev.Change = "declaration_removed"
			found[RuleRemovedDeclarationDependent] = append(found[RuleRemovedDeclarationDependent], ev)
		case bound:
			// Still resolves to the same, or a provably moved, declaration.
		case elsewhere:
			ev.Change = "binding_changed"
			found[RuleDependentRetargeted] = append(found[RuleDependentRetargeted], ev)
		case unbound:
			ev.Change = "resolution_lost"
			found[RuleDependentResolutionLost] = append(found[RuleDependentResolutionLost], ev)
		}
		// No reference by that name remains: the dependent no longer uses it.
	}
	for id := range removed {
		// A provably moved declaration keeps every unresolved caller it had.
		if !cited[id] && pairedTo[id] == "" {
			r.Uncertainty = append(r.Uncertainty, RiskUncertainty{Reason: "removed_declaration_without_resolved_dependents", Identity: id})
		}
	}

	changedDeps := map[string]*RiskEvidence{}
	for _, e := range headEdges {
		decl, dep := endpointIdentity(e.Target), endpointIdentity(e.Source)
		if e.Target.State != "resolved" || !changed[decl] || dep == decl {
			continue
		}
		key := canonical([]string{decl, dep})
		if changedDeps[key] == nil {
			changedDeps[key] = &RiskEvidence{Change: "declaration_changed", Declaration: decl, Dependent: dep, File: e.Source.Path}
		}
		changedDeps[key].Sites = append(changedDeps[key].Sites, edgeSite(e, withColumn))
	}
	for _, ev := range changedDeps {
		normalizeEvidence(ev)
		found[RuleChangedDeclarationDependent] = append(found[RuleChangedDeclarationDependent], *ev)
	}

	r.Components = riskComponents(found)
	score := 0
	for _, c := range r.Components {
		score += c.Points
	}
	score = min(score, RiskMaxScore)
	r.Score, r.Level = &score, RiskLevel(score)
	r.Uncertainty = sortUncertainty(r.Uncertainty)
	switch {
	case score > 0:
		r.Assessment = AssessmentRiskEvidence
	case len(r.Uncertainty) > 0:
		r.Assessment = AssessmentLimitedEvidence
	default:
		r.Assessment = AssessmentNoRiskEvidence
	}
	return r
}

// limitationUncertainty keeps every comparison limitation except the two that
// describe the whole repository: files without a recognized language and an
// unknown JVM scope are cited only through the changed files they affect, so a
// Go-only change is not limited by an unrelated README or JVM state.
func limitationUncertainty(changes []Change, lims []string) []RiskUncertainty {
	var out []RiskUncertainty
	jvmUnknown := false
	for _, lim := range lims {
		switch {
		case strings.HasSuffix(lim, ": "+limitJVMScope):
			jvmUnknown = true
		case strings.HasSuffix(lim, ": "+limitUnknownLanguage):
		default:
			out = append(out, RiskUncertainty{Reason: "comparison_limitation", Detail: lim})
		}
	}
	for _, c := range changes {
		if !strings.HasPrefix(c.Kind, "file_") {
			continue
		}
		for _, raw := range []json.RawMessage{c.Before, c.After} {
			var f SourceFile
			if raw == nil || json.Unmarshal(raw, &f) != nil {
				continue
			}
			switch {
			case f.Language == "":
				out = append(out, RiskUncertainty{Reason: "changed_file_without_semantic_coverage", Identity: c.Identity})
			case jvmUnknown && jvmLanguages[f.Language]:
				out = append(out, RiskUncertainty{Reason: "changed_file_jvm_scope_unknown", Identity: c.Identity})
			}
		}
	}
	return out
}

var jvmLanguages = map[string]bool{"java": true, "kotlin": true, "scala": true}

func riskComponents(found map[string][]RiskEvidence) []RiskComponent {
	out := make([]RiskComponent, 0, len(riskRules))
	for _, rule := range riskRules {
		evs := found[rule.rule]
		if evs == nil {
			evs = []RiskEvidence{}
		}
		sort.Slice(evs, func(i, j int) bool { return canonical(evs[i]) < canonical(evs[j]) })
		out = append(out, RiskComponent{Rule: rule.rule, PointsEach: rule.pointsEach, MaxPoints: rule.maxPoints, Count: len(evs), Points: min(len(evs)*rule.pointsEach, rule.maxPoints), Evidence: evs})
	}
	return out
}

func normalizeEvidence(ev *RiskEvidence) {
	ev.Sites = uniqueStrings(ev.Sites)
	ev.HeadSites = uniqueStrings(ev.HeadSites)
	ev.HeadStates = uniqueStrings(ev.HeadStates)
	ev.HeadTargets = uniqueStrings(ev.HeadTargets)
}

// referenceName is the last segment of a qualified or written reference
// ("p.T.M", "obj.M", "ns::M", "T#M", "o->M" are all "M"), the part a call site
// spells for both a resolved and an unresolved target.
func referenceName(qualified string) string {
	i := strings.LastIndexAny(qualified, ".:#>\\/")
	return qualified[i+1:]
}

func sortUncertainty(u []RiskUncertainty) []RiskUncertainty {
	sort.Slice(u, func(i, j int) bool { return canonical(u[i]) < canonical(u[j]) })
	out := u[:0]
	for i, v := range u {
		if i == 0 || v != u[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// removedDependents cites the base-side resolved edges (calls and any other
// edge kind) into each removed declaration from sources that survive into the
// head revision.
func removedDependents(removed map[string]bool, headDecls []store.SemanticDeclaration, baseEdges, headEdges []store.SemanticEdge) []RemovedDeclarationDependents {
	out := []RemovedDeclarationDependents{}
	if len(removed) == 0 {
		return out
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
	byDecl := map[string][]RiskEdge{}
	for _, e := range baseEdges {
		target := endpointIdentity(e.Target)
		if e.Target.State != "resolved" || !removed[target] || !surviving[endpointIdentity(e.Source)] {
			continue
		}
		call := RiskEdge{Site: site(e), Base: e}
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
		out = append(out, RemovedDeclarationDependents{Declaration: decl, Edges: calls})
	}
	return out
}
