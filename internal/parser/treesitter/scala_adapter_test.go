//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	scalagrammar "github.com/smacker/go-tree-sitter/scala"

	"github.com/isink17/codegraph/internal/graph"
)

func parseScala(t *testing.T, path, src string) graph.ParsedFile {
	t.Helper()
	pf, err := NewScala().Parse(context.Background(), path, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Edges) != 0 {
		t.Fatalf("Scala produced call edges: %+v", pf.Edges)
	}
	return pf
}

// scalaKeys renders each symbol as "kind stable_key".
func scalaKeys(pf graph.ParsedFile) []string {
	out := make([]string, 0, len(pf.Symbols))
	for _, s := range pf.Symbols {
		out = append(out, s.Kind+" "+s.StableKey)
	}
	sort.Strings(out)
	return out
}

func scalaRefs(pf graph.ParsedFile) []string {
	out := make([]string, 0, len(pf.References))
	for _, r := range pf.References {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

func TestScalaAdapterDeclarations(t *testing.T) {
	const src = `package com.acme
package billing

/** Money in minor units. */
case class Money(cents: Long)

class Invoice(val id: String) extends Entity with Auditable {
  private val lines = Vector.empty[Money]
  var status: String = "open"
  type Total = Long
  def total: Long = 0
  object Rules { def strict = true }
  class Line(amount: Money)
}

object Invoice {
  def apply(id: String): Invoice = new Invoice(id)
  implicit val ordering: Ordering[Invoice] = Ordering.by(_.id)
}

trait Auditable { def audit(): Unit; val actor: String }

sealed trait State
case object Draft extends State

enum Currency { case Eur, Usd; def code: String = toString }

def topLevel(x: Int): Int = x
`
	pf := parseScala(t, "Invoice.scala", src)
	want := []string{
		"class type:scala:com.acme.billing.Invoice",
		"class type:scala:com.acme.billing.Invoice.Line",
		"class type:scala:com.acme.billing.Money",
		"enum type:scala:com.acme.billing.Currency",
		"function func:scala:com.acme.billing.Auditable.audit",
		"function func:scala:com.acme.billing.Currency.code",
		"function func:scala:com.acme.billing.Invoice$.apply",
		"function func:scala:com.acme.billing.Invoice.Rules$.strict",
		"function func:scala:com.acme.billing.Invoice.total",
		"function func:scala:com.acme.billing.topLevel",
		"object object:scala:com.acme.billing.Draft",
		"object object:scala:com.acme.billing.Invoice",
		"object object:scala:com.acme.billing.Invoice.Rules",
		"trait type:scala:com.acme.billing.Auditable",
		"trait type:scala:com.acme.billing.State",
		"type type:scala:com.acme.billing.Invoice.Total",
		"value value:scala:com.acme.billing.Auditable.actor",
		"value value:scala:com.acme.billing.Invoice$.ordering",
		"value value:scala:com.acme.billing.Invoice.lines",
		"value value:scala:com.acme.billing.Invoice.status",
	}
	if got := scalaKeys(pf); !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols:\n got %q\nwant %q", got, want)
	}
	for _, s := range pf.Symbols {
		switch s.StableKey {
		case "type:scala:com.acme.billing.Money":
			if !strings.Contains(s.DocSummary, "Money in minor units") {
				t.Errorf("doc = %q", s.DocSummary)
			}
			if s.Signature != "case class Money(cents: Long)" {
				t.Errorf("signature = %q", s.Signature)
			}
		case "func:scala:com.acme.billing.Invoice$.apply":
			if s.QualifiedName != "com.acme.billing.Invoice.apply" || s.ContainerName != "com.acme.billing.Invoice" {
				t.Errorf("companion member = %+v", s)
			}
		case "value:scala:com.acme.billing.Invoice.lines":
			if s.Visibility != "private" {
				t.Errorf("visibility = %q", s.Visibility)
			}
		case "func:scala:com.acme.billing.topLevel":
			if s.ContainerName != "com.acme.billing" || s.Signature != "def topLevel(x: Int): Int" {
				t.Errorf("top-level def = %+v", s)
			}
		}
	}
}

func TestScalaAdapterPackageBodiesAndLocals(t *testing.T) {
	const src = `package a.b {
  object Y {
    def g() = { def local() = 1; val tmp = local(); tmp }
  }
}
package d { class Z }
class Top
`
	pf := parseScala(t, "Pkg.scala", src)
	want := []string{
		"class type:scala:Top",
		"class type:scala:d.Z",
		"function func:scala:a.b.Y$.g",
		"object object:scala:a.b.Y",
	}
	if got := scalaKeys(pf); !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols = %q, want %q (local defs and vals are not members)", got, want)
	}
}

