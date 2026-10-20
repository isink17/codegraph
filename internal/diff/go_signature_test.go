package diff

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
)

// TestGoSignatureChangeMatrix pins how a Go signature change reaches the diff
// under every Go adapter of this build: the stored signature is part of the
// declaration identity, so a changed signature is a removed and an added
// declaration, and an unchanged caller of it is risk evidence. Where both
// adapters are built in, they must produce the same document.
func TestGoSignatureChangeMatrix(t *testing.T) {
	const lib = `package p

type Point struct{ X int }

func (p Point) Norm() int { return p.X }

func Target(a int, s string) (int, error) { return a, nil }

func Sum(xs []int) int { return len(xs) }

func Apply[T any](x T, f func(T) T) T { return f(x) }

func Each(f func(int) int) {}
`
	const caller = `package p

func Use() {
	_, _ = Target(1, "s")
	_ = Sum(nil)
	_ = Apply(1, func(v int) int { return v })
	Each(func(v int) int { return v })
	var pt Point
	_ = pt.Norm()
}
`
	cases := []struct {
		name, from, to string
		newCaller      string
		before, after  string
		dependent      bool
	}{
		{name: "body only", from: "return a, nil", to: "return a + 1, nil"},
		{name: "param added", from: "Target(a int, s string)", to: "Target(a int, s string, upper bool)",
			before: "func Target(a int, s string) (int, error)", after: "func Target(a int, s string, upper bool) (int, error)", dependent: true},
		{name: "param removed", from: "Target(a int, s string)", to: "Target(a int)",
			before: "func Target(a int, s string) (int, error)", after: "func Target(a int) (int, error)", dependent: true},
		{name: "param type changed", from: "Target(a int, s string)", to: "Target(a int64, s string)",
			before: "func Target(a int, s string) (int, error)", after: "func Target(a int64, s string) (int, error)", dependent: true},
		{name: "result changed", from: "(int, error) { return a, nil }", to: "error { return nil }",
			before: "func Target(a int, s string) (int, error)", after: "func Target(a int, s string) error", dependent: true},
		{name: "variadic", from: "Sum(xs []int)", to: "Sum(xs ...int)",
			before: "func Sum(xs []int) int", after: "func Sum(xs ...int) int", dependent: true},
		{name: "receiver", from: "(p Point) Norm", to: "(p *Point) Norm",
			before: "func (p Point) Norm() int", after: "func (p *Point) Norm() int", dependent: true},
		{name: "generic", from: "Apply[T any](x T, f func(T) T)", to: "Apply[T any](x []T, f func(T) T)",
			before: "func Apply[T any](x T, f func(T) T) T", after: "func Apply[T any](x []T, f func(T) T) T", dependent: true},
		{name: "nested func type", from: "Each(f func(int) int)", to: "Each(f func(int) (int, bool))",
			before: "func Each(f func(int) int)", after: "func Each(f func(int) (int, bool))", dependent: true},
		{name: "compiling refactor", from: "Target(a int, s string)", to: "Target(a int, s string, upper bool)",
			newCaller: strings.Replace(caller, `Target(1, "s")`, `Target(1, "s", false)`, 1),
			before:    "func Target(a int, s string) (int, error)", after: "func Target(a int, s string, upper bool) (int, error)", dependent: true},
	}
	docs := map[string]map[string]string{}
	for name, adapter := range testGoAdapters {
		docs[name] = map[string]string{}
		t.Run(name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					if !strings.Contains(lib, tc.from) {
						t.Fatalf("edit %q not in the source", tc.from)
					}
					got := editScenario(t, adapter, map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "p/lib.go": lib, "p/use.go": caller}, func(r *gittest.Repo) {
						r.Write("p/lib.go", strings.Replace(lib, tc.from, tc.to, 1))
						if tc.newCaller != "" {
							r.Write("p/use.go", tc.newCaller)
						}
					})
					d := got.Diff
					removed, added := signatures(t, d.Symbols.Removed, "before"), signatures(t, d.Symbols.Added, "after")
					if tc.before == "" {
						if len(removed)+len(added)+len(d.Symbols.Changed) != 0 || d.Risk.Assessment != "no_risk_evidence" {
							t.Fatalf("unchanged signature: removed %v, added %v, risk %s", removed, added, canonical(d.Risk))
						}
					} else if strings.Join(removed, "|") != tc.before || strings.Join(added, "|") != tc.after {
						t.Fatalf("signatures: removed %v, added %v; want %q -> %q", removed, added, tc.before, tc.after)
					}
					if tc.dependent && (d.Risk.Assessment != "risk_evidence" || ruleCount(d.Risk, RuleDependentRetargeted) == 0) {
						t.Fatalf("caller of a changed signature not cited: %s", canonical(d.Risk))
					}
					docs[name][tc.name] = canonical(d)
				})
			}
		})
	}
	if len(docs) > 1 {
		for _, tc := range cases {
			if a, b := docs["go-ast"][tc.name], docs["tree-sitter"][tc.name]; a != b {
				t.Errorf("%s: adapters disagree:\n go/ast:      %s\n tree-sitter: %s", tc.name, a, b)
			}
		}
	}
}

