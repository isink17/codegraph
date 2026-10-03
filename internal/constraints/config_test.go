package constraints

import (
	"runtime"
	"strings"
	"testing"
)

// TestParseConfigErrorCodes holds one fixture per static config error code.
// group_overlap needs the index and is covered by TestGroupOverlapIsConfigError.
func TestParseConfigErrorCodes(t *testing.T) {
	const okGroups = `"groups":{"a":{"include":["a/**"]},"b":{"include":["b/**"]}}`
	cases := []struct {
		name, doc, code, location string
	}{
		{"json", `{"schema_version":1,`, CodeJSON, ""},
		{"json wrong type", `{"schema_version":1,"groups":{"a":{"include":"a/**"}}}`, CodeJSON, "groups.a.include"},
		{"unknown top field", `{"schema_version":1,"grups":{}}`, CodeUnknownField, "grups"},
		{"unknown group field", `{"schema_version":1,"groups":{"a":{"include":["a/**"],"exlude":["x"]}}}`, CodeUnknownField, "groups.a.exlude"},
		{"unknown rule field", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"r","kind":"forbidden_dependency","from":["a"],"to":["b"],"severity":"x"}]}`, CodeUnknownField, "rules[0].severity"},
		{"schema_version missing", `{"groups":{}}`, CodeSchemaVersion, "schema_version"},
		{"schema_version 2", `{"schema_version":2}`, CodeSchemaVersion, "schema_version"},
		{"group_name uppercase", `{"schema_version":1,"groups":{"Api":{"include":["a/**"]}}}`, CodeGroupName, "groups.Api"},
		{"group_name duplicate", `{"schema_version":1,"groups":{"a":{"include":["a/**"]},"a":{"include":["b/**"]}}}`, CodeGroupName, "groups.a"},
		{"group_name empty", `{"schema_version":1,"groups":{"":{"include":["a/**"]}}}`, CodeGroupName, "groups."},
		{"group_include_empty", `{"schema_version":1,"groups":{"a":{"include":[]}}}`, CodeGroupIncludeEmpty, "groups.a.include"},
		{"group_include_missing", `{"schema_version":1,"groups":{"a":{"exclude":["a/x"]}}}`, CodeGroupIncludeEmpty, "groups.a.include"},
		{"pattern backslash", `{"schema_version":1,"groups":{"a":{"include":["internal\\store\\**"]}}}`, CodePattern, "groups.a.include[0]"},
		{"pattern dotdot", `{"schema_version":1,"groups":{"a":{"include":["internal/../cmd/**"]}}}`, CodePattern, "groups.a.include[0]"},
		{"pattern leading slash", `{"schema_version":1,"groups":{"a":{"include":["/internal/**"]}}}`, CodePattern, "groups.a.include[0]"},
		{"pattern bad bracket", `{"schema_version":1,"groups":{"a":{"include":["internal/[a"]}}}`, CodePattern, "groups.a.include[0]"},
		{"pattern drive letter", `{"schema_version":1,"groups":{"a":{"include":["C:/src/**"]}}}`, CodePattern, "groups.a.include[0]"},
		{"rule_id empty", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"","kind":"forbidden_cycles","groups":["a","b"]}]}`, CodeRuleID, "rules[0].id"},
		{"rule_id duplicate", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"r","kind":"forbidden_cycles","groups":["a","b"]},{"id":"r","kind":"forbidden_cycles","groups":["a","b"]}]}`, CodeRuleID, "rules[1].id"},
		{"rule_kind", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"r","kind":"layer_allow","from":["a"]}]}`, CodeRuleKind, "rules[0].kind"},
		{"rule_field not allowed", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"r","kind":"forbidden_dependency","from":["a"],"to":["b"],"of":["a"]}]}`, CodeRuleField, "rules[0].of"},
		{"rule_field missing", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"r","kind":"allowed_dependencies","from":["a"]}]}`, CodeRuleField, "rules[0].to"},
		{"rule_field empty", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"r","kind":"forbidden_dependency","from":[],"to":["b"]}]}`, CodeRuleField, "rules[0].from"},
		{"rule_group", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"r","kind":"forbidden_dependency","from":["a"],"to":["c"]}]}`, CodeRuleGroup, "rules[0].to[0]"},
		{"rule_contradiction forbidden", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"r","kind":"forbidden_dependency","from":["a"],"to":["a","b"]}]}`, CodeRuleContradiction, "rules[0]"},
		{"rule_contradiction cycles", `{"schema_version":1,` + okGroups + `,"rules":[{"id":"r","kind":"forbidden_cycles","groups":["a","a"]}]}`, CodeRuleContradiction, "rules[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, errs := ParseConfig([]byte(tc.doc))
			if cfg != nil || len(errs) == 0 {
				t.Fatalf("ParseConfig accepted an invalid document: %+v", cfg)
			}
			for _, e := range errs {
				if e.Code == tc.code && e.Location == tc.location {
					return
				}
			}
			t.Fatalf("errors = %+v, want code %s at %q", errs, tc.code, tc.location)
		})
	}
}

func TestParseConfigAcceptsContractExample(t *testing.T) {
	cfg, errs := ParseConfig([]byte(`{
  "schema_version": 1,
  "groups": {
    "domain": {"include": ["internal/domain/**"], "exclude": ["internal/domain/**/*_test.go"]},
    "infra":  {"include": ["internal/infra/**"]},
    "api":    {"include": ["cmd/**", "internal/api/**"]}
  },
  "rules": [
    {"id": "domain-no-infra", "kind": "forbidden_dependency", "from": ["domain"], "to": ["infra"]},
    {"id": "api-layer",       "kind": "allowed_dependencies", "from": ["api"],    "to": ["domain"]},
    {"id": "domain-owned",    "kind": "allowed_dependents",   "of":   ["domain"], "from": ["api"]},
    {"id": "no-group-cycles", "kind": "forbidden_cycles",     "groups": ["api", "domain", "infra"]}
  ]
}`))
	if len(errs) > 0 {
		t.Fatalf("errors = %+v", errs)
	}
	if len(cfg.Groups) != 3 || len(cfg.Rules) != 4 || cfg.Groups[0].Name != "api" || cfg.Rules[0].ID != "api-layer" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestParseConfigErrorsAreSorted(t *testing.T) {
	_, errs := ParseConfig([]byte(`{"zzz":1,"groups":{"B":{"include":[]}}}`))
	for i := 1; i < len(errs); i++ {
		a, b := errs[i-1], errs[i]
		if a.Location > b.Location || (a.Location == b.Location && a.Code > b.Code) {
			t.Fatalf("errors not sorted by (location, code): %+v", errs)
		}
	}
	if len(errs) < 3 {
		t.Fatalf("errors = %+v, want every problem reported", errs)
	}
}

// TestAnchoredSegmentGlob pins the pattern grammar fixtures.
func TestAnchoredSegmentGlob(t *testing.T) {
	tree := []string{"internal/store/a.go", "internal/store/a_test.go", "cmd/x/main.go"}
	cases := []struct {
		pattern string
		want    []string
	}{
		{"internal/**", []string{"internal/store/a.go", "internal/store/a_test.go"}},
		{"**/*_test.go", []string{"internal/store/a_test.go"}},
		{"*.go", nil},
		{" internal/**", nil},
		{"Internal/**", nil},
		{"**", tree},
		{"cmd/*/main.go", []string{"cmd/x/main.go"}},
		{"internal/**/a.go", []string{"internal/store/a.go"}},
		{"internal/store", nil},
	}
	for _, tc := range cases {
		p, err := compilePattern(tc.pattern)
		if err != nil {
			t.Fatalf("compilePattern(%q) error = %v", tc.pattern, err)
		}
		var got []string
		for _, f := range tree {
			if p.match(strings.Split(f, "/")) {
				got = append(got, f)
			}
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%q matched %v, want %v", tc.pattern, got, tc.want)
		}
	}
	for _, bad := range []string{`internal\store\**`, "internal/../cmd/**", "", "a//b", "a/", "./a"} {
		if _, err := compilePattern(bad); err == nil {
			t.Errorf("compilePattern(%q) accepted an invalid pattern", bad)
		}
	}
}

// TestBackslashFileNameMatchesOnlyThroughWildcard: on POSIX a backslash in a
// stored path is data. A literal pattern cannot name it (backslash is a pattern
// error), but `?` can.
func TestBackslashFileNameMatchesOnlyThroughWildcard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a backslash cannot occur in a logical path stored on Windows")
	}
	stored := strings.Split(`dir/a\b`, "/")
	p, err := compilePattern("dir/a?b")
	if err != nil || !p.match(stored) {
		t.Fatalf("dir/a?b does not match dir/a\\b (err %v)", err)
	}
	if _, err := compilePattern(`dir/a\b`); err == nil {
		t.Fatal(`literal dir/a\b accepted; want a pattern error`)
	}
}