func TestScalaAdapterOverloadsShareKeyWithDistinctSignatures(t *testing.T) {
	const src = `object Fmt {
  def show(x: Int): String = x.toString
  def show(x: String): String = x
  def show(x: Int, width: Int): String = show(x)
}
`
	first := parseScala(t, "Fmt.scala", src)
	second := parseScala(t, "Fmt.scala", src)
	if !reflect.DeepEqual(first.Symbols, second.Symbols) {
		t.Fatal("overload identities are not stable across parses")
	}
	var sigs []string
	positions := map[[2]int]bool{}
	for _, s := range first.Symbols {
		if s.Name != "show" {
			continue
		}
		if s.StableKey != "func:scala:Fmt$.show" {
			t.Fatalf("overload key = %q", s.StableKey)
		}
		sigs = append(sigs, s.Signature)
		positions[[2]int{s.Range.StartLine, s.Range.StartCol}] = true
	}
	want := []string{"def show(x: Int): String", "def show(x: String): String", "def show(x: Int, width: Int): String"}
	if !reflect.DeepEqual(sigs, want) || len(positions) != 3 {
		t.Fatalf("overload signatures = %q positions = %v", sigs, positions)
	}
}

func TestScalaAdapterImports(t *testing.T) {
	const src = `import scala.collection.mutable
import java.util.{List => JList, Map}
import a.b._
import c.d.{e, f => _, _}
import x.y.given
import p.q.*
import r.s.{T as U}
import k.l, m.n
import g.h.{given Ord[?], Z}
class C { import inner.Thing; def m = 1 }
`
	pf := parseScala(t, "Imports.scala", src)
	wantImports := []string{
		"scala.collection.mutable", "java.util.List", "java.util.Map", "a.b._",
		"c.d.e", "c.d._", "x.y.given", "p.q._", "r.s.T", "k.l", "m.n", "g.h.Z", "inner.Thing",
	}
	if !reflect.DeepEqual(pf.Imports, wantImports) {
		t.Fatalf("imports:\n got %q\nwant %q", pf.Imports, wantImports)
	}
	aliases := map[string]string{}
	wildcards := []string{}
	for _, imp := range pf.Scope.Imports {
		if imp.Wildcard {
			wildcards = append(wildcards, imp.SourceSpecifier)
			continue
		}
		aliases[imp.SourceSpecifier+"."+imp.ImportedName] = imp.LocalName
	}
	if aliases["java.util.List"] != "JList" || aliases["r.s.T"] != "U" || aliases["java.util.Map"] != "Map" {
		t.Fatalf("aliases = %v", aliases)
	}
	if _, hidden := aliases["c.d.f"]; hidden {
		t.Fatal("hidden selector `f => _` recorded as an import")
	}
	if !reflect.DeepEqual(wildcards, []string{"a.b", "c.d", "p.q"}) {
		t.Fatalf("wildcards = %q (a given import is not a name wildcard)", wildcards)
	}
}

