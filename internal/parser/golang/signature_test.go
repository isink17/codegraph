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
