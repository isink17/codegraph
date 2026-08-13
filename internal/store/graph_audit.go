package store

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// Read-only graph invariant checks (P9).
//
// These are the queries behind `codegraph audit`. They live in this package
// because they are statements about the schema -- which column may be NULL,
// which pair of columns must agree, which join must find a row -- and the
// schema lives here. internal/graphaudit consumes them and owns the reporting
// vocabulary (codes, severities, messages); it never writes SQL.
//
// Three rules constrain everything in this file:
//
//  1. Observational only. A check may prove that an existing row violates an
//     invariant. It may not decide what the row should have been instead. There
//     is no candidate ranking here and no second resolver.
//  2. Aggregate first, sample second. Every check is one COUNT(*) plus, only
//     when the count is non-zero and examples were requested, one LIMIT-ed
//     SELECT. Nothing iterates edges in Go to count them.
//  3. Derived from the source of truth, never restated. The strategy and
//     confidence value sets come from resolutionConfidenceByStrategy in
//     edge_resolution.go, rendered into SQL at call time. Adding a strategy
//     there changes these checks automatically; it cannot leave a hand-copied
//     list in this file stale.

// EdgeKindCrossLanguageRef is the edge_kind ResolveCrossLanguageLinks writes.
// It is the only kind whose endpoints are expected to be in different
// languages, and the only one not produced by a parser from a call site.
//
// The two INSERT statements in ResolveCrossLanguageLinks spell it inline inside
// their SQL text; this constant names the same value for readers rather than
// rewriting those statements, and edge_resolution_test.go pins the written
// value.
const EdgeKindCrossLanguageRef = "cross_language_ref"

// EdgeAuditExample identifies one violating edge. It carries identity only --
// the endpoints, the recorded target name, and the resolution provenance --
// because examples exist to be looked up by a human or an agent, not to
// reconstruct the violation offline.
type EdgeAuditExample struct {
	EdgeID     int64  `json:"edge_id"`
	SrcFile    string `json:"src_file,omitempty"`
	SrcSymbol  string `json:"src_symbol,omitempty"`
	DstName    string `json:"dst_name,omitempty"`
	DstFile    string `json:"dst_file,omitempty"`
	DstSymbol  string `json:"dst_symbol,omitempty"`
	EdgeKind   string `json:"edge_kind,omitempty"`
	SrcLang    string `json:"src_language,omitempty"`
	DstLang    string `json:"dst_language,omitempty"`
	Strategy   string `json:"resolution_strategy,omitempty"`
	Confidence string `json:"resolution_confidence,omitempty"`
	// Detail names which clause of a multi-clause check this row tripped. It is
	// empty for single-clause checks.
	Detail string `json:"detail,omitempty"`
}

