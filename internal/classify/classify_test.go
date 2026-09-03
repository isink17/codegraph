package classify

import "testing"

// The controls in this file are organised by the claim each one defends, not by
// function. A classification is only worth having if it is hard to obtain, so
// roughly half of these assert that a plausible-looking case yields `unknown`.

func TestTarget_ResolvedEdgeIsProjectRegardlessOfName(t *testing.T) {
	// A bound destination is a project symbol even when its name is spelled
	// exactly like a builtin or a stdlib call. The graph outranks the name.
	cases := []Evidence{
		{Resolved: true, Language: "python", DstName: "sorted"},
		{Resolved: true, Language: "go", DstName: "fmt.Println", Imports: []string{"fmt"}},
		{Resolved: true, Language: "typescript", DstName: "express.Router", Imports: []string{"express"}},
		{Resolved: true, Language: "go", DstName: "len"},
	}
	for _, ev := range cases {
		if got := Target(ev); got != Project {
			t.Errorf("Target(%+v) = %q, want %q", ev, got, Project)
		}
	}
}

func TestTarget_Builtin(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
	}{
		{"python sorted", Evidence{Language: "python", DstName: "sorted"}},
		{"python len", Evidence{Language: "python", DstName: "len"}},
		{"python builtin exception", Evidence{Language: "python", DstName: "ValueError"}},
		{"go len", Evidence{Language: "go", DstName: "len"}},
		{"go make", Evidence{Language: "go", DstName: "make"}},
		{"go append", Evidence{Language: "go", DstName: "append"}},
		{"go conversion", Evidence{Language: "go", DstName: "int64"}},
		{"typescript JSON member", Evidence{Language: "typescript", DstName: "JSON.stringify"}},
		{"typescript Array member", Evidence{Language: "typescript", DstName: "Array.isArray"}},
		{"typescript Math member", Evidence{Language: "typescript", DstName: "Math.max"}},
		// Unrelated imports in the file are not evidence against a builtin: a
		// predeclared identifier needs no import.
		{"builtin alongside imports", Evidence{Language: "python", DstName: "sorted", Imports: []string{"os", "requests"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Target(tc.ev); got != Builtin {
				t.Fatalf("Target(%+v) = %q, want %q", tc.ev, got, Builtin)
			}
		})
	}
}

// TestTarget_BuiltinAbstainsWhenProjectDefinesTheName is the guard that keeps a
// P3 ambiguity from being relabelled as a certainty. If the project defines
// `sorted` in Python and the edge is still unresolved, the reason is a resolver
// decision, not "it is obviously the builtin".
func TestTarget_BuiltinAbstainsWhenProjectDefinesTheName(t *testing.T) {
	shadowed := Evidence{Language: "python", DstName: "sorted", ProjectDefinesTarget: true}
	if got := Target(shadowed); got != Unknown {
		t.Fatalf("Target(shadowed python sorted) = %q, want %q", got, Unknown)
	}
	goShadowed := Evidence{Language: "go", DstName: "len", ProjectDefinesTarget: true}
	if got := Target(goShadowed); got != Unknown {
		t.Fatalf("Target(shadowed go len) = %q, want %q", got, Unknown)
	}
}

// TestTarget_BuiltinRequiresAnUnqualifiedName: `x.len()` is a method call on a
// receiver, not Go's `len`.
func TestTarget_BuiltinRequiresAnUnqualifiedName(t *testing.T) {
	cases := []Evidence{
		{Language: "go", DstName: "x.len"},
		{Language: "python", DstName: "self.sorted"},
		{Language: "python", DstName: "collections.len"},
	}
	for _, ev := range cases {
		if got := Target(ev); got == Builtin {
			t.Errorf("Target(%+v) = %q, want anything but %q", ev, got, Builtin)
		}
	}
}

