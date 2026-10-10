package diff

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func TestFileRenamesPairOnlyUniqueIdenticalContent(t *testing.T) {
	f := func(path, hash string) SourceFile { return SourceFile{Path: path, ContentHash: hash, Language: "go"} }
	exact := func(from, to, hash string) []FileRename {
		return []FileRename{{From: from, To: to, ContentHash: hash, Language: "go"}}
	}
	for _, tc := range []struct {
		name      string
		base      []SourceFile
		head      []SourceFile
		exact     []FileRename
		ambiguous []string // reasons
	}{
		{name: "rename", base: []SourceFile{f("A.go", "h1")}, head: []SourceFile{f("B.go", "h1")}, exact: exact("A.go", "B.go", "h1")},
		{name: "case only", base: []SourceFile{f("A.go", "h1")}, head: []SourceFile{f("a.go", "h1")}, exact: exact("A.go", "a.go", "h1")},
		{name: "across directories", base: []SourceFile{f("x/A.go", "h1")}, head: []SourceFile{f("y/z/A.go", "h1")}, exact: exact("x/A.go", "y/z/A.go", "h1")},
		{name: "reverse", base: []SourceFile{f("B.go", "h1")}, head: []SourceFile{f("A.go", "h1")}, exact: exact("B.go", "A.go", "h1")},
		{name: "copy keeps source", base: []SourceFile{f("A.go", "h1")}, head: []SourceFile{f("A.go", "h1"), f("B.go", "h1")}},
		{name: "copy source is another retained file", base: []SourceFile{f("A.go", "h1"), f("K.go", "h1")}, head: []SourceFile{f("B.go", "h1"), f("K.go", "h1")}, ambiguous: []string{"a file present in both revisions shares this content, so the addition may be a copy"}},
		{name: "two added candidates", base: []SourceFile{f("A.go", "h1")}, head: []SourceFile{f("B.go", "h1"), f("C.go", "h1")}, ambiguous: []string{"several removed or added files share this content"}},
		{name: "two removed candidates", base: []SourceFile{f("A.go", "h1"), f("B.go", "h1")}, head: []SourceFile{f("C.go", "h1")}, ambiguous: []string{"several removed or added files share this content"}},
		{name: "content changed", base: []SourceFile{f("A.go", "h1")}, head: []SourceFile{f("B.go", "h2")}},
		{name: "delete and recreate same path", base: []SourceFile{f("A.go", "h1")}, head: []SourceFile{f("A.go", "h1")}},
		{name: "deletion only", base: []SourceFile{f("A.go", "h1")}, head: []SourceFile{}},
		{name: "language differs", base: []SourceFile{f("A.go", "h1")}, head: []SourceFile{{Path: "A.txt", ContentHash: "h1"}}, ambiguous: []string{"language differs between the removed and added file"}},
		{name: "missing hash", base: []SourceFile{f("A.go", "")}, head: []SourceFile{f("B.go", "")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Compare(Revision{TreeFiles: tc.base}, Revision{TreeFiles: tc.head}, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			r := got.Diff.FileRenames
			if tc.exact == nil {
				tc.exact = []FileRename{}
			}
			if !reflect.DeepEqual(r.Exact, tc.exact) {
				t.Fatalf("exact = %+v, want %+v", r.Exact, tc.exact)
			}
			var reasons []string
			for _, a := range r.Ambiguous {
				reasons = append(reasons, a.Reason)
			}
			if !reflect.DeepEqual(reasons, tc.ambiguous) {
				t.Fatalf("ambiguous = %+v, want %v", r.Ambiguous, tc.ambiguous)
			}
			// The annotation never replaces the file changes it describes.
			for _, p := range r.Exact {
				if !hasFileChange(got.Diff.Files.Removed, p.From) || !hasFileChange(got.Diff.Files.Added, p.To) {
					t.Fatalf("rename %v hid its file changes: %+v", p, got.Diff.Files)
				}
			}
		})
	}
}

func TestFileRenamesRefusedWhenPoliciesDiffer(t *testing.T) {
	base := Revision{TreeFiles: []SourceFile{{Path: "A.go", ContentHash: "h1", Language: "go"}}, IndexPolicy: IndexPolicy{Exclude: []string{"x/**"}}}
	head := Revision{TreeFiles: []SourceFile{{Path: "B.go", ContentHash: "h1", Language: "go"}}}
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r := got.Diff.FileRenames; len(r.Exact) != 0 || r.Limitation == "" {
		t.Fatalf("paired renames across differing policies: %+v", r)
	}
}

// A rename keeps declarations removed and added and keeps risk citing the
// removed declaration's surviving callers: a moved file may change package or
// import semantics, so identity across paths is not asserted.
func TestFileRenameKeepsDeclarationRemovalAndRisk(t *testing.T) {
	oldT, newT, caller := riskDecl("a.go", "T"), riskDecl("b.go", "T"), riskDecl("c.go", "C")
	files := func(p string) []store.SemanticFile {
		fs := indexed(p, "c.go")
		fs[0].ContentHash, fs[1].ContentHash = "h1", "hc"
		return fs
	}
	base := riskRevision(files("a.go"), []store.SemanticDeclaration{oldT, caller}, []store.SemanticEdge{riskCall(caller, oldT, 3, 2, "resolved")})
	head := riskRevision(files("b.go"), []store.SemanticDeclaration{newT, caller}, []store.SemanticEdge{riskCall(caller, newT, 3, 2, "resolved")})
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Diff.FileRenames.Exact) != 1 {
		t.Fatalf("rename not annotated: %+v", got.Diff.FileRenames)
	}
	if len(got.Diff.Symbols.Removed) != 1 || len(got.Diff.Symbols.Added) != 1 {
		t.Fatalf("rename rewrote declaration identities: %+v", got.Diff.Symbols)
	}
	if len(got.Diff.Edges.Retargeted) != 1 {
		t.Fatalf("retarget hidden by rename: %+v", got.Diff.Edges)
	}
	if deps := got.Diff.Risk.RemovedDeclarationDependents; len(deps) != 1 || len(deps[0].Edges) != 1 {
		t.Fatalf("rename suppressed removed-declaration risk: %+v", got.Diff.Risk)
	}
	again, _ := Compare(base, head, 0, 100)
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatal("non-deterministic diff")
	}
}