// TestGoTypeParameterChangeMatrix pins that a change to a type parameter list
// alone reaches the diff under every Go adapter of this build, as any other
// signature change does. Signatures are source text, so a reformatted list is
// a change too. A compiling head is not proof that behavior is unchanged, so
// no case expects a lower risk because the module still builds.
func TestGoTypeParameterChangeMatrix(t *testing.T) {
	const lib = `package p

type Box[T any] struct{ v T }

func (b Box[T]) Get() T { return b.v }

func Id[T any](x T) T { return x }

func Pair[K comparable, V any](k K, v V) map[K]V { return map[K]V{k: v} }

func Num[T ~int | ~int64](x T) T { return x }
`
	const caller = `package p

func Use() {
	_ = Id("s")
	_ = Pair("k", 1)
	_ = Num(3)
	var b Box[int]
	_ = b.Get()
}
`
	cases := []struct {
		name, from, to string
		callerFrom     string
		callerTo       string
		before, after  string
		unscored       bool
	}{
		{name: "body only", from: "return x }", to: "return (x) }"},
		{name: "constraint changed", from: "Id[T any]", to: "Id[T ~int]",
			before: "func Id[T any](x T) T", after: "func Id[T ~int](x T) T"},
		{name: "parameter added", from: "Id[T any](x T) T", to: "Id[T any, U any](x T) T",
			before: "func Id[T any](x T) T", after: "func Id[T any, U any](x T) T"},
		{name: "parameter removed", from: "Pair[K comparable, V any](k K, v V) map[K]V", to: "Pair[K comparable](k K, v int) map[K]int",
			before: "func Pair[K comparable, V any](k K, v V) map[K]V", after: "func Pair[K comparable](k K, v int) map[K]int"},
		{name: "parameter renamed", from: "Id[T any](x T) T", to: "Id[E any](x E) E",
			before: "func Id[T any](x T) T", after: "func Id[E any](x E) E"},
		{name: "comparable replaced", from: "Pair[K comparable", to: "Pair[K ~string",
			before: "func Pair[K comparable, V any](k K, v V) map[K]V", after: "func Pair[K ~string, V any](k K, v V) map[K]V"},
		{name: "type set changed", from: "Num[T ~int | ~int64]", to: "Num[T ~int64 | ~float64]",
			before: "func Num[T ~int | ~int64](x T) T", after: "func Num[T ~int64 | ~float64](x T) T"},
		{name: "generic type constraint", from: "type Box[T any]", to: "type Box[T comparable]",
			before: "[T any] struct", after: "[T comparable] struct", unscored: true},
		{name: "generic receiver renamed", from: "(b Box[T]) Get() T", to: "(b Box[E]) Get() E",
			before: "func (b Box[T]) Get() T", after: "func (b Box[E]) Get() E"},
		{name: "reformatted list", from: "Pair[K comparable, V any]", to: "Pair[K comparable,V any]",
			before: "func Pair[K comparable, V any](k K, v V) map[K]V", after: "func Pair[K comparable,V any](k K, v V) map[K]V"},
		{name: "callers updated", from: "Id[T any]", to: "Id[T ~int]", callerFrom: `Id("s")`, callerTo: "Id(1)",
			before: "func Id[T any](x T) T", after: "func Id[T ~int](x T) T"},
	}
	files := map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "p/lib.go": lib, "p/use.go": caller}
	docs := map[string]map[string]string{}
	for name, adapter := range testGoAdapters {
		docs[name] = map[string]string{}
		t.Run(name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					if !strings.Contains(lib, tc.from) || !strings.Contains(caller, tc.callerFrom) {
						t.Fatalf("edit %q / %q not in the source", tc.from, tc.callerFrom)
					}
					edit := func(r *gittest.Repo) {
						r.Write("p/lib.go", strings.Replace(lib, tc.from, tc.to, 1))
						if tc.callerFrom != "" {
							r.Write("p/use.go", strings.Replace(caller, tc.callerFrom, tc.callerTo, 1))
						}
					}
					d := editScenario(t, adapter, files, edit).Diff
					removed, added := signatures(t, d.Symbols.Removed, "before"), signatures(t, d.Symbols.Added, "after")
					if tc.before == "" {
						if len(removed)+len(added)+len(d.Symbols.Changed) != 0 || d.Risk.Assessment != "no_risk_evidence" {
							t.Fatalf("unchanged signature: removed %v, added %v, risk %s", removed, added, canonical(d.Risk))
						}
					} else {
						if strings.Join(removed, "|") != tc.before || strings.Join(added, "|") != tc.after {
							t.Fatalf("signatures: removed %v, added %v; want %q -> %q", removed, added, tc.before, tc.after)
						}
						if tc.unscored {
							// A type use is no resolved edge, so the change is uncertainty, not points.
							if d.Risk.Assessment != "limited_evidence" || len(d.Risk.Uncertainty) != 1 || d.Risk.Uncertainty[0].Reason != "removed_declaration_without_resolved_dependents" {
								t.Fatalf("changed type: %s", canonical(d.Risk))
							}
						} else if d.Risk.Assessment != "risk_evidence" || ruleCount(d.Risk, RuleDependentRetargeted) == 0 {
							t.Fatalf("caller of a changed signature not cited: %s", canonical(d.Risk))
						}
					}
					doc := canonical(d)
					if again := canonical(editScenario(t, adapter, files, edit).Diff); again != doc {
						t.Fatalf("repeated diff differs:\n%s\n%s", doc, again)
					}
					docs[name][tc.name] = doc
				})
			}
		})
	}
	if len(docs) > 1 {
		for _, tc := range cases {
			if a, b := docs["go-ast"][tc.name], docs["tree-sitter"][tc.name]; a != b {
				t.Errorf("%s: adapters disagree:\n go/ast:      %s\n tree-sitter: %s", tc.name, a, b)
			}
		}
	}
}

func signatures(t *testing.T, changes []Change, side string) []string {
	t.Helper()
	var out []string
	for _, c := range changes {
		raw := c.Before
		if side == "after" {
			raw = c.After
		}
		var v struct {
			Signature string `json:"signature"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v.Signature)
	}
	return out
}
