package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var sumLine = regexp.MustCompile(`^([0-9a-f]{64})  ([A-Za-z0-9._-]+)$`)

// verifyBundle checks a bundle written by produce: its checksums, the binary
// and fixture manifest it names, and that produce regenerates every file byte
// for byte from the recorded commit and date. Any difference is reported per
// file and makes the command fail.
func verifyBundle(args []string, invoke func([]string, io.Writer) error, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify-bundle", flag.ContinueOnError)
	dir := fs.String("bundle", "", "bundle directory written by produce")
	manifest := fs.String("manifest", "", "committed fixture manifest the bundle was produced from")
	binary := fs.String("codegraph", "", "CodeGraph binary rebuilt from the recorded commit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *manifest == "" || *binary == "" || fs.NArg() != 0 {
		return fmt.Errorf("verify-bundle: --bundle --manifest --codegraph required; no positional arguments")
	}
	bundle, err := readBundle(*dir)
	if err != nil {
		return err
	}
	var problems []string
	sums, err := parseSums(bundle["SHA256SUMS"])
	if err != nil {
		return err
	}
	for name, want := range sums {
		b, ok := bundle[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: listed in SHA256SUMS but missing", name))
		case hashBytes(b) != want:
			problems = append(problems, fmt.Sprintf("%s: sha256 %s, SHA256SUMS says %s", name, hashBytes(b), want))
		}
	}
	for name := range bundle {
		if _, ok := sums[name]; !ok && name != "SHA256SUMS" {
			problems = append(problems, fmt.Sprintf("%s: not listed in SHA256SUMS", name))
		}
	}
	if len(problems) > 0 {
		return report(stdout, problems)
	}
	var bm bundleManifest
	if err := decodeStrictBytes("MANIFEST.json", bundle["MANIFEST.json"], &bm); err != nil {
		return err
	}
	if bm.Schema != "codegraph.benchmark_bundle/v1" {
		return fmt.Errorf("verify-bundle: MANIFEST.json schema %q", bm.Schema)
	}
	binaryBytes, err := os.ReadFile(*binary)
	if err != nil {
		return err
	}
	if h := hashBytes(binaryBytes); h != bm.Binary {
		problems = append(problems, fmt.Sprintf("binary sha256 %s differs from MANIFEST.json %s (a different build or commit)", h, bm.Binary))
	}
	inputBytes, err := os.ReadFile(*manifest)
	if err != nil {
		return err
	}
	if h := hashBytes(inputBytes); h != bm.Input {
		problems = append(problems, fmt.Sprintf("fixture manifest sha256 %s differs from MANIFEST.json %s", h, bm.Input))
	}
	if len(problems) > 0 {
		return report(stdout, problems)
	}
	scratch, err := os.MkdirTemp("", "benchharness-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	out := filepath.Join(scratch, "bundle")
	if err := produce([]string{"--manifest", *manifest, "--codegraph", *binary, "--sha", bm.Commit, "--date", bm.Date, "--out", out}, invoke); err != nil {
		return fmt.Errorf("verify-bundle: rerun: %w", err)
	}
	rerun, err := readBundle(out)
	if err != nil {
		return err
	}
	for _, name := range unionNames(bundle, rerun) {
		a, inBundle := bundle[name]
		b, inRerun := rerun[name]
		switch {
		case !inRerun:
			problems = append(problems, fmt.Sprintf("%s: in the bundle, not produced by the rerun", name))
		case !inBundle:
			problems = append(problems, fmt.Sprintf("%s: produced by the rerun, not in the bundle", name))
		case !bytes.Equal(a, b):
			problems = append(problems, name+": differs from the rerun\n"+lineDiff(a, b))
		}
	}
	if len(problems) > 0 {
		return report(stdout, problems)
	}
	fmt.Fprintf(stdout, "verify-bundle: %d files match a rerun of commit %s\n", len(bundle), bm.Commit)
	return nil
}

// readBundle reads every regular file of a flat bundle directory.
func readBundle(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("verify-bundle: %s is not a regular file", e.Name())
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files[e.Name()] = b
	}
	return files, nil
}

func parseSums(b []byte) (map[string]string, error) {
	if b == nil {
		return nil, fmt.Errorf("verify-bundle: SHA256SUMS missing")
	}
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		m := sumLine.FindStringSubmatch(sc.Text())
		if m == nil || m[2] == "SHA256SUMS" {
			return nil, fmt.Errorf("verify-bundle: malformed SHA256SUMS line %q", sc.Text())
		}
		if _, dup := sums[m[2]]; dup {
			return nil, fmt.Errorf("verify-bundle: SHA256SUMS lists %s twice", m[2])
		}
		sums[m[2]] = m[1]
	}
	return sums, sc.Err()
}

func unionNames(a, b map[string][]byte) []string {
	seen := map[string]bool{}
	for n := range a {
		seen[n] = true
	}
	for n := range b {
		seen[n] = true
	}
	var names []string
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// lineDiff lists the first differing lines by position, bounded.
func lineDiff(a, b []byte) string {
	x, y := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	var out strings.Builder
	shown := 0
	for i := 0; i < len(x) || i < len(y); i++ {
		var l, r string
		if i < len(x) {
			l = x[i]
		}
		if i < len(y) {
			r = y[i]
		}
		if l == r {
			continue
		}
		if shown == 10 {
			out.WriteString("  ...\n")
			break
		}
		fmt.Fprintf(&out, "  line %d\n  - bundle: %s\n  + rerun:  %s\n", i+1, l, r)
		shown++
	}
	return out.String()
}

func report(w io.Writer, problems []string) error {
	sort.Strings(problems)
	for _, p := range problems {
		fmt.Fprintln(w, p)
	}
	return fmt.Errorf("verify-bundle: %d problem(s)", len(problems))
}