// TestBuiltinCandidate_OnlyForLanguagesWithAValidatedSet keeps the
// ProjectDefinesTarget batch query narrow and prevents a language from picking up
// another language's builtins.
func TestBuiltinCandidate_OnlyForLanguagesWithAValidatedSet(t *testing.T) {
	if name, ok := BuiltinCandidate("python", "sorted"); !ok || name != "sorted" {
		t.Fatalf(`BuiltinCandidate("python", "sorted") = (%q, %v), want ("sorted", true)`, name, ok)
	}
	for _, tc := range []struct{ dst, root string }{{"JSON.stringify", "JSON"}, {"Array.isArray", "Array"}} {
		if root, ok := BuiltinCandidate("typescript", tc.dst); !ok || root != tc.root {
			t.Errorf("BuiltinCandidate(typescript, %q) = (%q, %v), want (%q, true)", tc.dst, root, ok, tc.root)
		}
	}
	// `sorted` is not a Go builtin, and Swift has no builtin set at all.
	for _, tc := range []struct{ language, name string }{
		{"go", "sorted"},
		{"swift", "sorted"},
		{"rust", "len"},
		{"typescript", "print"},
		{"typescript", "myArray.map"},
		{"", "len"},
	} {
		if _, ok := BuiltinCandidate(tc.language, tc.name); ok {
			t.Errorf("BuiltinCandidate(%q, %q) = true, want false", tc.language, tc.name)
		}
	}
}

func TestTarget_TypeScriptBuiltinShadowingAndRuntimeGlobals(t *testing.T) {
	if got := Target(Evidence{Language: "typescript", DstName: "Array.isArray", ProjectDefinesTarget: true}); got != Unknown {
		t.Fatalf("shadowed Array = %q, want %q", got, Unknown)
	}
	for _, dst := range []string{"console.log", "Buffer.from", "process.exit"} {
		if got := Target(Evidence{Language: "typescript", DstName: dst}); got != Unknown {
			t.Errorf("runtime global %q = %q, want %q", dst, got, Unknown)
		}
	}
}

func TestTarget_CppStdlibRequiresExplicitStdNamespace(t *testing.T) {
	for _, dst := range []string{"std::move", "std::sort", "::std::move"} {
		if got := Target(Evidence{Language: "cpp", DstName: dst}); got != Stdlib {
			t.Errorf("cpp %q = %q, want %q", dst, got, Stdlib)
		}
	}
	for _, dst := range []string{"move", "sort", "my::std::thing"} {
		if got := Target(Evidence{Language: "cpp", DstName: dst}); got != Unknown {
			t.Errorf("cpp %q = %q, want %q", dst, got, Unknown)
		}
	}
}

func TestTarget_Stdlib(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
	}{
		// Go: both adapters rewrite the qualifier into the full import path, so
		// the import is inside dst_name.
		{"go fmt", Evidence{Language: "go", DstName: "fmt.Println", Imports: []string{"fmt"}}},
		{"go nested stdlib path", Evidence{Language: "go", DstName: "net/http.NewRequest", Imports: []string{"net/http"}}},
		{"go aliased import still stores the path", Evidence{Language: "go", DstName: "encoding/json.Marshal", Imports: []string{"encoding/json"}}},
		// Python: module prefix preserved by the tree-sitter adapter.
		{"python os", Evidence{Language: "python", DstName: "os.getcwd", Imports: []string{"os"}}},
		{"python submodule", Evidence{Language: "python", DstName: "os.path.join", Imports: []string{"os.path"}}},
		{"python alias artifact normalised", Evidence{Language: "python", DstName: "json.dumps", Imports: NormalizeImports("python", []string{"json as j"})}},
		// Java: the import's last segment is the identifier at the call site.
		{"java imported type", Evidence{Language: "java", DstName: "List.of", Imports: []string{"java.util.List"}}},
		{"java fully qualified call", Evidence{Language: "java", DstName: "java.util.Objects.requireNonNull"}},
		{"java static import artifact", Evidence{Language: "java", DstName: "requireNonNull", Imports: NormalizeImports("java", []string{"static java.util.Objects.requireNonNull"})}},
		{"jdk javax subtree", Evidence{Language: "java", DstName: "Cipher.getInstance", Imports: []string{"javax.crypto.Cipher"}}},
		// Rust: direct path, brace group, and `self` in a brace group.
		{"rust direct path", Evidence{Language: "rust", DstName: "std::mem::swap"}},
		{"rust use last segment", Evidence{Language: "rust", DstName: "HashMap::new", Imports: []string{"std::collections::HashMap"}}},
		{"rust brace group", Evidence{Language: "rust", DstName: "HashSet::new", Imports: NormalizeImports("rust", []string{"std::collections::{HashMap, HashSet}"})}},
		{"rust core", Evidence{Language: "rust", DstName: "core::mem::size_of"}},
		// TypeScript: the runtime, not node_modules.
		{"node prefixed", Evidence{Language: "typescript", DstName: "fs.readFileSync", Imports: []string{"node:fs"}}},
		{"node core bare", Evidence{Language: "typescript", DstName: "path.join", Imports: []string{"path"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Target(tc.ev); got != Stdlib {
				t.Fatalf("Target(%+v) = %q, want %q", tc.ev, got, Stdlib)
			}
		})
	}
}