// Shadowing, lambdas, implicits, givens and extension methods are exactly the
// cases where a by-name binding would be wrong, so every call stays a bare
// reference and no edge exists.
func TestScalaAdapterUncertainCallsStayReferences(t *testing.T) {
	const src = `object Shadow {
  def helper(): Int = 1
  def run(helper: () => Int): Int = helper()
  def local(): Int = { def helper(): Int = 2; helper() }
  val inc = (x: Int) => x + 1
  def lambdas(xs: List[Int]) = xs.map(inc).foreach(println)
  implicit class RichInt(i: Int) { def twice: Int = i * 2 }
  given intOrd: Ordering[Int] = Ordering.Int
  given Ordering[String] = Ordering.String
  extension (s: String) def shout: String = s.toUpperCase
  def uses(): Unit = { sorted(List(3, 1)); Shadow.helper(); obj.member.call[Int](1) }
}
`
	pf := parseScala(t, "Shadow.scala", src)
	want := []string{"List", "Shadow.helper", "foreach", "helper", "helper", "obj.member.call", "sorted", "xs.map"}
	if got := scalaRefs(pf); !reflect.DeepEqual(got, want) {
		t.Fatalf("references = %q, want %q", got, want)
	}
	keys := scalaKeys(pf)
	for _, k := range []string{
		"class type:scala:Shadow$.RichInt",
		"function func:scala:Shadow$.shout",
		"function func:scala:Shadow$.RichInt.twice",
		"value value:scala:Shadow$.intOrd",
		"value value:scala:Shadow$.inc",
	} {
		if !slices.Contains(keys, k) {
			t.Errorf("missing %q in %q", k, keys)
		}
	}
}

func TestScalaAdapterSyntaxErrorDropsRecoveredDeclarations(t *testing.T) {
	const src = `import a.b.C
object Before { def ok() = 1 }
class Broken( { def inside() = 1
import x.y.Z
object After { def fine() = 2 }
`
	pf := parseScala(t, "Broken.scala", src)
	// Only what ends before the first error is recorded: Broken's extent is
	// unknown, and everything after it, imports included, is dropped.
	want := []string{"function func:scala:Before$.ok", "object object:scala:Before"}
	if got := scalaKeys(pf); !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols = %q, want only the clean declarations %q", got, want)
	}
	if !reflect.DeepEqual(pf.Imports, []string{"a.b.C"}) {
		t.Fatalf("imports = %q, want only the one before the error", pf.Imports)
	}
	if bad := parseScala(t, "Pkg.scala", "package a.b.\nclass X\n"); len(bad.Symbols) != 0 {
		t.Fatalf("declarations under a broken package clause = %q", scalaKeys(bad))
	}
	// A missing `{` lets the next `}` close Circle early, so area and k would
	// land at the top level before the only error, the excess `}`. No owner
	// is known then; nothing is recorded, though file-scoped imports are.
	stray := parseScala(t, "Stray.scala", "import a.b.C\nobject Ok { def fine() = 1 }\nclass Circle(r: Double)\n  def area = r * r\n  val k = 1\n}\nclass Last\n")
	if len(stray.Symbols) != 0 || !reflect.DeepEqual(stray.Imports, []string{"a.b.C"}) {
		t.Fatalf("missing brace: symbols = %q, imports = %q", scalaKeys(stray), stray.Imports)
	}
}

func TestScalaAdapterTopLevelStatementsAndExtensions(t *testing.T) {
	a := NewScala()
	for path, want := range map[string]bool{"A.scala": true, "build.sc": false, "B.SCALA": true, "a.sbt": false, "a.kt": false} {
		if a.Supports(path) != want {
			t.Errorf("Supports(%q) = %v", path, !want)
		}
	}
	pf := parseScala(t, "Count.scala", `import scala.io.Source
val lines = Source.fromFile("x").getLines().toList
def count(xs: List[String]) = xs.size
println(count(lines))
`)
	if got := scalaKeys(pf); !reflect.DeepEqual(got, []string{"function func:scala:count", "value value:scala:lines"}) {
		t.Fatalf("script symbols = %q", got)
	}
	if got := scalaRefs(pf); !reflect.DeepEqual(got, []string{"Source.fromFile", "count", "getLines", "println"}) {
		t.Fatalf("script references = %q", got)
	}
}

