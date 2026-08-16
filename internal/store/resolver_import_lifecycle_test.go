package store_test

import "testing"

// P22.8 §18/§20: the import-path class over a file's whole life, driven by the
// real indexer rather than by hand-written rows.
//
// Every step's answer must be a function of the CURRENT tree only. In
// particular the project declares its own unique `store.Open`, which the
// retired bare-tail fallback used to hand to every one of these spellings.
func TestImportPathBindingFollowsCurrentEvidence(t *testing.T) {
	f := newReindexFixture(t)
	f.write("go.mod", "module example.com/project\n")
	f.mkdir("pkg")
	f.mkdir("cmd")
	f.mkdir("internal")
	f.mkdir("internal/store")
	f.write("pkg/open.go", "package pkg\n\nfunc Open() int { return 0 }\n")
	// The decoy: unique in the repository, and never what any of these calls name.
	f.write("internal/store/store.go", "package store\n\nfunc Open() int { return 1 }\n")
	ownModule := "package main\n\nimport \"example.com/project/pkg\"\n\nfunc main() { pkg.Open() }\n"
	external := "package main\n\nimport \"github.com/other/pkg\"\n\nfunc main() { pkg.Open() }\n"
	f.write("cmd/main.go", ownModule)
	f.index()

	binding := func() string {
		f.t.Helper()
		for _, e := range f.edges() {
			if e.FilePath == "cmd/main.go" && e.DstSymbolID != nil {
				return e.DstQualifiedName + "|" + e.ResolutionStrategy
			}
		}
		return "<unresolved>"
	}
	step := func(label, want string) {
		t.Helper()
		if got := binding(); got != want {
			t.Fatalf("%s: binding = %q, want %q", label, got, want)
		}
	}

	step("own-module import", "pkg.Open|module_import")

	// The import becomes an external module with the same package tail. The
	// old project target must go, and the spelling must not tail-bind
	// store.Open in its place.
	f.write("cmd/main.go", external)
	f.update()
	step("import moved outside the module", "<unresolved>")

	f.write("cmd/main.go", ownModule)
	f.update()
	step("import moved back", "pkg.Open|module_import")

	// The target itself disappears: unresolved, and again no fallthrough.
	f.remove("pkg/open.go")
	f.update()
	step("own-module target deleted", "<unresolved>")

	f.write("pkg/open.go", "package pkg\n\nfunc Open() int { return 0 }\n")
	f.update()
	step("own-module target recreated", "pkg.Open|module_import")
}