func TestTarget_External(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
	}{
		{"bare specifier", Evidence{Language: "typescript", DstName: "express.Router", Imports: []string{"express"}}},
		{"scoped package", Evidence{Language: "typescript", DstName: "core.render", Imports: []string{"@scope/core"}}},
		{"require form", Evidence{Language: "typescript", DstName: "axios.get", Imports: []string{"axios"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Target(tc.ev); got != External {
				t.Fatalf("Target(%+v) = %q, want %q", tc.ev, got, External)
			}
		})
	}
}

// TestTarget_BuiltinAbstainsWhenTheCallSiteShowsAReceiver is the guard against
// the loudest false positive available: the non-cgo Python adapter matches calls
// with a regex that drops the receiver, so `Model.objects.filter(x)` persists as
// dst_name `filter` -- a Python builtin. `edges.evidence` still holds the call
// text, so the truncation is detectable.
func TestTarget_BuiltinAbstainsWhenTheCallSiteShowsAReceiver(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
	}{
		{"django orm filter", Evidence{Language: "python", DstName: "filter", CallSite: "return Model.objects.filter(active=True)"}},
		{"str format", Evidence{Language: "python", DstName: "format", CallSite: "msg = template.format(x)"}},
		{"dict copy", Evidence{Language: "python", DstName: "copy", CallSite: "other = payload.copy()"}},
		{"regex compile", Evidence{Language: "python", DstName: "compile", CallSite: "pattern = re.compile(raw)"}},
		{"go member len", Evidence{Language: "go", DstName: "len", CallSite: "b.len(x)"}},
		{"cpp scope resolution", Evidence{Language: "go", DstName: "max", CallSite: "std::max(a, b)"}},
		{"pointer accessor", Evidence{Language: "go", DstName: "copy", CallSite: "p->copy(x)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Target(tc.ev); got != Unknown {
				t.Fatalf("Target(%+v) = %q, want %q", tc.ev, got, Unknown)
			}
		})
	}
}

// TestTarget_BuiltinSurvivesACallSiteWithoutAReceiver: the guard must not fire on
// an ordinary unqualified call, including one that merely mentions attributes
// elsewhere on the line.
func TestTarget_BuiltinSurvivesACallSiteWithoutAReceiver(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
	}{
		{"audit case A", Evidence{Language: "python", DstName: "sorted", CallSite: "return sorted(items)"}},
		{"attribute argument", Evidence{Language: "python", DstName: "sorted", CallSite: "return sorted(obj.values)"}},
		{"go builtin", Evidence{Language: "go", DstName: "len", CallSite: "len(xs)"}},
		{"no call site recorded", Evidence{Language: "python", DstName: "sorted"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Target(tc.ev); got != Builtin {
				t.Fatalf("Target(%+v) = %q, want %q", tc.ev, got, Builtin)
			}
		})
	}
}

