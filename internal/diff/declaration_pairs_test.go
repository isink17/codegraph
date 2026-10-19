package diff

import (
	"os"
	"path"
	"path/filepath"
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

// withTree adds files, such as go.mod, to a revision's tree.
func withTree(r Revision, files ...SourceFile) Revision {
	r.TreeFiles = append(r.TreeFiles, files...)
	return r
}

func goFile(path string) store.SemanticFile {
	return store.SemanticFile{Path: path, Language: "go", ContentHash: "h1", ParseState: store.ParseStateIndexed, ParserProfile: "go", ParserCallEdges: true, ParserSemanticEpoch: 1}
}

func goFunc(name string) store.SemanticDeclaration {
	return store.SemanticDeclaration{Language: "go", Kind: "function", Name: name, QualifiedName: "p." + name, StableKey: "func:p::" + name, StartLine: 3, EndLine: 3}
}

// goReasons drops the go.mod renames' language_not_proven rows.
func goReasons(d DeclarationPairs) []string {
	out := []string{}
	for _, r := range reasons(d) {
		if r != refuseLanguageNotProven {
			out = append(out, r)
		}
	}
	return out
}

func reasons(d DeclarationPairs) []string {
	out := []string{}
	for _, r := range d.Refused {
		out = append(out, r.Reason)
	}
	return out
}

func TestDeclarationPairsSameDirectory(t *testing.T) {
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
		{name: "across directories without module evidence", base: pairRevision(goFile("x/a.go"), fn), head: pairRevision(goFile("y/a.go"), fn), refused: []string{refuseModuleEvidenceMissing}},
		{name: "case-only directory without module evidence", base: pairRevision(goFile("P/a.go"), fn), head: pairRevision(goFile("p/a.go"), fn), refused: []string{refuseModuleEvidenceMissing}},
		{name: "into a test file", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(goFile("p/a_test.go"), fn), refused: []string{refuseBuildRoleChanged}},
		{name: "into a GOOS file", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(goFile("p/a_linux.go"), fn), refused: []string{refuseBuildRoleChanged}},
		{name: "into an ignored file", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(goFile("p/_a.go"), fn), refused: []string{refuseBuildRoleChanged}},
		{name: "suffix order", base: pairRevision(goFile("p/a_linux_amd64.go"), fn), head: pairRevision(goFile("p/b_linux_amd64.go"), fn), pairs: 1},
		{name: "not Go", base: func() Revision {
			f := goFile("p/a.lua")
			f.Language = "lua"
			return pairRevision(f, store.SemanticDeclaration{Language: "lua", Kind: "function", Name: "f", QualifiedName: "M.f", StableKey: "func:lua:global:M.f:1:1"})
		}(), head: func() Revision {
			f := goFile("p/b.lua")
			f.Language = "lua"
			return pairRevision(f, store.SemanticDeclaration{Language: "lua", Kind: "function", Name: "f", QualifiedName: "M.f", StableKey: "func:lua:global:M.f:1:1"})
		}(), refused: []string{refuseLanguageNotProven}},
		{name: "same GOOS", base: pairRevision(goFile("p/a_linux.go"), fn), head: pairRevision(goFile("p/b_linux.go"), fn), pairs: 1},
		{name: "duplicate declarations", base: pairRevision(goFile("p/a.go"), goFunc("init"), goFunc("init"), fn), head: pairRevision(goFile("p/b.go"), goFunc("init"), goFunc("init"), fn), pairs: 1, refused: []string{refuseDeclarationNotUnique, refuseDeclarationNotUnique}},
		{name: "path-derived stable key", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(goFile("p/b.go"), store.SemanticDeclaration{Language: "go", Kind: "function", Name: "A", QualifiedName: "p.A", StableKey: "func:b::A", StartLine: 3, EndLine: 3}), refused: []string{refuseDeclarationUnmatched}},
		{name: "parser profile differs", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(func() store.SemanticFile { f := goFile("p/b.go"); f.ParserProfile = "go2"; return f }(), fn), refused: []string{refuseParserSemanticsDiffer}},
		{name: "renamed file not parsed", base: pairRevision(goFile("p/a.go"), fn), head: pairRevision(func() store.SemanticFile { f := goFile("p/b.go"); f.ParseState = store.ParseStateFailed; return f }()), refused: []string{refuseFileNotParsed}},
		{name: "go.mod added in the directory", base: pairRevision(goFile("p/a.go"), fn), head: withTree(pairRevision(goFile("p/b.go"), fn), SourceFile{Path: "p/go.mod", ContentHash: "m"}), refused: []string{refuseModuleRootChanged}},
		{name: "root go.mod changed", base: withTree(pairRevision(goFile("p/a.go"), fn), SourceFile{Path: "go.mod", ContentHash: "m1"}), head: withTree(pairRevision(goFile("p/b.go"), fn), SourceFile{Path: "go.mod", ContentHash: "m2"}), refused: []string{refuseModuleRootChanged}},
		{name: "sibling go.mod changed", base: withTree(pairRevision(goFile("p/a.go"), fn), SourceFile{Path: "go.mod", ContentHash: "m"}, SourceFile{Path: "q/go.mod", ContentHash: "q1"}), head: withTree(pairRevision(goFile("p/b.go"), fn), SourceFile{Path: "go.mod", ContentHash: "m"}, SourceFile{Path: "q/go.mod", ContentHash: "q2"}), pairs: 1},
		{name: "go.work added", base: pairRevision(goFile("p/a.go"), fn), head: withTree(pairRevision(goFile("p/b.go"), fn), SourceFile{Path: "go.work", ContentHash: "w"}), refused: []string{refuseModuleRootChanged}},
		{name: "tree unknown", base: pairRevision(goFile("p/a.go"), fn), head: func() Revision { r := pairRevision(goFile("p/b.go"), fn); r.TreeFiles = nil; return r }(), refused: []string{refuseModuleEvidenceMissing}},
		{name: "external test package within the test role", base: pairRevision(goFile("p/a_test.go"), goFunc("TestA")), head: pairRevision(goFile("p/b_test.go"), goFunc("TestA")), pairs: 1},
		{name: "build-constrained content", base: pairRevision(func() store.SemanticFile { f := goFile("p/a.go"); f.ContentHash = "linux"; return f }(), fn), head: pairRevision(func() store.SemanticFile { f := goFile("p/b.go"); f.ContentHash = "linux"; return f }(), fn), pairs: 1},
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
	r.Git("mv", "a_test.go", "a.go")
	plain := r.Commit("", "out of the test file")
	r.Git("mv", "a.go", "b.go")
	r.Write("go.mod", "module example.com/m\n")
	nested := r.Commit("", "rename with a new module")
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
	if mod := diff(plain, nested); len(mod.Pairs) != 0 || !reflect.DeepEqual(reasons(mod), []string{refuseModuleRootChanged}) {
		t.Fatalf("rename with a new go.mod = %+v", mod)
	}
	if got := r.Git("status", "--porcelain", "--untracked-files=all"); got != status {
		t.Fatalf("worktree changed: %q => %q", status, got)
	}
	if got := r.Git("rev-parse", "HEAD"); got != currentHead {
		t.Fatalf("HEAD changed: %s => %s", currentHead, got)
	}
}

// inModule gives f the package identity an index records for a file of the
// module rooted at root.
func inModule(f store.SemanticFile, root, modulePath, importPath string) store.SemanticFile {
	f.Go = &store.SemanticGoFile{PackageClause: "p", ModuleState: store.GoModuleComplete, ModuleRoot: root, ModulePath: modulePath, ImportPath: importPath}
	return f
}

// withModules records modules and puts an identical go.mod for each in the tree.
func withModules(r Revision, modules ...store.SemanticGoModule) Revision {
	r.GraphData.GoModules = &store.SemanticGoModules{Modules: append([]store.SemanticGoModule{}, modules...), Workspaces: []string{}}
	for _, m := range modules {
		r.TreeFiles = append(r.TreeFiles, SourceFile{Path: path.Join(m.Root, "go.mod"), ContentHash: "gomod"})
	}
	return r
}

func TestDeclarationPairsAcrossDirectories(t *testing.T) {
	fn := goFunc("A")
	root := store.SemanticGoModule{Root: ".", Path: "example.com/m"}
	movedRoot := func(r string) store.SemanticGoModule { return store.SemanticGoModule{Root: r, Path: "example.com/m"} }
	moveBase := func() Revision {
		return withModules(pairRevision(inModule(goFile("x/p/a.go"), "x", "example.com/m", "example.com/m/p"), fn), movedRoot("x"))
	}
	moveHead := func() Revision {
		return withModules(pairRevision(inModule(goFile("y/p/a.go"), "y", "example.com/m", "example.com/m/p"), fn), movedRoot("y"))
	}
	edit := func(r Revision, f func(*Revision)) Revision { f(&r); return r }
	for _, tc := range []struct {
		name    string
		base    Revision
		head    Revision
		pairs   int
		refused []string
	}{
		{name: "module root moved with its package", base: moveBase(), head: moveHead(), pairs: 1},
		{name: "same names in distinct packages", base: withModules(pairRevision(inModule(goFile("x/a.go"), ".", "example.com/m", "example.com/m/x"), fn), root), head: withModules(pairRevision(inModule(goFile("y/a.go"), ".", "example.com/m", "example.com/m/y"), fn), root), refused: []string{refuseModuleIdentityChanged}},
		{name: "module renamed while moved", base: moveBase(), head: withModules(pairRevision(inModule(goFile("y/p/a.go"), "y", "example.com/n", "example.com/n/p"), fn), store.SemanticGoModule{Root: "y", Path: "example.com/n"}), refused: []string{refuseModuleIdentityChanged}},
		{name: "module split keeps import path", base: withModules(pairRevision(inModule(goFile("a/b/x.go"), ".", "example.com/m", "example.com/m/a/b"), fn), root), head: withModules(pairRevision(inModule(goFile("c/b/x.go"), "c", "example.com/m/a", "example.com/m/a/b"), fn), root, store.SemanticGoModule{Root: "c", Path: "example.com/m/a"}), refused: []string{refuseModuleIdentityChanged}},
		{name: "go.mod edited while moved", base: moveBase(), head: edit(moveHead(), func(r *Revision) {
			for i := range r.TreeFiles {
				if r.TreeFiles[i].Path == "y/go.mod" {
					r.TreeFiles[i].ContentHash = "edited"
				}
			}
		}), refused: []string{refuseModuleRootChanged}},
		{name: "go.mod missing from the tree", base: moveBase(), head: edit(moveHead(), func(r *Revision) { r.TreeFiles = r.TreeFiles[:1] }), refused: []string{refuseModuleEvidenceMissing}},
		{name: "module path declared twice", base: moveBase(), head: edit(moveHead(), func(r *Revision) {
			r.GraphData.GoModules.Modules = append(r.GraphData.GoModules.Modules, movedRoot("z"))
		}), refused: []string{refuseModulePathNotUnique}},
		{name: "module evidence not recorded", base: moveBase(), head: edit(moveHead(), func(r *Revision) { r.GraphData.GoModules = nil }), refused: []string{refuseModuleEvidenceMissing}},
		{name: "file indexed without package identity", base: moveBase(), head: edit(moveHead(), func(r *Revision) { r.GraphData.Files[0].Go = nil }), refused: []string{refuseModuleEvidenceMissing}},
		{name: "outside every module", base: moveBase(), head: edit(moveHead(), func(r *Revision) {
			r.GraphData.Files[0].Go = &store.SemanticGoFile{PackageClause: "p", ModuleState: store.GoModuleNone}
		}), refused: []string{refuseModuleEvidenceMissing}},
		{name: "go.work present", base: moveBase(), head: edit(moveHead(), func(r *Revision) { r.GraphData.GoModules.Workspaces = []string{"."} }), refused: []string{refuseWorkspaceUncertain}},
		{name: "replace by a directory", base: edit(moveBase(), func(r *Revision) { r.GraphData.GoModules.Modules[0].LocalReplace = true }), head: moveHead(), refused: []string{refuseModuleReplaceInRepo}},
		{name: "package clause differs", base: moveBase(), head: edit(moveHead(), func(r *Revision) { r.GraphData.Files[0].Go.PackageClause = "q" }), refused: []string{refusePackageChanged}},
		{name: "external test role differs", base: moveBase(), head: edit(moveHead(), func(r *Revision) { r.GraphData.Files[0].Go.ExternalTest = true }), refused: []string{refusePackageChanged}},
		{name: "build constraint differs", base: moveBase(), head: edit(moveHead(), func(r *Revision) { r.GraphData.Files[0].Go.BuildConstraint = "linux" }), refused: []string{refuseBuildRoleChanged}},
		{name: "file name build role differs", base: moveBase(), head: withModules(pairRevision(inModule(goFile("y/p/a_linux.go"), "y", "example.com/m", "example.com/m/p"), fn), movedRoot("y")), refused: []string{refuseBuildRoleChanged}},
		{name: "into testdata", base: moveBase(), head: withModules(pairRevision(inModule(goFile("y/testdata/p/a.go"), "y", "example.com/m", "example.com/m/testdata/p"), fn), movedRoot("y")), refused: []string{refusePackageIgnored}},
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
			if !reflect.DeepEqual(goReasons(d), tc.refused) {
				t.Fatalf("refused = %v, want %v", goReasons(d), tc.refused)
			}
			if len(got.Diff.Symbols.Removed) != 1 || len(got.Diff.Symbols.Added) != 1 {
				t.Fatalf("declaration changes hidden: %+v", got.Diff.Symbols)
			}
		})
	}
}

