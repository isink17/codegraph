package constraints

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/limits"
	"github.com/isink17/codegraph/internal/store"
)

// SchemaID versions the result shape.
const SchemaID = "codegraph.constraints/v1"

// ConfigFileName is the repo-root location of the constraints document. It is
// committed with the code, unlike the git-ignored `.codegraph/` database dir.
const ConfigFileName = ".codegraph-constraints.json"

// Statuses, in precedence order.
const (
	StatusConfigError   = "config_error"
	StatusNotConfigured = "not_configured"
	StatusNotIndexed    = "not_indexed"
	StatusStale         = "stale"
	StatusViolations    = "violations"
	StatusOK            = "ok"
)

// maxOverlapErrors bounds the group_overlap errors one result reports.
const maxOverlapErrors = 20

// unownedKey spells "no group" in coverage map keys. Parentheses cannot occur
// in a group name, so the key cannot collide with a declared group.
const unownedKey = "(unowned)"

// ExitCode maps a status to the process exit contract: 0 ok, 1 violations, 2
// for every state in which the graph could not be vouched for.
func ExitCode(status string) int {
	switch status {
	case StatusOK:
		return 0
	case StatusViolations:
		return 1
	default:
		return 2
	}
}

// Strict freshness verdicts. None of them means "fresh": uncommitted edits
// and watcher events that were never queued are never checked.
const (
	// VerdictKnownStale: a recorded fact says the graph is behind (a failed
	// or running latest scan, a non-empty dirty queue, a moved HEAD).
	VerdictKnownStale = "known_stale"
	// VerdictUnknown: the evidence needed to decide was not recorded or could
	// not be read (no completed scan, a database from before coverage
	// recording, an unreadable HEAD, a full scan that overlapped another
	// writer, or scan activity during the check itself).
	VerdictUnknown = "unknown"
	// VerdictInsufficientCoverage: no known staleness, but the recorded scans
	// do not prove the whole repository was walked at the current HEAD.
	VerdictInsufficientCoverage = "insufficient_coverage"
	// VerdictFullCoverageAtHead: no known staleness, and the newest completed
	// full scan started and finished at the current HEAD, overlapped no other
	// scan, and no scan has failed since.
	VerdictFullCoverageAtHead = "full_coverage_at_head"
)

// Strict exit codes, beyond 0, 1 and 2. Used only with Options.StrictFreshness.
const (
	ExitKnownStale           = 3
	ExitFreshnessUnknown     = 4
	ExitInsufficientCoverage = 5
)

// ResultExitCode is the process exit code for res: ExitCode(res.Status), or
// the strict verdict's code when strict freshness was evaluated.
func ResultExitCode(res Result) int {
	if res.Freshness != nil {
		return res.Freshness.ExitCode
	}
	return ExitCode(res.Status)
}

// ValidatePage checks paging arguments. limit 0 selects the default page;
// out-of-range values are errors, never clamped.
func ValidatePage(limit, offset int) error {
	if limit < 0 || limit > limits.MaxPage {
		return fmt.Errorf("invalid limit %d: want 0..%d", limit, limits.MaxPage)
	}
	if offset < 0 {
		return fmt.Errorf("invalid offset %d: want 0 or more", offset)
	}
	return nil
}

// Options configures one evaluation.
type Options struct {
	// RepoRoot is the repository root on the host.
	RepoRoot string
	// ConfigPath overrides the repo-root config file (CLI only). Empty means
	// RepoRoot/ConfigFileName.
	ConfigPath string
	Limit      int
	Offset     int
	// StrictFreshness adds the freshness block to an evaluated result and
	// moves its exit code to the strict contract (see ResultExitCode).
	StrictFreshness bool
}

// Opener opens the index. An error wrapping store.ErrRepoNotIndexed becomes
// status not_indexed; any other error aborts the evaluation.
type Opener func(context.Context) (*store.Store, int64, error)

// ConfigInfo identifies the document that was evaluated.
type ConfigInfo struct {
	Path          *string `json:"path"`
	Source        string  `json:"source"`
	SchemaVersion *int    `json:"schema_version"`
	SHA256        *string `json:"sha256"`
}

// IndexInfo is history metadata. It is excluded from fresh==incremental parity.
type IndexInfo struct {
	LastIndexedAt string `json:"last_indexed_at"`
	LastScanID    int64  `json:"last_scan_id"`
	DirtyFiles    int64  `json:"dirty_files"`
}

