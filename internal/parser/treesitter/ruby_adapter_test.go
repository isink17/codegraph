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

func TestRubyProfileV4(t *testing.T) {
	if got := NewRuby().Profile(); got.ID != "treesitter:ruby:v4" || !got.EmitsCallEdges {
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

// rubyVisibilityFacts renders the singleton-visibility evidence of one parse as
// "owner.name=specifier" entries plus "owner=?" for each owner-level hazard.
func rubyVisibilityFacts(p graph.ParsedFile) map[string]bool {
	got := map[string]bool{}
	for _, fact := range p.Scope.Imports {
		switch fact.Kind {
		case graph.ScopeImportRubySingletonVisibility:
			if !fact.Static {
				continue
			}
			got[fact.OwnerModule+"."+fact.LocalName+"="+fact.SourceSpecifier] = true
		case graph.ScopeImportRubySingletonVisibilityUnknown:
			got[fact.OwnerModule+"=?"] = true
		}
	}
	return got
}

func rubySingletonVisibility(p graph.ParsedFile) map[string]string {
	got := map[string]string{}
	for _, s := range p.Symbols {
		if s.Kind == "function" && s.Static != nil && *s.Static {
			got[s.QualifiedName] = s.Visibility
		}
	}
	return got
}

func parseRuby(t *testing.T, src string) graph.ParsedFile {
	t.Helper()
	p, err := NewRuby().Parse(context.Background(), "service.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A singleton method's visibility is public by default, and a bare toggle in a
// `class << self` body moves that default for the definitions that follow it.
func TestRubySingletonVisibilityDefaultAndEigenclassState(t *testing.T) {
	p := parseRuby(t, `class Service
  def self.a; end
  class << self
    def b; end
    private
    def c; end
    protected
    def d; end
    public
    def e; end
  end
end
`)
	want := map[string]string{
		"Service.a": "public",
		"Service.b": "public",
		"Service.c": "private",
		"Service.d": "protected",
		"Service.e": "public",
	}
	got := rubySingletonVisibility(p)
	for q, expected := range want {
		if got[q] != expected {
			t.Errorf("%s visibility = %q, want %q", q, got[q], expected)
		}
	}
	if facts := rubyVisibilityFacts(p); len(facts) != 0 {
		t.Fatalf("bare toggles emitted override facts: %#v", facts)
	}
}

// A bare `private` in an ordinary class body sets the default for that class's
// INSTANCE methods. `def self.run` after it is still public, and so is a
// `class << self` body, which is a different cref with its own state.
func TestRubyClassBodyPrivateDoesNotReachSingletons(t *testing.T) {
	p := parseRuby(t, `class Service
  private

  def self.run; end

  class << self
    def build; end
  end

  def helper; end
end
`)
	got := rubySingletonVisibility(p)
	if got["Service.run"] != "public" || got["Service.build"] != "public" {
		t.Fatalf("singleton visibility = %#v, want both public", got)
	}
	if facts := rubyVisibilityFacts(p); len(facts) != 0 {
		t.Fatalf("class-body private emitted facts: %#v", facts)
	}
	// Instance visibility is not modelled, so it stays unstated rather than
	// claiming a `private` this parser did not track.
	for _, s := range p.Symbols {
		if s.QualifiedName == "Service.helper" && s.Visibility != "" {
			t.Fatalf("instance visibility = %q, want unstated", s.Visibility)
		}
	}
}

// Literal-name overrides are persisted as facts, not folded into the symbol:
// Ruby lets another file make them, so a resolver has to read them per owner.
func TestRubySingletonVisibilityOverrideFacts(t *testing.T) {
	p := parseRuby(t, `module App
  class Service
    def self.a; end
    def self.b; end
    private_class_method :a
    public_class_method :b

    class << self
      def c; end
      def d; end
      def e; end
      private :c
      protected :d
      public :e
    end
  end
end
`)
	want := map[string]bool{
		"App.Service.a=private":   true,
		"App.Service.b=public":    true,
		"App.Service.c=private":   true,
		"App.Service.d=protected": true,
		"App.Service.e=public":    true,
	}
	got := rubyVisibilityFacts(p)
	if len(got) != len(want) {
		t.Fatalf("facts = %#v, want %#v", got, want)
	}
	for key := range want {
		if !got[key] {
			t.Errorf("missing fact %s in %#v", key, got)
		}
	}
	// The override is a fact about the owner, not a rewrite of the definition:
	// the symbol keeps the visibility its definition site proved.
	if vis := rubySingletonVisibility(p); vis["App.Service.a"] != "public" {
		t.Fatalf("App.Service.a visibility = %q, want the definition-site public", vis["App.Service.a"])
	}
}

// A named override with an unprovable argument list, and `module_function` in
// either spelling, withdraw the whole owner instead of naming a method.
func TestRubySingletonVisibilityHazards(t *testing.T) {
	cases := map[string]string{
		"splat":            "private_class_method(*names)",
		"variable":         "private_class_method name",
		"inline def":       "private_class_method def self.x; end",
		"no arguments":     "private_class_method",
		"module_function":  "module_function :helper",
		"bare mod func":    "module_function",
		"interpolated":     `private_class_method "#{prefix}_run"`,
		"eigenclass splat": "class << self\n    private(*names)\n  end",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			p := parseRuby(t, "module App\n  class Service\n    def self.run; end\n    "+line+"\n  end\nend\n")
			got := rubyVisibilityFacts(p)
			if !got["App.Service=?"] {
				t.Fatalf("no owner hazard for %q: %#v", line, got)
			}
			for key := range got {
				if key != "App.Service=?" {
					t.Fatalf("%q also emitted %s", line, key)
				}
			}
		})
	}
}

// `private_class_method` is an ordinary public method of Module, so a literal
// `self` receiver is the same cref as the bare spelling and must be honoured --
// otherwise `Service.run` binds a method that raises.
func TestRubySelfReceiverVisibilityIsTheCref(t *testing.T) {
	p := parseRuby(t, `module App
  class Service
    def self.run; end
    self.private_class_method :run
  end
end
`)
	want := map[string]bool{"App.Service.run=private": true}
	if got := rubyVisibilityFacts(p); len(got) != 1 || !got["App.Service.run=private"] {
		t.Fatalf("facts = %#v, want %#v", got, want)
	}
}

// Any other receiver names an owner this parser has not proven. It still proves
// that SOMETHING changed the owner's singleton visibility, so the owner is
// withdrawn rather than trusted -- including the owner's own constant, where
// the receiver happens to be right but the parser has not established it.
func TestRubyForeignReceiverVisibilityIsAHazard(t *testing.T) {
	for name, line := range map[string]string{
		"foreign constant":      "Other.private_class_method :run",
		"own constant":          "Service.private_class_method :run",
		"eigenclass send":       "singleton_class.send(:private, :run)",
		"eigenclass class_eval": "singleton_class.class_eval { private :run }",
	} {
		t.Run(name, func(t *testing.T) {
			p := parseRuby(t, "module App\n  class Service\n    def self.run; end\n    "+line+"\n  end\nend\n")
			got := rubyVisibilityFacts(p)
			if len(got) != 1 || !got["App.Service=?"] {
				t.Fatalf("%q produced %#v, want the owner hazard alone", line, got)
			}
		})
	}
}

// A receiver-bearing call that is not a visibility operation says nothing.
func TestRubyOrdinaryReceiverCallIsNotAVisibilityFact(t *testing.T) {
	p := parseRuby(t, `module App
  class Service
    def self.run; end
    Other.configure :run
    logger.info "built"
  end
end
`)
	if facts := rubyVisibilityFacts(p); len(facts) != 0 {
		t.Fatalf("ordinary receiver call produced %#v", facts)
	}
}

// The class-method visibility API inside `class << self` names a method of the
// singleton's singleton. Attributing it to the outer owner would invent a fact,
// so the owner is withdrawn instead.
func TestRubyClassMethodVisibilityInsideEigenclassIsAHazard(t *testing.T) {
	p := parseRuby(t, `module App
  class Service
    class << self
      def run; end
      private_class_method :run
    end
  end
end
`)
	got := rubyVisibilityFacts(p)
	if len(got) != 1 || !got["App.Service=?"] {
		t.Fatalf("facts = %#v, want the owner hazard alone", got)
	}
}

// A literal string argument names a method exactly as a symbol does.
func TestRubyStringArgumentVisibilityOverride(t *testing.T) {
	p := parseRuby(t, `module App
  class Service
    def self.run; end
    private_class_method "run"
  end
end
`)
	if got := rubyVisibilityFacts(p); len(got) != 1 || !got["App.Service.run=private"] {
		t.Fatalf("facts = %#v, want App.Service.run=private", got)
	}
}

// Top-level visibility operations name no constant, so they record nothing.
func TestRubyTopLevelVisibilityIgnored(t *testing.T) {
	p := parseRuby(t, "def self.run; end\nprivate_class_method :run\nmodule_function\n")
	if facts := rubyVisibilityFacts(p); len(facts) != 0 {
		t.Fatalf("top level produced %#v", facts)
	}
}

// The constant-receiver call shapes P22.48 resolves keep their exact spelling,
// and the operator is the only thing that differs between them.
func TestRubyConstantReceiverSpellings(t *testing.T) {
	p := parseRuby(t, `module App
  class Caller
    def f
      Service.run()
      Service::run()
      App::Service.run()
      ::Service.run()
      Service.build.run()
    end
  end
end
`)
	want := map[string]string{
		"Service.run":       "ruby:constant_receiver",
		"Service::run":      "ruby:constant_receiver",
		"App::Service.run":  "ruby:constant_receiver",
		"::Service.run":     "ruby:constant_receiver",
		"Service.build.run": "ruby:chained_receiver",
	}
	got := map[string]string{}
	for _, e := range p.Edges {
		got[e.DstName] = e.Evidence
	}
	for name, evidence := range want {
		if got[name] != evidence {
			t.Errorf("%s evidence = %q, want %q", name, got[name], evidence)
		}
	}
}

// `class A::B` pushes exactly one lexical frame. Recording A as a parent of the
// body would invent a nesting Ruby never had, which is why the qualified-open
// form reports the root boundary instead.
func TestRubyQualifiedOpenLexicalParentIsRoot(t *testing.T) {
	p := parseRuby(t, `class App::Caller
  def f
    Service.run()
  end
end
`)
	parents := map[string]string{}
	for _, fact := range p.Scope.Imports {
		if fact.Kind == graph.ScopeImportRubyLexicalParent {
			parents[fact.OwnerModule] = fact.SourceSpecifier
		}
	}
	if len(parents) != 1 || parents["App.Caller"] != "" {
		t.Fatalf("lexical parents = %#v, want App.Caller at the root boundary", parents)
	}
}
