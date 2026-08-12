package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteJSON emits the report as stable JSON. Formatting matches the CLI's own
// JSON writer: two-space indent, HTML escaping off.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText emits a concise human-readable summary suitable for CI logs.
func (r *Report) WriteText(w io.Writer) error {
	var b strings.Builder

	fmt.Fprintf(&b, "resolver correctness audit  fixture=%s cases=%d files=%d\n",
		r.Fixture.Version, r.Fixture.Cases, r.Fixture.Files)
	fmt.Fprintf(&b, "context: %s %s/%s cpus=%d sqlite=%s\n",
		r.Context.GoVersion, r.Context.GOOS, r.Context.GOARCH, r.Context.NumCPU, r.Context.SQLiteDriver)
	if r.Context.CommitSHA != "" {
		fmt.Fprintf(&b, "commit: %s\n", r.Context.CommitSHA)
	}

	m := r.Metrics
	fmt.Fprintf(&b, "\nedges: inspected=%d resolved=%d unresolved=%d cross_language=%d same_language=%d unknown_language=%d\n",
		m.CallEdgesInspected, m.Resolved, m.Unresolved, m.CrossLanguageResolved, m.SameLanguageResolved, m.UnknownLanguageResolved)
	fmt.Fprintf(&b, "ambiguity: cases_with_multiple_candidate_definitions=%d\n", m.AmbiguousCases)
	fmt.Fprintf(&b, "cases (unanimous across repeats): expected_valid=%d expected_unresolved=%d correctly_unresolved=%d miswired=%d missing_edge=%d no_call_edge=%d nondeterministic=%d\n",
		m.ExpectedValid, m.ExpectedUnresolved, m.CorrectlyUnresolved, m.Miswired,
		m.MissingEdge, m.NoCallEdge, m.Nondeterministic)
	fmt.Fprintf(&b, "cases (worst case, any repeat): miswired=%d test_shadow=%d\n",
		m.MiswiredAnyRun, m.TestShadowBindingsAnyRun)

	b.WriteString("\ncase outcomes:\n")
	for _, c := range r.Cases {
		fmt.Fprintf(&b, "  %-2s %-22s %-10s src=%s(%s) dst_name=%s candidates=%d\n",
			c.ID, c.Outcome, c.Expectation, c.SrcFile, c.SrcLanguage, c.DstName, c.CandidateCount)
		switch {
		case c.Selected != nil && c.Selected.Resolved:
			fmt.Fprintf(&b, "       -> selected %s (%s) in %s [kind=%s]\n",
				c.Selected.DstQualifiedName, c.Selected.DstLanguage, c.Selected.DstFile, c.Selected.DstKind)
		case c.Selected != nil:
			b.WriteString("       -> unresolved\n")
		default:
			for _, v := range c.SelectedVariants {
				fmt.Fprintf(&b, "       -> variant %s\n", v)
			}
		}
	}

	fmt.Fprintf(&b, "\ndeterminism (sampled over %d repeats): metrics_stable=%t case_outcomes_stable=%t selected_targets_stable=%t\n",
		r.Determinism.Repeats, r.Determinism.MetricsStable,
		r.Determinism.CaseOutcomesStable, r.Determinism.SelectedTargetsStable)
	fmt.Fprintf(&b, "  note: %s\n", r.Determinism.Note)
	for _, d := range r.Determinism.Divergences {
		fmt.Fprintf(&b, "  divergence: %s\n", d)
	}

	if r.Incremental != nil {
		fmt.Fprintf(&b, "\nincremental parity: full_mode=%s incremental_mode=%s agrees=%t\n",
			r.Incremental.FullResolveMode, r.Incremental.IncrementalResolveMode, r.Incremental.Agrees)
		for _, d := range r.Incremental.Divergences {
			fmt.Fprintf(&b, "  divergence: %s\n", d)
		}
	}

	if len(r.Unclassified) > 0 {
		fmt.Fprintf(&b, "\nunclassified call edges: %d\n", len(r.Unclassified))
		for _, e := range r.Unclassified {
			fmt.Fprintf(&b, "  %s:%d %s -> %s\n", e.SrcFile, e.Line, e.SrcQualifiedName, e.target())
		}
	}

	b.WriteString("\nfindings:\n")
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "  - %s\n", f)
	}

	_, err := io.WriteString(w, b.String())
	return err
}
