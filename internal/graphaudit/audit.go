// Package graphaudit audits an already-indexed repository graph for integrity,
// resolver-correctness, and trust defects.
//
// # What this is, and what it is not
//
// `codegraph doctor` answers "is CodeGraph and its database operational?" -- it
// looks at platform directories, the config file, the binary on PATH, the
// SQLite driver, and the database file's pragmas and integrity_check. It never
// reads a graph row.
//
// This package answers a different question: "how trustworthy is the graph
// inside that database?" It reads only graph rows and never looks at the
// installation. The two do not overlap and neither subsumes the other: a
// perfectly healthy installation can hold a graph full of dangling edges, and a
// pristine graph can sit in a database whose directory permissions are wrong.
//
// It is also distinct from internal/audit, which builds a synthetic adversarial
// fixture, indexes it, and measures resolver behaviour against known-correct
// answers. That harness proves things about the resolver; this package proves
// things about one user's existing database, and indexes nothing.
//
// # Observational, not corrective
//
// Every check is a statement that a persisted row violates an invariant the
// write paths are supposed to maintain. No check decides what a row should have
// contained instead. Audit will report that an edge is bound across a language
// boundary an implicit strategy cannot cross; it will not report that the edge
// "should have pointed at Foo". Determining the correct destination is
// resolution, and re-running resolution to grade resolution would only prove
// the resolver agrees with itself.
//
// # Read-only
//
// Run issues nothing but SELECT. Callers are expected to hand it a store opened
// with store.OpenReadOnly, so the guarantee is enforced by the SQLite driver
// rather than by review. Nothing here indexes, re-resolves, backfills, or
// repairs -- in particular the legacy pre-019 rows reported by
// CodeResolvedMissingMetadata are reported and left alone.
package graphaudit

import (
	"context"
	"errors"
	"fmt"

	"github.com/isink17/codegraph/internal/classify"
	"github.com/isink17/codegraph/internal/store"
)

// SchemaID versions the report shape. Consumers should check it before reading
// fields; a breaking change to the contract bumps the version.
const SchemaID = "codegraph.graph_audit/v1"

// DefaultExampleLimit is how many examples a finding carries unless the caller
// asks for more. Small on purpose: a report's job is to say how many edges are
// wrong and show enough of them to start debugging, not to be a second copy of
// the graph. See Options.ExampleLimit.
const DefaultExampleLimit = 5

// Severity is the three-level finding severity.
type Severity string

const (
	// SeverityError marks a graph state no write path can legitimately
	// produce: a broken reference, or metadata that contradicts itself.
	SeverityError Severity = "error"
	// SeverityWarning marks a state that is legal but reduces trust: missing
	// legacy provenance, or a binding made on weak evidence.
	SeverityWarning Severity = "warning"
	// SeverityInfo marks composition reported for context, never a defect.
	SeverityInfo Severity = "info"
)

// Status is the overall verdict, derived deterministically from the findings:
// the highest severity present, or StatusOK when no finding fired.
type Status string

const (
	StatusOK      Status = "ok"
	StatusWarning Status = "warning"
	StatusError   Status = "error"
)