// A small service in the shape of a typical Scala application: a repository
// trait, an implementation with a companion, a domain ADT and a controller.
func TestScalaAdapterRealisticSample(t *testing.T) {
	const src = `package io.shop.orders

import scala.concurrent.{ExecutionContext, Future}
import io.shop.common.Clock
import io.shop.db.{Database => Db}

sealed trait OrderEvent
object OrderEvent {
  final case class Placed(id: OrderId, total: BigDecimal) extends OrderEvent
  final case class Cancelled(id: OrderId, reason: String) extends OrderEvent
}

final case class OrderId(value: String) extends AnyVal

trait OrderRepository {
  def find(id: OrderId): Future[Option[Order]]
  def save(order: Order): Future[Unit]
}

class SqlOrderRepository(db: Db)(implicit ec: ExecutionContext) extends OrderRepository {
  override def find(id: OrderId): Future[Option[Order]] =
    db.run(OrderQueries.byId(id)).map(_.headOption)

  override def save(order: Order): Future[Unit] =
    db.run(OrderQueries.upsert(order)).map(_ => ())
}

object SqlOrderRepository {
  def apply(db: Db)(implicit ec: ExecutionContext): SqlOrderRepository = new SqlOrderRepository(db)
}

class OrderService(repo: OrderRepository, clock: Clock)(implicit ec: ExecutionContext) {
  def cancel(id: OrderId, reason: String): Future[Either[String, OrderEvent]] =
    repo.find(id).flatMap {
      case Some(order) if order.isOpen =>
        repo.save(order.copy(closedAt = Some(clock.now()))).map(_ => Right(OrderEvent.Cancelled(id, reason)))
      case Some(_) => Future.successful(Left("already closed"))
      case None    => Future.successful(Left(s"unknown order ${id.value}"))
    }
}
`
	pf := parseScala(t, "OrderService.scala", src)
	if len(pf.Symbols) != 15 {
		t.Fatalf("symbols = %d: %q", len(pf.Symbols), scalaKeys(pf))
	}
	for _, k := range []string{
		"class type:scala:io.shop.orders.OrderEvent$.Placed",
		"function func:scala:io.shop.orders.SqlOrderRepository$.apply",
		"function func:scala:io.shop.orders.SqlOrderRepository.find",
		"function func:scala:io.shop.orders.OrderService.cancel",
		"object object:scala:io.shop.orders.SqlOrderRepository",
	} {
		if !slices.Contains(scalaKeys(pf), k) {
			t.Errorf("missing %q", k)
		}
	}
	if !slices.Contains(pf.Imports, "io.shop.db.Database") || !slices.Contains(pf.Imports, "scala.concurrent.Future") {
		t.Fatalf("imports = %q", pf.Imports)
	}
	refs := scalaRefs(pf)
	for _, name := range []string{"repo.find", "repo.save", "OrderEvent.Cancelled", "Future.successful", "clock.now", "OrderQueries.byId", "db.run"} {
		if !slices.Contains(refs, name) {
			t.Errorf("missing reference %q in %q", name, refs)
		}
	}
}

func TestScalaAdapterMultiMethodExtensions(t *testing.T) {
	const src = `object Ext {
  extension (s: String)
    def shout: String = s.toUpperCase
    def whisper: String = s.toLowerCase

  extension (n: Int) {
    def double: Int = n * 2
    def half: Int = n / 2
  }
}
`
	want := []string{
		"function func:scala:Ext$.double",
		"function func:scala:Ext$.half",
		"function func:scala:Ext$.shout",
		"function func:scala:Ext$.whisper",
		"object object:scala:Ext",
	}
	if got := scalaKeys(parseScala(t, "Ext.scala", src)); !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols = %q, want %q", got, want)
	}
}

func TestScalaAdapterUnbracedRenameImport(t *testing.T) {
	pf := parseScala(t, "Rename.scala", "import p.q as r\nimport a.b.c as d\nimport x.y as _\n")
	if !reflect.DeepEqual(pf.Imports, []string{"p.q", "a.b.c"}) {
		t.Fatalf("imports = %q", pf.Imports)
	}
	want := []graph.ScopeImport{
		{SourceSpecifier: "p", ImportedName: "q", LocalName: "r", Kind: graph.ScopeImportNamed},
		{SourceSpecifier: "a.b", ImportedName: "c", LocalName: "d", Kind: graph.ScopeImportNamed},
	}
	if !reflect.DeepEqual(pf.Scope.Imports, want) {
		t.Fatalf("scope imports = %+v", pf.Scope.Imports)
	}
}