// TestTarget_ConflictingImportsRefuseToGuess: when two imports bind the same
// name, row order must not decide the answer. This is the P3 posture applied to
// import evidence.
func TestTarget_ConflictingImportsRefuseToGuess(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
	}{
		{"rust std vs crate", Evidence{Language: "rust", DstName: "Result::from", Imports: []string{"std::io::Result", "crate::error::Result"}}},
		{"rust crate vs std", Evidence{Language: "rust", DstName: "Result::from", Imports: []string{"crate::error::Result", "std::io::Result"}}},
		{"ts local vs package", Evidence{Language: "typescript", DstName: "path.join", Imports: []string{"./path", "path"}}},
		{"ts package vs local", Evidence{Language: "typescript", DstName: "path.join", Imports: []string{"path", "./path"}}},
		{"java jdk vs first party", Evidence{Language: "java", DstName: "List.of", Imports: []string{"java.util.List", "com.acme.List"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Target(tc.ev); got != Unknown {
				t.Fatalf("Target(%+v) = %q, want %q", tc.ev, got, Unknown)
			}
		})
	}
}

// TestTarget_ExternalIsNotClaimedForProjectLocalSpecifiers: a relative specifier
// is proven to be this project's own file, but no symbol was bound, so the
// answer is `unknown`. Claiming `project` here would assert a destination that
// was never found.
func TestTarget_ExternalIsNotClaimedForProjectLocalSpecifiers(t *testing.T) {
	cases := []Evidence{
		{Language: "typescript", DstName: "helpers.run", Imports: []string{"./helpers"}},
		{Language: "typescript", DstName: "helpers.run", Imports: []string{"../lib/helpers"}},
		{Language: "typescript", DstName: "helpers.run", Imports: []string{"/abs/helpers"}},
		{Language: "typescript", DstName: "helpers.run", Imports: []string{"#internal/helpers"}},
		{Language: "typescript", DstName: "helpers.run", Imports: []string{"./helpers.js"}},
		// The two near-universal bundler path aliases. `@/` cannot be a package:
		// npm forbids an empty scope.
		{Language: "typescript", DstName: "Button.render", Imports: []string{"@/components/Button"}},
		{Language: "typescript", DstName: "helpers.run", Imports: []string{"~/lib/helpers"}},
		{Language: "typescript", DstName: "helpers.run", Imports: []string{"~lib/helpers"}},
		// Unscoped multi-segment specifiers are project paths once `baseUrl` is
		// set, and the string is the same either way -- so `unknown`, even though
		// this also gives up the `lodash/isEqual` deep-import form.
		{Language: "typescript", DstName: "helpers.run", Imports: []string{"src/utils/helpers"}},
		{Language: "typescript", DstName: "isEqual.call", Imports: []string{"lodash/isEqual"}},
		// A scoped package is only accepted in its exact `@scope/pkg` form.
		{Language: "typescript", DstName: "isEqual.call", Imports: []string{"@lodash/fp/isEqual"}},
	}
	for _, ev := range cases {
		if got := Target(ev); got != Unknown {
			t.Errorf("Target(%+v) = %q, want %q", ev, got, Unknown)
		}
	}
}