// Summary counts are exact regardless of paging.
type Summary struct {
	Rules       int            `json:"rules"`
	Findings    int            `json:"findings"`
	Occurrences int            `json:"occurrences"`
	Cycles      int            `json:"cycles"`
	ByRule      map[string]int `json:"by_rule"`
}

// Coverage says how much of the graph the verdict rests on.
type Coverage struct {
	TrustedDependenciesEvaluated            int                       `json:"trusted_dependencies_evaluated"`
	ExcludedByTrustFilter                   map[string]int            `json:"excluded_by_trust_filter"`
	UnresolvedDependencyRowsBySourceGroup   map[string]int            `json:"unresolved_dependency_rows_by_source_group"`
	FilesWithoutCallEdgesByGroupAndLanguage map[string]map[string]int `json:"files_without_call_edges_by_group_and_language"`
	UnownedFiles                            int                       `json:"unowned_files"`
	UnmatchedPatterns                       []string                  `json:"unmatched_patterns"`
}

// SourceEnd is the dependency's source endpoint.
type SourceEnd struct {
	Path            string `json:"path"`
	Line            int64  `json:"line"`
	Symbol          string `json:"symbol"`
	SymbolStartLine int64  `json:"symbol_start_line"`
	StableKey       string `json:"stable_key"`
}

// TargetEnd is the dependency's target endpoint.
type TargetEnd struct {
	Path      string `json:"path"`
	Symbol    string `json:"symbol"`
	StartLine int64  `json:"start_line"`
	StableKey string `json:"stable_key"`
}

// Dependency is one collapsed dependency identity.
type Dependency struct {
	EdgeKind             string    `json:"edge_kind"`
	Source               SourceEnd `json:"source"`
	Target               TargetEnd `json:"target"`
	ResolutionStrategy   string    `json:"resolution_strategy"`
	ResolutionConfidence string    `json:"resolution_confidence"`
	Evidence             string    `json:"evidence"`
	Occurrences          int       `json:"occurrences"`
}

// Finding is one rule violation.
type Finding struct {
	RuleID      string  `json:"rule_id"`
	Kind        string  `json:"kind"`
	SourceGroup *string `json:"source_group"`
	TargetGroup *string `json:"target_group"`
	Dependency
}

// Hop is one step of a cycle witness with its witnessing dependency.
type Hop struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	Dependency Dependency `json:"dependency"`
}

// Cycle is one nontrivial strongly connected component of a rule's group graph.
type Cycle struct {
	RuleID  string   `json:"rule_id"`
	Members []string `json:"members"`
	Witness []string `json:"witness"`
	Hops    []Hop    `json:"hops"`
}

// Result is the `codegraph.constraints/v1` document. It holds no host-absolute
// path.
type Result struct {
	Schema            string     `json:"schema"`
	Status            string     `json:"status"`
	Errors            []Error    `json:"errors"`
	Config            ConfigInfo `json:"config"`
	Index             *IndexInfo `json:"index"`
	Summary           Summary    `json:"summary"`
	Coverage          *Coverage  `json:"coverage"`
	Findings          []Finding  `json:"findings"`
	FindingsTruncated bool       `json:"findings_truncated"`
	Cycles            []Cycle    `json:"cycles"`
	CyclesTruncated   bool       `json:"cycles_truncated"`
	// Freshness is present only in strict mode, and only once the index was
	// opened (status stale, violations or ok). Like index, it is history
	// metadata and outside the same-bytes-for-the-same-tree guarantee.
	Freshness *StrictFreshness `json:"freshness,omitempty"`
}

// StrictFreshness is the strict-mode verdict and the evidence behind it. It
// holds no host path.
type StrictFreshness struct {
	Verdict string `json:"verdict"`
	// Reasons explain the verdict; empty for full_coverage_at_head.
	Reasons []string `json:"reasons"`
	// State and StateReasons are the graph_stats freshness state and reasons
	// (never "fresh"; no_known_staleness at best).
	State        string               `json:"state"`
	StateReasons []string             `json:"state_reasons"`
	HeadNow      string               `json:"head_now"`
	LastFullScan *graph.FreshnessScan `json:"last_full_scan"`
	ExitCode     int                  `json:"exit_code"`
}

