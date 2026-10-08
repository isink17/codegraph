//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// scalaEdges renders each edge as "callee@call line:col->def line:col".
func scalaEdges(pf graph.ParsedFile) []string {
	out := make([]string, 0, len(pf.Edges))
	for _, e := range pf.Edges {
		out = append(out, fmt.Sprintf("%s@%d:%d->%s", e.DstName, e.Line, e.Col, strings.TrimPrefix(e.Evidence, graph.ScalaLocalFunctionEvidence)))
	}
	return out
}

func TestScalaLocalFunctionCallsBind(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"braced recursion through a case clause", `object P {
  def sum(xs: List[Int]): Int = {
    @tailrec def go(rest: List[Int], acc: Int): Int = rest match {
      case Nil => acc
      case h :: t => go(t, acc + h)
    }
    go(xs, 0)
  }
}
`, []string{"go@5:22->3:5", "go@7:5->3:5"}},
		{"indentation syntax", `object Q:
  def run(n: Int): Int =
    def helper(i: Int): Int = i * 2
    def twice(i: Int) = helper(helper(i))
    twice(n)
`, []string{"helper@4:25->3:5", "helper@4:32->3:5", "twice@5:5->4:5"}},
		{"def in a case clause body", `object C {
  def m(x: Int) = x match { case 1 => def f() = 2; f() case _ => 0 }
}
`, []string{"f@2:52->2:39"}},
		// A local def shadows every outer binding of its name: a member,
		// an inherited member and any import, wildcard or explicit.
		{"local def shadows members and outer imports", `import scala.math._
import scala.math.max
class R extends Base {
  def max(a: Int, b: Int): Int = 0
  def m: Int = { def max(a: Int): Int = a; max(1) }
}
`, []string{"max@5:44->5:18"}},
		{"def in a member value initializer", `class V {
  val total: Int = { def add(a: Int, b: Int) = a + b; add(1, 2) }
}
`, []string{"add@2:55->2:22"}},
		// The inner def is the innermost binding for the inner call; the
		// outer call's scope spells the name twice and is refused.
		{"nested same-name defs", `object N {
  def m = {
    def f() = 1
    def g = { def f() = 2; f() }
    f()
  }
}
`, []string{"f@4:28->4:15"}},
		{"selection of the same name is not a binding", `object S {
  def m(o: Other) = { def f() = 1; o.f(); f() }
}
`, []string{"f@2:43->2:23"}},
		{"curried and block arguments", `object K {
  def m = { def f(a: Int)(b: Int) = a + b; def g(h: => Int) = h; f(1)(2) + g { 3 } }
}
`, []string{"f@2:66->2:13", "g@2:76->2:44"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scalaEdges(parseScala(t, "Bind.scala", tc.src)); !slices.Equal(got, tc.want) {
				t.Fatalf("edges = %q, want %q", got, tc.want)
			}
		})
	}
}

