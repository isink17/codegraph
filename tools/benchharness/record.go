package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SchemaVersion identifies the record layout. Bump on any incompatible change.
const SchemaVersion = "codegraph.benchmark/v1"

// MethodologyVersion identifies how observations are produced and counted.
// Records with different methodology versions are never compared.
const MethodologyVersion = "fixture-callgraph-context/v1"

// Record is one benchmark run. Every aggregate metric is recomputable from
// Observations; summaries are rendered from a Record only.
type Record struct {
	Schema        string        `json:"schema"`
	Benchmark     string        `json:"benchmark"`
	CodeGraph     CodeGraph     `json:"codegraph"`
	DateUTC       string        `json:"date_utc"`
	Platform      Platform      `json:"platform"`
	Fixtures      []Fixture     `json:"fixtures"`
	Commands      []string      `json:"commands"`
	Methodology   Methodology   `json:"methodology"`
	Metrics       []Metric      `json:"metrics"`
	Observations  []Observation `json:"observations"`
	Limitations   []string      `json:"limitations"`
	EvidencePaths []string      `json:"evidence_paths,omitempty"`
}

type CodeGraph struct {
	SHA     string `json:"sha"`
	Version string `json:"version"`
}

type Platform struct {
	OS   string            `json:"os"`
	Arch string            `json:"arch"`
	Go   string            `json:"go"`
	Env  map[string]string `json:"env"`
}

type Fixture struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	SHA   string `json:"sha"`
	Setup string `json:"setup"`
}

type Methodology struct {
	Version             string `json:"version"`
	WarmCold            string `json:"warm_cold"`
	Repetitions         int    `json:"repetitions"`
	Aggregation         string `json:"aggregation"`
	GroundTruth         string `json:"ground_truth"`
	TruthSampling       string `json:"truth_sampling"`
	Tokenizer           string `json:"tokenizer"`
	CompleteEnumeration bool   `json:"complete_enumeration"`
}

// Metric is a count-based aggregate. Value is nil when the denominator is not
// valid; a rate is never serialized without its numerator and denominator.
type Metric struct {
	Name                  string   `json:"name"`
	Value                 *float64 `json:"value"`
	Unit                  string   `json:"unit"`
	Numerator             *int     `json:"numerator,omitempty"`
	Denominator           *int     `json:"denominator,omitempty"`
	DenominatorDefinition string   `json:"denominator_definition,omitempty"`
	Scope                 string   `json:"scope"`
	Limitations           []string `json:"limitations,omitempty"`
}