func newResult() Result {
	return Result{
		Schema:   SchemaID,
		Errors:   []Error{},
		Summary:  Summary{ByRule: map[string]int{}},
		Findings: []Finding{},
		Cycles:   []Cycle{},
	}
}

// Check runs the frozen evaluation order: config, index, data-dependent
// validation, staleness, evaluation. The returned error is reserved for
// failures that are not a status (I/O, SQL); every status is a Result.
func Check(ctx context.Context, opts Options, open Opener) (Result, error) {
	if err := ValidatePage(opts.Limit, opts.Offset); err != nil {
		return Result{}, err
	}
	limit := opts.Limit
	if limit == 0 {
		limit = limits.DefaultPage
	}
	res := newResult()

	// 1. Config.
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = filepath.Join(opts.RepoRoot, ConfigFileName)
	}
	logical := logicalConfigPath(opts.RepoRoot, cfgPath)
	res.Config.Source = "external"
	if logical != "" {
		res.Config.Source = "repo"
		res.Config.Path = &logical
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Result{}, fmt.Errorf("read constraints config: %w", err)
		}
		res.Status = StatusNotConfigured
		where := ConfigFileName
		if logical != "" {
			where = logical
		}
		msg := fmt.Sprintf("no constraints config found; create %s at the repository root", ConfigFileName)
		if opts.ConfigPath != "" {
			// The caller named a file; do not point them at the default one.
			msg = "the given constraints config file does not exist"
			if logical == "" {
				where = "--config"
			}
		}
		res.Errors = []Error{{Location: where, Code: StatusNotConfigured, Message: msg}}
		return res, nil
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	res.Config.SHA256 = &digest
	cfg, cfgErrs := ParseConfig(raw)
	if len(cfgErrs) > 0 {
		res.Status = StatusConfigError
		res.Errors = cfgErrs
		return res, nil
	}
	version := 1
	res.Config.SchemaVersion = &version
	res.Summary.Rules = len(cfg.Rules)

	// 2. Index.
	st, repoID, err := open(ctx)
	if err != nil {
		if errors.Is(err, store.ErrRepoNotIndexed) {
			res.Status = StatusNotIndexed
			res.Errors = []Error{{Location: "", Code: StatusNotIndexed, Message: "the repository is not indexed; run `codegraph index` first"}}
			return res, nil
		}
		return Result{}, err
	}
	// Strict mode reads freshness before any graph row, so a scan that lands
	// between this read and the last one is seen as activity during the
	// check rather than mixing older files with newer edges unnoticed.
	var before graph.Freshness
	if opts.StrictFreshness {
		if before, err = st.FreshnessStatus(ctx, repoID); err != nil {
			return Result{}, err
		}
		if strictBeforeHook != nil {
			strictBeforeHook()
		}
	}
	info, err := st.ConstraintIndexInfo(ctx, repoID)
	if err != nil {
		return Result{}, err
	}
	if info.LastScanID == 0 {
		res.Status = StatusNotIndexed
		res.Errors = []Error{{Location: "", Code: StatusNotIndexed, Message: "the repository has never been scanned; run `codegraph index` first"}}
		return res, nil
	}
	res.Index = &IndexInfo{LastIndexedAt: info.LastIndexedAt, LastScanID: info.LastScanID, DirtyFiles: info.DirtyFiles}

	// 3. Data-dependent validation: group membership and overlap.
	files, err := st.ConstraintFiles(ctx, repoID)
	if err != nil {
		return Result{}, err
	}
	cov := &Coverage{
		ExcludedByTrustFilter:                   map[string]int{},
		UnresolvedDependencyRowsBySourceGroup:   map[string]int{},
		FilesWithoutCallEdgesByGroupAndLanguage: map[string]map[string]int{},
		UnmatchedPatterns:                       []string{},
	}
	groupOf := map[string]string{}
	matched := map[string]bool{}
	var overlaps []Error
	for _, f := range files {
		if f.Path == logical {
			continue
		}
		segs := strings.Split(f.Path, "/")
		var owners []string
		for _, g := range cfg.Groups {
			in := false
			for _, p := range g.Include {
				if p.match(segs) {
					matched[p.raw] = true
					in = true
				}
			}
			out := false
			for _, p := range g.Exclude {
				if p.match(segs) {
					matched[p.raw] = true
					out = true
				}
			}
			if in && !out {
				owners = append(owners, g.Name)
			}
		}
		if len(owners) > 1 {
			overlaps = append(overlaps, Error{Location: f.Path, Code: CodeGroupOverlap, Message: "groups " + strings.Join(owners, ", ")})
			continue
		}
		key := unownedKey
		if len(owners) == 1 {
			groupOf[f.Path] = owners[0]
			key = owners[0]
		} else {
			cov.UnownedFiles++
		}
		if f.ParserProfile != "" && !f.ParserCallEdges {
			byLang := cov.FilesWithoutCallEdgesByGroupAndLanguage[key]
			if byLang == nil {
				byLang = map[string]int{}
				cov.FilesWithoutCallEdgesByGroupAndLanguage[key] = byLang
			}
			byLang[f.Language]++
		}
	}
	if len(overlaps) > 0 {
		if len(overlaps) > maxOverlapErrors {
			overlaps = overlaps[:maxOverlapErrors]
		}
		res.Status = StatusConfigError
		res.Errors = overlaps
		return res, nil
	}
	unmatched := map[string]bool{}
	for _, g := range cfg.Groups {
		for _, p := range append(append([]pattern(nil), g.Include...), g.Exclude...) {
			if !matched[p.raw] {
				unmatched[p.raw] = true
			}
		}
	}
	for p := range unmatched {
		cov.UnmatchedPatterns = append(cov.UnmatchedPatterns, p)
	}
	sort.Strings(cov.UnmatchedPatterns)

	// 4. Staleness. Findings are still computed under stale.
	stale := info.DirtyFiles > 0

	// 5. Evaluate.
	edges, err := st.ConstraintEdges(ctx, repoID)
	if err != nil {
		return Result{}, err
	}
	aggs := map[identity]*agg{}
	for _, e := range edges {
		src := groupOf[e.SourcePath]
		if !e.Resolved {
			cov.UnresolvedDependencyRowsBySourceGroup[keyOf(src)]++
			continue
		}
		dst := groupOf[e.TargetPath]
		if e.Confidence != store.ResolutionConfidenceHigh && e.Confidence != store.ResolutionConfidenceMedium {
			cov.ExcludedByTrustFilter[keyOf(src)+"→"+keyOf(dst)]++
			continue
		}
		cov.TrustedDependenciesEvaluated++
		if src == dst {
			continue
		}
		id := identityOf(e)
		a := aggs[id]
		if a == nil {
			a = &agg{id: id, rep: e, src: src, dst: dst}
			aggs[id] = a
		} else if repLess(e, a.rep) {
			a.rep = e
		}
		a.count++
	}
	ordered := make([]*agg, 0, len(aggs))
	for _, a := range aggs {
		ordered = append(ordered, a)
	}
	sort.Slice(ordered, func(i, j int) bool { return identityLess(ordered[i].id, ordered[j].id) })

	var findings []Finding
	var cycles []Cycle
	for _, r := range cfg.Rules {
		if r.Kind == KindForbiddenCycles {
			found := ruleCycles(r, ordered)
			if len(found) > 0 {
				res.Summary.ByRule[r.ID] = len(found)
			}
			cycles = append(cycles, found...)
			continue
		}
		n := 0
		for _, a := range ordered {
			if violates(r, a.src, a.dst) {
				findings = append(findings, Finding{
					RuleID: r.ID, Kind: r.Kind,
					SourceGroup: groupPtr(a.src), TargetGroup: groupPtr(a.dst),
					Dependency: a.dependency(),
				})
				res.Summary.Occurrences += a.count
				n++
			}
		}
		if n > 0 {
			res.Summary.ByRule[r.ID] = n
		}
	}
	// Rules are sorted by id and each rule's rows by identity, so findings are
	// already in (rule_id, identity) order; cycles are in (rule_id, members).
	res.Summary.Findings = len(findings)
	res.Summary.Cycles = len(cycles)
	res.Coverage = cov

	start := min(opts.Offset, len(findings))
	end := min(start+limit, len(findings))
	res.Findings = append(res.Findings, findings[start:end]...)
	res.FindingsTruncated = end < len(findings)
	if len(cycles) > limit {
		res.Cycles = append(res.Cycles, cycles[:limit]...)
		res.CyclesTruncated = true
	} else {
		res.Cycles = append(res.Cycles, cycles...)
	}

	switch {
	case stale:
		res.Status = StatusStale
	case len(findings) > 0 || len(cycles) > 0:
		res.Status = StatusViolations
	default:
		res.Status = StatusOK
	}
	if opts.StrictFreshness {
		// Read again after the evaluation: a scan that started, finished or
		// failed meanwhile may have changed the rows the findings rest on.
		after, err := st.FreshnessStatus(ctx, repoID)
		if err != nil {
			return Result{}, err
		}
		res.Freshness = strictFreshness(res.Status, info.LastScanID, before, after)
	}
	return res, nil
}

