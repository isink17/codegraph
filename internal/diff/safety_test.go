package diff

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEditSafetyNoImpactDoesNotImplySafe(t *testing.T) {
	e := NewEditSafetyEvidence(EditSafetyEvidence{
		SemanticChanges: []Change{{Kind: "declaration_changed", Identity: "pkg.A"}},
		Evidence:        []SafetyEvidence{{Boundary: "graph", Source: "index", Detail: "complete", State: EvidenceSatisfied, Confidence: ConfidenceVerified}},
	})
	if e.OverallState != EditSafetyUnknown {
		t.Fatalf("state = %q, want unknown without resolved impact", e.OverallState)
	}
}

func TestEditSafetyRequiresCompleteVerifiedBoundariesForSafe(t *testing.T) {
	base := EditSafetyEvidence{
		SemanticChanges:    []Change{{Kind: "declaration_changed", Identity: "pkg.A"}},
		ResolvedImpact:     []ImpactEvidence{{Symbol: "pkg.B", File: "b.go"}},
		RequiredBoundaries: []string{"graph"},
		Evidence:           []SafetyEvidence{{Boundary: "graph", Source: "index", Detail: "complete graph", State: EvidenceSatisfied, Confidence: ConfidenceVerified}},
	}
	if got := NewEditSafetyEvidence(base).OverallState; got != EditSafetySafe {
		t.Fatalf("complete evidence state = %q, want safe", got)
	}
	base.SemanticChanges[0].Identity = ""
	if got := NewEditSafetyEvidence(base).OverallState; got != EditSafetyUnknown {
		t.Fatalf("missing change identity state = %q, want unknown", got)
	}
	base.SemanticChanges[0].Identity = "pkg.A"
	base.ResolvedImpact[0].File = " "
	if got := NewEditSafetyEvidence(base).OverallState; got != EditSafetyUnknown {
		t.Fatalf("missing impact evidence state = %q, want unknown", got)
	}
	base.ResolvedImpact[0].File = "b.go"
	base.Evidence[0].Confidence = ConfidenceDerived
	if got := NewEditSafetyEvidence(base).OverallState; got != EditSafetyUnknown {
		t.Fatalf("derived evidence state = %q, want unknown", got)
	}
	base.Evidence[0].Confidence = ConfidenceVerified
	base.UnresolvedBoundaries = []SafetyBoundary{{Name: "target", Reason: "ambiguous"}}
	if got := NewEditSafetyEvidence(base).OverallState; got != EditSafetyUnknown {
		t.Fatalf("unresolved boundary state = %q, want unknown", got)
	}
	base.UnresolvedBoundaries = nil
	base.UnsupportedBoundaries = []SafetyBoundary{{Name: "language", Reason: "unsupported"}}
	if got := NewEditSafetyEvidence(base).OverallState; got != EditSafetyUnknown {
		t.Fatalf("unsupported boundary state = %q, want unknown", got)
	}
	base.UnsupportedBoundaries = nil
	base.Evidence = nil
	if got := NewEditSafetyEvidence(base).OverallState; got != EditSafetyUnknown {
		t.Fatalf("missing required evidence state = %q, want unknown", got)
	}
}

func TestEditSafetyAmbiguousSemanticChangeRemainsUnknown(t *testing.T) {
	e := EditSafetyEvidence{
		SemanticChanges:    []Change{{Kind: "declaration_changed", Identity: "pkg.A", Ambiguous: true}},
		ResolvedImpact:     []ImpactEvidence{{Symbol: "pkg.B", File: "b.go"}},
		RequiredBoundaries: []string{"graph"},
		Evidence:           []SafetyEvidence{{Boundary: "graph", Source: "index", Detail: "complete graph", State: EvidenceSatisfied, Confidence: ConfidenceVerified}},
	}
	if got := NewEditSafetyEvidence(e).OverallState; got != EditSafetyUnknown {
		t.Fatalf("ambiguous change state = %q, want unknown", got)
	}
}

func TestEditSafetyUnsafeRequiresExplicitEvidence(t *testing.T) {
	e := NewEditSafetyEvidence(EditSafetyEvidence{Evidence: []SafetyEvidence{{State: EvidenceUnsafe}}})
	if e.OverallState != EditSafetyUnknown {
		t.Fatalf("unsupported unsafe assertion state = %q, want unknown", e.OverallState)
	}
	e = NewEditSafetyEvidence(EditSafetyEvidence{Evidence: []SafetyEvidence{{
		Boundary: "constraint", Source: "constraint checker", Detail: "forbidden dependency", State: EvidenceUnsafe, Confidence: ConfidenceVerified,
	}}})
	if e.OverallState != EditSafetyUnsafe {
		t.Fatalf("explicit unsafe evidence state = %q, want unsafe", e.OverallState)
	}
}

func TestEditSafetyEvidenceOrderingIsDeterministic(t *testing.T) {
	e := EditSafetyEvidence{
		SemanticChanges: []Change{{Kind: "z", Identity: "z"}, {Kind: "a", Identity: "a"}},
		ResolvedImpact:  []ImpactEvidence{{Symbol: "z", File: "z.go"}, {Symbol: "a", File: "a.go"}},
		Evidence: []SafetyEvidence{
			{Boundary: "z", Source: "s", Detail: "d", State: EvidenceSatisfied, Confidence: ConfidenceVerified},
			{Boundary: "a", Source: "s", Detail: "d", State: EvidenceSatisfied, Confidence: ConfidenceVerified},
		},
		RequiredBoundaries: []string{"z", "a"},
	}
	first, err := json.Marshal(NewEditSafetyEvidence(e))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(NewEditSafetyEvidence(e))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("serialization differs: %s != %s", first, second)
	}
	ordered := NewEditSafetyEvidence(e)
	if ordered.SemanticChanges[0].Kind != "a" || ordered.ResolvedImpact[0].Symbol != "a" || ordered.Evidence[0].Boundary != "a" || ordered.RequiredBoundaries[0] != "a" {
		t.Fatalf("evidence not sorted: %+v", ordered)
	}
}

func TestEditSafetyOrderingUsesTieBreakFields(t *testing.T) {
	changeA := Change{Kind: "changed", Identity: "pkg.A", Before: json.RawMessage(`{"x":1}`)}
	changeB := Change{Kind: "changed", Identity: "pkg.A", Before: json.RawMessage(`{"x":2}`)}
	evidenceA := SafetyEvidence{Boundary: "graph", Source: "index", Detail: "same", State: EvidenceIncomplete, Confidence: ConfidenceUnknown}
	evidenceB := SafetyEvidence{Boundary: "graph", Source: "index", Detail: "same", State: EvidenceSatisfied, Confidence: ConfidenceVerified}
	a, err := json.Marshal(NewEditSafetyEvidence(EditSafetyEvidence{
		SemanticChanges: []Change{changeB, changeA}, Evidence: []SafetyEvidence{evidenceB, evidenceA},
	}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(NewEditSafetyEvidence(EditSafetyEvidence{
		SemanticChanges: []Change{changeA, changeB}, Evidence: []SafetyEvidence{evidenceA, evidenceB},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("tie ordering differs:\n%s\n%s", a, b)
	}
}
