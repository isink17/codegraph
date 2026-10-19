package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/diff"
)

// Exit codes of `codegraph diff` once --fail-on-risk is given. Without it the
// command keeps the default convention: 0 on success, 1 on any error.
const (
	diffExitPass         = 0
	diffExitRisk         = 1
	diffExitInvalid      = 2
	diffExitInsufficient = 3
)

// diffGate is the opt-in CI policy. threshold 0 means no gate.
type diffGate struct {
	threshold        int
	failInsufficient bool
}

func runDiff(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRoot := fs.String("repo-root", "", "repository root (defaults to current repository)")
	limit := fs.Int("limit", 0, "changes per page (default 20, max 500)")
	offset := fs.Int("offset", 0, "offset into the deterministic change list")
	failOnRisk := fs.String("fail-on-risk", "", "fail when the risk score reaches a score 1..100 or a level: low, medium, high")
	insufficient := fs.String("insufficient-evidence", "", "with --fail-on-risk: fail (default) or pass when risk cannot be assessed")
	valueFlags := map[string]bool{"--repo-root": true, "--limit": true, "--offset": true, "--fail-on-risk": true, "--insufficient-evidence": true}
	var positional, flags []string
	flagName := func(arg string) string {
		name, _, _ := strings.Cut(arg, "=")
		return "--" + strings.TrimLeft(name, "-")
	}
	// Invalid input exits 2 once a gate is requested anywhere on the line.
	gated := false
	for _, a := range args {
		gated = gated || strings.HasPrefix(a, "-") && flagName(a) == "--fail-on-risk"
	}
	seen := map[string]bool{}
	gateFlags := map[string]bool{"--fail-on-risk": true, "--insufficient-evidence": true}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positional = append(positional, args[i])
			continue
		}
		name := flagName(args[i])
		// Last-wins parsing stays for the original flags; a gate flag given
		// twice is contradictory.
		if seen[name] && gateFlags[name] {
			return diffUsageError(gated, fmt.Errorf("%s given more than once", name))
		}
		seen[name] = true
		flags = append(flags, args[i])
		if valueFlags[name] && !strings.Contains(args[i], "=") {
			i++
			if i >= len(args) {
				return diffUsageError(gated, fmt.Errorf("missing value for %s", args[i-1]))
			}
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return diffUsageError(gated, err)
	}
	gate, err := parseDiffGate(gated, *failOnRisk, seen["--insufficient-evidence"], *insufficient)
	if err != nil {
		return diffUsageError(gated, err)
	}
	if len(positional) != 2 {
		return diffUsageError(gated, fmt.Errorf("usage: codegraph diff <base> <head> [--repo-root PATH] [--limit N --offset N] [--fail-on-risk SCORE|LEVEL [--insufficient-evidence fail|pass]]"))
	}
	root, err := config.ResolveRepoRoot(*repoRoot, "")
	if err != nil {
		return diffUsageError(gated, err)
	}
	res, err := diff.CompareCommits(ctx, root, positional[0], positional[1], newDefaultRegistry(), *offset, *limit)
	if err != nil {
		return diffUsageError(gated, err)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return diffUsageError(gated, err)
	}
	if gate.threshold == 0 {
		return nil
	}
	code, verdict := gate.evaluate(res.Diff.Risk)
	r := res.Diff.Risk
	score := "null"
	if r.Score != nil {
		score = strconv.Itoa(*r.Score)
	}
	fmt.Fprintf(stderr, "diff risk: %s, assessment %s, score %s (%s), threshold %d, %d uncertainty rows, comparison %s\n", verdict, r.Assessment, score, r.Level, gate.threshold, len(r.Uncertainty), res.Comparison.Status)
	if code != diffExitPass {
		return &ExitError{Code: code}
	}
	return nil
}

func diffUsageError(gated bool, err error) error {
	if gated {
		return &ExitError{Code: diffExitInvalid, Err: err}
	}
	return err
}

// parseDiffGate takes whether each flag was given, so an empty value is an
// error rather than an absent gate.
func parseDiffGate(gated bool, failOnRisk string, insufficientSet bool, insufficient string) (diffGate, error) {
	if !gated {
		if insufficientSet {
			return diffGate{}, fmt.Errorf("--insufficient-evidence requires --fail-on-risk")
		}
		return diffGate{}, nil
	}
	g := diffGate{failInsufficient: true}
	switch {
	case !insufficientSet, insufficient == "fail":
	case insufficient == "pass":
		g.failInsufficient = false
	default:
		return diffGate{}, fmt.Errorf("invalid --insufficient-evidence %q: want fail or pass", insufficient)
	}
	levels := map[string]int{diff.RiskLevelLow: 1, diff.RiskLevelMedium: 25, diff.RiskLevelHigh: 50}
	if n, ok := levels[failOnRisk]; ok {
		g.threshold = n
		return g, nil
	}
	n, err := strconv.Atoi(failOnRisk)
	if err != nil || n < 1 || n > diff.RiskMaxScore {
		return diffGate{}, fmt.Errorf("invalid --fail-on-risk %q: want a score 1..%d or low, medium, high", failOnRisk, diff.RiskMaxScore)
	}
	g.threshold = n
	return g, nil
}

func (g diffGate) evaluate(r diff.Risk) (int, string) {
	if r.Score == nil {
		if g.failInsufficient {
			return diffExitInsufficient, "fail (insufficient evidence)"
		}
		return diffExitPass, "pass (insufficient evidence allowed)"
	}
	if *r.Score >= g.threshold {
		return diffExitRisk, "fail"
	}
	return diffExitPass, "pass"
}