// Stable finding codes. These are a contract: they appear in JSON consumed by
// CI and by agents, so they are added and deprecated, never renamed in place.
const (
	// CodeDanglingEdgeTarget: edges.dst_symbol_id points at a symbol row that
	// does not exist. The column carries no foreign key, so nothing but the
	// unbind paths keeps this at zero.
	CodeDanglingEdgeTarget = "dangling_edge_target"

	// CodeDanglingEdgeSource: edges.src_symbol_id points at a symbol row that
	// does not exist. Also unenforced, and unlike the target it can never be
	// legitimately NULL, so any miss is corruption.
	CodeDanglingEdgeSource = "dangling_edge_source"

	// CodeDanglingTestLinkReference: a test_links row references a symbol or
	// file that does not exist. NULL references are not counted: they are the
	// documented result of unbinding, not a break.
	CodeDanglingTestLinkReference = "dangling_test_link_reference"

	// CodeInvalidResolutionMetadata: the pair (resolution_strategy,
	// resolution_confidence) is in a state no writer can produce -- present on
	// an unresolved edge, half-populated, an unregistered strategy, or a
	// confidence that disagrees with the tier its strategy maps to.
	CodeInvalidResolutionMetadata = "invalid_resolution_metadata"

	// CodeImplicitCrossLanguageBinding: an edge bound by an implicit strategy
	// connects two different languages. Every implicit strategy runs under the
	// P2 language gate, so this is unreachable by construction; a row in this
	// state means the gate was bypassed or the row predates it.
	CodeImplicitCrossLanguageBinding = "implicit_cross_language_binding"

	// CodeResolvedTargetDeletedFile: a bound destination lives in a file marked
	// is_deleted. The symbol row still exists, so this is not dangling -- it is
	// a binding into graph state that has been retired but not purged.
	CodeResolvedTargetDeletedFile = "resolved_target_deleted_file"

	// CodeResolvedMissingMetadata: an edge is bound but carries no provenance.
	// Expected on rows written before migration 019, which deliberately did not
	// backfill. Reported so the untrusted population is visible; never fixed
	// here, because a reconstructed strategy would be a guess.
	CodeResolvedMissingMetadata = "resolved_missing_metadata"

	// CodeLowConfidenceResolution: an edge was bound by low-tier evidence --
	// the LIKE-based dot_suffix fallback, or a cross-language link derived from
	// name or import-path coincidence rather than a call site.
	CodeLowConfidenceResolution = "low_confidence_resolution"

	// CodeCrossLanguageEdgeImplicitStrategy: a cross_language_ref edge whose
	// provenance names an implicit same-language strategy. The kind and the
	// strategy contradict each other, which happens when such an edge is
	// unbound by a re-index and then re-bound by the ordinary resolver.
	CodeCrossLanguageEdgeImplicitStrategy = "cross_language_edge_implicit_strategy"
)

// Finding is one check that fired. Findings with a zero count are omitted from
// the report entirely, so the report's size scales with the number of distinct
// defect kinds present plus the example cap -- not with the size of the graph.
type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	// Count is the full population of violating rows, independent of how many
	// examples were returned.
	Count   int64  `json:"count"`
	Message string `json:"message"`
	// Examples is capped at Options.ExampleLimit and ordered by row id, so the
	// same database yields the same sample every run.
	Examples []store.EdgeAuditExample `json:"examples,omitempty"`
	// TestLinkExamples carries examples for findings about test_links rather
	// than edges. Exactly one of the two example fields is ever populated,
	// decided by the code.
	TestLinkExamples []store.TestLinkAuditExample `json:"test_link_examples,omitempty"`
}

// SkippedCheck records a check that could not run against this database.
//
// A skipped check is deliberately not reported as a passing one. "No violations
// found" and "the question could not be asked" are different answers, and
// collapsing them would let an audit of an old database look cleaner than an
// audit of a current one.
type SkippedCheck struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// Repository identifies what was audited.
type Repository struct {
	Root          string `json:"root"`
	CanonicalPath string `json:"canonical_path"`
	DBPath        string `json:"db_path"`
}

// Report is the machine-readable audit result and the stable contract shared by
// the CLI and the MCP tool. Both render this exact struct; there is no second
// serialisation.
type Report struct {
	Schema     string                  `json:"schema"`
	Repository Repository              `json:"repository"`
	Graph      store.GraphAuditSummary `json:"graph"`
	Status     Status                  `json:"status"`
	// SchemaVersion is the highest migration applied to the audited database.
	// Audit never migrates, so this can lag the binary's own schema.
	SchemaVersion int64 `json:"schema_version"`
	// SkippedChecks lists checks this database's schema cannot answer, with the
	// reason. Empty on a current schema.
	SkippedChecks []SkippedCheck `json:"skipped_checks,omitempty"`
	// ExampleLimit echoes the cap actually applied, so a reader can tell a
	// truncated example list from an exhaustive one.
	ExampleLimit int `json:"example_limit"`
	// Findings holds only the checks that fired, in a fixed catalogue order:
	// errors first, then warnings, each group in declaration order.
	Findings []Finding `json:"findings"`
	// UnresolvedClassification counts unresolved edges by what their target
	// name can be shown to be. Informational: `unknown` means the evidence did
	// not support a stronger claim, not that anything is wrong.
	UnresolvedClassification map[string]int64 `json:"unresolved_classification"`
	// ResolutionStrategy and ResolutionConfidence are distributions over
	// resolved edges. The empty-string key is the legacy pre-019 population.
	ResolutionStrategy   map[string]int64 `json:"resolution_strategy"`
	ResolutionConfidence map[string]int64 `json:"resolution_confidence"`
}