// TestLinkAuditExample identifies one violating test_links row.
type TestLinkAuditExample struct {
	TestLinkID     int64  `json:"test_link_id"`
	TestFile       string `json:"test_file,omitempty"`
	TestSymbolID   int64  `json:"test_symbol_id,omitempty"`
	TargetFileID   int64  `json:"target_file_id,omitempty"`
	TargetSymbolID int64  `json:"target_symbol_id,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// EdgeAuditResult is a total count plus a bounded, deterministically ordered
// sample. Count is always the full population; Examples is capped.
type EdgeAuditResult struct {
	Count    int64
	Examples []EdgeAuditExample
}

// TestLinkAuditResult is EdgeAuditResult for test_links.
type TestLinkAuditResult struct {
	Count    int64
	Examples []TestLinkAuditExample
}

// GraphAuditSummary describes the size of the graph being audited, so a report
// carries the denominator its counts are relative to.
type GraphAuditSummary struct {
	Files           int64 `json:"files"`
	DeletedFiles    int64 `json:"deleted_files"`
	Symbols         int64 `json:"symbols"`
	Edges           int64 `json:"edges"`
	ResolvedEdges   int64 `json:"resolved_edges"`
	UnresolvedEdges int64 `json:"unresolved_edges"`
	TestLinks       int64 `json:"test_links"`
}

// GraphAuditCapabilities records what the database being audited can actually
// be asked.
//
// A production audit runs against whatever database the user already has, which
// may predate any given migration -- migration 019 added the resolution
// metadata columns and deliberately did not backfill, and OpenReadOnly does not
// migrate. Querying e.resolution_strategy on a pre-019 database is not a
// warning, it is a "no such column" error that would fail the whole run.
//
// So capability is detected once, from the schema itself rather than from the
// recorded migration version: a column that exists can be queried regardless of
// what schema_migrations claims, and a hand-modified database is described
// accurately instead of optimistically.
type GraphAuditCapabilities struct {
	// SchemaVersion is the highest applied migration, reported for context.
	SchemaVersion int64
	// HasResolutionMetadata is true when edges.resolution_strategy and
	// edges.resolution_confidence both exist (migration 019).
	HasResolutionMetadata bool
}

// GraphAuditCapabilitiesFor inspects the schema of the open database.
func (s *Store) GraphAuditCapabilitiesFor(ctx context.Context) (GraphAuditCapabilities, error) {
	var caps GraphAuditCapabilities
	// A database with no schema_migrations row is possible in principle; treat
	// the version as 0 rather than failing, since the column probe below is
	// what actually decides behaviour.
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&caps.SchemaVersion)

	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(edges)`)
	if err != nil {
		return GraphAuditCapabilities{}, err
	}
	defer rows.Close()
	columns := map[string]struct{}{}
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			defaultVal any
			pk         int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defaultVal, &pk); err != nil {
			return GraphAuditCapabilities{}, err
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return GraphAuditCapabilities{}, err
	}
	_, hasStrategy := columns["resolution_strategy"]
	_, hasConfidence := columns["resolution_confidence"]
	caps.HasResolutionMetadata = hasStrategy && hasConfidence
	return caps, nil
}

// EdgeAuditCheck names one invariant over `edges`.
type EdgeAuditCheck int

const (
	// EdgeCheckDanglingTarget: a bound destination whose symbol row is gone.
	// CountDanglingEdgeTargets states the same invariant; this adds examples.
	EdgeCheckDanglingTarget EdgeAuditCheck = iota
	// EdgeCheckDanglingSource: an edge whose source symbol row is gone.
	// src_symbol_id is NOT NULL but carries no foreign key.
	EdgeCheckDanglingSource
	// EdgeCheckInvalidResolutionMetadata: the resolution columns are in a state
	// no write path can produce. Four disjoint clauses, see the predicate.
	EdgeCheckInvalidResolutionMetadata
	// EdgeCheckImplicitCrossLanguage: an edge bound by an implicit strategy
	// whose endpoints are in different languages.
	EdgeCheckImplicitCrossLanguage
	// EdgeCheckResolvedTargetDeletedFile: a bound destination in a file marked
	// deleted but not yet purged.
	EdgeCheckResolvedTargetDeletedFile
	// EdgeCheckResolvedMissingMetadata: bound, with no provenance at all. The
	// expected state for rows written before migration 019.
	EdgeCheckResolvedMissingMetadata
	// EdgeCheckLowConfidenceResolution: bound by low-tier evidence.
	EdgeCheckLowConfidenceResolution
	// EdgeCheckCrossLanguageImplicitStrategy: a cross_language_ref edge whose
	// provenance says an implicit same-language strategy bound it.
	EdgeCheckCrossLanguageImplicitStrategy
)

