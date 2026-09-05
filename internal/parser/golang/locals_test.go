package golang

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser/gofixture"
)

func TestGoLocalScopeShadowMatrix(t *testing.T) {
	pf, err := New().Parse(context.Background(), "main.go", []byte(gofixture.GoShadowMatrixSource))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := map[int]string{}
	for _, e := range pf.Edges {
		if prev, dup := got[e.Line]; dup {
			t.Fatalf("two calls on line %d (%q and %q); the matrix assumes one per line", e.Line, prev, e.DstName)
		}
		got[e.Line] = e.DstName
	}
	for line, want := range gofixture.GoShadowMatrixCalls {
		if got[line] != want {
			t.Errorf("line %d: dst_name = %q, want %q", line, got[line], want)
		}
	}
	for line, name := range got {
		if _, ok := gofixture.GoShadowMatrixCalls[line]; !ok {
			t.Errorf("unexpected call on line %d: %q", line, name)
		}
	}

	var rendered []string
	for _, b := range pf.Scope.GoLocals {
		rendered = append(rendered, fmt.Sprintf("%s [%d-%d] %s%s: %v", b.Name, b.ScopeStartLine, b.ScopeEndLine, b.TypePackage, b.TypeName, b.Pointer))
	}
	sort.Strings(rendered)
	if strings.Join(rendered, "\n") != strings.Join(gofixture.GoShadowMatrixLocals, "\n") {
		t.Errorf("local bindings:\n got:\n%s\nwant:\n%s", strings.Join(rendered, "\n  "), strings.Join(gofixture.GoShadowMatrixLocals, "\n  "))
	}
}

// TestGoBlankAndDotImportsAreNotReceiverEvidence pins matrix case L. Both
// parsers drop the `_` and `.` names and register the path's base as an alias,
// so those spellings behave like any other import: a local of the same name
// still shadows it, and nothing about a blank or dot import becomes evidence
// that a local has a type.
func TestGoBlankAndDotImportsAreNotReceiverEvidence(t *testing.T) {
	src := `package main

import (
	_ "example.com/project/pq"
	. "example.com/project/dot"
)

type Thing struct{}

func (t *Thing) Get() {}

func shadowed(pq *Thing) { pq.Get() }

func unshadowed() { pq.Get() }
`
	pf, err := New().Parse(context.Background(), "main.go", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[int]string{}
	for _, e := range pf.Edges {
		got[e.Line] = e.DstName
	}
	if got[12] != "pq.Get" {
		t.Errorf("shadowed blank-import alias: dst_name = %q, want %q", got[12], "pq.Get")
	}
	if got[14] != "example.com/project/pq.Get" {
		t.Errorf("unshadowed blank-import alias: dst_name = %q, want %q", got[14], "example.com/project/pq.Get")
	}
	for _, b := range pf.Scope.GoLocals {
		if b.TypePackage != "" && b.TypeImportPath == "" {
			t.Errorf("binding %q carries a package qualifier %q that resolves to no import", b.Name, b.TypePackage)
		}
	}
}

// TestGoQualifiedReceiverTypeCarriesImportPath pins that `var x pkg.Type`
// records the import the qualifier resolves to, so the resolver can prove the
// package rather than binding on the bare type name.
func TestGoQualifiedReceiverTypeCarriesImportPath(t *testing.T) {
	src := `package main

import store "example.com/project/store"

func run() {
	var s *store.Store
	s.Close()
	var u *Unknown
	u.Close()
}
`
	pf, err := New().Parse(context.Background(), "main.go", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byName := map[string]string{}
	for _, b := range pf.Scope.GoLocals {
		byName[b.Name] = b.TypePackage + "|" + b.TypeName + "|" + b.TypeImportPath
	}
	if got, want := byName["s"], "store|Store|example.com/project/store"; got != want {
		t.Errorf("qualified receiver type = %q, want %q", got, want)
	}
	if got, want := byName["u"], "|Unknown|"; got != want {
		t.Errorf("own-package receiver type = %q, want %q", got, want)
	}
}