// Options configures a run.
type Options struct {
	// RepoID scopes every query. Required: a database may hold more than one
	// repository, and an unscoped audit would attribute one repo's defects to
	// another.
	RepoID int64
	// Repository is echoed into the report for provenance.
	Repository Repository
	// ExampleLimit caps examples per finding. Zero means DefaultExampleLimit;
	// negative means no examples at all (counts only).
	ExampleLimit int
}

// edgeCheck binds a store-level invariant to its reporting vocabulary.
type edgeCheck struct {
	code     string
	severity Severity
	check    store.EdgeAuditCheck
	message  string
}

// edgeCheckCatalogue is the fixed order findings are emitted in. Errors precede
// warnings so a truncated read of the findings list sees the worst first, and
// the order within each group is stable across runs and releases.
var edgeCheckCatalogue = []edgeCheck{
	{
		code:     CodeDanglingEdgeTarget,
		severity: SeverityError,
		check:    store.EdgeCheckDanglingTarget,
		message:  "edges resolve to a symbol id that no longer exists",
	},
	{
		code:     CodeDanglingEdgeSource,
		severity: SeverityError,
		check:    store.EdgeCheckDanglingSource,
		message:  "edges originate from a symbol id that no longer exists",
	},
	{
		code:     CodeInvalidResolutionMetadata,
		severity: SeverityError,
		check:    store.EdgeCheckInvalidResolutionMetadata,
		message:  "edges carry resolution metadata no write path can produce",
	},
	{
		code:     CodeImplicitCrossLanguageBinding,
		severity: SeverityError,
		check:    store.EdgeCheckImplicitCrossLanguage,
		message:  "edges bound by an implicit strategy cross a language boundary that strategy cannot cross",
	},
	{
		code:     CodeResolvedTargetDeletedFile,
		severity: SeverityError,
		check:    store.EdgeCheckResolvedTargetDeletedFile,
		message:  "edges resolve into a file marked deleted but not yet purged",
	},
	{
		code:     CodeResolvedMissingMetadata,
		severity: SeverityWarning,
		check:    store.EdgeCheckResolvedMissingMetadata,
		message:  "resolved edges carry no resolution provenance (expected for rows written before migration 019)",
	},
	{
		code:     CodeLowConfidenceResolution,
		severity: SeverityWarning,
		check:    store.EdgeCheckLowConfidenceResolution,
		message:  "resolved edges were bound by low-confidence evidence",
	},
	{
		code:     CodeCrossLanguageEdgeImplicitStrategy,
		severity: SeverityWarning,
		check:    store.EdgeCheckCrossLanguageImplicitStrategy,
		message:  "cross-language edges carry an implicit same-language resolution strategy",
	},
}

