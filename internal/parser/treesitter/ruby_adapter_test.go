//go:build cgo

package treesitter

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestRubySemanticScopeFacts(t *testing.T) {
	const src = `module App
  module Services
    class Service
      def run
        helper()
      end
      def self.build
        self.prepare()
      end
      class << self
        def prepare
        end
      end
    end
  end
end
`
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		container string
		static    bool
		key       string
	}{
		"App":                          {"", false, "type:ruby:App"},
		"App.Services":                 {"App", false, "type:ruby:App.Services"},
		"App.Services.Service":         {"App.Services", false, "type:ruby:App.Services.Service"},
		"App.Services.Service.run":     {"App.Services.Service", false, "func:ruby:App.Services.Service:instance:run"},
		"App.Services.Service.build":   {"App.Services.Service", true, "func:ruby:App.Services.Service:singleton:build"},
		"App.Services.Service.prepare": {"App.Services.Service", true, "func:ruby:App.Services.Service:singleton:prepare"},
	}
	got := map[string]struct {
		container string
		static    bool
		key       string
	}{}
	for _, s := range p.Symbols {
		if s.Static == nil {
			got[s.QualifiedName] = struct {
				container string
				static    bool
				key       string
			}{s.ContainerName, false, s.StableKey}
			continue
		}
		got[s.QualifiedName] = struct {
			container string
			static    bool
			key       string
		}{s.ContainerName, *s.Static, s.StableKey}
	}
	for q, expected := range want {
		if got[q] != expected {
			t.Errorf("%s = %#v, want %#v", q, got[q], expected)
		}
	}
	parents := map[string]string{}
	for _, fact := range p.Scope.Imports {
		if fact.Kind == graph.ScopeImportRubyLexicalParent {
			parents[fact.OwnerModule] = fact.SourceSpecifier
		}
	}
	if parents["App"] != "" || parents["App.Services"] != "App" || parents["App.Services.Service"] != "App.Services" {
		t.Fatalf("lexical parents = %#v", parents)
	}
	for _, e := range p.Edges {
		if e.DstName != "helper" && e.DstName != "self.prepare" {
			continue
		}
		if e.DstName == "helper" && e.Evidence != "ruby:implicit_receiver" || e.DstName == "self.prepare" && e.Evidence != "ruby:self_receiver" {
			t.Errorf("edge %#v", e)
		}
	}
}

func TestRubyReceiverSpellingAndFacts(t *testing.T) {
	const src = `run()
self.run()
Service.run()
A::B.run()
Service::run()
obj.run()
obj&.run()
factory.service.run()
`
	p, err := NewRuby().Parse(context.Background(), "calls.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"run": "ruby:implicit_receiver", "self.run": "ruby:self_receiver", "Service.run": "ruby:constant_receiver",
		"A::B.run": "ruby:constant_receiver", "Service::run": "ruby:constant_receiver", "obj.run": "ruby:value_receiver",
		"obj&.run": "ruby:safe_navigation", "factory.service.run": "ruby:chained_receiver",
	}
	got := map[string]string{}
	for _, e := range p.Edges {
		got[e.DstName] = e.Evidence
	}
	for name, evidence := range want {
		if got[name] != evidence {
			t.Errorf("%s evidence=%q, want %q", name, got[name], evidence)
		}
	}
	for _, r := range p.References {
		if want[r.Name] != "" && r.QualifiedName != r.Name {
			t.Errorf("reference %#v", r)
		}
	}
}

func TestRubySingletonInstanceCollisionAndQualifiedDisposition(t *testing.T) {
	const src = `class Service
  def run; end
  def self.run; end
end
class A::B; end
module X
  class A::B; end
end
def run; end
`
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, s := range p.Symbols {
		keys[s.StableKey] = true
		if s.QualifiedName == "X.A.B" {
			t.Fatal("ambiguous relative qualified declaration attributed to X")
		}
	}
	if !keys["func:ruby:Service:instance:run"] || !keys["func:ruby:Service:singleton:run"] {
		t.Fatalf("collision keys missing: %#v", keys)
	}
	for _, s := range p.Symbols {
		if s.QualifiedName == "run" && s.ContainerName != "" {
			t.Fatalf("top-level method owned by %q", s.ContainerName)
		}
	}
}

func TestRubyProfileV2(t *testing.T) {
	if got := NewRuby().Profile(); got.ID != "treesitter:ruby:v2" || !got.EmitsCallEdges {
		t.Fatalf("profile=%+v", got)
	}
}

func TestRubyNestedSingletonScopesFailClosed(t *testing.T) {
	const src = `class Service
  class << self
    def normal; end
    def self.meta; end
    class << self
      def deeper; end
    end
  end
end
`
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range p.Symbols {
		got[s.QualifiedName] = true
	}
	if !got["Service.normal"] {
		t.Fatal("ordinary method inside class << self omitted")
	}
	if got["Service.meta"] || got["Service.deeper"] {
		t.Fatalf("nested singleton methods leaked: %#v", got)
	}
}