// Observation is one raw measured fact. Kind "pair" is a ground-truth call
// pair; kind "unexpected" is a resolved edge to a labeled target that is not
// a labeled pair; kind "outside_universe" is a resolved edge to an unlabeled
// callee (excluded from precision); kind "context" is one task/mode context
// measurement.
type Observation struct {
	Kind          string  `json:"kind"`
	ID            string  `json:"id"`
	Mode          string  `json:"mode,omitempty"`
	Outcome       string  `json:"outcome,omitempty"`
	Bytes         *int    `json:"context_bytes,omitempty"`
	EstTokens     *int    `json:"estimated_tokens,omitempty"`
	FactsExpected *int    `json:"facts_expected,omitempty"`
	FactsPresent  *int    `json:"facts_present,omitempty"`
	AnswerQuality *string `json:"answer_quality"`
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

var rateName = regexp.MustCompile(`precision|coverage|recall|share|completeness|rate`)

var allowedUnits = map[string]bool{"count": true, "bytes": true, "estimated_tokens": true, "ratio": true}

// forbiddenMetricNames are claims this harness cannot support from its data.
var forbiddenMetricNames = []string{"accuracy", "f1", "perfect"}

// Validate rejects records that lack identity, denominators, methodology,
// ground truth, or that serialize unsupported claims.
func Validate(r *Record) error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if r.Schema != SchemaVersion {
		bad("schema %q, want %q", r.Schema, SchemaVersion)
	}
	if r.Benchmark != "correctness+context" {
		bad("benchmark %q unsupported", r.Benchmark)
	}
	if !shaRE.MatchString(r.CodeGraph.SHA) {
		bad("codegraph.sha missing or not a full hex SHA: %q", r.CodeGraph.SHA)
	}
	if r.DateUTC == "" {
		bad("date_utc missing")
	}
	if r.Platform.OS == "" || r.Platform.Arch == "" || r.Platform.Go == "" {
		bad("platform os/arch/go incomplete")
	}
	if len(r.Fixtures) == 0 {
		bad("no fixture identity")
	}
	for _, f := range r.Fixtures {
		if f.Name == "" || !shaRE.MatchString(f.SHA) {
			bad("fixture %q missing name or sha", f.Name)
		}
	}
	if len(r.Commands) == 0 {
		bad("commands missing")
	}
	m := r.Methodology
	if m.Version != MethodologyVersion {
		bad("methodology.version %q, want %q", m.Version, MethodologyVersion)
	}
	if m.GroundTruth == "" || m.Tokenizer == "" || m.Aggregation == "" || m.Repetitions < 1 {
		bad("methodology incomplete (ground_truth, tokenizer, aggregation, repetitions)")
	}
	if len(r.Limitations) == 0 {
		bad("limitations must be explicit")
	}
	if len(r.Observations) == 0 {
		bad("no observations")
	}
	for _, mt := range r.Metrics {
		lower := strings.ToLower(mt.Name)
		for _, f := range forbiddenMetricNames {
			if strings.Contains(lower, f) {
				bad("metric %q: unsupported claim %q", mt.Name, f)
			}
		}
		if strings.Contains(lower, "recall") && !m.CompleteEnumeration {
			bad("metric %q: recall requires complete_enumeration ground truth", mt.Name)
		}
		if rateName.MatchString(lower) && mt.Unit != "ratio" {
			bad("metric %q: rate-like name requires unit ratio", mt.Name)
		}
		if (strings.Contains(lower, "coverage") || strings.Contains(lower, "unresolved_share")) && !m.CompleteEnumeration && mt.Value != nil {
			bad("metric %q: must be null without complete_enumeration ground truth", mt.Name)
		}
		if !allowedUnits[mt.Unit] {
			bad("metric %q: unit %q not allowed", mt.Name, mt.Unit)
		}
		if mt.Scope == "" {
			bad("metric %q: scope missing", mt.Name)
		}
		if mt.Unit == "ratio" && mt.Value != nil {
			if mt.Numerator == nil || mt.Denominator == nil || mt.DenominatorDefinition == "" {
				bad("metric %q: ratio without numerator/denominator/definition", mt.Name)
			} else if *mt.Denominator <= 0 {
				bad("metric %q: non-positive denominator", mt.Name)
			} else if got := float64(*mt.Numerator) / float64(*mt.Denominator); got != *mt.Value {
				bad("metric %q: value %v != %d/%d", mt.Name, *mt.Value, *mt.Numerator, *mt.Denominator)
			}
		}
	}
	return errors.Join(errs...)
}

// CheckCurrent rejects a record whose CodeGraph or fixture SHA is stale
// relative to the expected values. Empty expectations are skipped.
func CheckCurrent(r *Record, wantCodeGraphSHA, fixtureDir string) error {
	if wantCodeGraphSHA != "" && r.CodeGraph.SHA != wantCodeGraphSHA {
		return fmt.Errorf("stale record: codegraph sha %s, expected %s", r.CodeGraph.SHA, wantCodeGraphSHA)
	}
	if fixtureDir != "" {
		got, err := HashDir(fixtureDir)
		if err != nil {
			return err
		}
		if len(r.Fixtures) != 1 || r.Fixtures[0].SHA != got {
			return fmt.Errorf("stale record: fixture sha does not match %s (%s)", fixtureDir, got)
		}
	}
	return nil
}

// HashDir hashes every regular file under dir (sorted slash paths, length
// prefixed contents), skipping .codegraph database directories.
func HashDir(dir string) (string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".codegraph" {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Compare refuses records that do not measure the same thing the same way.
func Compare(a, b *Record) error {
	if err := errors.Join(Validate(a), Validate(b)); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	switch {
	case a.Schema != b.Schema:
		return fmt.Errorf("incompatible: schema %s vs %s", a.Schema, b.Schema)
	case a.Benchmark != b.Benchmark:
		return fmt.Errorf("incompatible: benchmark %s vs %s", a.Benchmark, b.Benchmark)
	case a.Methodology != b.Methodology:
		return fmt.Errorf("incompatible: methodology differs")
	case len(a.Fixtures) != len(b.Fixtures):
		return fmt.Errorf("incompatible: fixture sets differ")
	}
	for i := range a.Fixtures {
		if a.Fixtures[i].Name != b.Fixtures[i].Name || a.Fixtures[i].SHA != b.Fixtures[i].SHA {
			return fmt.Errorf("incompatible: fixture %s@%s vs %s@%s",
				a.Fixtures[i].Name, a.Fixtures[i].SHA, b.Fixtures[i].Name, b.Fixtures[i].SHA)
		}
	}
	return nil
}