// Run audits the repository and returns the report.
//
// s must be a read-only handle for the guarantee in the package comment to
// hold; Run itself issues only SELECT either way. It performs a fixed number of
// aggregate queries -- one COUNT per check, one sampling SELECT per check that
// fired, two GROUP BY distributions, one summary, and one paged walk of the
// unresolved population -- so the query count depends on how many checks fire,
// never on how many edges exist.
func Run(ctx context.Context, s *store.Store, opts Options) (*Report, error) {
	exampleLimit := opts.ExampleLimit
	if exampleLimit == 0 {
		exampleLimit = DefaultExampleLimit
	}
	if exampleLimit < 0 {
		exampleLimit = 0
	}

	caps, err := s.GraphAuditCapabilitiesFor(ctx)
	if err != nil {
		return nil, fmt.Errorf("schema capabilities: %w", err)
	}

	summary, err := s.GraphAuditSummaryFor(ctx, opts.RepoID)
	if err != nil {
		return nil, fmt.Errorf("graph summary: %w", err)
	}

	report := &Report{
		Schema:        SchemaID,
		Repository:    opts.Repository,
		Graph:         summary,
		SchemaVersion: caps.SchemaVersion,
		ExampleLimit:  exampleLimit,
		Findings:      []Finding{},
	}

	const preP4Reason = "database predates migration 019; edges carry no resolution metadata to check"

	for _, spec := range edgeCheckCatalogue {
		result, err := s.RunEdgeAuditCheck(ctx, opts.RepoID, spec.check, caps, exampleLimit)
		if errors.Is(err, store.ErrAuditCheckUnsupported) {
			report.SkippedChecks = append(report.SkippedChecks, SkippedCheck{Code: spec.code, Reason: preP4Reason})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("check %s: %w", spec.code, err)
		}
		if result.Count == 0 {
			continue
		}
		report.Findings = append(report.Findings, Finding{
			Code:     spec.code,
			Severity: spec.severity,
			Count:    result.Count,
			Message:  spec.message,
			Examples: result.Examples,
		})
	}

	// test_links has its own row shape, so it is not part of the edge
	// catalogue. It is inserted after the edge errors and before the warnings
	// to keep the errors-then-warnings ordering of the findings list.
	testLinks, err := s.RunDanglingTestLinkCheck(ctx, opts.RepoID, exampleLimit)
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", CodeDanglingTestLinkReference, err)
	}
	if testLinks.Count > 0 {
		report.Findings = insertFinding(report.Findings, Finding{
			Code:             CodeDanglingTestLinkReference,
			Severity:         SeverityError,
			Count:            testLinks.Count,
			Message:          "test links reference a symbol or file that no longer exists",
			TestLinkExamples: testLinks.Examples,
		})
	}

	if report.UnresolvedClassification, err = s.UnresolvedTargetClassificationCounts(ctx, opts.RepoID, caps); err != nil {
		return nil, fmt.Errorf("unresolved classification: %w", err)
	}
	seedClassifications(report.UnresolvedClassification)

	// The distributions describe resolved edges only, so on a pre-019 schema
	// they are empty rather than absent -- an empty map says "no provenance
	// recorded", which is exactly the state of such a database.
	report.ResolutionStrategy = map[string]int64{}
	report.ResolutionConfidence = map[string]int64{}
	if caps.HasResolutionMetadata {
		if report.ResolutionStrategy, err = s.ResolutionStrategyDistribution(ctx, opts.RepoID, caps); err != nil {
			return nil, fmt.Errorf("strategy distribution: %w", err)
		}
		if report.ResolutionConfidence, err = s.ResolutionConfidenceDistribution(ctx, opts.RepoID, caps); err != nil {
			return nil, fmt.Errorf("confidence distribution: %w", err)
		}
	}

	report.Status = statusFor(report.Findings)
	return report, nil
}

// insertFinding places f after the last error finding, preserving the
// errors-before-warnings invariant of the findings list without re-sorting it.
func insertFinding(findings []Finding, f Finding) []Finding {
	at := len(findings)
	for i := range findings {
		if findings[i].Severity != SeverityError {
			at = i
			break
		}
	}
	findings = append(findings, Finding{})
	copy(findings[at+1:], findings[at:])
	findings[at] = f
	return findings
}

// seedClassifications ensures the four classification keys are always present,
// so a consumer can read counts["builtin"] without distinguishing "none" from
// "field absent". Keys the classifier produced that are not in the seed set are
// left in place rather than dropped -- an unexpected `project` among unresolved
// edges is exactly the kind of thing an audit should not hide.
func seedClassifications(counts map[string]int64) {
	for _, key := range []string{classify.Builtin, classify.Stdlib, classify.External, classify.Unknown} {
		if _, ok := counts[key]; !ok {
			counts[key] = 0
		}
	}
}

// statusFor derives the overall status from the findings: the highest severity
// present. It is a pure function of the findings list, so the same graph always
// produces the same status.
func statusFor(findings []Finding) Status {
	status := StatusOK
	for i := range findings {
		switch findings[i].Severity {
		case SeverityError:
			return StatusError
		case SeverityWarning:
			status = StatusWarning
		}
	}
	return status
}

// FailOn is the CI exit policy. It affects the process exit code only; the
// report is byte-identical under every policy.
type FailOn string

const (
	// FailOnNone always exits zero. The default: an audit that reports defects
	// has succeeded at its job, and making that a command failure would break
	// every pipeline that merely wants the report.
	FailOnNone FailOn = "none"
	// FailOnError exits non-zero when any error finding is present.
	FailOnError FailOn = "error"
	// FailOnWarning exits non-zero on any warning or error finding.
	FailOnWarning FailOn = "warning"
)

// ParseFailOn validates a --fail-on value.
func ParseFailOn(value string) (FailOn, error) {
	switch FailOn(value) {
	case FailOnNone, FailOnError, FailOnWarning:
		return FailOn(value), nil
	}
	return "", fmt.Errorf("invalid --fail-on %q: want one of none, error, warning", value)
}

// ShouldFail reports whether a status violates the policy.
func (f FailOn) ShouldFail(status Status) bool {
	switch f {
	case FailOnError:
		return status == StatusError
	case FailOnWarning:
		return status == StatusError || status == StatusWarning
	default:
		return false
	}
}
