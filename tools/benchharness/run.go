package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/tokenest"
)

// Truth is the fixture's hand-labeled ground truth (truth.json).
type Truth struct {
	Description         string   `json:"description"`
	CompleteEnumeration bool     `json:"complete_enumeration"`
	Labeling            string   `json:"labeling"`
	Callers             []string `json:"callers"`
	Targets             []string `json:"targets"`
	Pairs               []struct {
		Caller string `json:"caller"`
		Callee string `json:"callee"`
	} `json:"pairs"`
	Tasks []struct {
		ID            string   `json:"id"`
		Type          string   `json:"type"`
		Symbol        string   `json:"symbol"`
		Prompt        string   `json:"prompt"`
		ExpectedFacts []string `json:"expected_facts"`
	} `json:"tasks"`
}

// RunConfig names everything a run needs; nothing is inferred from the host
// checkout so a record never silently claims the wrong SHA.
type RunConfig struct {
	FixtureDir   string
	Binary       string
	CodeGraphSHA string
	DateUTC      string
	WorkDir      string
}

const (
	modeBaseline = "baseline-read-all-sources"
	modeCLI      = "codegraph-cli-query"
)

// Run indexes a fresh copy of the fixture sources and measures it.
func Run(cfg RunConfig) (*Record, error) {
	raw, err := os.ReadFile(filepath.Join(cfg.FixtureDir, "truth.json"))
	if err != nil {
		return nil, fmt.Errorf("ground truth: %w", err)
	}
	var truth Truth
	if err := json.Unmarshal(raw, &truth); err != nil {
		return nil, fmt.Errorf("ground truth: %w", err)
	}
	if len(truth.Pairs) == 0 || truth.Description == "" || len(truth.Targets) == 0 {
		return nil, fmt.Errorf("ground truth: empty universe, targets or missing description")
	}
	isTarget := map[string]bool{}
	for _, t := range truth.Targets {
		isTarget[t] = true
	}
	fixtureSHA, err := HashDir(cfg.FixtureDir)
	if err != nil {
		return nil, err
	}
	repo := filepath.Join(cfg.WorkDir, "repo")
	if err := os.RemoveAll(repo); err != nil {
		return nil, err
	}
	if err := os.CopyFS(repo, os.DirFS(filepath.Join(cfg.FixtureDir, "src"))); err != nil {
		return nil, err
	}

	var commands []string
	cg := func(args ...string) ([]byte, error) {
		commands = append(commands, "codegraph "+strings.ReplaceAll(strings.Join(args, " "), repo, "<workdir>/repo"))
		out, err := exec.Command(cfg.Binary, args...).Output()
		if err != nil {
			return nil, fmt.Errorf("codegraph %v: %w", args, err)
		}
		return out, nil
	}
	ver, err := cg("version")
	if err != nil {
		return nil, err
	}
	if _, err := cg("index", repo); err != nil {
		return nil, err
	}

	var obs []Observation
	// Correctness: resolved callees per truth caller, matched by symbol name.
	got := map[string]map[string]bool{}
	callers := append([]string(nil), truth.Callers...)
	sort.Strings(callers)
	for _, c := range callers {
		out, err := cg("find_callees", repo, c)
		if err != nil {
			return nil, err
		}
		names, err := resultNames(out, "callees")
		if err != nil {
			return nil, err
		}
		got[c] = names
	}
	inTruth := map[string]bool{}
	matched := 0
	for _, p := range truth.Pairs {
		key := p.Caller + "->" + p.Callee
		inTruth[key] = true
		outcome := "not_resolved"
		if got[p.Caller][p.Callee] {
			outcome = "resolved_to_truth_target"
			matched++
		}
		obs = append(obs, Observation{Kind: "pair", ID: key, Outcome: outcome})
	}
	// Edges to a labeled target that are not a labeled pair are wrong for
	// this fixture; edges to anything else are outside the universe and are
	// excluded from the precision denominator.
	unexpected, outside := 0, 0
	for _, c := range callers {
		for n := range got[c] {
			key := c + "->" + n
			switch {
			case inTruth[key]:
			case isTarget[n]:
				unexpected++
				obs = append(obs, Observation{Kind: "unexpected", ID: key, Outcome: "resolved_outside_truth"})
			default:
				outside++
				obs = append(obs, Observation{Kind: "outside_universe", ID: key, Outcome: "resolved_outside_universe"})
			}
		}
	}

	// Context volume and evidence completeness per task and mode.
	baseline, err := readAllSources(repo)
	if err != nil {
		return nil, err
	}
	totals := map[string][3]int{} // bytes, facts present, facts expected
	for _, t := range truth.Tasks {
		cmd := map[string]string{"find_callers": "find_callers", "find_callees": "find_callees"}[t.Type]
		if cmd == "" {
			return nil, fmt.Errorf("task %s: unsupported type %q", t.ID, t.Type)
		}
		out, err := cg(cmd, repo, t.Symbol)
		if err != nil {
			return nil, err
		}
		for _, mc := range []struct {
			mode string
			ctx  []byte
		}{{modeBaseline, baseline}, {modeCLI, out}} {
			present := factsPresent(mc.ctx, t.ExpectedFacts)
			obs = append(obs, Observation{
				Kind: "context", ID: t.ID, Mode: mc.mode, Outcome: "measured",
				Bytes: ip(len(mc.ctx)), EstTokens: ip(tokenest.FromBytes(len(mc.ctx))),
				FactsExpected: ip(len(t.ExpectedFacts)), FactsPresent: ip(present),
			})
			tt := totals[mc.mode]
			totals[mc.mode] = [3]int{tt[0] + len(mc.ctx), tt[1] + present, tt[2] + len(t.ExpectedFacts)}
		}
	}
	sortObservations(obs)

	n := len(truth.Pairs)
	scope := "measured corpus: fixture " + filepath.Base(cfg.FixtureDir)
	metrics := []Metric{
		count("candidate_opportunities", n, scope),
		count("resolved_to_truth_target", matched, scope),
		count("not_resolved", n-matched, scope),
		count("resolved_outside_truth", unexpected, scope),
		count("resolved_outside_universe", outside, scope),
	}
	if truth.CompleteEnumeration {
		metrics = append(metrics, ratio("coverage", matched, n,
			"all hand-labeled (caller, callee) pairs in the fixture", scope))
	}
	if matched+unexpected > 0 {
		metrics = append(metrics, ratio("audited_resolved_precision", matched, matched+unexpected,
			"every resolved callee edge from the truth callers whose callee name-matches a labeled target (full audit of this fixture, not a sample); edges to unlabeled callees are excluded",
			"audited resolved subset: "+scope))
	}
	for _, mode := range []string{modeBaseline, modeCLI} {
		t := totals[mode]
		metrics = append(metrics,
			Metric{Name: "context_bytes_total/" + mode, Value: fp(float64(t[0])), Unit: "bytes", Scope: scope},
			Metric{Name: "estimated_tokens_total/" + mode, Value: fp(float64(sumTokens(obs, mode))), Unit: "estimated_tokens", Scope: scope},
			ratio("evidence_completeness/"+mode, t[1], t[2],
				"expected facts over all tasks; a fact counts when it appears in the context delimited by non-identifier characters", scope))
	}

	env := map[string]string{"num_cpu": strconv.Itoa(runtime.NumCPU())}
	for _, k := range []string{"CGO_ENABLED", "GOFLAGS", "GOMAXPROCS"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	return &Record{
		Schema:    SchemaVersion,
		Benchmark: "correctness+context",
		CodeGraph: CodeGraph{SHA: cfg.CodeGraphSHA, Version: strings.TrimSpace(string(ver))},
		DateUTC:   cfg.DateUTC,
		Platform:  Platform{OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(), Env: env},
		Fixtures: []Fixture{{Name: filepath.Base(cfg.FixtureDir), Path: filepath.ToSlash(cfg.FixtureDir), SHA: fixtureSHA,
			Setup: "copy <fixture>/src to a fresh <workdir>/repo, then codegraph index (fresh database)"}},
		Commands: commands,
		Methodology: Methodology{
			Version:             MethodologyVersion,
			WarmCold:            "fresh database per run; OS file cache not controlled",
			Repetitions:         1,
			Aggregation:         "sums and count ratios over all observations; no sampling, no averaging",
			GroundTruth:         truth.Labeling + "; " + truth.Description,
			TruthSampling:       "none: every eligible pair is labeled",
			Tokenizer:           "estimated tokens = ceil(utf8_bytes/4) (internal/tokenest); not provider tokens",
			CompleteEnumeration: truth.CompleteEnumeration,
		},
		Metrics:      metrics,
		Observations: obs,
		Limitations: []string{
			"single small synthetic Go fixture; results do not generalize to other corpora or languages",
			"pairs are matched by unqualified callee name; overloaded or same-named symbols would be conflated",
			"answer_quality is not measured: no model is run, so every answer_quality is null",
			"evidence_completeness is lexical presence of expected identifiers, not proof the context answers the task",
			"baseline mode is a fixed read-all-sources procedure, not an agent's discovery strategy",
			"fewer context bytes is not by itself better; compare only together with evidence_completeness and answer quality",
			"correctness counts are not accuracy, recall or F1 claims beyond this measured fixture",
		},
	}, nil
}

func resultNames(out []byte, field string) (map[string]bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parse codegraph output: %w", err)
	}
	var items []struct {
		Name string `json:"name"`
	}
	if raw, ok := doc[field]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("parse %s: %w", field, err)
		}
	}
	names := map[string]bool{}
	for _, it := range items {
		names[it.Name] = true
	}
	return names, nil
}

