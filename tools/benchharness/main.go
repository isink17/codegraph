// Command benchharness produces and checks reproducible correctness and
// context-volume benchmark records for CodeGraph. It is a developer tool, not
// part of the codegraph binary. See README.md for commands and methodology.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "benchharness:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: benchharness run|validate|summarize|compare ...")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "run":
		var cfg RunConfig
		out := fs.String("out", "", "record JSON output path (required)")
		fs.StringVar(&cfg.FixtureDir, "fixture", "", "fixture directory with src/ and truth.json")
		fs.StringVar(&cfg.Binary, "codegraph", "", "codegraph binary built from --sha")
		fs.StringVar(&cfg.CodeGraphSHA, "sha", "", "full CodeGraph commit SHA the binary was built from")
		fs.StringVar(&cfg.DateUTC, "date", "", "UTC timestamp to record (RFC 3339)")
		fs.StringVar(&cfg.WorkDir, "workdir", "", "scratch directory (fixture copy and database)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *out == "" || cfg.FixtureDir == "" || cfg.Binary == "" || cfg.CodeGraphSHA == "" || cfg.DateUTC == "" || cfg.WorkDir == "" {
			return fmt.Errorf("run: --out --fixture --codegraph --sha --date --workdir are all required")
		}
		rec, err := Run(cfg)
		if err != nil {
			return err
		}
		if err := Validate(rec); err != nil {
			return fmt.Errorf("produced record is invalid: %w", err)
		}
		b, err := Marshal(rec)
		if err != nil {
			return err
		}
		return os.WriteFile(*out, b, 0o644)
	case "validate":
		sha := fs.String("expect-sha", "", "reject the record unless codegraph.sha equals this")
		fixture := fs.String("fixture", "", "reject the record unless the fixture hash matches this directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		rec, err := load(fs.Arg(0))
		if err != nil {
			return err
		}
		if err := Validate(rec); err != nil {
			return err
		}
		if err := CheckCurrent(rec, *sha, *fixture); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, "valid")
		return err
	case "summarize":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		rec, err := load(fs.Arg(0))
		if err != nil {
			return err
		}
		if err := Validate(rec); err != nil {
			return err
		}
		_, err = io.WriteString(stdout, Summarize(rec))
		return err
	case "compare":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		a, err := load(fs.Arg(0))
		if err != nil {
			return err
		}
		b, err := load(fs.Arg(1))
		if err != nil {
			return err
		}
		if err := Compare(a, b); err != nil {
			return err
		}
		_, err = io.WriteString(stdout, CompareSummary(a, b))
		return err
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func load(path string) (*Record, error) {
	if path == "" {
		return nil, fmt.Errorf("record path required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r Record
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// Marshal is the canonical encoding: indented, trailing newline.
func Marshal(r *Record) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	return append(b, '\n'), err
}

func fmtValue(m Metric) string {
	if m.Value == nil {
		return "n/a"
	}
	v := strconv.FormatFloat(*m.Value, 'f', -1, 64)
	if m.Unit == "ratio" {
		v = strconv.FormatFloat(*m.Value, 'f', 4, 64) + fmt.Sprintf(" (%d/%d)", *m.Numerator, *m.Denominator)
	}
	return v
}

// Summarize renders the record deterministically; it adds no information
// that is not in the record.
func Summarize(r *Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Benchmark record: %s\n\n", r.Benchmark)
	fmt.Fprintf(&b, "- Schema: %s\n- Methodology: %s\n- CodeGraph: %s (%s)\n- Date (UTC): %s\n",
		r.Schema, r.Methodology.Version, r.CodeGraph.SHA, r.CodeGraph.Version, r.DateUTC)
	fmt.Fprintf(&b, "- Platform: %s/%s, %s\n", r.Platform.OS, r.Platform.Arch, r.Platform.Go)
	for _, f := range r.Fixtures {
		fmt.Fprintf(&b, "- Fixture: %s sha256=%s\n", f.Name, f.SHA)
	}
	fmt.Fprintf(&b, "- Ground truth: %s\n- Tokenizer: %s\n- Repetitions: %d; %s\n- Warm/cold: %s\n\n",
		r.Methodology.GroundTruth, r.Methodology.Tokenizer, r.Methodology.Repetitions, r.Methodology.Aggregation, r.Methodology.WarmCold)
	b.WriteString("## Metrics\n\n| Metric | Value | Unit | Scope | Denominator |\n|---|---|---|---|---|\n")
	for _, m := range r.Metrics {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", m.Name, fmtValue(m), m.Unit, m.Scope, m.DenominatorDefinition)
	}
	b.WriteString("\n## Observations\n\n| Kind | ID | Mode | Outcome | Bytes | Est. tokens | Facts | Answer quality |\n|---|---|---|---|---|---|---|---|\n")
	for _, o := range r.Observations {
		facts, q := "", "not measured"
		if o.FactsExpected != nil {
			facts = fmt.Sprintf("%d/%d", *o.FactsPresent, *o.FactsExpected)
		}
		if o.AnswerQuality != nil {
			q = *o.AnswerQuality
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", o.Kind, o.ID, o.Mode, o.Outcome, optInt(o.Bytes), optInt(o.EstTokens), facts, q)
	}
	b.WriteString("\n## Commands\n\n")
	for _, c := range r.Commands {
		fmt.Fprintf(&b, "    %s\n", c)
	}
	b.WriteString("\n## Limitations\n\n")
	for _, l := range r.Limitations {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	return b.String()
}

// CompareSummary lists metric values side by side. It states differences
// only; it does not rank either record as better.
func CompareSummary(a, b *Record) string {
	var s strings.Builder
	fmt.Fprintf(&s, "# Comparison (same schema, methodology, fixtures)\n\n- A: %s %s\n- B: %s %s\n\n| Metric | A | B |\n|---|---|---|\n",
		a.CodeGraph.SHA, a.DateUTC, b.CodeGraph.SHA, b.DateUTC)
	bm := map[string]Metric{}
	for _, m := range b.Metrics {
		bm[m.Name] = m
	}
	for _, m := range a.Metrics {
		other, ok := bm[m.Name]
		bv := "absent"
		if ok {
			bv = fmtValue(other)
		}
		fmt.Fprintf(&s, "| %s | %s | %s |\n", m.Name, fmtValue(m), bv)
	}
	s.WriteString("\nContext-volume differences are not improvements unless evidence completeness and answer quality are not worse.\n")
	return s.String()
}

func optInt(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}
