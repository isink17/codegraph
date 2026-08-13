// Package audit contains a developer-facing correctness harness that measures
// the current edge-resolution behaviour of CodeGraph against a deliberately
// adversarial multi-language fixture.
//
// The harness is a measurement tool, not a product feature: it is not
// registered as a CLI or MCP command, and it never changes resolver behaviour.
// It drives the real indexer and store, then reads the persisted graph back
// through the existing export APIs.
//
// Guiding rule: a wrong graph edge is worse than a missing graph edge. The
// fixture therefore declares, per case, which resolutions are legitimate and
// which are name coincidences, so the harness can classify an observed edge as
// expected-valid, expected-unresolved, or miswired instead of merely counting
// cross-language edges.
package audit

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FixtureVersion identifies the fixture contents. Bump it whenever a case is
// added, removed, or its source files change, so a recorded baseline cannot be
// compared against a different fixture by accident.
const FixtureVersion = "p1-adversarial-v1"

// Expectation is the fixture's declared verdict for a case: what a correct
// resolver is allowed to do with the call edge.
type Expectation string

const (
	// ExpectValid means a legitimate definition exists and must be selected.
	ExpectValid Expectation = "expected_valid"
	// ExpectUnresolved means no project definition exists, so staying
	// unresolved is the only honest outcome.
	ExpectUnresolved Expectation = "expected_unresolved"
	// ExpectInvalid means every same-named definition in the fixture is a name
	// coincidence with no call relationship, so any resolution is a false edge.
	// The fixture contains no FFI or binding declarations of any kind.
	ExpectInvalid Expectation = "expected_invalid"
	// ExpectAmbiguous means several same-language definitions compete and the
	// resolver holds no evidence that distinguishes them. One of them may well
	// be what the call meant, but nothing in the graph says which, so binding
	// any of them is an arbitrary choice and staying unresolved is the only
	// reproducible outcome. Selecting the "reasonable" one is not credited: a
	// ranking rule that earns that credit does not exist yet.
	ExpectAmbiguous Expectation = "expected_ambiguous"
)

// Outcome is the harness verdict for a case after observing the real resolver.
type Outcome string

const (
	// OutcomeExpectedValid: resolved to a declared legitimate target.
	OutcomeExpectedValid Outcome = "expected_valid"
	// OutcomeExpectedUnresolved: correctly left unresolved.
	OutcomeExpectedUnresolved Outcome = "expected_unresolved"
	// OutcomeCorrectlyUnresolved: an ExpectInvalid case that was not wired up.
	OutcomeCorrectlyUnresolved Outcome = "correctly_unresolved"
	// OutcomeMiswired: resolved to a destination the fixture declares invalid.
	OutcomeMiswired Outcome = "miswired"
	// OutcomeMissingEdge: an ExpectValid case that was left unresolved.
	OutcomeMissingEdge Outcome = "missing_edge"
	// OutcomeNoEdge: the parser never produced the call edge this case needs,
	// i.e. the fixture no longer exercises what it claims to.
	OutcomeNoEdge Outcome = "no_call_edge"
	// OutcomeNondeterministic: repeats disagreed on the classification itself.
	OutcomeNondeterministic Outcome = "nondeterministic"
)

// Case is one adversarial scenario. A case is identified by the pair
// (SrcFile, DstName): every case uses a distinct DstName so cases cannot
// contaminate each other.
type Case struct {
	ID     string
	Title  string
	Expect Expectation

	// SrcFile is the fixture-relative path of the calling file.
	SrcFile string
	// DstName is the unresolved call name the parser is expected to emit.
	DstName string

	// ValidTargets lists qualified names that a correct resolver may select.
	// Only meaningful for ExpectValid.
	ValidTargets []string
	// CompetingTargets lists the qualified names that make an ExpectAmbiguous
	// case ambiguous. It documents what the resolver had to choose between; it
	// is never a permission to select one of them.
	CompetingTargets []string
	// ShadowTargets lists qualified names that are test/mock/fixture
	// definitions competing with ValidTargets. Selecting one is a miswire and
	// is additionally counted as a test-shadow binding.
	ShadowTargets []string

	// Note explains what defect the case probes.
	Note string
}

