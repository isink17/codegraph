package diff

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
)

// TestGoUnmodeledDeclarationChangeMatrix pins that a Go declaration change the
// graph has no declaration for (a struct field, an interface method, a
// constant's value or type) is never a silent no_risk_evidence, while
// comments, formatting and function bodies add no uncertainty. The score
// stays evidence-only: such a change is limited_evidence at score 0.
func TestGoUnmodeledDeclarationChangeMatrix(t *testing.T) {
	const lib = `package p

// Base is documented.
type Base struct {
	X int
	Y int
}

type Getter interface {
	Get() int
	Put(int)
}

const Limit = 10

const (
	A Kind = iota
	B
	C
)

const P, Q = 1, 2

var V, W int

type Kind int

var Defaults = []int{1, 2}

func Use(b Base) int { return b.X + b.Y + Limit }
`
	const caller = `package p

type impl struct{}

func (impl) Get() int { return 1 }
func (impl) Put(int)  {}

func Main() {
	var g Getter = impl{}
	g.Put(Use(Base{X: 1, Y: 2}))
	_ = g.Get() + Limit + int(B) + int(C) + P + Q + V + W
}
`
	cases := []struct {
		name, from, to string
		unmodeled      string
	}{
		{name: "field removed", from: "\tY int\n", to: "", unmodeled: "type Base"},
		{name: "field type changed", from: "\tY int\n", to: "\tY string\n", unmodeled: "type Base"},
		{name: "interface method removed", from: "\tPut(int)\n", to: "", unmodeled: "type Getter"},
		{name: "const type changed", from: "const Limit = 10", to: `const Limit = "ten"`, unmodeled: "const Limit"},
		{name: "implicit const type changed", from: "A Kind = iota", to: "A int = iota", unmodeled: "const A|const B|const C"},
		{name: "iota reordered", from: "\tB\n\tC\n", to: "\tC\n\tB\n", unmodeled: "const B|const C"},
		{name: "iota blank inserted", from: "\tB\n", to: "\t_\n\tB\n", unmodeled: "const B|const C"},
		{name: "multi-name const split", from: "const P, Q = 1, 2", to: "const P = 1\nconst Q = \"two\"", unmodeled: "const Q"},
		{name: "multi-name const split same values", from: "const P, Q = 1, 2", to: "const P = 1\nconst Q = 2"},
		{name: "multi-name var split", from: "var V, W int", to: "var V int64\nvar W int", unmodeled: "var V"},
		{name: "multi-name var grouped", from: "var V, W int", to: "var (\n\tV, W int\n)"},
		{name: "single spec parenthesized", from: "type Getter interface {\n\tGet() int\n\tPut(int)\n}", to: "type (\n\tGetter interface {\n\t\tGet() int\n\t\tPut(int)\n\t}\n)"},
		{name: "field change beside added declaration", from: "type Base struct {\n\tX int\n\tY int\n}", to: "type Base struct{ X int; Y string }; var Extra = 1", unmodeled: "type Base"},
		{name: "comment only", from: "// Base is documented.", to: "// Base holds two values."},
		{name: "field comment", from: "\tX int\n", to: "\tX int // the first\n"},
		{name: "reformatted", from: "type Base struct {\n\tX int\n\tY int\n}", to: "type Base struct{ X int; Y int }"},
		{name: "trailing comma", from: "[]int{1, 2}", to: "[]int{\n\t1,\n\t2,\n}"},
		{name: "var value changed", from: "[]int{1, 2}", to: "[]int{1, 3}", unmodeled: "var Defaults"},
		{name: "body only", from: "return b.X + b.Y + Limit", to: "return b.Y + b.X + Limit"},
	}
	files := map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "p/lib.go": lib, "p/use.go": caller}
	docs := map[string]map[string]string{}
	for name, adapter := range testGoAdapters {
		docs[name] = map[string]string{}
		t.Run(name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					if !strings.Contains(lib, tc.from) {
						t.Fatalf("edit %q not in the source", tc.from)
					}
					d := editScenario(t, adapter, files, func(r *gittest.Repo) {
						r.Write("p/lib.go", strings.Replace(lib, tc.from, tc.to, 1))
					}).Diff
					var got []string
					for _, u := range d.Risk.Uncertainty {
						if u.Reason == "go_declaration_change_unmodeled" && u.Identity == "p/lib.go" {
							got = append(got, u.Detail)
						}
					}
					if strings.Join(got, "|") != tc.unmodeled {
						t.Fatalf("unmodeled = %v, want %q; risk %s", got, tc.unmodeled, canonical(d.Risk))
					}
					want := "no_risk_evidence"
					if tc.unmodeled != "" {
						want = "limited_evidence"
					}
					if d.Risk.Assessment != want || d.Risk.Score == nil || *d.Risk.Score != 0 {
						t.Fatalf("assessment %s score %v, want %s at 0: %s", d.Risk.Assessment, d.Risk.Score, want, canonical(d.Risk))
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

// TestGoUnmodeledSkipsModeledChange pins that a declaration change the graph
// already reports (a changed signature) adds no unmodeled row of its own.
func TestGoUnmodeledSkipsModeledChange(t *testing.T) {
	files := map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "p/lib.go": "package p\n\nfunc T(a int) int { return a }\n", "p/use.go": "package p\n\nfunc U() { _ = T(1) }\n"}
	for name, adapter := range testGoAdapters {
		t.Run(name, func(t *testing.T) {
			d := editScenario(t, adapter, files, func(r *gittest.Repo) {
				r.Write("p/lib.go", "package p\n\nfunc T(a int64) int { return int(a) }\n")
			}).Diff
			for _, u := range d.Risk.Uncertainty {
				if u.Reason == "go_declaration_change_unmodeled" {
					t.Fatalf("modeled signature change reported unmodeled: %s", canonical(d.Risk))
				}
			}
			if d.Risk.Assessment != "risk_evidence" {
				t.Fatalf("assessment = %s", d.Risk.Assessment)
			}
		})
	}
}

func TestGoShapesKeysAndNormalization(t *testing.T) {
	a, ok := goShapes([]byte("package p\n\nimport \"fmt\"\n\n// T doc.\ntype T struct {\n\tA int // a\n\tB []int\n}\n\nfunc (t *T) M(x int) { fmt.Println(x) }\n\nconst (\n\tX K = iota\n\tY\n)\n\nvar V = []int{\n\t1,\n\t2,\n}\n"))
	if !ok {
		t.Fatal("parse failed")
	}
	b, ok := goShapes([]byte("package p\n\nimport \"fmt\"\n\ntype T struct{ A int; B []int }\n\nfunc (t *T) M(x int) {\n\tfmt.Println(x + 1)\n}\n\nconst (\n\tX K = iota // first\n\tY\n)\n\nvar V = []int{1, 2}\n"))
	if !ok {
		t.Fatal("parse failed")
	}
	var keys []string
	for k := range a {
		keys = append(keys, k)
		if !sameShapes(a[k], b[k]) {
			t.Errorf("%s: %q != %q", k, a[k][0].tokens, b[k][0].tokens)
		}
	}
	sort.Strings(keys)
	if got := strings.Join(keys, "|"); got != "const X|const Y|func (*T) M|type T|var V" {
		t.Fatalf("keys = %s", got)
	}
	if a["const Y"][0].tokens != "iota#1 K = iota" {
		t.Fatalf("implicit const = %q", a["const Y"][0].tokens)
	}
	if _, ok := goShapes([]byte("package p\nfunc {")); ok {
		t.Fatal("broken source parsed")
	}
}

func TestGoUnmodeledReportsUnreadableSource(t *testing.T) {
	src := []byte("package p\n\ntype T struct{ A int }\n")
	file := func(b []byte) json.RawMessage {
		raw, _ := json.Marshal(SourceFile{Path: "p/a.go", ContentHash: sha256Hex(b), Language: "go"})
		return raw
	}
	head := []byte("package p\n\ntype T struct{ A string }\n")
	changes := []Change{{Kind: "file_modified", Identity: "p/a.go", Before: file(src), After: file(head)}}
	read := func(b []byte) func(string) ([]byte, bool) { return func(string) ([]byte, bool) { return b, true } }
	broken := []byte("package p\n\ntype T struct{\n")
	for name, h := range map[string]Revision{"nil source": {}, "unparsable": {source: read(broken)}} {
		changes[0].After = file(broken)
		if name == "nil source" {
			changes[0].After = file(head)
		}
		got := goUnmodeledUncertainty(changes, Revision{source: read(src)}, h, nil)
		if len(got) != 1 || got[0].Reason != "go_declaration_shape_unavailable" || got[0].Identity != "p/a.go" {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	got := goUnmodeledUncertainty(changes[:0:0], Revision{}, Revision{}, nil)
	if len(got) != 0 {
		t.Fatalf("no changes: %+v", got)
	}
	changes[0].After = file(head)
	if got := goUnmodeledUncertainty(changes, Revision{source: read(src)}, Revision{source: read(head)}, nil); len(got) != 1 || got[0].Detail != "type T" {
		t.Fatalf("readable: %+v", got)
	}
}

// TestGoBuildConstraintChangeIsUncertain pins that a changed build constraint,
// which can add or drop a file from a build while the graph stays the same,
// is reported as limited_evidence at score 0, and that rewrites with the same
// meaning stay quiet.
func TestGoBuildConstraintChangeIsUncertain(t *testing.T) {
	const body = "package p\n\nfunc T() int { return 1 }\n"
	cases := []struct {
		name, from, to, detail string
		head                   string // whole head file, when set
	}{
		{name: "added", from: "", to: "//go:build linux\n\n", detail: "none -> linux"},
		{name: "added ignore", from: "", to: "//go:build ignore\n\n", detail: "none -> ignore"},
		{name: "removed", from: "//go:build linux\n\n", to: "", detail: "linux -> none"},
		{name: "narrowed", from: "//go:build linux\n\n", to: "//go:build linux && amd64\n\n", detail: "linux -> linux && amd64"},
		{name: "plus build changed", from: "// +build linux\n\n", to: "// +build darwin\n\n", detail: "linux -> darwin"},
		{name: "spacing only", from: "//go:build linux&&amd64\n\n", to: "//go:build linux && amd64\n\n"},
		{name: "plus build to go:build", from: "// +build linux\n// +build amd64\n\n", to: "//go:build linux && amd64\n\n"},
		{name: "doc comment", from: "", to: "// Package p is documented.\n"},
		{name: "directive after package clause", head: "package p\n\n//go:build linux\n\nfunc T() int { return 1 }\n"},
	}
	files := map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "p/use.go": "package p\n\nfunc U() { _ = T() }\n"}
	docs := map[string]map[string]string{}
	for name, adapter := range testGoAdapters {
		docs[name] = map[string]string{}
		t.Run(name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					base := map[string]string{"p/lib.go": tc.from + body}
					for k, v := range files {
						base[k] = v
					}
					to := tc.to + body
					if tc.head != "" {
						to = tc.head
					}
					d := editScenario(t, adapter, base, func(r *gittest.Repo) {
						r.Write("p/lib.go", to)
					}).Diff
					var got []string
					for _, u := range d.Risk.Uncertainty {
						if u.Reason == "go_build_constraint_change_unmodeled" && u.Identity == "p/lib.go" {
							got = append(got, u.Detail)
						}
					}
					if strings.Join(got, "|") != tc.detail {
						t.Fatalf("constraint uncertainty = %v, want %q; risk %s", got, tc.detail, canonical(d.Risk))
					}
					want := "no_risk_evidence"
					if tc.detail != "" {
						want = "limited_evidence"
					}
					if d.Risk.Assessment != want || d.Risk.Score == nil || *d.Risk.Score != 0 {
						t.Fatalf("assessment %s score %v, want %s at 0: %s", d.Risk.Assessment, d.Risk.Score, want, canonical(d.Risk))
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