// Each fixture's only candidate is a local def that a hazard in its scope
// may not be the binding of.
func TestScalaLocalFunctionCallsRefuse(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"overloads in one block", "object O { def m = { def f(i: Int) = i; def f(s: String) = 0; f(1) } }\n"},
		{"shadowed by a val", "object O { def m = { def f() = 1; { val f = () => 2; f() } } }\n"},
		{"shadowed by a var", "object O { def m = { def f() = 1; var f = () => 2; f() } }\n"},
		{"shadowed by a parameter", "object O { def m = { def f(i: Int) = i; def g(f: Int => Int) = f(1); 0 } }\n"},
		{"shadowed by a lambda parameter", "object O { def m(xs: List[Int => Int]) = { def f(i: Int) = i; xs.map(f => f(1)) } }\n"},
		{"shadowed by a pattern", "object O { def m(x: Any) = { def f(i: Int) = i; x match { case f: (Int => Int) => f(1) } } }\n"},
		{"shadowed by an extractor binding", "object O { def m(x: Any) = { def f(i: Int) = i; x match { case Some(f) => f(1) } } }\n"},
		{"shadowed by a for binding", "object O { def m(fs: List[Int => Int]) = { def f(i: Int) = i; for (f <- fs) yield f(1) } }\n"},
		{"shadowed by a nested def", "object O { def m = { def f() = 1; def g = { def f() = 2; 0 }; f() } }\n"},
		{"shadowed by a local object", "object O { def m = { def f(i: Int) = i; { object f { def apply(i: Int) = 0 }; f(1) } } }\n"},
		{"shadowed by a local case class", "object O { def m = { def f(i: Int) = i; { case class f(i: Int); f(1) } } }\n"},
		{"shadowed by a given", "object O { def m = { def f() = 1; { given f: (() => Int) = () => 2; f() } } }\n"},
		{"shadowed by an implicit val", "object O { def m = { def f() = 1; { implicit val f: (() => Int) = () => 2; f() } } }\n"},
		{"local extension of the name", "object O { def m = { def f() = 1; extension (s: String) def f(): Int = 2; f() } }\n"},
		{"wildcard import in the block", "object O { def m = { import Other._; def f() = 1; f() } }\n"},
		{"wildcard import between", "object O { def m = { def f() = 1; { import Other._; f() } } }\n"},
		{"explicit import between", "object O { def m = { def f() = 1; { import Other.f; f() } } }\n"},
		{"Scala 3 wildcard import between", "object O:\n  def m =\n    def f() = 1\n    locally:\n      import Other.*\n      f()\n"},
		{"inherited member of an anonymous class", "object O { def m = { def f() = 1; new Base { def g = f() } } }\n"},
		{"inherited member of a local class", "object O { def m = { def f() = 1; class L extends Base { def g = f() }; 0 } }\n"},
		{"member of a local object", "object O { def m = { def f() = 1; object L { def g = f() }; 0 } }\n"},
		{"used as a value", "object O { def m = { def f(i: Int) = i; List(1).map(f); f(1) } }\n"},
		{"eta-expanded", "object O { def m = { def f(i: Int) = i; val g = f _; f(1) } }\n"},
		{"named argument", "object O { def m = { def f(i: Int) = i; h(f = 1); f(1) } }\n"},
		{"interpolated", "object O { def m = { def f(i: Int) = i; s\"$f\"; f(1) } }\n"},
		{"type arguments", "object O { def m = { def f[T](t: T) = t; f[Int](1) } }\n"},
		{"quoted", "object O { def m(using Quotes) = { def f() = 1; '{ f() } } }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scalaEdges(parseScala(t, "Refuse.scala", tc.src)); len(got) != 0 {
				t.Fatalf("edges = %q, want none", got)
			}
		})
	}
	// Member calls, receiver calls and apply on an object never bind,
	// whatever the members are.
	const members = `object O {
  def f(): Int = 1
  def g: Int = f() + this.f() + O.f() + O()
  def apply(): Int = 2
}
class C extends Base { def h = f() }
trait T { def f(): Int; def k = f() }
extension (s: String) def twice: String = s + s
def top(): Int = 1
def callsTop(): Int = top() + "a".twice
`
	if got := scalaEdges(parseScala(t, "Members.scala", members)); len(got) != 0 {
		t.Fatalf("member edges = %q, want none", got)
	}
}

// A damaged file records no local def and proves no call, wherever the
// damage is: a parse error, an indentation layout the grammar accepts, or a
// keyword the grammar took for an identifier.
func TestScalaLocalFunctionsNeedACleanFile(t *testing.T) {
	const clean = "object O {\n  def m = { def f() = 1; f() }\n}\n"
	if got := scalaEdges(parseScala(t, "Clean.scala", clean)); len(got) != 1 {
		t.Fatalf("clean edges = %q", got)
	}
	for _, src := range []string{
		clean + "object Broken { val = }\n",
		"object Broken { val = }\n" + clean,
		"object A:\n  def m =\n    def f() = 1\n    f()\n   def n = 2\n",
		"object A:\n  def m =\n    def f() = 1\n     f()\n",
		"object Indented:\n  def a = 1  class Deep:\n    def d = 2\n    def e = d()\n",
	} {
		pf := parseScala(t, "Damaged.scala", src)
		if got := scalaEdges(pf); len(got) != 0 {
			t.Errorf("%q: edges = %q, want none", src, got)
		}
		for _, s := range pf.Symbols {
			if strings.HasPrefix(s.StableKey, "func:scala:local:") {
				t.Errorf("%q: recorded local %s", src, s.StableKey)
			}
		}
	}
}