func TestRubyNestedSingletonPreservesSiblings(t *testing.T) {
	const src = `class Service
  class << self
    def before; end
    class << self
      def deeper; end
    end
    def after; end
  end
end
`
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range p.Symbols {
		got[s.QualifiedName] = true
	}
	for _, name := range []string{"Service.before", "Service.after"} {
		if !got[name] {
			t.Fatalf("sibling %s omitted: %#v", name, got)
		}
	}
	if got["Service.deeper"] {
		t.Fatalf("nested singleton method leaked: %#v", got)
	}
}

// `self.name = v` parses as an assignment whose left is a `call` node spelled
// `self.name`, but the method it invokes is the writer `name=`. Emitting the
// reader's spelling would let the resolver bind a syntax-proven self receiver
// to the wrong method, so no call edge is emitted for it at all. A read of the
// same attribute in the same file still is.
func TestRubyAssignmentTargetIsNotACall(t *testing.T) {
	const src = `class Service
  def name; @name; end
  def name=(v); @name = v; end

  def rename(v)
    self.name = v
    self.count += 1
    other = self.name
  end
end
`
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	for _, e := range p.Edges {
		if e.DstName != "self.name" && e.DstName != "self.count" {
			t.Errorf("unexpected edge %#v", e)
		}
		reads++
	}
	// `self.name = v` and `self.count += 1` are writes; only the read remains.
	if reads != 1 {
		t.Errorf("emitted %d self edges, want 1 (the read)", reads)
	}
	// The write site is still a real occurrence: only the call edge is dropped.
	refs := 0
	for _, r := range p.References {
		if r.Name == "self.name" || r.Name == "self.count" {
			refs++
		}
	}
	if refs != 3 {
		t.Errorf("self references = %d, want 3 (two writes and one read)", refs)
	}
}

// A `def` inside a block belongs to whatever object the block builds, which is
// not syntax-proven. It is not a symbol, so its body would otherwise be
// attributed to the enclosing method and its calls bound to that method's
// container. Fail closed: emit no call edges from inside it. A block that only
// calls, with no nested `def`, keeps its enclosing method's lexical self.
func TestRubyBlockLocalDefinitionEmitsNoCalls(t *testing.T) {
	const src = `class Service
  def run; end

  def build
    Class.new do
      def run; end
      def go; run(); end
    end
  end

  def lexical
    lambda { run() }
  end
end
`
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, e := range p.Edges {
		if e.DstName == "run" {
			lines = append(lines, e.Line)
		}
	}
	if len(lines) != 1 || lines[0] != 12 {
		t.Fatalf("bare run() edges at lines %v, want only the lambda at line 12", lines)
	}
}

// A block passed to instance_eval and friends runs with another object as
// `self`, so a receiver-less call inside it is not the enclosing method's
// receiver and no receiver evidence may be claimed for it. An ordinary block
// keeps the lexical self and still emits its call.
func TestRubySelfRebindingBlocksEmitNoImplicitCalls(t *testing.T) {
	const src = `class Service
  def run; end

  def rebound(other)
    other.instance_eval { run() }
    other.class_eval do
      run()
    end
  end

  def lexical(items)
    items.each { run() }
  end
end
`
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, e := range p.Edges {
		if e.DstName == "run" {
			lines = append(lines, e.Line)
		}
	}
	if len(lines) != 1 || lines[0] != 12 {
		t.Fatalf("bare run() edges at lines %v, want only the each block at line 12", lines)
	}
}

// `Class.new do ... end` evaluates its block against the anonymous class it
// builds, so `self` inside is that class, not the enclosing receiver. A bare
// call there has no receiver the parser can vouch for.
func TestRubyAnonymousClassBlockEmitsNoImplicitCalls(t *testing.T) {
	const src = `class Service
  def run; end

  def build
    Class.new do
      run()
    end
    Struct.new(:a) do
      run()
    end
  end
end
`
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p.Edges {
		if e.DstName == "run" {
			t.Fatalf("anonymous-class block emitted a call edge: %#v", e)
		}
	}
}

// A multiple assignment nests its targets under a left_assignment_list, which
// must not smuggle the reader spelling past the assignment-target guard.
func TestRubyMultipleAssignmentTargetsAreNotCalls(t *testing.T) {
	const src = `class Service
  def a; @a; end
  def b; @b; end

  def rename
    self.a, self.b = 1, 2
  end
end
`
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p.Edges {
		t.Fatalf("multiple-assignment target emitted a call edge: %#v", e)
	}
}