// strictBeforeHook, when set by a test, runs right after the strict
// freshness read that precedes every graph read.
var strictBeforeHook func()

// strictFreshness decides the strict verdict from the freshness read before
// every graph read and after the evaluation; lastScanID is the newest scan id
// the graph reads saw, which must be before's latest scan. A proven gap (insufficient_coverage) outranks a
// missing fact (unknown); known_stale outranks both.
func strictFreshness(status string, lastScanID int64, before, after graph.Freshness) *StrictFreshness {
	out := &StrictFreshness{
		State:        before.State,
		StateReasons: before.Reasons,
		HeadNow:      before.Worktree.HeadNow,
		LastFullScan: before.Coverage.LastFullScan,
	}
	var unknown, insufficient []string
	switch {
	case status == StatusStale || before.State == graph.FreshnessKnownStale:
		out.Verdict = VerdictKnownStale
		out.Reasons = append([]string{}, before.Reasons...)
		if status == StatusStale && !slices.Contains(out.Reasons, "dirty_queue_nonempty") {
			out.Reasons = append(out.Reasons, "dirty_queue_nonempty")
		}
		out.ExitCode = ExitKnownStale
		return out
	case before.State != graph.FreshnessNoKnownStaleness:
		unknown = append(unknown, before.Reasons...)
	case !before.Coverage.Recorded:
		unknown = append(unknown, "coverage_not_recorded")
	case before.Coverage.LastFullScan == nil:
		insufficient = append(insufficient, "no_recorded_full_scan")
	default:
		full := before.Coverage.LastFullScan
		if before.Coverage.FailedAfterLastFull > 0 {
			insufficient = append(insufficient, "scan_failed_after_full_scan")
		}
		switch full.Overlap {
		case "none":
		case "yes":
			unknown = append(unknown, "full_scan_overlapped_another_scan")
		default:
			unknown = append(unknown, "full_scan_overlap_not_recorded")
		}
		switch {
		case full.HeadAtStart == "" || full.HeadAtFinish == "":
			unknown = append(unknown, "full_scan_head_not_recorded")
		case full.HeadAtStart != full.HeadAtFinish:
			insufficient = append(insufficient, "head_moved_during_full_scan")
		case before.Worktree.HeadNow == "unknown":
			unknown = append(unknown, "head_now_unknown")
		case full.HeadAtFinish != before.Worktree.HeadNow:
			insufficient = append(insufficient, "head_moved_since_full_scan")
		}
	}
	// Any running row, even one older than the last completed scan, may be a
	// live writer changing rows under the check; scans hold no lease, so an
	// abandoned row cannot be told apart and also reads as unknown.
	if (before.RunningScans.Count > 0 || after.RunningScans.Count > 0) && !slices.Contains(unknown, "scan_running_or_abandoned") {
		unknown = append(unknown, "scan_running_or_abandoned")
	}
	if !sameScanState(before, after) || before.LatestScan == nil || before.LatestScan.ID != lastScanID {
		unknown = append(unknown, "scan_activity_during_check")
	}
	switch {
	case len(insufficient) > 0:
		out.Verdict, out.ExitCode = VerdictInsufficientCoverage, ExitInsufficientCoverage
	case len(unknown) > 0:
		out.Verdict, out.ExitCode = VerdictUnknown, ExitFreshnessUnknown
	default:
		out.Verdict, out.ExitCode = VerdictFullCoverageAtHead, ExitCode(status)
	}
	out.Reasons = append(append([]string{}, insufficient...), unknown...)
	return out
}