// Damaging a file by one byte must never give a declaration a different owner
// or invent a name: every symbol and import the damaged file yields is one
// the intact file declares, symbols with the same kind and owner. Edits are
// single-byte deletions and insertions of a brace, a parenthesis, a newline
// or `class `. Edits that only rename an identifier (inside one, or joining
// two) are skipped, as is any edit that leaves a valid program, which
// declares what it spells.
func TestScalaAdapterDamageNeverMisattributes(t *testing.T) {
	const src = `package com.acme
package billing

import scala.util.{Try, Success => Ok}

/** Docs. */
sealed trait Shape { def area: Double }
case class Circle(r: Double) extends Shape {
  def area: Double = r * r
  val k, m = 1
}
object Circle {
  def unit(): Circle = Circle(1)
  type Id = Int
}
class Box[T](x: T) {
  import scala.collection.mutable.Buffer
  private def get(): T = { helper(x); x }
  def this() = this(null)
}
enum Color { case Red, Green; def paint() = 1 }
object Ext {
  extension (s: String) {
    def twice: String = s + s
    def thrice: String = s * 3
  }
  given ord: Ordering[Int] = Ordering.Int
}
package inner {
  class Nested { def deep() = 1 }
}
def helper(n: Any): Unit = println(n)
val top = 1
class Last { def end() = 2 }
import java.time.{Clock as C, _}
object Indented:
  def a = 1
  class Deep:
    def d = 2
  def b = 3
end Indented
`
	key := func(s graph.Symbol) string { return s.Kind + " " + s.StableKey + " @" + s.ContainerName }
	imp := func(i graph.ScopeImport) string {
		return fmt.Sprintf("import %s %s as %s %s %v", i.SourceSpecifier, i.ImportedName, i.LocalName, i.Kind, i.Wildcard)
	}
	clean := map[string]bool{}
	cleanPF := parseScala(t, "Clean.scala", src)
	for _, s := range cleanPF.Symbols {
		clean[key(s)] = true
	}
	for _, i := range cleanPF.Scope.Imports {
		clean[imp(i)] = true
	}
	ident := func(b byte) bool {
		return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	type edit struct{ what, src string }
	var edits []edit
	for i := range len(src) {
		if !ident(src[i]) && !(i > 0 && i+1 < len(src) && ident(src[i-1]) && ident(src[i+1])) {
			edits = append(edits, edit{fmt.Sprintf("deleting byte %d (%q)", i, src[i]), src[:i] + src[i+1:]})
		}
		if i > 0 && ident(src[i-1]) && ident(src[i]) {
			continue // an insertion inside an identifier only splits it
		}
		for _, ins := range []string{"{", "}", "(", ")", "\n", "class "} {
			if ins == "class " && i > 0 && ident(src[i-1]) {
				continue // "class" would extend the identifier before it
			}
			edits = append(edits, edit{fmt.Sprintf("inserting %q at %d", ins, i), src[:i] + ins + src[i:]})
		}
	}
	check := func(e edit) {
		pf, err := NewScala().Parse(context.Background(), "Damaged.scala", []byte(e.src))
		if err != nil {
			t.Error(err)
			return
		}
		var wrong []string
		for _, s := range pf.Symbols {
			if k := key(s); !clean[k] {
				wrong = append(wrong, k)
			}
		}
		for _, i := range pf.Scope.Imports {
			if k := imp(i); !clean[k] {
				wrong = append(wrong, k)
			}
		}
		if len(wrong) == 0 {
			return
		}
		if root, err := parse(context.Background(), scalagrammar.GetLanguage(), []byte(e.src)); err == nil && !root.HasError() {
			return // a valid program declares what it spells
		}
		t.Errorf("%s yields %q", e.what, wrong)
	}
	// Each parse is independent; spreading them keeps the test under a second.
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
}
