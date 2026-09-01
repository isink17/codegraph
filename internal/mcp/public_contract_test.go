package mcp

import (
	"strings"
	"testing"
)

func TestQueryPublicDescriptionsStayTrustworthy(t *testing.T) {
	want := map[string][]string{
		"find_symbol":        {"exact", "substring"},
		"find_callers":       {"resolved", "indexed", "unresolved hints"},
		"find_callees":       {"resolved", "indexed"},
		"get_impact_radius":  {"resolved", "uncertainty"},
		"find_related_tests": {"target_found=false"},
		"trace_dependencies": {"resolved", "exact"},
	}
	for name, terms := range want {
		desc := toolByName[name].description
		for _, term := range terms {
			if !strings.Contains(strings.ToLower(desc), strings.ToLower(term)) {
				t.Errorf("%s description %q missing %q", name, desc, term)
			}
		}
		if strings.Contains(strings.ToLower(desc), "fuzzy") {
			t.Errorf("%s description overclaims fuzzy resolution: %q", name, desc)
		}
	}
	for _, name := range []string{"find_callers", "find_callees"} {
		for _, arg := range []string{"symbol", "symbol_id", "limit", "offset", "detail", "format"} {
			if _, ok := toolArgumentSpecs[name].properties[arg]; !ok {
				t.Errorf("%s schema/validator dropped %s", name, arg)
			}
		}
	}
}