func TestDeclarationPairsRefuseChangedContentMoves(t *testing.T) {
	fn := goFunc("A")
	mod := func(r string) store.SemanticGoModule { return store.SemanticGoModule{Root: r, Path: "example.com/m"} }
	side := func(root string, hash string, files ...string) Revision {
		var r Revision
		r.GraphData.GoModules = &store.SemanticGoModules{Modules: []store.SemanticGoModule{mod(root)}, Workspaces: []string{}}
		r.TreeFiles = []SourceFile{}
		for _, p := range files {
			f := inModule(goFile(root+"/"+p), root, "example.com/m", "example.com/m/"+path.Dir(p))
			f.ContentHash = hash
			r.GraphData.Files = append(r.GraphData.Files, f)
			r.TreeFiles = append(r.TreeFiles, SourceFile{Path: f.Path, ContentHash: hash, Language: "go"})
			d := fn
			d.Path = f.Path
			r.GraphData.Declarations = append(r.GraphData.Declarations, d)
		}
		return r
	}
	for _, tc := range []struct {
		name       string
		base, head Revision
		want       []RefusedPairing
	}{
		{name: "content changed", base: side("x", "h1", "p/a.go"), head: side("y", "h2", "p/a.go"),
			want: []RefusedPairing{{FromPath: "x/p/a.go", ToPath: "y/p/a.go", Reason: refuseContentChanged}}},
		{name: "identical files moved together", base: side("x", "h1", "p/a.go", "p/b.go"), head: side("y", "h1", "p/a.go", "p/b.go"),
			want: []RefusedPairing{{FromPath: "x/p/a.go", ToPath: "y/p/a.go", Reason: refuseMoveAmbiguous}, {FromPath: "x/p/b.go", ToPath: "y/p/b.go", Reason: refuseMoveAmbiguous}}},
		{name: "another package", base: side("x", "h1", "p/a.go"), head: side("y", "h2", "q/a.go"), want: []RefusedPairing{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Compare(tc.base, tc.head, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			d := got.Diff.DeclarationPairs
			if len(d.Pairs) != 0 || !reflect.DeepEqual(d.Refused, tc.want) {
				t.Fatalf("declaration pairs = %+v, want refused %+v", d, tc.want)
			}
		})
	}
}

// moveScenario commits files, applies change, commits again and diffs the two
// commits through real indexing.
func moveScenario(t *testing.T, files map[string]string, change func(r *gittest.Repo)) Result {
	t.Helper()
	r := gittest.Init(t)
	for p, c := range files {
		r.Write(p, c)
	}
	base := r.Commit("", "base")
	change(r)
	head := r.Commit("", "change")
	got, err := CompareCommits(t.Context(), r.Dir, base, head, parser.NewRegistry(goparser.New()), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	again, err := CompareCommits(t.Context(), r.Dir, base, head, parser.NewRegistry(goparser.New()), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(got, again) {
		t.Fatal("repeated diff differs")
	}
	return got
}

func gitMove(t *testing.T, r *gittest.Repo, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(r.Dir, filepath.Dir(to)), 0o755); err != nil {
		t.Fatal(err)
	}
	r.Git("mv", from, to)
}

func pairedPaths(d DeclarationPairs) []string {
	out := []string{}
	for _, p := range d.Pairs {
		out = append(out, p.FromPath+">"+p.ToPath+":"+p.Before.QualifiedName)
	}
	return out
}

func goRefusals(d DeclarationPairs) []string {
	out := []string{}
	for _, r := range d.Refused {
		if r.Reason != refuseLanguageNotProven {
			out = append(out, r.FromPath+">"+r.ToPath+":"+r.Reason)
		}
	}
	return out
}

func ruleCount(r Risk, rule string) int {
	for _, c := range r.Components {
		if c.Rule == rule {
			return c.Count
		}
	}
	return -1
}

func TestCompareCommitsCrossDirectoryLifecycle(t *testing.T) {
	app := map[string]string{
		"go.mod":      "module example.com/m\n\ngo 1.22\n",
		"p/a.go":      "package p\n\nfunc A() {}\n",
		"cmd/main.go": "package main\n\nimport \"example.com/m/p\"\n\nfunc main() { p.A() }\n",
	}
	// A dependent in one module, its callee in another.
	split := map[string]string{
		"go.mod":      "module example.com/app\n\ngo 1.22\n",
		"main.go":     "package main\n\nimport \"example.com/lib/p\"\n\nfunc main() { p.A() }\n",
		"libs/go.mod": "module example.com/lib\n\ngo 1.22\n",
		"libs/p/a.go": "package p\n\nfunc A() {}\n",
	}
	with := func(base map[string]string, extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	moveModule := func(r *gittest.Repo) {
		gitMove(t, r, "go.mod", "sub/go.mod")
		gitMove(t, r, "p", "sub/p")
		gitMove(t, r, "cmd", "sub/cmd")
	}
	moveLib := func(r *gittest.Repo) { gitMove(t, r, "libs", "lib") }

	t.Run("module root moved with dependent and callee", func(t *testing.T) {
		got := moveScenario(t, app, moveModule)
		d := got.Diff.DeclarationPairs
		if want := []string{"cmd/main.go>sub/cmd/main.go:main.main", "p/a.go>sub/p/a.go:p.A"}; !reflect.DeepEqual(pairedPaths(d), want) || len(goRefusals(d)) != 0 {
			t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
		}
		if got.Diff.Risk.Score == nil || *got.Diff.Risk.Score != 0 {
			t.Fatalf("risk = %s", canonical(got.Diff.Risk))
		}
		for _, u := range got.Diff.Risk.Uncertainty {
			if u.Reason != "changed_file_without_semantic_coverage" {
				t.Fatalf("moved declarations left uncertainty: %s", canonical(got.Diff.Risk.Uncertainty))
			}
		}
	})
	t.Run("callee module moved under a fixed dependent", func(t *testing.T) {
		got := moveScenario(t, split, moveLib)
		d := got.Diff.DeclarationPairs
		if want := []string{"libs/p/a.go>lib/p/a.go:p.A"}; !reflect.DeepEqual(pairedPaths(d), want) {
			t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
		}
		if n := ruleCount(got.Diff.Risk, RuleDependentRetargeted); n != 0 {
			t.Fatalf("paired move cited: %s", canonical(got.Diff.Risk))
		}
		// The same move without recorded module evidence is refused, and its
		// caller's new binding is cited as before.
		head := got.To
		head.GraphData.GoModules = nil
		unproven, err := Compare(got.From, head, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"libs/p/a.go>lib/p/a.go:" + refuseModuleEvidenceMissing}; !reflect.DeepEqual(goRefusals(unproven.Diff.DeclarationPairs), want) {
			t.Fatalf("refused = %v", goRefusals(unproven.Diff.DeclarationPairs))
		}
		if n := ruleCount(unproven.Diff.Risk, RuleDependentRetargeted); n != 1 {
			t.Fatalf("unproven move not cited: %s", canonical(unproven.Diff.Risk))
		}
	})
	t.Run("move to another package of one module", func(t *testing.T) {
		got := moveScenario(t, app, func(r *gittest.Repo) { gitMove(t, r, "p/a.go", "q/a.go") })
		if want := []string{"p/a.go>q/a.go:" + refuseModuleIdentityChanged}; !reflect.DeepEqual(goRefusals(got.Diff.DeclarationPairs), want) || len(got.Diff.DeclarationPairs.Pairs) != 0 {
			t.Fatalf("declaration pairs = %s", canonical(got.Diff.DeclarationPairs))
		}
	})
	t.Run("go.work present", func(t *testing.T) {
		got := moveScenario(t, with(split, map[string]string{"go.work": "go 1.22\n\nuse (\n\t.\n\t./libs\n)\n"}), func(r *gittest.Repo) {
			moveLib(r)
			r.Write("go.work", "go 1.22\n\nuse (\n\t.\n\t./lib\n)\n")
		})
		if want := []string{"libs/p/a.go>lib/p/a.go:" + refuseWorkspaceUncertain}; !reflect.DeepEqual(goRefusals(got.Diff.DeclarationPairs), want) {
			t.Fatalf("refused = %v", goRefusals(got.Diff.DeclarationPairs))
		}
		// A refused move keeps its caller cited.
		if n := ruleCount(got.Diff.Risk, RuleDependentRetargeted); n != 1 {
			t.Fatalf("risk = %s", canonical(got.Diff.Risk))
		}
	})
	t.Run("replace by a directory", func(t *testing.T) {
		got := moveScenario(t, with(split, map[string]string{"go.mod": "module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ./libs\n"}), func(r *gittest.Repo) {
			moveLib(r)
			r.Write("go.mod", "module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ./lib\n")
		})
		if want := []string{"libs/p/a.go>lib/p/a.go:" + refuseModuleReplaceInRepo}; !reflect.DeepEqual(goRefusals(got.Diff.DeclarationPairs), want) {
			t.Fatalf("refused = %v", goRefusals(got.Diff.DeclarationPairs))
		}
		// A refused move keeps its caller cited.
		if n := ruleCount(got.Diff.Risk, RuleDependentRetargeted); n != 1 {
			t.Fatalf("risk = %s", canonical(got.Diff.Risk))
		}
	})
	t.Run("module path declared by two roots", func(t *testing.T) {
		got := moveScenario(t, split, func(r *gittest.Repo) {
			moveLib(r)
			r.Write("other/go.mod", "module example.com/lib\n")
		})
		if want := []string{"libs/p/a.go>lib/p/a.go:" + refuseModulePathNotUnique}; !reflect.DeepEqual(goRefusals(got.Diff.DeclarationPairs), want) {
			t.Fatalf("refused = %v", goRefusals(got.Diff.DeclarationPairs))
		}
		// A refused move keeps its caller cited.
		if n := ruleCount(got.Diff.Risk, RuleRemovedDeclarationDependent); n != 1 {
			t.Fatalf("risk = %s", canonical(got.Diff.Risk))
		}
	})
	t.Run("nested module claims the moved import path", func(t *testing.T) {
		got := moveScenario(t, map[string]string{
			"go.mod":   "module example.com/m\n",
			"a/b/x.go": "package b\n\nfunc B() {}\n",
			"main.go":  "package main\n\nimport \"example.com/m/a/b\"\n\nfunc main() { b.B() }\n",
		}, func(r *gittest.Repo) {
			r.Write("c/go.mod", "module example.com/m/a\n")
			gitMove(t, r, "a/b/x.go", "c/b/x.go")
		})
		if want := []string{"a/b/x.go>c/b/x.go:" + refuseModuleIdentityChanged}; !reflect.DeepEqual(goRefusals(got.Diff.DeclarationPairs), want) || len(got.Diff.DeclarationPairs.Pairs) != 0 {
			t.Fatalf("declaration pairs = %s", canonical(got.Diff.DeclarationPairs))
		}
	})
	t.Run("module renamed so the import path coincides", func(t *testing.T) {
		got := moveScenario(t, map[string]string{
			"go.mod":   "module example.com/m\n",
			"x/p/a.go": "package p\n\nfunc A() {}\n",
		}, func(r *gittest.Repo) {
			r.Write("go.mod", "module example.com/m/x\n")
			gitMove(t, r, "x/p/a.go", "p/a.go")
		})
		if want := []string{"x/p/a.go>p/a.go:" + refuseModuleIdentityChanged}; !reflect.DeepEqual(goRefusals(got.Diff.DeclarationPairs), want) || len(got.Diff.DeclarationPairs.Pairs) != 0 {
			t.Fatalf("declaration pairs = %s", canonical(got.Diff.DeclarationPairs))
		}
	})
	t.Run("go.mod edited during the move", func(t *testing.T) {
		got := moveScenario(t, app, func(r *gittest.Repo) {
			moveModule(r)
			r.Write("sub/go.mod", "module example.com/m\n\ngo 1.23\n")
		})
		d := got.Diff.DeclarationPairs
		if want := []string{"cmd/main.go>sub/cmd/main.go:" + refuseModuleRootChanged, "p/a.go>sub/p/a.go:" + refuseModuleRootChanged}; !reflect.DeepEqual(goRefusals(d), want) || len(d.Pairs) != 0 {
			t.Fatalf("declaration pairs = %s", canonical(d))
		}
	})
	t.Run("content changed during the move", func(t *testing.T) {
		got := moveScenario(t, app, func(r *gittest.Repo) {
			moveModule(r)
			r.Write("sub/p/a.go", "package p\n\n// A does nothing.\nfunc A() {}\n")
		})
		d := got.Diff.DeclarationPairs
		if want := []string{"cmd/main.go>sub/cmd/main.go:main.main"}; !reflect.DeepEqual(pairedPaths(d), want) {
			t.Fatalf("pairs = %v", pairedPaths(d))
		}
		if want := []string{"p/a.go>sub/p/a.go:" + refuseContentChanged}; !reflect.DeepEqual(goRefusals(d), want) {
			t.Fatalf("refused = %v", goRefusals(d))
		}
	})
	t.Run("identical files moved together", func(t *testing.T) {
		got := moveScenario(t, with(app, map[string]string{"p/b.go": "package p\n\nfunc A() {}\n"}), moveModule)
		d := got.Diff.DeclarationPairs
		if want := []string{"p/a.go>sub/p/a.go:" + refuseMoveAmbiguous, "p/b.go>sub/p/b.go:" + refuseMoveAmbiguous}; !reflect.DeepEqual(goRefusals(d), want) {
			t.Fatalf("refused = %v", goRefusals(d))
		}
	})
}
