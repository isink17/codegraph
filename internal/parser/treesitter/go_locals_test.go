//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser/gofixture"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
)

func renderGoMatrix(t *testing.T, calls map[int]string, locals []string) string {
	t.Helper()
	lines := make([]string, 0, len(calls)+len(locals))
	for line, name := range calls {
		lines = append(lines, fmt.Sprintf("call %d %s", line, name))
	}
	lines = append(lines, locals...)
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestGoLocalScopeShadowMatrixTreeSitter holds the tree-sitter adapter to the
// same shadow matrix the go/ast adapter is held to.
func TestGoLocalScopeShadowMatrixTreeSitter(t *testing.T) {
	pf, err := NewGo().Parse(context.Background(), "main.go", []byte(gofixture.GoShadowMatrixSource))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := map[int]string{}
	for _, e := range pf.Edges {
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
		t.Errorf("local bindings:\n got:\n  %s\nwant:\n  %s", strings.Join(rendered, "\n  "), strings.Join(gofixture.GoShadowMatrixLocals, "\n  "))
	}
}

// TestGoAdapterParity is the projection comparison the receiver-scope strategy
// depends on: both adapters must emit the same call destinations, the same
// local bindings, the same scope ranges and the same proven types for the
// supported matrix. "One adapter is more precise" is not an acceptable outcome
// for a high-confidence strategy -- it would make the confidence depend on how
// the binary was built.
func TestGoAdapterParity(t *testing.T) {
	ctx := context.Background()
	for _, src := range []string{gofixture.GoShadowMatrixSource, gofixture.GoReceiverTypeSource} {
		native, err := goparser.New().Parse(ctx, "main.go", []byte(src))
		if err != nil {
			t.Fatalf("go/ast parse: %v", err)
		}
		sitter, err := NewGo().Parse(ctx, "main.go", []byte(src))
		if err != nil {
			t.Fatalf("tree-sitter parse: %v", err)
		}

		nativeCalls := map[int]string{}
		for _, e := range native.Edges {
			nativeCalls[e.Line] = e.DstName
		}
		sitterCalls := map[int]string{}
		for _, e := range sitter.Edges {
			sitterCalls[e.Line] = e.DstName
		}
		var nativeLocals, sitterLocals []string
		for _, b := range native.Scope.GoLocals {
			nativeLocals = append(nativeLocals, fmt.Sprintf("local %s [%d-%d] %s|%s|%s ptr=%v", b.Name, b.ScopeStartLine, b.ScopeEndLine, b.TypeName, b.TypePackage, b.TypeImportPath, b.Pointer))
		}
		for _, b := range sitter.Scope.GoLocals {
			sitterLocals = append(sitterLocals, fmt.Sprintf("local %s [%d-%d] %s|%s|%s ptr=%v", b.Name, b.ScopeStartLine, b.ScopeEndLine, b.TypeName, b.TypePackage, b.TypeImportPath, b.Pointer))
		}
		sort.Strings(nativeLocals)
		sort.Strings(sitterLocals)

		if a, b := renderGoMatrix(t, nativeCalls, nativeLocals), renderGoMatrix(t, sitterCalls, sitterLocals); a != b {
			t.Errorf("adapters disagree:\n go/ast:\n%s\n\n tree-sitter:\n%s", a, b)
		}
	}
}

func TestGoQualifiedReceiverTypeCarriesImportPathTreeSitter(t *testing.T) {
	src := `package main

import store "example.com/project/store"

func run() {
	var s *store.Store
	s.Close()
	var u *Unknown
	u.Close()
}
`
	pf, err := NewGo().Parse(context.Background(), "main.go", []byte(src))
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
