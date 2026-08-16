package store_test

import (
	"sort"
	"testing"
)

// Lifecycle coverage for P22.7 language-safe symbol seed lookup, driven by the
// real indexer rather than by hand-written rows.
//
// The two things a language gate can get wrong over time are both here: a
// deleted same-language target must leave the call unanswered instead of
// falling through to a foreign-language symbol of the same name, and a tree
// reached by incremental updates must answer exactly like a fresh full index of
// the same tree.

// languageLifecycleTree is one Python module calling a name that both Python
// and Go declare. `SharedName` is the only spelling under test, and it exists
// in both languages on purpose.
func languageLifecycleTree() map[string]string {
	return map[string]string{
		"gofoo/decl.go": "package gofoo\n\nfunc SharedName() int { return 0 }\n",
		"pyapp/lib.py":  "def SharedName():\n    return 0\n",
		"pyapp/app.py":  "def PyCaller():\n    return SharedName()\n",
	}
}

// newLanguageLifecycleFixture indexes a tree from scratch in its own store, so
// a fresh full index can be compared against an incrementally updated one.
func newLanguageLifecycleFixture(t *testing.T, files map[string]string) *reindexFixture {
	t.Helper()
	f := newPolyglotFixture(t)
	dirs := map[string]bool{}
	names := make([]string, 0, len(files))
	for rel := range files {
		names = append(names, rel)
	}
	sort.Strings(names)
	for _, rel := range names {
		if dir := dirOf(rel); dir != "" && !dirs[dir] {
			dirs[dir] = true
			f.mkdir(dir)
		}
		f.write(rel, files[rel])
	}
	f.index()
	return f
}

func dirOf(rel string) string {
	for i := len(rel) - 1; i >= 0; i-- {
		if rel[i] == '/' {
			return rel[:i]
		}
	}
	return ""
}

// languageProjection is the semantic answer these tests compare: what each
// surface says about the shared name, in both directions.
func languageProjection(t *testing.T, f *reindexFixture) []string {
	t.Helper()
	out := []string{
		"callees(app.PyCaller)=" + join(calleeNamesOf(t, f, "app.PyCaller")),
		"callers(gofoo.SharedName)=" + join(callerNamesOf(t, f, "gofoo.SharedName")),
		"ctx_callers(gofoo.SharedName)=" + join(contextCallerNamesOf(t, f, "gofoo.SharedName")),
	}
	return out
}

func join(names []string) string {
	sort.Strings(names)
	out := ""
	for i, name := range names {
		if i > 0 {
			out += ","
		}
		out += name
	}
	return out
}

// A deleted same-language target must never fall through to a same-named symbol
// in another language, and must resolve again when it comes back.
func TestLanguageSafeIdentityAcrossDeletionAndRecreation(t *testing.T) {
	files := languageLifecycleTree()
	f := newLanguageLifecycleFixture(t, files)

	if got := calleeNamesOf(t, f, "app.PyCaller"); !equalStringSlices(got, []string{"lib.SharedName"}) {
		t.Fatalf("initial callees of app.PyCaller = %v, want [lib.SharedName]", got)
	}

	// The Python declaration disappears. The call is now unanswerable, which is
	// the honest answer -- the Go declaration of the same name is not what the
	// Python source named.
	f.remove("pyapp/lib.py")
	f.update()
	if got := calleeNamesOf(t, f, "app.PyCaller"); len(got) != 0 {
		t.Fatalf("callees of app.PyCaller after deleting the python target = %v, want none", got)
	}
	if got := callerNamesOf(t, f, "gofoo.SharedName"); containsName(got, "app.PyCaller") {
		t.Fatalf("callers of gofoo.SharedName = %v: a deleted python target fell through to Go", got)
	}
	if got := contextCallerNamesOf(t, f, "gofoo.SharedName"); containsName(got, "app.PyCaller") {
		t.Fatalf("context callers of gofoo.SharedName = %v: a deleted python target fell through to Go", got)
	}

	// It comes back and is resolved again.
	f.write("pyapp/lib.py", files["pyapp/lib.py"])
	f.update()
	if got := calleeNamesOf(t, f, "app.PyCaller"); !equalStringSlices(got, []string{"lib.SharedName"}) {
		t.Fatalf("callees of app.PyCaller after restoring the python target = %v, want [lib.SharedName]", got)
	}
}

// A tree reached by incremental updates must answer exactly like a fresh full
// index of the same tree.
func TestLanguageSafeIdentityFullIndexEqualsUpdate(t *testing.T) {
	files := languageLifecycleTree()

	updated := newLanguageLifecycleFixture(t, map[string]string{
		"gofoo/decl.go": files["gofoo/decl.go"],
	})
	updated.mkdir("pyapp")
	updated.write("pyapp/lib.py", files["pyapp/lib.py"])
	updated.update()
	updated.write("pyapp/app.py", files["pyapp/app.py"])
	updated.update()
	// The foreign-language declaration appears and disappears in between, which
	// is where a language-blind cascade would leave a stolen answer behind.
	updated.write("gofoo/extra.go", "package gofoo\n\nfunc Extra() int { return SharedName() }\n")
	updated.update()
	updated.remove("gofoo/extra.go")
	updated.update()

	fresh := newLanguageLifecycleFixture(t, files)

	got := languageProjection(t, updated)
	want := languageProjection(t, fresh)
	if !equalStringSlices(got, want) {
		t.Fatalf("incremental projection\n%v\nwant fresh-index projection\n%v", got, want)
	}
	// Sanity: the projection actually carries the decision under test.
	if got[0] != "callees(app.PyCaller)=lib.SharedName" {
		t.Fatalf("projection missing the same-language callee: %v", got)
	}
}

// Public discovery stays language-agnostic end to end: a user typing the bare
// name still sees every language's declaration.
func TestLanguageGateKeepsPublicDiscovery(t *testing.T) {
	f := newLanguageLifecycleFixture(t, languageLifecycleTree())

	syms, err := f.store.FindSymbol(f.ctx, f.repoID, "SharedName", 50, 0)
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	names := qualifiedNamesOf(syms)
	if !containsName(names, "gofoo.SharedName") || !containsName(names, "lib.SharedName") {
		t.Fatalf("FindSymbol(SharedName) = %v, want both languages", names)
	}
}
