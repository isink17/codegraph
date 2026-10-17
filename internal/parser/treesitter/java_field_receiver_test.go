//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestJavaFieldReceiverEvidence(t *testing.T) {
	src := `class S {
  Repo repo; Repo[] arr;
  S(Repo repo) { this.repo.save(); }
  void f() { repo.save(); }
  void g() { arr.save(); }
  void h(Object o) { switch (o) { case Repo repo -> repo.save(); default -> {} } }
  { Object o = null; if (o instanceof Repo repo) repo.save(); }
  void a() { new Runnable() { Repo own; public void run() { own.save(); } }; }
}
enum E { A; Repo r; void f() { r.save(); } }
`
	want := map[int]string{
		3:  graph.JavaFieldReceiverEvidence + ";Repo",
		4:  graph.JavaFieldReceiverEvidence + ";Repo",
		5:  "",
		6:  "",
		7:  "",
		8:  "",
		10: "",
	}
	p, err := NewJava().Parse(context.Background(), "S.java", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range p.Edges {
		w, ok := want[e.Line]
		if e.Kind != "calls" || !ok {
			continue
		}
		field := strings.HasPrefix(e.Evidence, graph.JavaFieldReceiverEvidence)
		if (w == "" && field) || (w != "" && e.Evidence != w) {
			t.Errorf("line %d %s evidence %q, want %q", e.Line, e.DstName, e.Evidence, w)
		}
		seen++
	}
	if seen != len(want) {
		t.Errorf("saw %d of %d expected calls", seen, len(want))
	}
	if NewJavaV13().Profile().ID != "treesitter:java:v13" || NewJava().Profile().ID != "treesitter:java:v14" {
		t.Fatal("unexpected Java profiles")
	}
}
