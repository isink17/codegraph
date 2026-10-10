package diff

import (
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// pairRevision is one side of a single-file rename: the file's projection and
// its declarations, as an index of that revision would report them.
func pairRevision(file store.SemanticFile, decls ...store.SemanticDeclaration) Revision {
	for i := range decls {
		decls[i].Path = file.Path
	}
	return Revision{
		TreeFiles: []SourceFile{{Path: file.Path, ContentHash: file.ContentHash, Language: file.Language}},
		GraphData: store.SemanticGraph{Files: []store.SemanticFile{file}, Declarations: decls},
	}
}

func goFile(path string) store.SemanticFile {
	return store.SemanticFile{Path: path, Language: "go", ContentHash: "h1", ParseState: store.ParseStateIndexed, ParserProfile: "go", ParserCallEdges: true, ParserSemanticEpoch: 1}
}

func goFunc(name string) store.SemanticDeclaration {
	return store.SemanticDeclaration{Language: "go", Kind: "function", Name: name, QualifiedName: "p." + name, StableKey: "func:p::" + name, StartLine: 3, EndLine: 3}
}

func reasons(d DeclarationPairs) []string {
	out := []string{}
	for _, r := range d.Refused {
		out = append(out, r.Reason)
	}
	return out
}

func TestDeclarationPairsProveContinuityOnlyInsideOneDirectory(t *testing.T) {
	fn := goFunc("A")
	for _, tc := range []struct {
		name    string
		base    Revision
		head    Revision
		pairs   int
		refused []string
	}{
		{name: "same directory", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(goFile("p/b.go"), fn), pairs: 1},
		{name: "case only", base: pairRevision(goFile("p/A.go"), fn), head: pairRevision(goFile("p/a.go"), fn), pairs: 1},
		{name: "across directories with the same package clause", base: pairRevision(goFile("x/a.go"), fn), head: pairRevision(goFile("y/a.go"), fn), refused: []string{refuseModuleIdentityChanged}},
		{name: "case-only directory", base: pairRevision(goFile("P/a.go"), fn), head: pairRevision(goFile("p/a.go"), fn), refused: []string{refuseModuleIdentityChanged}},
		{name: "into a test file", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(goFile("p/a_test.go"), fn), refused: []string{refuseBuildRoleChanged}},
		{name: "into a GOOS file", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(goFile("p/a_linux.go"), fn), refused: []string{refuseBuildRoleChanged}},
		{name: "same GOOS", base: pairRevision(goFile("p/a_linux.go"), fn), head: pairRevision(goFile("p/b_linux.go"), fn), pairs: 1},
		{name: "duplicate declarations", base: pairRevision(goFile("p/a.go"), goFunc("init"), goFunc("init"), fn), head: pairRevision(goFile("p/b.go"), goFunc("init"), goFunc("init"), fn), pairs: 1, refused: []string{refuseDeclarationNotUnique, refuseDeclarationNotUnique}},
		{name: "path-derived stable key", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(goFile("p/b.go"), store.SemanticDeclaration{Language: "go", Kind: "function", Name: "A", QualifiedName: "p.A", StableKey: "func:b::A", StartLine: 3, EndLine: 3}), refused: []string{refuseDeclarationUnmatched}},
		{name: "parser profile differs", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(func() store.SemanticFile { f := goFile("p/b.go"); f.ParserProfile = "go2"; return f }(), fn), refused: []string{refuseParserSemanticsDiffer}},
		{name: "renamed file not parsed", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(func() store.SemanticFile { f := goFile("p/b.go"); f.ParseState = store.ParseStateFailed; return f }()), refused: []string{refuseFileNotParsed}},
		{name: "graph capability differs", base: pairRevision(goFile("p/a.go"), fn), head: func() Revision {
			r := pairRevision(goFile("p/b.go"), fn)
			r.GraphData.State.Pending = "resolver"
			return r
		}(), refused: []string{refuseSemanticsIncompatible}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Compare(tc.base, tc.head, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			d := got.Diff.DeclarationPairs
			if len(d.Pairs) != tc.pairs {
				t.Fatalf("pairs = %+v, want %d", d.Pairs, tc.pairs)
			}
			if tc.refused == nil {
				tc.refused = []string{}
			}
			if !reflect.DeepEqual(reasons(d), tc.refused) {
				t.Fatalf("refused = %v, want %v", reasons(d), tc.refused)
			}
			for _, p := range d.Pairs {
				if p.Before.Path != p.FromPath || p.After.Path != p.ToPath {
					t.Fatalf("pair lost its identities: %+v", p)
				}
			}
			// The annotation never hides the declaration changes it describes.
			if got.Comparison.Status != "incompatible" && (len(got.Diff.Symbols.Removed) == 0 || (len(tc.head.GraphData.Declarations) > 0 && len(got.Diff.Symbols.Added) == 0)) {
				t.Fatalf("declaration changes hidden: %+v", got.Diff.Symbols)
			}
		})
	}
}

func TestDeclarationPairsNeedAnExactRename(t *testing.T) {
	fn := goFunc("A")
	changed := pairRevision(goFile("p/b.go"), fn)
	changed.TreeFiles[0].ContentHash = "h2"
	copied := pairRevision(goFile("p/b.go"), fn)
	copied.TreeFiles = append(copied.TreeFiles, SourceFile{Path: "p/a.go", ContentHash: "h1", Language: "go"})
	for name, head := range map[string]Revision{"content changed": changed, "copy": copied} {
		got, err := Compare(pairRevision(goFile("p/a.go"), fn), head, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if d := got.Diff.DeclarationPairs; len(d.Pairs) != 0 || len(d.Refused) != 0 {
			t.Fatalf("%s: %+v", name, d)
		}
	}
	policy := pairRevision(goFile("p/b.go"), fn)
	policy.IndexPolicy = IndexPolicy{Exclude: []string{"vendor/**"}}
	got, err := Compare(pairRevision(goFile("p/a.go"), fn), policy, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if d := got.Diff.DeclarationPairs; len(d.Pairs) != 0 || d.Limitation == "" {
		t.Fatalf("changed index policy: %+v", d)
	}
}

func TestCompareCommitsDeclarationPairsLifecycle(t *testing.T) {
	r := gittest.Init(t)
	r.Write("main.go", "package main\nfunc main() { A() }\n")
	r.Write("a.go", "package main\nfunc A() {}\n")
	base := r.Commit("", "base")
	r.Git("mv", "a.go", "pkg_a.go")
	moved := r.Commit("", "rename")
	r.Git("mv", "pkg_a.go", "a.go")
	back := r.Commit("", "rename back")
	r.Git("mv", "a.go", "a_test.go")
	refused := r.Commit("", "into a test file")
	status, currentHead := r.Git("status", "--porcelain", "--untracked-files=all"), r.Git("rev-parse", "HEAD")
	reg := parser.NewRegistry(goparser.New())
	diff := func(from, to string) DeclarationPairs {
		t.Helper()
		got, err := CompareCommits(t.Context(), r.Dir, from, to, reg, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		return got.Diff.DeclarationPairs
	}
	forward := diff(base, moved)
	if len(forward.Pairs) != 1 || forward.Pairs[0].Before.QualifiedName != "main.A" || forward.Pairs[0].After.Path != "pkg_a.go" {
		t.Fatalf("forward = %+v", forward)
	}
	if again := diff(base, moved); !sameJSON(again, forward) {
		t.Fatal("repeated diff differs")
	}
	if reverse := diff(moved, back); len(reverse.Pairs) != 1 || reverse.Pairs[0].FromPath != "pkg_a.go" {
		t.Fatalf("reverse = %+v", reverse)
	}
	if noop := diff(base, back); len(noop.Pairs) != 0 || len(noop.Refused) != 0 {
		t.Fatalf("round trip = %+v", noop)
	}
	if role := diff(back, refused); len(role.Pairs) != 0 || !reflect.DeepEqual(reasons(role), []string{refuseBuildRoleChanged}) {
		t.Fatalf("test-file rename = %+v", role)
	}
	if got := r.Git("status", "--porcelain", "--untracked-files=all"); got != status {
		t.Fatalf("worktree changed: %q => %q", status, got)
	}
	if got := r.Git("rev-parse", "HEAD"); got != currentHead {
		t.Fatalf("HEAD changed: %s => %s", currentHead, got)
	}
}
