package diff

import "sort"

const EditSafetySchema = "codegraph.edit_safety/v1"

type EditSafetyState string

const (
	EditSafetySafe    EditSafetyState = "safe"
	EditSafetyUnsafe  EditSafetyState = "unsafe"
	EditSafetyUnknown EditSafetyState = "unknown"
)

type SafetyEvidenceState string

const (
	EvidenceSatisfied  SafetyEvidenceState = "satisfied"
	EvidenceUnsafe     SafetyEvidenceState = "unsafe"
	EvidenceIncomplete SafetyEvidenceState = "incomplete"
)

type EvidenceConfidence string

const (
	ConfidenceVerified EvidenceConfidence = "verified"
	ConfidenceDerived  EvidenceConfidence = "derived"
	ConfidenceUnknown  EvidenceConfidence = "unknown"
)

type ImpactEvidence struct {
	Symbol string `json:"symbol"`
	File   string `json:"file"`
}

type SafetyBoundary struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type SafetyEvidence struct {
	Boundary   string              `json:"boundary"`
	Source     string              `json:"source"`
	Detail     string              `json:"detail"`
	State      SafetyEvidenceState `json:"state"`
	Confidence EvidenceConfidence  `json:"confidence"`
}

// EditSafetyEvidence carries explainable evidence without inventing a score.
// Safe is derived only when every recorded boundary is verified and the
// assessment includes both semantic changes and resolved impact.
type EditSafetyEvidence struct {
	Schema                string           `json:"schema"`
	SemanticChanges       []Change         `json:"semantic_changes"`
	ResolvedImpact        []ImpactEvidence `json:"resolved_impact"`
	UnresolvedBoundaries  []SafetyBoundary `json:"unresolved_boundaries"`
	UnsupportedBoundaries []SafetyBoundary `json:"unsupported_boundaries"`
	RequiredBoundaries    []string         `json:"required_boundaries"`
	Evidence              []SafetyEvidence `json:"evidence"`
	OverallState          EditSafetyState  `json:"overall_state"`
}

// NewEditSafetyEvidence sorts the evidence and derives a conservative state.
func NewEditSafetyEvidence(e EditSafetyEvidence) EditSafetyEvidence {
	e.Schema = EditSafetySchema
	e.SemanticChanges = append([]Change(nil), e.SemanticChanges...)
	e.ResolvedImpact = append([]ImpactEvidence(nil), e.ResolvedImpact...)
	e.UnresolvedBoundaries = append([]SafetyBoundary(nil), e.UnresolvedBoundaries...)
	e.UnsupportedBoundaries = append([]SafetyBoundary(nil), e.UnsupportedBoundaries...)
	e.RequiredBoundaries = append([]string(nil), e.RequiredBoundaries...)
	e.Evidence = append([]SafetyEvidence(nil), e.Evidence...)
	if e.SemanticChanges == nil {
		e.SemanticChanges = []Change{}
	}
	if e.ResolvedImpact == nil {
		e.ResolvedImpact = []ImpactEvidence{}
	}
	if e.UnresolvedBoundaries == nil {
		e.UnresolvedBoundaries = []SafetyBoundary{}
	}
	if e.UnsupportedBoundaries == nil {
		e.UnsupportedBoundaries = []SafetyBoundary{}
	}
	if e.RequiredBoundaries == nil {
		e.RequiredBoundaries = []string{}
	}
	if e.Evidence == nil {
		e.Evidence = []SafetyEvidence{}
	}
	sort.Slice(e.SemanticChanges, func(i, j int) bool {
		if e.SemanticChanges[i].Kind != e.SemanticChanges[j].Kind {
			return e.SemanticChanges[i].Kind < e.SemanticChanges[j].Kind
		}
		return e.SemanticChanges[i].Identity < e.SemanticChanges[j].Identity
	})
	sort.Slice(e.ResolvedImpact, func(i, j int) bool {
		if e.ResolvedImpact[i].Symbol != e.ResolvedImpact[j].Symbol {
			return e.ResolvedImpact[i].Symbol < e.ResolvedImpact[j].Symbol
		}
		return e.ResolvedImpact[i].File < e.ResolvedImpact[j].File
	})
	sortBoundaries := func(values []SafetyBoundary) {
		sort.Slice(values, func(i, j int) bool {
			if values[i].Name != values[j].Name {
				return values[i].Name < values[j].Name
			}
			return values[i].Reason < values[j].Reason
		})
	}
	sortBoundaries(e.UnresolvedBoundaries)
	sortBoundaries(e.UnsupportedBoundaries)
	sort.Strings(e.RequiredBoundaries)
	sort.Slice(e.Evidence, func(i, j int) bool {
		a, b := e.Evidence[i], e.Evidence[j]
		if a.Boundary != b.Boundary {
			return a.Boundary < b.Boundary
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Detail < b.Detail
	})
	e.OverallState = deriveEditSafetyState(e)
	return e
}

func deriveEditSafetyState(e EditSafetyEvidence) EditSafetyState {
	for _, evidence := range e.Evidence {
		if evidence.State == EvidenceUnsafe && evidence.Source != "" && evidence.Detail != "" && evidence.Confidence != ConfidenceUnknown {
			return EditSafetyUnsafe
		}
	}
	if len(e.SemanticChanges) == 0 || len(e.ResolvedImpact) == 0 || len(e.RequiredBoundaries) == 0 || len(e.UnresolvedBoundaries) > 0 || len(e.UnsupportedBoundaries) > 0 {
		return EditSafetyUnknown
	}
	evidenceByBoundary := make(map[string]SafetyEvidence, len(e.Evidence))
	for _, evidence := range e.Evidence {
		if evidence.Boundary == "" || evidence.Source == "" || evidence.Detail == "" {
			return EditSafetyUnknown
		}
		if _, duplicate := evidenceByBoundary[evidence.Boundary]; duplicate {
			return EditSafetyUnknown
		}
		evidenceByBoundary[evidence.Boundary] = evidence
	}
	for _, boundary := range e.RequiredBoundaries {
		evidence, ok := evidenceByBoundary[boundary]
		if !ok || evidence.State != EvidenceSatisfied || evidence.Confidence != ConfidenceVerified {
			return EditSafetyUnknown
		}
	}
	return EditSafetySafe
}