// ExpectedDefinition is a symbol the fixture must produce for a case to be
// meaningful. Fixture-integrity tests assert every one of these exists.
type ExpectedDefinition struct {
	CaseID   string
	Name     string
	Language string
	File     string
}

// Cases returns the adversarial cases in stable order.
func Cases() []Case {
	return []Case{
		{
			ID:      "A",
			Title:   "cross-language global name collision (python call, swift definition)",
			Expect:  ExpectInvalid,
			SrcFile: "src/py/caller_a.py",
			DstName: "sorted",
			Note:    "Python calls the builtin sorted(); the only project definition is an unrelated Swift func sorted(). No FFI exists, so any edge is a name coincidence.",
		},
		{
			ID:           "B",
			Title:        "same-language legitimate call (python to python)",
			Expect:       ExpectValid,
			SrcFile:      "src/py/caller_b.py",
			DstName:      "normalize_b",
			ValidTargets: []string{"helpers_b.normalize_b"},
			Note:         "Control case. Guards against 'fixing' correctness by refusing to resolve anything.",
		},
		{
			ID:      "C",
			Title:   "same callable name defined in four languages, called from a fifth",
			Expect:  ExpectInvalid,
			SrcFile: "src/py/caller_c.py",
			DstName: "serialize_c",
			Note:    "java, kotlin, rust and php each define serialize_c; python calls it. Measures which definition wins today.",
		},
		{
			ID:      "D",
			Title:   "qualified/suffix collision selects an incompatible language",
			Expect:  ExpectInvalid,
			SrcFile: "src/go/caller_d.go",
			DstName: "Sorter.parse_d",
			Note:    "Go emits dst_name Sorter.parse_d; the only dot_tail2 match is a Java method in ShapesD.java. Exercises the suffix strategies rather than exact-name matching.",
		},
		{
			ID:      "E",
			Title:   "test-shadow candidate competing with the production definition",
			Expect:  ExpectAmbiguous,
			SrcFile: "src/py/caller_e.py",
			DstName: "load_config_e",
			CompetingTargets: []string{
				"config_e.load_config_e",
				"test_config_e.load_config_e",
			},
			ShadowTargets: []string{"test_config_e.load_config_e"},
			Note:          "Production code calls load_config_e; a production and a test definition compete, both python, both plain functions. Until a test-shadow ranking rule exists the resolver has nothing that separates them, so P3 requires a stable unresolved result: binding the production definition would be the right answer reached by an arbitrary route (symbol insertion order), and binding the test definition would be a false edge.",
		},
		{
			ID:      "F",
			Title:   "honest-negative control (no project definition anywhere)",
			Expect:  ExpectUnresolved,
			SrcFile: "src/py/caller_f.py",
			DstName: "no_such_symbol_f",
			Note:    "Nothing in the fixture defines this name. Any resolution would be fabricated.",
		},
	}
}

// ExpectedDefinitions lists the definition symbols the fixture relies on.
func ExpectedDefinitions() []ExpectedDefinition {
	return []ExpectedDefinition{
		{CaseID: "A", Name: "sorted", Language: "swift", File: "src/swift/SortKitA.swift"},
		{CaseID: "B", Name: "normalize_b", Language: "python", File: "src/py/helpers_b.py"},
		{CaseID: "C", Name: "serialize_c", Language: "java", File: "src/java/SerializerC.java"},
		{CaseID: "C", Name: "serialize_c", Language: "kotlin", File: "src/kotlin/serializer_c.kt"},
		{CaseID: "C", Name: "serialize_c", Language: "rust", File: "src/rust/serializer_c.rs"},
		{CaseID: "C", Name: "serialize_c", Language: "php", File: "src/php/serializer_c.php"},
		{CaseID: "D", Name: "parse_d", Language: "java", File: "src/java/ShapesD.java"},
		{CaseID: "E", Name: "load_config_e", Language: "python", File: "src/py/config_e.py"},
		{CaseID: "E", Name: "load_config_e", Language: "python", File: "tests/py/test_config_e.py"},
	}
}