// sameScanState reports whether no scan began, closed or queued work between
// two freshness reads, and HEAD did not move.
func sameScanState(a, b graph.Freshness) bool {
	scan := func(sc *graph.FreshnessScan) string {
		if sc == nil {
			return ""
		}
		return fmt.Sprintf("%d/%s", sc.ID, sc.Status)
	}
	return scan(a.LatestScan) == scan(b.LatestScan) &&
		a.RunningScans.Count == b.RunningScans.Count &&
		a.DirtyQueue == b.DirtyQueue &&
		a.Worktree.HeadNow == b.Worktree.HeadNow
}

// logicalConfigPath returns the config's repository-relative logical path, or
// "" when it lies outside the repository. Both sides are resolved through
// symlinks so a --config spelled through a link (macOS /var vs /private/var)
// is still recognised as inside the repository.
func logicalConfigPath(repoRoot, cfgPath string) string {
	resolve := func(p string) (string, error) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return abs, nil
		}
		return filepath.Join(dir, filepath.Base(abs)), nil
	}
	abs, err := resolve(cfgPath)
	if err != nil {
		return ""
	}
	rootAbs, err := filepath.Abs(repoRoot)
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = r
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func keyOf(group string) string {
	if group == "" {
		return unownedKey
	}
	return group
}