func TestTarget_Unknown(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
	}{
		// The core negative: a bare project-looking name with no proof at all.
		{"bare unresolved name", Evidence{Language: "python", DstName: "no_such_symbol_f"}},
		{"bare go name", Evidence{Language: "go", DstName: "RunD"}},
		// Dotted is not external. This is the exact inference P8 refuses.
		{"dotted receiver call", Evidence{Language: "python", DstName: "foo.bar"}},
		{"dotted with unrelated imports", Evidence{Language: "python", DstName: "foo.bar", Imports: []string{"os", "requests"}}},
		{"go dotted receiver", Evidence{Language: "go", DstName: "Sorter.parse_d"}},
		// Co-occurrence is not linkage: the file imports stdlib, the call does not use it.
		{"unlinked stdlib import", Evidence{Language: "go", DstName: "helper", Imports: []string{"fmt", "os"}}},
		{"unlinked node import", Evidence{Language: "typescript", DstName: "map", Imports: []string{"lodash"}}},
		{"python symbol import drops the symbol", Evidence{Language: "python", DstName: "join", Imports: []string{"os.path"}}},
		// A stdlib-looking prefix that is not actually the matched import.
		{"go prefix without separator", Evidence{Language: "go", DstName: "ossify.Run", Imports: []string{"os"}}},
		// Go external is deliberately unprovable.
		{"go dependency path", Evidence{Language: "go", DstName: "github.com/gin-gonic/gin.New", Imports: []string{"github.com/gin-gonic/gin"}}},
		{"go own module path", Evidence{Language: "go", DstName: "github.com/isink17/codegraph/internal/store.Open", Imports: []string{"github.com/isink17/codegraph/internal/store"}}},
		// Rust and Python third-party: same shape as project-local.
		{"rust bare crate", Evidence{Language: "rust", DstName: "Serialize::serialize", Imports: []string{"serde::Serialize"}}},
		// An `as` alias is the binding, and no adapter persists it, so the call
		// site name cannot be linked back to the stdlib path.
		{"rust aliased stdlib import", Evidence{Language: "rust", DstName: "Map::new", Imports: NormalizeImports("rust", []string{"std::collections::HashMap as Map"})}},
		{"rust crate-relative", Evidence{Language: "rust", DstName: "helper", Imports: []string{"crate::util::helper"}}},
		{"python third party", Evidence{Language: "python", DstName: "requests.get", Imports: []string{"requests"}}},
		{"python relative import", Evidence{Language: "python", DstName: "mod.run", Imports: NormalizeImports("python", []string{".mod"})}},
		{"java first party", Evidence{Language: "java", DstName: "Bar.baz", Imports: []string{"com.acme.Bar"}}},
		// Missing evidence fails closed.
		{"empty language", Evidence{DstName: "sorted"}},
		{"empty dst name", Evidence{Language: "python"}},
		{"whitespace dst name", Evidence{Language: "python", DstName: "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Target(tc.ev); got != Unknown {
				t.Fatalf("Target(%+v) = %q, want %q", tc.ev, got, Unknown)
			}
		})
	}
}

// TestTarget_LanguagesWithoutRulesAlwaysReturnUnknown pins the conservative
// gaps: a language with no import rule must not borrow another language's rules,
// however familiar its import strings look.
func TestTarget_LanguagesWithoutRulesAlwaysReturnUnknown(t *testing.T) {
	cases := []Evidence{
		{Language: "cpp", DstName: "vector.push_back", Imports: []string{"vector"}},
		{Language: "cpp", DstName: "printf", Imports: []string{"stdio.h"}},
		{Language: "csharp", DstName: "Encoding.GetBytes", Imports: []string{"System.Text"}},
		{Language: "kotlin", DstName: "println", Imports: []string{"kotlin.io.println"}},
		{Language: "php", DstName: "User.find", Imports: []string{"App\\Models\\User"}},
		{Language: "ruby", DstName: "JSON.parse", Imports: []string{"json"}},
		{Language: "swift", DstName: "Foundation.Date", Imports: []string{"Foundation"}},
	}
	for _, ev := range cases {
		if got := Target(ev); got != Unknown {
			t.Errorf("Target(%+v) = %q, want %q -- %s", ev, got, Unknown, conservativeGaps[ev.Language])
		}
	}
}