// readAllSources is the fixed baseline procedure: every regular file under
// repo except the database, in sorted path order, each prefixed by its path.
func readAllSources(repo string) ([]byte, error) {
	var paths []string
	err := filepath.WalkDir(repo, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".codegraph" {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var buf []byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(repo, p)
		buf = append(buf, "=== "+filepath.ToSlash(rel)+"\n"...)
		buf = append(buf, b...)
	}
	return buf, nil
}

func factsPresent(ctx []byte, facts []string) int {
	n := 0
	for _, f := range facts {
		// Explicit identifier boundaries; \b would misbehave for facts that
		// start or end with a non-word character.
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(f) + `($|[^A-Za-z0-9_])`)
		if re.Match(ctx) {
			n++
		}
	}
	return n
}

func sumTokens(obs []Observation, mode string) int {
	n := 0
	for _, o := range obs {
		if o.Mode == mode && o.EstTokens != nil {
			n += *o.EstTokens
		}
	}
	return n
}

func sortObservations(obs []Observation) {
	sort.SliceStable(obs, func(i, j int) bool {
		a, b := obs[i], obs[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Mode < b.Mode
	})
}

func count(name string, n int, scope string) Metric {
	return Metric{Name: name, Value: fp(float64(n)), Unit: "count", Scope: scope}
}

// ratio returns a null-valued metric when the denominator is not positive.
func ratio(name string, num, den int, def, scope string) Metric {
	m := Metric{Name: name, Unit: "ratio", Numerator: ip(num), Denominator: ip(den), DenominatorDefinition: def, Scope: scope}
	if den > 0 {
		m.Value = fp(float64(num) / float64(den))
	} else {
		m.Limitations = []string{"denominator is zero; value omitted"}
	}
	return m
}

func ip(v int) *int         { return &v }
func fp(v float64) *float64 { return &v }