// auditEdgeFromSQL is the FROM clause every edge check's example query uses,
// and the COUNT of any check whose predicate reads a joined table.
//
// All joins are LEFT so a check can assert that a join found nothing; an inner
// join would silently drop the very rows the dangling checks look for.
//
// Every join is additionally scoped to the edge's own repo_id. Nothing in the
// schema stops two repositories sharing a database, and an edge in repo A bound
// to a symbol owned by repo B is a broken binding, not a valid one: without the
// scope that row would join successfully and read as healthy, and the language
// comparison would be made against a foreign symbol. Scoping makes such a row
// report as dangling, which is the truth about it.
const auditEdgeFromSQL = `
	FROM edges e
	LEFT JOIN files sf ON sf.id = e.file_id AND sf.repo_id = e.repo_id
	LEFT JOIN symbols ss ON ss.id = e.src_symbol_id AND ss.repo_id = e.repo_id
	LEFT JOIN symbols ds ON ds.id = e.dst_symbol_id AND ds.repo_id = e.repo_id
	LEFT JOIN files df ON df.id = ds.file_id AND df.repo_id = e.repo_id
`

// auditEdgeOnlyFromSQL is auditEdgeFromSQL without the joins, for counting
// checks whose predicate reads nothing but `edges`.
//
// The joins are all LEFT and all on rowid, so dropping them cannot change a
// count -- but keeping them costs four index probes per matching edge. On a
// multi-million-edge graph that is tens of millions of wasted lookups for a
// predicate that only ever looks at e.resolution_confidence.
const auditEdgeOnlyFromSQL = `
	FROM edges e
`

// auditEdgeExampleColumnsFor is the example projection, matching the Scan order
// in RunEdgeAuditCheck.
//
// The two resolution columns become empty-string literals on a pre-019 schema,
// where selecting them by name would be a "no such column" error rather than a
// NULL. The projection stays the same width and order either way, so one Scan
// handles both schemas and an example from an old database simply reports no
// provenance -- which is the truth about it.
func auditEdgeExampleColumnsFor(caps GraphAuditCapabilities) string {
	const identity = `e.id, COALESCE(sf.path, ''), COALESCE(ss.qualified_name, ''), e.dst_name, ` +
		`COALESCE(df.path, ''), COALESCE(ds.qualified_name, ''), e.edge_kind, ` +
		`COALESCE(sf.language, ''), COALESCE(ds.language, '')`
	if caps.HasResolutionMetadata {
		return identity + `, e.resolution_strategy, e.resolution_confidence`
	}
	return identity + `, '', ''`
}

// unresolvedClassificationColumnsFor is exportEdgeColumnsSQL, with the same
// substitution and for the same reason. It MUST stay positionally identical to
// exportEdgeColumnsSQL, because scanExportEdges reads both by position.
func unresolvedClassificationColumnsFor(caps GraphAuditCapabilities) string {
	if caps.HasResolutionMetadata {
		return exportEdgeColumnsSQL
	}
	return `e.id, e.src_symbol_id, COALESCE(src.qualified_name, ''), e.dst_symbol_id, ` +
		`COALESCE(dst.qualified_name, ''), e.dst_name, e.edge_kind, COALESCE(f.path, ''), e.line, ` +
		`'', '', e.file_id, COALESCE(f.language, ''), e.evidence`
}

