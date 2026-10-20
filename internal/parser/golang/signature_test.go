package golang

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser/gofixture"
)

func TestGoSignatureMatrix(t *testing.T) {
	pf, err := New().Parse(context.Background(), "sig.go", []byte(gofixture.GoSignatureSource))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(gofixture.RenderGoSignatures(pf), "\n"), strings.Join(gofixture.GoSignatures, "\n"); got != want {
		t.Errorf("signatures:\n got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestGoTypeAliasSymbols(t *testing.T) {
	pf, err := New().Parse(context.Background(), "alias.go", []byte(gofixture.GoTypeAliasSource))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(gofixture.RenderGoTypeSymbols(pf), "\n"), strings.Join(gofixture.GoTypeAliasSymbols, "\n"); got != want {
		t.Errorf("type symbols:\n got:\n%s\n\nwant:\n%s", got, want)
	}
}
