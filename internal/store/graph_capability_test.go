package store

import (
	"reflect"
	"testing"
)

func TestClassifyGraphCapability(t *testing.T) {
	g := func(language, profile string, callEdges bool) FileParserProfileGroup {
		return FileParserProfileGroup{Language: language, Profile: profile, CallEdges: callEdges, Files: 1}
	}
	cases := []struct {
		name       string
		groups     []FileParserProfileGroup
		state      string
		perLang    map[string]string
		limitLangs []string
	}{
		{"empty", nil, "", map[string]string{}, nil},
		{"rich", []FileParserProfileGroup{g("go", "treesitter:go:v1", true), g("java", "treesitter:java:v5", true)},
			GraphCallCapable, map[string]string{"go": GraphCallCapable, "java": GraphCallCapable}, nil},
		{"go fallback is complete", []FileParserProfileGroup{g("go", "go-ast:go:v1", true)},
			GraphCallCapable, map[string]string{"go": GraphCallCapable}, nil},
		{"symbols only", []FileParserProfileGroup{g("java", "heuristic:java:v1", false)},
			GraphSymbolsOnly, map[string]string{"java": GraphSymbolsOnly}, []string{"java"}},
		{"unknown provenance", []FileParserProfileGroup{g("go", "", false), g("go", "treesitter:go:v1", true)},
			GraphUnknownProvenance, map[string]string{"go": GraphUnknownProvenance}, []string{"go"}},
		{"mixed profiles in one language", []FileParserProfileGroup{g("java", "treesitter:java:v5", true), g("java", "heuristic:java:v1", false)},
			GraphMixed, map[string]string{"java": GraphMixed}, []string{"java"}},
		{"two call-capable profiles still mixed", []FileParserProfileGroup{g("python", "treesitter:python:v3", true), g("python", "python-regex:python:v3", true)},
			GraphMixed, map[string]string{"python": GraphMixed}, []string{"python"}},
		{"languages disagree", []FileParserProfileGroup{g("go", "go-ast:go:v1", true), g("java", "heuristic:java:v1", false)},
			GraphMixed, map[string]string{"go": GraphCallCapable, "java": GraphSymbolsOnly}, []string{"java"}},
		{"approximate fallback is disclosed", []FileParserProfileGroup{g("python", "python-regex:python:v3", true)},
			GraphCallCapable, map[string]string{"python": GraphCallCapable}, []string{"python"}},
		{"empty groups ignored", []FileParserProfileGroup{{Language: "java", Profile: "", Files: 0}, {Language: "", Profile: "x", Files: 3}},
			"", map[string]string{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyGraphCapability(tc.groups)
			if got.State != tc.state {
				t.Fatalf("state = %q, want %q", got.State, tc.state)
			}
			perLang := map[string]string{}
			for _, l := range got.Languages {
				perLang[l.Language] = l.Capability
			}
			if !reflect.DeepEqual(perLang, tc.perLang) {
				t.Fatalf("languages = %v, want %v", perLang, tc.perLang)
			}
			var limitLangs []string
			for _, l := range got.Limitations() {
				if l.Effect == "" || l.Capability != perLang[l.Language] {
					t.Fatalf("limitation %+v: empty effect or capability disagrees with %q", l, perLang[l.Language])
				}
				limitLangs = append(limitLangs, l.Language)
			}
			if !reflect.DeepEqual(limitLangs, tc.limitLangs) {
				t.Fatalf("limitations for %v, want %v", limitLangs, tc.limitLangs)
			}
		})
	}
}

// One database, files written by different parsers: the persisted state, not
// any binary, decides the capability. A file that owns no evidence and carries
// no profile (never parsed) must not turn the graph into unknown provenance --
// the cheap first pass sees it, the exact population does not.
func TestGraphCapabilityFromPersistedMixedProfiles(t *testing.T) {
	f := newGateFixture(t)
	richJava := f.file(t, "src/A.java", "java")
	fallbackJava := f.file(t, "src/B.java", "java")
	goFile := f.file(t, "main.go", "go")
	f.file(t, "empty.go", "go") // no evidence, profile ''
	f.symbol(t, richJava, "A", "A", "java")
	f.symbol(t, fallbackJava, "B", "B", "java")
	f.symbol(t, goFile, "main", "main", "go")
	for _, stmt := range []string{
		`UPDATE files SET parser_profile = 'treesitter:java:v5', parser_call_edges = 1 WHERE path = 'src/A.java'`,
		`UPDATE files SET parser_profile = 'heuristic:java:v1', parser_call_edges = 0 WHERE path = 'src/B.java'`,
		`UPDATE files SET parser_profile = 'treesitter:go:v1', parser_call_edges = 1 WHERE path = 'main.go'`,
		`UPDATE files SET parser_profile = '', parser_call_edges = 0 WHERE path = 'empty.go'`,
	} {
		if _, err := f.store.db.ExecContext(f.ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	got, err := f.store.GraphCapability(f.ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	want := GraphCapability{State: GraphMixed, Languages: []LanguageCapability{
		{Language: "go", Capability: GraphCallCapable, Profiles: []string{"treesitter:go:v1"}},
		{Language: "java", Capability: GraphMixed, Profiles: []string{"heuristic:java:v1", "treesitter:java:v5"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capability = %+v, want %+v", got, want)
	}
	limitations, err := f.store.GraphLimitations(f.ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(limitations) != 1 || limitations[0].Language != "java" || limitations[0].Capability != GraphMixed {
		t.Fatalf("limitations = %+v, want java/mixed only", limitations)
	}

	// Converge Java on the rich profile: nothing left to disclose.
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET parser_profile = 'treesitter:java:v5', parser_call_edges = 1 WHERE language = 'java'`); err != nil {
		t.Fatal(err)
	}
	limitations, err = f.store.GraphLimitations(f.ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if limitations != nil {
		t.Fatalf("limitations = %+v, want nil on a call-capable graph", limitations)
	}

	// Legacy rows (pre-provenance) that still own evidence: unknown provenance.
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET parser_profile = '', parser_call_edges = 0 WHERE path = 'main.go'`); err != nil {
		t.Fatal(err)
	}
	got, err = f.store.GraphCapability(f.ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Languages[0].Language != "go" || got.Languages[0].Capability != GraphUnknownProvenance {
		t.Fatalf("go = %+v, want unknown_provenance", got.Languages[0])
	}
}
