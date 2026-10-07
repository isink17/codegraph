package diff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/limits"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
)

func TestCompareCommitsExactTreeDeterminismAndCheckoutImmutability(t *testing.T) {
	r := gittest.Init(t)
	r.Write("main.go", "package main\nfunc A() {}\n")
	r.Write("ignored.go", "package main\nfunc Ignored() {}\n")
	r.Write("expand.txt", "$Format:%H$\n")
	base := r.Commit("", "base")
	r.Write(".gitattributes", "ignored.go export-ignore\nexpand.txt export-subst\n")
	r.Write("main.go", "package main\nfunc A() { B() }\nfunc B() {}\n")
	head := r.Commit("", "head")
	r.Write("untracked.go", "package main\nfunc LocalOnly() {}\n")
	r.Write("main.go", "package main\nfunc DirtyOnly() {}\n")
	status, currentHead := r.Git("status", "--porcelain", "--untracked-files=all"), r.Git("rev-parse", "HEAD")
	reg := parser.NewRegistry(goparser.New())
	a, err := CompareCommits(t.Context(), r.Dir, base, head, reg, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CompareCommits(t.Context(), r.Dir, base, head, reg, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("repeat output differs\n%s\n%s", ja, jb)
	}
	if a.Schema != "codegraph.change/v1" || a.Diff.Schema != Schema || a.From.Commit != base || a.To.Commit != head || a.From.Tree == "" || a.To.Tree == "" {
		t.Fatalf("revision envelope = %+v", a)
	}
	baseFiles, headFiles := sourceFilesByPath(a.From.TreeFiles), sourceFilesByPath(a.To.TreeFiles)
	if _, ok := headFiles["ignored.go"]; !ok {
		t.Fatal("export-ignore removed a file from the exact tree")
	}
	want := sha256.Sum256([]byte("$Format:%H$\n"))
	if got := baseFiles["expand.txt"].ContentHash; got != hex.EncodeToString(want[:]) {
		t.Fatalf("base raw blob hash = %s", got)
	}
	if got := headFiles["expand.txt"].ContentHash; got != hex.EncodeToString(want[:]) {
		t.Fatalf("export-subst changed raw blob hash: %s", got)
	}
	for _, c := range allChanges(a) {
		if c.Kind == "file_modified" && c.Identity == "expand.txt" {
			t.Fatal("export-subst changed indexed source bytes")
		}
		if c.Kind == "declaration_added" && strings.Contains(c.Identity, "LocalOnly") {
			t.Fatal("uncommitted file entered snapshot")
		}
	}
	if !hasChange(a, "file_added", ".gitattributes") || !hasChange(a, "declaration_added", "B") {
		t.Fatalf("expected changes missing: %+v", a.Diff)
	}
	if got := r.Git("status", "--porcelain", "--untracked-files=all"); got != status {
		t.Fatalf("worktree changed: %q => %q", status, got)
	}
	if got := r.Git("rev-parse", "HEAD"); got != currentHead {
		t.Fatalf("HEAD changed: %s => %s", currentHead, got)
	}
}

func TestCompareSelfDiffAndRefusal(t *testing.T) {
	r := gittest.Init(t)
	r.Write("main.go", "package main\nfunc A() {}\n")
	rev := r.Commit("", "one")
	reg := parser.NewRegistry(goparser.New())
	a, err := CompareCommits(t.Context(), r.Dir, rev, rev, reg, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CompareCommits(t.Context(), r.Dir, rev, rev, reg, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Diff.Total != 0 || len(allChanges(a)) != 0 {
		t.Fatalf("self diff = %+v", a.Diff)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("self diff differs: %s != %s", ja, jb)
	}
	for _, tc := range []struct {
		base, head    string
		offset, limit int
	}{{"--all", "HEAD", 0, 20}, {"missing", "HEAD", 0, 20}, {rev, rev, -1, 20}, {rev, rev, 0, limits.MaxPage + 1}} {
		if _, err := CompareCommits(t.Context(), r.Dir, tc.base, tc.head, reg, tc.offset, tc.limit); err == nil {
			t.Fatalf("accepted invalid request %+v", tc)
		}
	}
}

func TestParseFailureDoesNotInventDeclarationRemovals(t *testing.T) {
	r := gittest.Init(t)
	r.Write(".codegraph/config.json", `{"parse_error_policy":"best_effort"}`)
	r.Write("main.go", "package main\nfunc A() {}\nfunc B() {}\n")
	r.Git("add", "-f", ".codegraph/config.json")
	base := r.Commit("", "valid")
	r.Write("main.go", "package main\nfunc A( {\n")
	head := r.Commit("", "parse failure")
	res, err := CompareCommits(t.Context(), r.Dir, base, head, parser.NewRegistry(goparser.New()), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.To.Graph.ParseFailures == 0 || res.Comparison.Status == "comparable" {
		t.Fatalf("parse state not disclosed: %+v", res.Comparison)
	}
	for _, c := range allChanges(res) {
		if c.Kind == "declaration_removed" {
			t.Fatalf("parse failure fabricated removal: %+v", c)
		}
	}
}

func TestPolicyDifferenceSuppressesSemanticDeltas(t *testing.T) {
	r := gittest.Init(t)
	r.Write(".codegraph/config.json", `{"exclude":["hidden.go"]}`)
	r.Write("main.go", "package main\nfunc A() {}\n")
	r.Write("hidden.go", "package main\nfunc Hidden() {}\n")
	r.Git("add", "-f", ".codegraph/config.json")
	base := r.Commit("", "excluded")
	r.Write(".codegraph/config.json", `{"exclude":[]}`)
	r.Write("main.go", "package main\nfunc A() { Hidden() }\n")
	r.Git("add", "-f", ".codegraph/config.json")
	head := r.Commit("", "included")
	if tree := r.Git("ls-tree", "-r", base); !strings.Contains(tree, ".codegraph/config.json") {
		t.Fatalf("base config was not committed: %s", tree)
	}
	res, err := CompareCommits(t.Context(), r.Dir, base, head, parser.NewRegistry(goparser.New()), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Comparison.Status != "incompatible" || len(res.Diff.Symbols.Added)+len(res.Diff.Symbols.Removed)+len(res.Diff.Symbols.Changed) != 0 {
		t.Fatalf("policy mismatch was compared: %+v", res)
	}
	if !strings.Contains(strings.Join(res.Comparison.Limitations, ";"), "indexing policies differ") {
		t.Fatalf("missing policy limitation: %+v", res.Comparison)
	}
}

func TestGitCommandsDisableLazyFetchAndReplacement(t *testing.T) {
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	t.Setenv("GIT_NO_REPLACE_OBJECTS", "0")
	cmd := gitCommand(t.Context(), t.TempDir(), "rev-parse")
	got := map[string]string{}
	for _, v := range cmd.Env {
		if k, value, ok := strings.Cut(v, "="); ok && (k == "GIT_NO_LAZY_FETCH" || k == "GIT_NO_REPLACE_OBJECTS") {
			got[k] = value
		}
	}
	if got["GIT_NO_LAZY_FETCH"] != "1" || got["GIT_NO_REPLACE_OBJECTS"] != "1" {
		t.Fatalf("Git safety env = %v", got)
	}
}

func TestReplaceRefsIgnoredAndDashPathsAllowed(t *testing.T) {
	r := gittest.Init(t)
	r.Write("-main.go", "package main\nfunc A() {}\n")
	old := r.Commit("", "old")
	r.Write("-main.go", "package main\nfunc A() { B() }\nfunc B() {}\n")
	newer := r.Commit("", "new")
	r.Git("replace", old, newer)
	res, err := CompareCommits(t.Context(), r.Dir, old, newer, parser.NewRegistry(goparser.New()), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.From.Commit != old || res.To.Commit != newer || len(res.Diff.Symbols.Added) == 0 {
		t.Fatalf("replacement altered requested commit: %+v", res)
	}
}

func TestSnapshotCancellationCleansScratch(t *testing.T) {
	r := gittest.Init(t)
	r.Write("a.fake", "source")
	rev := r.Commit("", "one")
	ctx, cancel := context.WithCancel(t.Context())
	reg := parser.NewRegistry(cancelAdapter{cancel})
	before := diffScratchDirs(t)
	_, err := snapshot(ctx, r.Dir, rev, reg)
	if err == nil {
		t.Fatal("expected canceled indexing")
	}
	if after := diffScratchDirs(t); strings.Join(before, "\x00") != strings.Join(after, "\x00") {
		t.Fatalf("scratch leaked: before=%v after=%v", before, after)
	}
}

type cancelAdapter struct{ cancel context.CancelFunc }

func (a cancelAdapter) Language() string       { return "fake" }
func (a cancelAdapter) Supports(p string) bool { return strings.HasSuffix(p, ".fake") }
func (a cancelAdapter) Extensions() []string   { return []string{".fake"} }
func (a cancelAdapter) Parse(context.Context, string, []byte) (graph.ParsedFile, error) {
	a.cancel()
	return graph.ParsedFile{}, nil
}

func TestParseTreeRejectsGitlinkAndUnsafePath(t *testing.T) {
	for _, raw := range [][]byte{[]byte("160000 commit abc\tsubmodule\x00"), []byte("100644 blob abc\t../escape\x00"), append([]byte("100644 blob abc\tbad-"), append([]byte{0xff}, 0)...)} {
		if _, err := parseTree(raw); err == nil {
			t.Fatalf("accepted unsafe entry %q", raw)
		}
	}
}

func TestSnapshotLimits(t *testing.T) {
	if err := validateSnapshotFileCount(maxSnapshotFiles); err != nil {
		t.Fatalf("limit file count: %v", err)
	}
	if err := validateSnapshotFileCount(maxSnapshotFiles + 1); err == nil {
		t.Fatal("accepted too many files")
	}
	if got, err := addSnapshotBytes(maxSnapshotBytes-1, 1); err != nil || got != maxSnapshotBytes {
		t.Fatalf("boundary bytes = %d, %v", got, err)
	}
	for _, tc := range []struct{ total, size int64 }{{maxSnapshotBytes, 1}, {0, maxSnapshotBytes + 1}, {0, -1}} {
		if _, err := addSnapshotBytes(tc.total, tc.size); err == nil {
			t.Fatalf("accepted total=%d size=%d", tc.total, tc.size)
		}
	}
}

func TestBoundedGitOutputCapture(t *testing.T) {
	buf := newBoundedCapture(4, nil)
	if n, err := buf.Write([]byte("abcde")); err != nil || n != 5 {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	if !buf.exceeded || buf.buffer.String() != "abcd" {
		t.Fatalf("capture = %q exceeded=%v", buf.buffer.String(), buf.exceeded)
	}
}

func diffScratchDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "codegraph-diff-") {
			out = append(out, e.Name())
		}
	}
	return out
}
func sourceFilesByPath(v []SourceFile) map[string]SourceFile {
	out := make(map[string]SourceFile, len(v))
	for _, f := range v {
		out[f.Path] = f
	}
	return out
}
func hasChange(r Result, kind, contains string) bool {
	for _, c := range allChanges(r) {
		if c.Kind == kind && strings.Contains(c.Identity, contains) {
			return true
		}
	}
	return false
}
func allChanges(r Result) []Change {
	var out []Change
	out = append(out, r.Diff.Files.Added...)
	out = append(out, r.Diff.Files.Removed...)
	out = append(out, r.Diff.Files.Modified...)
	out = append(out, r.Diff.Symbols.Added...)
	out = append(out, r.Diff.Symbols.Removed...)
	out = append(out, r.Diff.Symbols.Changed...)
	out = append(out, r.Diff.Edges.Added...)
	out = append(out, r.Diff.Edges.Removed...)
	out = append(out, r.Diff.Edges.Retargeted...)
	out = append(out, r.Diff.Edges.ResolutionChanged...)
	out = append(out, r.Diff.Edges.EvidenceChanged...)
	out = append(out, r.Diff.TestLinks.Added...)
	out = append(out, r.Diff.TestLinks.Removed...)
	out = append(out, r.Diff.TestLinks.Changed...)
	return out
}