// sqlStringList renders values as a parenthesised SQL literal list. Callers
// pass compile-time constants from edge_resolution.go, never user input; the
// values are sorted so statement text is reproducible.
func sqlStringList(values []string) string {
	ordered := append([]string(nil), values...)
	sort.Strings(ordered)
	quoted := make([]string, len(ordered))
	for i, v := range ordered {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}

// knownResolutionStrategies returns every registered strategy.
func knownResolutionStrategies() []string {
	out := make([]string, 0, len(resolutionConfidenceByStrategy))
	for strategy := range resolutionConfidenceByStrategy {
		out = append(out, strategy)
	}
	return out
}

// explicitCrossLanguageStrategies returns the strategies that legitimately bind
// across languages. These are the only provenance values for which a
// cross-language edge is expected rather than a defect, so they are the exact
// exclusion list for EdgeCheckImplicitCrossLanguage.
func explicitCrossLanguageStrategies() []string {
	return []string{
		ResolutionStrategyCrossLanguageSharedName,
		ResolutionStrategyCrossLanguageImportPath,
	}
}

// implicitResolutionStrategies returns every registered strategy that is not an
// explicit cross-language one. Derived by subtraction rather than listed, so a
// new implicit strategy is covered the day it is registered.
func implicitResolutionStrategies() []string {
	explicit := map[string]struct{}{}
	for _, s := range explicitCrossLanguageStrategies() {
		explicit[s] = struct{}{}
	}
	out := make([]string, 0, len(resolutionConfidenceByStrategy))
	for strategy := range resolutionConfidenceByStrategy {
		if _, skip := explicit[strategy]; skip {
			continue
		}
		out = append(out, strategy)
	}
	return out
}

// confidenceMismatchSQL renders "the confidence column disagrees with the tier
// this strategy is defined to have", straight from resolutionConfidenceByStrategy.
// Rendering it keeps the check honest: it compares against the same map the
// writers use, so the two cannot drift apart.
func confidenceMismatchSQL() string {
	strategies := knownResolutionStrategies()
	sort.Strings(strategies)
	var b strings.Builder
	b.WriteString("CASE e.resolution_strategy")
	for _, strategy := range strategies {
		b.WriteString(" WHEN '" + strategy + "' THEN '" + resolutionConfidenceFor(strategy) + "'")
	}
	// No ELSE: an unregistered strategy yields NULL here, which makes the
	// comparison NULL rather than true. That clause is covered separately by
	// the unknown-strategy clause, so a row is reported once, not twice.
	b.WriteString(" END")
	return "e.resolution_confidence <> " + b.String()
}

// ErrAuditCheckUnsupported reports that a check cannot run against this
// database's schema. Callers report the check as skipped, with a reason,
// rather than as passing: "not asked" and "asked and found nothing" are
// different answers and an audit must not conflate them.
var ErrAuditCheckUnsupported = errors.New("store: audit check unsupported by this schema")

// crossLanguageSymbolMismatchSQL asserts that an edge's two endpoints are in
// genuinely different languages.
//
// It compares `symbols.language` on both ends, not `files.language`. The
// distinction matters because the two can legitimately disagree for a while:
// TouchFilesMetadataBatch rewrites files.language from freshly detected
// metadata for any file whose content hash is unchanged, without re-parsing it,
// so a release that changes an extension-to-language mapping (this codebase
// already collapses C into cpp and JS into typescript) leaves files.language
// new and that file's symbols.language old. Comparing file language against
// destination symbol language would then flag every resolved intra-file edge in
// that file as an error -- turning a version upgrade into a red build with no
// repository change.
//
// Both symbol languages are written by the same parse, so they move together
// and cannot drift apart that way. A real gate bypass still shows up: the two
// symbols really are in different languages.
//
// Empty on either side is absence of evidence, not proof of a mismatch, and the
// resolver's own gate fails closed on it -- so audit refuses to call it a
// violation.
const crossLanguageSymbolMismatchSQL = `ss.language <> '' AND ds.language <> '' AND ss.language <> ds.language`

// edgeAuditPredicate returns the WHERE fragment and the CASE expression that
// labels which clause a row tripped.
//
// Every predicate here is a statement about persisted columns only. None of
// them consults candidate sets, name similarity, or anything else that would
// amount to re-running resolution.
//
// caps decides which checks are answerable at all: the five that read the P4
// resolution columns return ErrAuditCheckUnsupported on a pre-019 schema.
func edgeAuditPredicate(check EdgeAuditCheck, caps GraphAuditCapabilities) (predicate string, detail string, needsJoins bool, err error) {
	if !caps.HasResolutionMetadata {
		switch check {
		case EdgeCheckInvalidResolutionMetadata,
			EdgeCheckResolvedMissingMetadata,
			EdgeCheckLowConfidenceResolution,
			EdgeCheckCrossLanguageImplicitStrategy:
			return "", "", false, ErrAuditCheckUnsupported
		case EdgeCheckImplicitCrossLanguage:
			// This one survives without provenance. Before migration 019 the
			// only marker of a deliberate cross-language link is the edge kind
			// ResolveCrossLanguageLinks writes, and it is a sound one: every
			// other kind comes from a parser reading a call site, and every
			// strategy that binds such an edge runs under the P2 language gate.
			// Excluding that kind therefore excludes exactly the explicit
			// links, which is what the P4 strategy list does on a newer schema.
			return "e.dst_symbol_id IS NOT NULL" +
				" AND e.edge_kind <> '" + EdgeKindCrossLanguageRef + "'" +
				" AND " + crossLanguageSymbolMismatchSQL, "''", true, nil
		}
	}
	switch check {
	case EdgeCheckDanglingTarget:
		return "e.dst_symbol_id IS NOT NULL AND ds.id IS NULL", "''", true, nil

	case EdgeCheckDanglingSource:
		return "ss.id IS NULL", "''", true, nil

	case EdgeCheckInvalidResolutionMetadata:
		known := sqlStringList(knownResolutionStrategies())
		// Four disjoint impossible states:
		//  1. unresolved but carrying provenance -- every unbind path clears
		//     both columns together (resolverClearResolutionSQL);
		//  2. resolved with a strategy no writer can emit;
		//  3. resolved with a confidence the strategy does not map to --
		//     confidence is a pure function of strategy;
		//  4. resolved with exactly one of the two columns set -- both writers
		//     set them in a single SET clause.
		// Resolved-with-both-empty is deliberately absent: that is the legacy
		// pre-019 state, reported by EdgeCheckResolvedMissingMetadata as a
		// warning, not corruption.
		stale := "(e.dst_symbol_id IS NULL AND (e.resolution_strategy <> '' OR e.resolution_confidence <> ''))"
		unknown := "(e.dst_symbol_id IS NOT NULL AND e.resolution_strategy <> '' AND e.resolution_strategy NOT IN " + known + ")"
		mismatch := "(e.dst_symbol_id IS NOT NULL AND e.resolution_strategy IN " + known + " AND " + confidenceMismatchSQL() + ")"
		half := "(e.dst_symbol_id IS NOT NULL AND ((e.resolution_strategy <> '' AND e.resolution_confidence = '') OR (e.resolution_strategy = '' AND e.resolution_confidence <> '')))"
		predicate = stale + " OR " + unknown + " OR " + mismatch + " OR " + half
		detail = "CASE" +
			" WHEN " + stale + " THEN 'unresolved_edge_carries_resolution_metadata'" +
			" WHEN " + unknown + " THEN 'unknown_resolution_strategy'" +
			" WHEN " + half + " THEN 'partial_resolution_metadata'" +
			" WHEN " + mismatch + " THEN 'confidence_does_not_match_strategy'" +
			" ELSE '' END"
		return predicate, detail, false, nil

	case EdgeCheckImplicitCrossLanguage:
		// Both languages must be non-empty. An empty language is absence of
		// evidence, not proof of a mismatch, and the resolver's own gate fails
		// closed on it -- so audit refuses to call it a violation.
		return "e.dst_symbol_id IS NOT NULL" +
			" AND e.resolution_strategy IN " + sqlStringList(implicitResolutionStrategies()) +
			" AND " + crossLanguageSymbolMismatchSQL, "''", true, nil

	case EdgeCheckResolvedTargetDeletedFile:
		// ds.id IS NOT NULL keeps this disjoint from EdgeCheckDanglingTarget: a
		// purged destination is dangling, a soft-deleted one is stale.
		return "e.dst_symbol_id IS NOT NULL AND ds.id IS NOT NULL AND df.id IS NOT NULL AND df.is_deleted = 1", "''", true, nil

	case EdgeCheckResolvedMissingMetadata:
		return "e.dst_symbol_id IS NOT NULL AND e.resolution_strategy = '' AND e.resolution_confidence = ''", "''", false, nil

	case EdgeCheckLowConfidenceResolution:
		return "e.dst_symbol_id IS NOT NULL AND e.resolution_confidence = '" + ResolutionConfidenceLow + "'", "''", false, nil

	case EdgeCheckCrossLanguageImplicitStrategy:
		return "e.edge_kind = '" + EdgeKindCrossLanguageRef + "'" +
			" AND e.resolution_strategy <> ''" +
			" AND e.resolution_strategy NOT IN " + sqlStringList(explicitCrossLanguageStrategies()), "''", false, nil
	}
	// Unreachable for the constants above; an unhandled check must not silently
	// report zero violations.
	panic("store: unhandled edge audit check")
}

// RunEdgeAuditCheck counts the edges violating one invariant and returns up to
// exampleLimit of them, ordered by edge id so the sample is stable across runs
// and across machines.
//
// exampleLimit <= 0 returns the count with no examples. The count is always the
// full population, never the number of examples returned.
//
// It returns ErrAuditCheckUnsupported when this database's schema cannot answer
// the check.
func (s *Store) RunEdgeAuditCheck(ctx context.Context, repoID int64, check EdgeAuditCheck, caps GraphAuditCapabilities, exampleLimit int) (EdgeAuditResult, error) {
	predicate, detailExpr, needsJoins, err := edgeAuditPredicate(check, caps)
	if err != nil {
		return EdgeAuditResult{}, err
	}

	// The count uses the narrowest FROM the predicate allows; the example query
	// below always uses the full one, because it projects paths and names.
	countFrom := auditEdgeOnlyFromSQL
	if needsJoins {
		countFrom = auditEdgeFromSQL
	}
	var result EdgeAuditResult
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)`+countFrom+`WHERE e.repo_id = ? AND (`+predicate+`)`,
		repoID,
	).Scan(&result.Count); err != nil {
		return EdgeAuditResult{}, err
	}
	if result.Count == 0 || exampleLimit <= 0 {
		return result, nil
	}

	exampleRows, err := s.db.QueryContext(ctx,
		`SELECT `+auditEdgeExampleColumnsFor(caps)+`, `+detailExpr+auditEdgeFromSQL+
			`WHERE e.repo_id = ? AND (`+predicate+`) ORDER BY e.id ASC LIMIT ?`,
		repoID, exampleLimit,
	)
	if err != nil {
		return EdgeAuditResult{}, err
	}
	defer exampleRows.Close()
	for exampleRows.Next() {
		var ex EdgeAuditExample
		if err := exampleRows.Scan(&ex.EdgeID, &ex.SrcFile, &ex.SrcSymbol, &ex.DstName, &ex.DstFile,
			&ex.DstSymbol, &ex.EdgeKind, &ex.SrcLang, &ex.DstLang, &ex.Strategy, &ex.Confidence, &ex.Detail); err != nil {
			return EdgeAuditResult{}, err
		}
		result.Examples = append(result.Examples, ex)
	}
	return result, exampleRows.Err()
}

// RunDanglingTestLinkCheck counts test_links rows referencing a symbol or file
// that no longer exists.
//
// Only the three columns without a foreign key are checked: test_file_id has
// one and is enforced by SQLite. A NULL reference is not dangling -- it is the
// documented result of the unbind paths -- so every clause requires NOT NULL.
func (s *Store) RunDanglingTestLinkCheck(ctx context.Context, repoID int64, exampleLimit int) (TestLinkAuditResult, error) {
	const from = `
		FROM test_links tl
		LEFT JOIN files tf ON tf.id = tl.test_file_id
		LEFT JOIN symbols ts ON ts.id = tl.test_symbol_id
		LEFT JOIN symbols gs ON gs.id = tl.target_symbol_id
		LEFT JOIN files gf ON gf.id = tl.target_file_id
	`
	const missingTestSymbol = "(tl.test_symbol_id IS NOT NULL AND ts.id IS NULL)"
	const missingTargetSymbol = "(tl.target_symbol_id IS NOT NULL AND gs.id IS NULL)"
	const missingTargetFile = "(tl.target_file_id IS NOT NULL AND gf.id IS NULL)"
	const predicate = missingTestSymbol + " OR " + missingTargetSymbol + " OR " + missingTargetFile
	const detailExpr = "CASE" +
		" WHEN " + missingTestSymbol + " THEN 'missing_test_symbol'" +
		" WHEN " + missingTargetSymbol + " THEN 'missing_target_symbol'" +
		" WHEN " + missingTargetFile + " THEN 'missing_target_file'" +
		" ELSE '' END"

	var result TestLinkAuditResult
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)`+from+`WHERE tl.repo_id = ? AND (`+predicate+`)`,
		repoID,
	).Scan(&result.Count); err != nil {
		return TestLinkAuditResult{}, err
	}
	if result.Count == 0 || exampleLimit <= 0 {
		return result, nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT tl.id, COALESCE(tf.path, ''), COALESCE(tl.test_symbol_id, 0), `+
			`COALESCE(tl.target_file_id, 0), COALESCE(tl.target_symbol_id, 0), `+detailExpr+from+
			`WHERE tl.repo_id = ? AND (`+predicate+`) ORDER BY tl.id ASC LIMIT ?`,
		repoID, exampleLimit,
	)
	if err != nil {
		return TestLinkAuditResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var ex TestLinkAuditExample
		if err := rows.Scan(&ex.TestLinkID, &ex.TestFile, &ex.TestSymbolID,
			&ex.TargetFileID, &ex.TargetSymbolID, &ex.Detail); err != nil {
			return TestLinkAuditResult{}, err
		}
		result.Examples = append(result.Examples, ex)
	}
	return result, rows.Err()
}

// GraphAuditSummaryFor returns the size of the graph in one pass per table.
func (s *Store) GraphAuditSummaryFor(ctx context.Context, repoID int64) (GraphAuditSummary, error) {
	var out GraphAuditSummary
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(is_deleted), 0)
		FROM files WHERE repo_id = ?
	`, repoID).Scan(&out.Files, &out.DeletedFiles); err != nil {
		return GraphAuditSummary{}, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM symbols WHERE repo_id = ?`, repoID).Scan(&out.Symbols); err != nil {
		return GraphAuditSummary{}, err
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN dst_symbol_id IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM edges WHERE repo_id = ?
	`, repoID).Scan(&out.Edges, &out.ResolvedEdges); err != nil {
		return GraphAuditSummary{}, err
	}
	out.UnresolvedEdges = out.Edges - out.ResolvedEdges
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM test_links WHERE repo_id = ?`, repoID).Scan(&out.TestLinks); err != nil {
		return GraphAuditSummary{}, err
	}
	return out, nil
}

// ResolutionStrategyDistribution counts resolved edges per strategy, and
// ResolutionConfidenceDistribution per confidence tier. Both are GROUP BY
// aggregates over the whole repository, not samples. The empty-string key is
// retained rather than dropped: for strategy it is the legacy pre-019
// population, which is the interesting part of the distribution.
// Both return ErrAuditCheckUnsupported on a pre-019 schema, where the columns
// do not exist.
func (s *Store) ResolutionStrategyDistribution(ctx context.Context, repoID int64, caps GraphAuditCapabilities) (map[string]int64, error) {
	if !caps.HasResolutionMetadata {
		return nil, ErrAuditCheckUnsupported
	}
	return s.resolvedEdgeDistribution(ctx, repoID, "resolution_strategy", knownResolutionStrategies())
}

// ResolutionConfidenceDistribution counts resolved edges per confidence tier.
func (s *Store) ResolutionConfidenceDistribution(ctx context.Context, repoID int64, caps GraphAuditCapabilities) (map[string]int64, error) {
	if !caps.HasResolutionMetadata {
		return nil, ErrAuditCheckUnsupported
	}
	return s.resolvedEdgeDistribution(ctx, repoID, "resolution_confidence", []string{
		ResolutionConfidenceHigh, ResolutionConfidenceMedium, ResolutionConfidenceLow,
	})
}

// DistributionUnregisteredKey is the bucket every value outside the registered
// strategy and confidence sets is folded into.
//
// Without it the distributions would be the one unbounded part of the report:
// on exactly the corrupt database this command exists to describe -- arbitrary
// strings in resolution_strategy, which is what unknown_resolution_strategy
// reports -- the map would grow one key per distinct garbage value, defeating
// the bounded-output guarantee everything else here maintains. The count is
// still exact; only the number of keys is capped.
const DistributionUnregisteredKey = "<unregistered>"

// resolvedEdgeDistribution groups resolved edges by one provenance column,
// folding unregistered values into DistributionUnregisteredKey.
//
// The column name and the allowed value set are chosen by this package, never
// by a caller.
func (s *Store) resolvedEdgeDistribution(ctx context.Context, repoID int64, column string, allowed []string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+column+`, COUNT(*)
		FROM edges
		WHERE repo_id = ? AND dst_symbol_id IS NOT NULL
		GROUP BY `+column+`
		ORDER BY `+column+` ASC
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// The empty string is retained as its own key rather than folded in: for
	// strategy it is the legacy pre-019 population, which is the interesting
	// part of the distribution, not garbage.
	permitted := map[string]struct{}{"": {}}
	for _, value := range allowed {
		permitted[value] = struct{}{}
	}
	out := map[string]int64{}
	for rows.Next() {
		var key string
		var count int64
		if err := rows.Scan(&key, &count); err != nil {
			return nil, err
		}
		if _, ok := permitted[key]; !ok {
			key = DistributionUnregisteredKey
		}
		out[key] += count
	}
	return out, rows.Err()
}