func hasFileChange(cs []Change, path string) bool {
	for _, c := range cs {
		if c.Identity == path {
			return true
		}
	}
	return false
}

func TestCompareCommitsRenameLifecycle(t *testing.T) {
	r := gittest.Init(t)
	body := "package main\nfunc A() {}\n"
	r.Write("main.go", "package main\nfunc main() { A() }\n")
	r.Write("a.go", body)
	base := r.Commit("", "base")
	r.Git("mv", "a.go", "pkg_a.go")
	moved := r.Commit("", "rename")
	r.Git("mv", "pkg_a.go", "a.go")
	back := r.Commit("", "rename back")
	r.Git("rm", "-q", "a.go")
	deleted := r.Commit("", "delete")
	r.Write("untracked.go", "package main\n")
	r.Write("main.go", "package main\nfunc Dirty() {}\n")
	status, currentHead := r.Git("status", "--porcelain", "--untracked-files=all"), r.Git("rev-parse", "HEAD")
	reg := parser.NewRegistry(goparser.New())
	diff := func(from, to string) Result {
		t.Helper()
		got, err := CompareCommits(t.Context(), r.Dir, from, to, reg, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	forward := diff(base, moved)
	want := []FileRename{{From: "a.go", To: "pkg_a.go", ContentHash: forward.Diff.FileRenames.Exact[0].ContentHash, Language: "go"}}
	if !reflect.DeepEqual(forward.Diff.FileRenames.Exact, want) || forward.Diff.FileRenames.Exact[0].ContentHash == "" {
		t.Fatalf("forward renames = %+v", forward.Diff.FileRenames)
	}
	if len(forward.Diff.Symbols.Removed) != 1 || len(forward.Diff.Symbols.Added) != 1 {
		t.Fatalf("rename changed declaration identities: %+v", forward.Diff.Symbols)
	}
	if again := diff(base, moved); !sameJSON(again, forward) {
		t.Fatal("repeated rename diff differs")
	}
	if reverse := diff(moved, back).Diff.FileRenames.Exact; len(reverse) != 1 || reverse[0].From != "pkg_a.go" || reverse[0].To != "a.go" {
		t.Fatalf("reverse rename = %+v", reverse)
	}
	if noop := diff(base, back); noop.Diff.Total != 0 || len(noop.Diff.FileRenames.Exact) != 0 {
		t.Fatalf("rename round trip is not a no-op: %+v", noop.Diff)
	}
	if del := diff(back, deleted); len(del.Diff.FileRenames.Exact) != 0 || len(del.Diff.Files.Removed) != 1 {
		t.Fatalf("deletion reported as rename: %+v", del.Diff)
	}
	if _, err := CompareCommits(t.Context(), r.Dir, base, "missing", reg, 0, 200); err == nil {
		t.Fatal("accepted missing revision")
	}
	if got := r.Git("status", "--porcelain", "--untracked-files=all"); got != status {
		t.Fatalf("worktree changed: %q => %q", status, got)
	}
	if got := r.Git("rev-parse", "HEAD"); got != currentHead {
		t.Fatalf("HEAD changed: %s => %s", currentHead, got)
	}
}
