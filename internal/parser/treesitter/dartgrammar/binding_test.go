//go:build cgo

package dartgrammar

import (
	"context"
	"strings"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
)

func TestABIIsLoadableByRuntime(t *testing.T) {
	if abi := ABI(); abi < 13 || abi > 14 {
		t.Fatalf("ABI = %d, the go-tree-sitter runtime loads 13..14 only", abi)
	}
}

// SetLanguage ignores an ABI the runtime rejects; a parse then yields no tree.
func TestParseReturnsTree(t *testing.T) {
	p := sitter.NewParser()
	p.SetLanguage(GetLanguage())
	src := "import 'package:a/a.dart' as a show B;\nsealed class S {}\nfinal class F extends S { F.named(); }\nextension type const Id(int v) {}\nvoid main() { final (x, y) = (1, 2); print(x + y); }\n"
	tree, err := p.ParseCtx(context.Background(), nil, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if tree == nil || tree.RootNode() == nil {
		t.Fatal("parse returned no tree")
	}
	if s := tree.RootNode().String(); strings.Contains(s, "ERROR") || strings.Contains(s, "MISSING") {
		t.Fatalf("tree has errors: %s", s)
	}
}
