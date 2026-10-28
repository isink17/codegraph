package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
)

type produceFixture struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type produceFixtureManifest struct {
	Schema   string           `json:"schema"`
	Fixtures []produceFixture `json:"fixtures"`
}
type bundleManifest struct {
	Schema   string           `json:"schema"`
	Commit   string           `json:"codegraph_commit"`
	Binary   string           `json:"binary_sha256"`
	Date     string           `json:"date"`
	Go       string           `json:"go_version"`
	OS       string           `json:"os"`
	Arch     string           `json:"arch"`
	Input    string           `json:"fixture_manifest_sha256"`
	Fixtures []produceFixture `json:"fixtures"`
	Commands [][]string       `json:"commands"`
}

// Command paths use explicit substitution parameters so temporary paths never
// affect bundle bytes. The ledger records argv, without shell quoting ambiguity.
func produce(args []string, invoke func([]string, io.Writer) error) error {
	fs := flag.NewFlagSet("produce", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "committed fixture manifest")
	binary := fs.String("codegraph", "", "CodeGraph binary")
	sha := fs.String("sha", "", "CodeGraph commit")
	date := fs.String("date", "", "UTC timestamp")
	out := fs.String("out", "", "empty or absent bundle directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifest == "" || *binary == "" || *sha == "" || *date == "" || *out == "" || fs.NArg() != 0 {
		return fmt.Errorf("produce: --manifest --codegraph --sha --date --out required; no positional arguments")
	}
	entries, err := os.ReadDir(*out)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("produce: output directory must be empty")
	}
	var input produceFixtureManifest
	if err := decodeStrict(*manifest, &input); err != nil {
		return err
	}
	if input.Schema != "codegraph.benchmark_fixtures/v1" || len(input.Fixtures) == 0 {
		return fmt.Errorf("produce: invalid fixture manifest")
	}
	sort.Slice(input.Fixtures, func(i, j int) bool { return input.Fixtures[i].Name < input.Fixtures[j].Name })
	seen := map[string]bool{}
	for _, f := range input.Fixtures {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`).MatchString(f.Name) || f.Name == "fixtures" || f.Name == "manifest" || seen[f.Name] || isAbsolutePath(f.Path) || f.Path == "" {
			return fmt.Errorf("produce: invalid or duplicate fixture %q", f.Name)
		}
		seen[f.Name] = true
		h, err := HashDir(filepath.Join(filepath.Dir(*manifest), f.Path))
		if err != nil {
			return err
		}
		if h != f.SHA256 {
			return fmt.Errorf("produce: fixture %s hash differs from manifest", f.Name)
		}
	}
	// Treat --codegraph as a filesystem path consistently: a bare name must
	// not hash a local file and then execute a different binary from PATH.
	binaryPath, err := filepath.Abs(*binary)
	if err != nil {
		return err
	}
	binaryBytes, err := os.ReadFile(binaryPath)
	if err != nil {
		return err
	}
	inputBytes, err := os.ReadFile(*manifest)
	if err != nil {
		return err
	}
	bm := bundleManifest{Schema: "codegraph.benchmark_bundle/v1", Commit: *sha, Binary: hashBytes(binaryBytes), Date: *date, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, Input: hashBytes(inputBytes), Fixtures: input.Fixtures}
	scratch, err := os.MkdirTemp("", "benchharness-produce-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	files := map[string][]byte{"fixtures.json": inputBytes}
	var summary bytes.Buffer
	for _, f := range input.Fixtures {
		fixture := filepath.Join(filepath.Dir(*manifest), f.Path)
		record := filepath.Join(scratch, f.Name+".json")
		work := filepath.Join(scratch, f.Name)
		cmd := []string{"run", "--fixture", fixture, "--codegraph", binaryPath, "--sha", *sha, "--date", *date, "--workdir", work, "--out", record}
		if err := invoke(cmd, io.Discard); err != nil {
			return err
		}
		if err := invoke([]string{"validate", "--expect-sha", *sha, "--fixture", fixture, record}, io.Discard); err != nil {
			return fmt.Errorf("produce: %s: %w", f.Name, err)
		}
		rec, err := load(record)
		if err != nil {
			return err
		}
		if len(rec.Fixtures) != 1 || rec.Fixtures[0].SHA != f.SHA256 {
			return fmt.Errorf("produce: fixture changed during run")
		}
		rec.Fixtures[0].Path = f.Path
		files[f.Name+".json"], err = Marshal(rec)
		if err != nil {
			return err
		}
		if err := invoke([]string{"summarize", record}, &summary); err != nil {
			return err
		}
		bm.Commands = append(bm.Commands,
			[]string{"go", "run", "./tools/benchharness", "run", "--fixture", "<manifest-dir>/" + f.Path, "--codegraph", "<codegraph>", "--sha", *sha, "--date", *date, "--workdir", "<scratch>/" + f.Name, "--out", f.Name + ".json"},
			[]string{"go", "run", "./tools/benchharness", "validate", "--expect-sha", *sha, "--fixture", "<manifest-dir>/" + f.Path, f.Name + ".json"},
			[]string{"go", "run", "./tools/benchharness", "summarize", f.Name + ".json"})
	}
	files["summary.md"] = summary.Bytes()
	files["MANIFEST.json"], err = marshalJSON(bm)
	if err != nil {
		return err
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var sums bytes.Buffer
	for _, name := range names {
		fmt.Fprintf(&sums, "%s  %s\n", hashBytes(files[name]), name)
	}
	files["SHA256SUMS"] = sums.Bytes()
	// Publish only after all records passed the existing validator.
	return writeBundle(*out, files)
}

// writeBundle writes every file into a sibling staging directory and renames
// it to out, so a failed write never leaves a partial bundle that a rerun
// would then refuse as a nonempty output directory. out is absent or empty.
// A symlinked out is refused: removing it would replace the link, not fill
// its target.
func writeBundle(out string, files map[string][]byte) error {
	if fi, err := os.Lstat(out); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("produce: output directory %s is a symlink", out)
	}
	parent := filepath.Dir(filepath.Clean(out))
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".produce-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0755); err != nil {
		return err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(stage, name), b, 0644); err != nil {
			return err
		}
	}
	if err := os.Remove(out); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(stage, out)
}
func hashBytes(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
