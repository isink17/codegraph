package parser

import (
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestGoFileIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      *graph.GoFileIdentity
	}{
		{"plain", "package p\n", &graph.GoFileIdentity{Package: "p"}},
		{"go:build normalized", "//go:build linux&&(amd64||arm64)\n\npackage p\n", &graph.GoFileIdentity{Package: "p", BuildConstraint: "linux && (amd64 || arm64)"}},
		{"go:build wins over +build", "//go:build linux\n// +build darwin\n\npackage p\n", &graph.GoFileIdentity{Package: "p", BuildConstraint: "linux"}},
		{"+build lines are anded", "// +build linux darwin\n// +build amd64\n\npackage p\n", &graph.GoFileIdentity{Package: "p", BuildConstraint: "(linux || darwin) && amd64"}},
		{"after a block comment", "/* license */\n\n//go:build ignore\n\npackage main\n", &graph.GoFileIdentity{Package: "main", BuildConstraint: "ignore"}},
		{"comment below the clause is not a constraint", "package p\n\n//go:build linux\n", &graph.GoFileIdentity{Package: "p"}},
		{"external test package", "package p_test\n", &graph.GoFileIdentity{Package: "p_test"}},
		{"malformed constraint", "//go:build linux &&\n\npackage p\n", &graph.GoFileIdentity{Package: "p", BuildConstraint: "invalid://go:build linux &&"}},
		{"no package clause", "func f() {}\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GoFileIdentity([]byte(tc.src))
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("GoFileIdentity = %+v, want %+v", got, tc.want)
			}
		})
	}
}