// auditClassificationPageSize is how many unresolved edges are classified per
// round trip. It bounds memory: the loop holds one page, never the population.
const auditClassificationPageSize = 1000

// UnresolvedTargetClassificationCounts counts unresolved edges by what their
// target name can be shown to be: builtin, stdlib, external, or unknown.
//
// It streams the unresolved population in keyset-paged batches through
// classifyExportEdges -- the same batched evidence loader export and the
// resolver audit use -- rather than reimplementing the P8 rules in SQL. Those
// rules need per-file import lists and same-language project shadowing, which
// are Go-side semantics; a SQL restatement of them would be a second
// implementation free to drift from internal/classify.
//
// Cost is two batched evidence queries per page plus the page query itself, so
// it is O(unresolved edges / page size) round trips and O(page size) memory,
// not one query per edge.
//
// Paging is by `e.id > afterID` rather than OFFSET so the walk does not
// re-scan the prefix on each page. EXPLAIN QUERY PLAN shows it served by
// idx_edges_repo_dst as a (repo_id, dst_symbol_id, rowid>?) range scan, with no
// temporary b-tree for the ORDER BY.
func (s *Store) UnresolvedTargetClassificationCounts(ctx context.Context, repoID int64, caps GraphAuditCapabilities) (map[string]int64, error) {
	counts := map[string]int64{}
	var afterID int64
	for {
		rows, err := s.db.QueryContext(ctx, `
			SELECT `+unresolvedClassificationColumnsFor(caps)+`
			FROM edges e
			LEFT JOIN symbols src ON src.id = e.src_symbol_id
			LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id
			LEFT JOIN files f ON f.id = e.file_id
			WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND e.id > ?
			ORDER BY e.id ASC
			LIMIT ?
		`, repoID, afterID, auditClassificationPageSize)
		if err != nil {
			return nil, err
		}
		edges, evidence, err := scanExportEdges(rows)
		if err != nil {
			return nil, err
		}
		if len(edges) == 0 {
			return counts, nil
		}
		if err := s.classifyExportEdges(ctx, repoID, edges, evidence); err != nil {
			return nil, err
		}
		for i := range edges {
			counts[edges[i].TargetClassification]++
			if edges[i].ID > afterID {
				afterID = edges[i].ID
			}
		}
		if len(edges) < auditClassificationPageSize {
			return counts, nil
		}
	}
}