// TestConservativeGapsCoverEveryParsedLanguage keeps the gap table honest: every
// language CodeGraph persists must either have documented gaps or be added here
// deliberately, so coverage cannot widen or narrow silently.
func TestConservativeGapsCoverEveryParsedLanguage(t *testing.T) {
	parsed := make([]string, 0, len(Capabilities()))
	for _, capability := range Capabilities() {
		parsed = append(parsed, capability.Language)
	}
	for _, language := range parsed {
		if conservativeGaps[language] == "" {
			t.Errorf("conservativeGaps has no entry for %q", language)
		}
	}
	for language := range conservativeGaps {
		found := false
		for _, candidate := range parsed {
			if candidate == language {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("conservativeGaps documents %q, which no adapter persists", language)
		}
	}
	// Every language that HAS rules must still be listed, because none of them
	// can prove every dimension.
	for language := range importRules {
		if conservativeGaps[language] == "" {
			t.Errorf("language %q has import rules but no documented gap", language)
		}
	}
	// The one cross-cutting gap is documented too, so the asymmetry between the
	// builtin rule and the import rules cannot become folklore.
	if shadowingGap == "" {
		t.Error("shadowingGap is empty; the builtin-only abstention must stay documented")
	}
}

func TestCapabilitiesAreCompleteAndDeterministic(t *testing.T) {
	capabilities := Capabilities()
	if len(capabilities) != len(canonicalLanguages) {
		t.Fatalf("capability count = %d, want %d", len(capabilities), len(canonicalLanguages))
	}
	seen := map[string]bool{}
	for i, capability := range capabilities {
		if seen[capability.Language] {
			t.Fatalf("duplicate capability %q", capability.Language)
		}
		seen[capability.Language] = true
		if i > 0 && capabilities[i-1].Language >= capability.Language {
			t.Fatalf("capabilities not sorted: %q before %q", capabilities[i-1].Language, capability.Language)
		}
		if !capability.Project || !capability.Unknown {
			t.Errorf("%q lacks universal project/unknown capability: %+v", capability.Language, capability)
		}
	}
	if !seen["cpp"] || !seen["typescript"] {
		t.Fatal("newly covered languages missing from capability table")
	}
	want := map[string][3]bool{
		"cpp": {false, true, false}, "go": {true, true, false},
		"java": {false, true, false}, "kotlin": {false, false, false},
		"csharp": {false, false, false}, "php": {false, false, false},
		"python": {true, true, false}, "ruby": {false, false, false},
		"rust": {false, true, false}, "swift": {false, false, false},
		"typescript": {true, true, true},
	}
	for _, capability := range capabilities {
		got := [3]bool{capability.Builtin, capability.Stdlib, capability.External}
		if got != want[capability.Language] {
			t.Errorf("capability %q = %v, want %v", capability.Language, got, want[capability.Language])
		}
	}
}

func TestNormalizeImports(t *testing.T) {
	cases := []struct {
		name     string
		language string
		raw      []string
		want     []string
	}{
		{"python alias artifact", "python", []string{"numpy as np"}, []string{"numpy"}},
		{"python relative dropped", "python", []string{".mod", "..pkg", "os"}, []string{"os"}},
		{"java static prefix", "java", []string{"static java.util.Objects.requireNonNull"}, []string{"java.util.Objects.requireNonNull"}},
		{"rust brace group", "rust", []string{"std::collections::{HashMap, HashSet}"}, []string{"std::collections::HashMap", "std::collections::HashSet"}},
		{"rust self in group", "rust", []string{"std::io::{self, Read}"}, []string{"std::io", "std::io::Read"}},
		{"rust as clause", "rust", []string{"std::collections::HashMap as Map"}, []string{"std::collections::HashMap"}},
		{"go untouched", "go", []string{"net/http"}, []string{"net/http"}},
		{"blank dropped", "go", []string{"", "   ", "fmt"}, []string{"fmt"}},
		{"typescript untouched", "typescript", []string{"@scope/pkg"}, []string{"@scope/pkg"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeImports(tc.language, tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("NormalizeImports(%q, %v) = %v, want %v", tc.language, tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("NormalizeImports(%q, %v) = %v, want %v", tc.language, tc.raw, got, tc.want)
				}
			}
		})
	}
}

// TestTarget_PrefersTheLongestMatchingImport: when both `os` and a longer path
// match, the longer one decides the root module.
func TestTarget_PrefersTheLongestMatchingImport(t *testing.T) {
	ev := Evidence{
		Language: "python",
		DstName:  "os.path.join.helper",
		Imports:  []string{"os", "os.path.join"},
	}
	if got := Target(ev); got != Stdlib {
		t.Fatalf("Target(%+v) = %q, want %q", ev, got, Stdlib)
	}
}

// TestTarget_DoesNotMutateEvidence: the classifier is read-only over its input,
// so a caller can safely share one preloaded import slice across every edge in a
// file.
func TestTarget_DoesNotMutateEvidence(t *testing.T) {
	imports := NormalizeImports("rust", []string{"std::collections::{HashMap, HashSet}", "serde::Serialize"})
	before := append([]string(nil), imports...)
	Target(Evidence{Language: "rust", DstName: "HashMap::new", Imports: imports})
	for i := range imports {
		if imports[i] != before[i] {
			t.Fatalf("Target mutated Imports: %v, want %v", imports, before)
		}
	}
}