// Damage to a file with proven local calls may lose edges and local defs,
// never add one: every edge and local def of a damaged variant, mapped back
// to the clean source's byte offsets, is one the clean source has.
func TestScalaLocalFunctionDamageNeverAddsBindings(t *testing.T) {
	const src = `package a

object Outer:
  def run(n: Int): Int =
    def helper(i: Int): Int = i * 2
    def twice(i: Int) =
      helper(helper(i))
    twice(n)

  def other(x: Int) =
    x match
      case 0 =>
        def z() = 1
        z()
      case _ => helper(x)

object Braced {
  def sum(xs: List[Int]): Int = {
    def go(rest: List[Int], acc: Int): Int = rest match {
      case Nil => acc
      case h :: t => go(t, acc + h)
    }
    go(xs, 0)
  }
  def helper(i: Int) = i
}
`
	type edit struct {
		what       string
		at, del    int
		ins        string
		damagedSrc string
	}
	var edits []edit
	add := func(what string, at, del int, ins string) {
		edits = append(edits, edit{what, at, del, ins, src[:at] + ins + src[at+del:]})
	}
	ident := func(b byte) bool {
		return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	for i := range len(src) {
		if !ident(src[i]) {
			add(fmt.Sprintf("deleting byte %d (%q)", i, src[i]), i, 1, "")
		}
		if i > 0 && ident(src[i-1]) && ident(src[i]) {
			continue
		}
		for _, ins := range []string{"{", "}", "(", ")", "\n", ":", " "} {
			add(fmt.Sprintf("inserting %q at %d", ins, i), i, 0, ins)
		}
		if i == 0 || src[i-1] == '\n' {
			add(fmt.Sprintf("indenting line at %d", i), i, 0, "  ")
			for _, kw := range []string{"end ", "val ", "import x._\n"} {
				add(fmt.Sprintf("inserting %q at line start %d", kw, i), i, 0, kw)
			}
		}
	}
	type binding struct{ call, def int }
	offsets := func(content string) []int {
		starts := []int{0}
		for i := range len(content) {
			if content[i] == '\n' {
				starts = append(starts, i+1)
			}
		}
		return starts
	}
	byteAt := func(starts []int, line, col int) int { return starts[line-1] + col - 1 }
	collect := func(content string, back func(int) int) (map[binding]bool, map[int]bool, error) {
		pf, err := NewScala().Parse(context.Background(), "F.scala", []byte(content))
		if err != nil {
			return nil, nil, err
		}
		starts := offsets(content)
		edges, locals := map[binding]bool{}, map[int]bool{}
		for _, e := range pf.Edges {
			pos := strings.Split(strings.TrimPrefix(e.Evidence, graph.ScalaLocalFunctionEvidence), ":")
			l, _ := strconv.Atoi(pos[0])
			c, _ := strconv.Atoi(pos[1])
			edges[binding{back(byteAt(starts, e.Line, e.Col)), back(byteAt(starts, l, c))}] = true
		}
		for _, s := range pf.Symbols {
			if strings.HasPrefix(s.StableKey, "func:scala:local:") {
				locals[back(byteAt(starts, s.Range.StartLine, s.Range.StartCol))] = true
			}
		}
		return edges, locals, nil
	}
	cleanEdges, cleanLocals, err := collect(src, func(o int) int { return o })
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanEdges) != 6 || len(cleanLocals) != 4 {
		t.Fatalf("clean source: %d edges, %d locals; want 6 and 4", len(cleanEdges), len(cleanLocals))
	}
	var mu sync.Mutex
	failures := 0
	check := func(e edit) {
		back := func(o int) int {
			switch {
			case o < e.at:
				return o
			case o < e.at+len(e.ins):
				return -1 // inside the inserted text
			default:
				return o - len(e.ins) + e.del
			}
		}
		edges, locals, err := collect(e.damagedSrc, back)
		if err != nil {
			t.Error(err)
			return
		}
		var wrong []string
		for b := range edges {
			if !cleanEdges[b] {
				wrong = append(wrong, fmt.Sprintf("edge %d->%d", b.call, b.def))
			}
		}
		for l := range locals {
			if !cleanLocals[l] {
				wrong = append(wrong, fmt.Sprintf("local %d", l))
			}
		}
		if len(wrong) != 0 {
			mu.Lock()
			failures++
			mu.Unlock()
			t.Errorf("%s yields %v", e.what, wrong)
		}
	}
	var wg sync.WaitGroup
	workers := runtime.GOMAXPROCS(0)
	for w := range workers {
		wg.Go(func() {
			for j := w; j < len(edits); j += workers {
				check(edits[j])
			}
		})
	}
	wg.Wait()
	if failures != 0 {
		t.Logf("%d of %d edits add a binding", failures, len(edits))
	}
}