// fixtureFiles is the fixture source tree.
//
// Constraints these snippets respect, because the default (non-cgo) heuristic
// adapters strip strings by scanning for quote characters: no string literals,
// no apostrophes, no Rust lifetimes. Every snippet was chosen to match the
// adapter's actual definition regex.
var fixtureFiles = map[string]string{
	// Case A: python caller, swift-only definition of the same name.
	"src/py/caller_a.py": `def run_a(items):
    return sorted(items)
`,
	"src/swift/SortKitA.swift": `func sorted(values: Int) -> Int {
    return values
}
`,

	// Case B: legitimate same-language resolution.
	"src/py/helpers_b.py": `def normalize_b(value):
    return value
`,
	"src/py/caller_b.py": `def run_b(value):
    return normalize_b(value)
`,

	// Case C: four languages define serialize_c, python calls it.
	"src/py/caller_c.py": `def run_c(payload):
    return serialize_c(payload)
`,
	"src/java/SerializerC.java": `class SerializerC {
    public static int serialize_c(int x) {
        return x;
    }
}
`,
	"src/kotlin/serializer_c.kt": `fun serialize_c(x: Int): Int {
    return x
}
`,
	"src/rust/serializer_c.rs": `pub fn serialize_c(x: i32) -> i32 {
    x
}
`,
	"src/php/serializer_c.php": `<?php
function serialize_c($x) {
    return $x;
}
`,

	// Case D: Go emits a dotted dst_name whose only suffix match is Java.
	"src/go/caller_d.go": `package fixture

func RunD(payload int) int {
	return Sorter.parse_d(payload)
}
`,
	"src/java/ShapesD.java": `class Sorter {
    public static int parse_d(int x) {
        return x;
    }
}
`,

	// Case E: production definition competing with a test definition.
	"src/py/config_e.py": `def load_config_e():
    return 1
`,
	"tests/py/test_config_e.py": `def load_config_e():
    return 2
`,
	"src/py/caller_e.py": `def run_e():
    return load_config_e()
`,

	// Case F: honest negative.
	"src/py/caller_f.py": `def run_f(x):
    return no_such_symbol_f(x)
`,
}

// FixtureFilePaths returns the fixture-relative paths in sorted order.
func FixtureFilePaths() []string {
	paths := make([]string, 0, len(fixtureFiles))
	for path := range fixtureFiles {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// WriteFixture materialises the fixture tree under root. Files are written in
// sorted path order so the on-disk tree is byte-identical between runs.
func WriteFixture(root string) error {
	for _, rel := range FixtureFilePaths() {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(fixtureFiles[rel]), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ffiMarkers are tokens that would indicate a real foreign-function interface
// between two fixture languages. If any appeared, a cross-language edge could be
// legitimate and the fixture's "every cross-language edge is a coincidence"
// premise would be void.
var ffiMarkers = []string{
	"extern", "cgo", "import \"C\"", "JNIEXPORT", "jni", "@_cdecl", "@_silgen_name",
	"ctypes", "cffi", "dllimport", "DllImport", "NativeMethod", "System.loadLibrary",
	"PyObject", "napi", "wasm_bindgen", "no_mangle",
}

// FixtureDeclaresFFI reports whether any fixture source declares a foreign
// function interface. It must be false.
func FixtureDeclaresFFI() bool {
	for _, content := range fixtureFiles {
		lower := strings.ToLower(content)
		for _, marker := range ffiMarkers {
			if strings.Contains(lower, strings.ToLower(marker)) {
				return true
			}
		}
	}
	return false
}

// looksLikeTestPath reports whether a fixture-relative path is a test, mock or
// fixture location. It is a harness-local heuristic used only for reporting;
// the production resolver has no such notion, which is precisely what case E
// measures.
func looksLikeTestPath(rel string) bool {
	rel = "/" + filepath.ToSlash(rel)
	for _, seg := range []string{"/test/", "/tests/", "/mocks/", "/fixtures/", "/testdata/"} {
		if strings.Contains(rel, seg) {
			return true
		}
	}
	base := filepath.Base(rel)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	return strings.HasPrefix(stem, "test_") ||
		strings.HasPrefix(stem, "mock_") ||
		strings.HasSuffix(stem, "_test") ||
		strings.HasSuffix(stem, "Test")
}