func groupPtr(group string) *string {
	if group == "" {
		return nil
	}
	return &group
}

func violates(r Rule, src, dst string) bool {
	switch r.Kind {
	case KindForbiddenDependency:
		return src != "" && dst != "" && contains(r.From, src) && contains(r.To, dst)
	case KindAllowedDependencies:
		return src != "" && contains(r.From, src) && (dst == "" || !contains(r.To, dst))
	case KindAllowedDependents:
		return dst != "" && contains(r.Of, dst) && (src == "" || !contains(r.From, src))
	}
	return false
}

// identity is the §3.3 finding identity without the rule id. stable_key is not
// unique, so the start lines are part of it.
type identity struct {
	srcPath  string
	line     int64
	edgeKind string
	srcStart int64
	srcKey   string
	dstPath  string
	dstStart int64
	dstKey   string
}

func identityOf(e store.ConstraintEdge) identity {
	return identity{e.SourcePath, e.Line, e.EdgeKind, e.SourceStartLine, e.SourceStableKey, e.TargetPath, e.TargetStartLine, e.TargetStableKey}
}

func identityLess(a, b identity) bool {
	switch {
	case a.srcPath != b.srcPath:
		return a.srcPath < b.srcPath
	case a.line != b.line:
		return a.line < b.line
	case a.edgeKind != b.edgeKind:
		return a.edgeKind < b.edgeKind
	case a.srcStart != b.srcStart:
		return a.srcStart < b.srcStart
	case a.srcKey != b.srcKey:
		return a.srcKey < b.srcKey
	case a.dstPath != b.dstPath:
		return a.dstPath < b.dstPath
	case a.dstStart != b.dstStart:
		return a.dstStart < b.dstStart
	default:
		return a.dstKey < b.dstKey
	}
}

// repLess picks the representative row among rows of one identity: first by
// (resolution_strategy, resolution_confidence, evidence), then by the symbol
// names so the choice is total.
func repLess(a, b store.ConstraintEdge) bool {
	switch {
	case a.Strategy != b.Strategy:
		return a.Strategy < b.Strategy
	case a.Confidence != b.Confidence:
		return a.Confidence < b.Confidence
	case a.Evidence != b.Evidence:
		return a.Evidence < b.Evidence
	case a.SourceSymbol != b.SourceSymbol:
		return a.SourceSymbol < b.SourceSymbol
	default:
		return a.TargetSymbol < b.TargetSymbol
	}
}

type agg struct {
	id       identity
	rep      store.ConstraintEdge
	src, dst string
	count    int
}

func (a *agg) dependency() Dependency {
	e := a.rep
	return Dependency{
		EdgeKind:             e.EdgeKind,
		Source:               SourceEnd{Path: e.SourcePath, Line: e.Line, Symbol: e.SourceSymbol, SymbolStartLine: e.SourceStartLine, StableKey: e.SourceStableKey},
		Target:               TargetEnd{Path: e.TargetPath, Symbol: e.TargetSymbol, StartLine: e.TargetStartLine, StableKey: e.TargetStableKey},
		ResolutionStrategy:   e.Strategy,
		ResolutionConfidence: e.Confidence,
		Evidence:             e.Evidence,
		Occurrences:          a.count,
	}
}
