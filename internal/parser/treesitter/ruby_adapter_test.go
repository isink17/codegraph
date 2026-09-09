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

// rubyIdentityHazardRows counts the constant-identity evidence of one parse per
// name, so a fact emitted twice fails rather than collapsing into a set.
func rubyIdentityHazardRows(p graph.ParsedFile) map[string]int {
	got := map[string]int{}
	for _, fact := range p.Scope.Imports {
		if fact.Kind == graph.ScopeImportRubyConstantIdentityUnknown {
			got[fact.OwnerModule+"|"+fact.LocalName]++
		}
	}
	return got
}

// rubyIdentityHazards is the same evidence as a set, for the cases that only
// care which constants were withdrawn.
func rubyIdentityHazards(p graph.ParsedFile) map[string]bool {
	got := map[string]bool{}
	for name, n := range rubyIdentityHazardRows(p) {
		if n != 1 {
			panic("duplicate constant-identity rows for " + name)
		}
		got[name] = true
	}
	return got
}

// Every syntax shape that moves a constant's identity inside a proven lexical
// owner records that exact qname. The walk descends through control flow --
// Ruby's constant scope is the enclosing class/module body, whatever `if`,
// `begin` or block sits in between.
func TestRubyConstantIdentityHazardShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"plain assignment":     {"    Service = Other\n", []string{"App.Service|Service"}},
		"or assignment":        {"    Service ||= Other\n", []string{"App.Service|Service"}},
		"and assignment":       {"    Service &&= Other\n", []string{"App.Service|Service"}},
		"multiple assignment":  {"    Service, Helper = a, b\n", []string{"App.Service|Service", "App.Helper|Helper"}},
		"conditional":          {"    if cond\n      Service = Other\n    end\n", []string{"App.Service|Service"}},
		"modifier conditional": {"    Service = Other if cond\n", []string{"App.Service|Service"}},
		"begin rescue":         {"    begin\n      Service = Other\n    rescue\n    end\n", []string{"App.Service|Service"}},
		"inside a block":       {"    [1].each do\n      Service = Other\n    end\n", []string{"App.Service|Service"}},
		"case when":            {"    case x\n    when 1\n      Service = Other\n    end\n", []string{"App.Service|Service"}},
		"const_set":            {"    const_set(:Service, Other)\n", []string{"App.Service|Service"}},
		"self const_set":       {"    self.const_set(:Service, Other)\n", []string{"App.Service|Service"}},
		"remove_const":         {"    remove_const(:Service)\n", []string{"App.Service|Service"}},
		"autoload":             {"    autoload :Lazy, \"lazy\"\n", []string{"App.Lazy|Lazy"}},
		"absolute qualified":   {"    ::Root::Service = Other\n", []string{"Root.Service|Service"}},
		// A relative qualified target names its constant through whatever its
		// first segment resolves to, which this phase does not model. Neither
		// reading is proven, so both are recorded: `App::Service = Other`
		// inside a wrapper almost always means the root `App::Service`, and
		// only stating the owner-relative reading would miss the wrong edge.
		"relative qualified": {"    Inner::Service = Other\n",
			[]string{"App.Inner.Service|Service", "Inner.Service|Service"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := parseRuby(t, "module App\n"+tc.body+"end\n")
			got := rubyIdentityHazards(p)
			if len(got) != len(tc.want) {
				t.Fatalf("hazards = %#v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Fatalf("missing %s in %#v", w, got)
				}
			}
		})
	}
}

// Nothing that does not move a constant's identity records a hazard, and an
// assignment is attributed to the body it sits in, never to an outer owner.
func TestRubyConstantIdentityHazardBoundaries(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		want   []string
	}{
		"declaration only":    {"module App\n  class Service\n    def self.run; end\n  end\nend\n", nil},
		"local, ivar, global": {"module App\n  lower = 1\n  @ivar = 2\n  $glob = 3\n  SOME[:k] = 4\nend\n", nil},
		// The nested class owns it: App::Inner::Service moved, App::Service did not.
		"nested class body": {"module App\n  class Inner\n    Service = Other\n  end\nend\n",
			[]string{"App.Inner.Service|Service"}},
		"nested module body": {"module App\n  module Inner\n    Service = Other\n  end\nend\n",
			[]string{"App.Inner.Service|Service"}},
		// Ruby raises SyntaxError for this, but tree-sitter parses it cleanly,
		// so the walker has to refuse it rather than trust the parse.
		"inside a method":    {"module App\n  def f\n    Service = Other\n  end\nend\n", nil},
		"inside self method": {"module App\n  def self.f\n    Service = Other\n  end\nend\n", nil},
		// The eigenclass is a different cref: this puts the constant on the
		// singleton class, and App::Service is untouched.
		"inside class << self": {"module App\n  class << self\n    Service = Other\n  end\nend\n", nil},
		// A root-level constant is never a single-segment lexical candidate.
		"root level": {"Service = Other\n", nil},
		// A qualified assignment at root names its constant absolutely.
		"root qualified": {"App::Service = Other\n", []string{"App.Service|Service"}},
		// Dynamic names are not modelled, and neither is a value receiver.
		"dynamic const_set": {"module App\n  const_set(name, Other)\n  const_set(\"#{p}X\", Other)\nend\n", nil},
		"value const_set":   {"module App\n  target.const_set(:Service, X)\nend\n", nil},
		// A constant receiver does name its target: this moves some
		// `Other::Service`, whichever lexical level `Other` resolves to.
		"constant receiver const_set": {"module App\n  Other.const_set(:Service, X)\nend\n",
			[]string{"App.Other.Service|Service", "Other.Service|Service"}},
		"unrelated api":       {"module App\n  configure(:Service)\nend\n", nil},
		"qualified open body": {"class App::Caller\n  Service = Other\nend\n", []string{"App.Caller.Service|Service"}},
		// A relative qualified opening inside another scope names an owner this
		// parser cannot resolve, so its body is attributed to nothing.
		"nested qualified open body": {"module App\n  class Deep::Caller\n    Service = Other\n  end\nend\n", nil},
		// A class nested under control flow is still a lexical owner: the
		// symbol walk only sees direct children, the assignment scan does not.
		"class under a conditional": {"module App\n  if cond\n    class Inner\n      Service = Other\n    end\n  end\nend\n",
			[]string{"App.Inner.Service|Service"}},
		// The wrong edge the reviewer proved: `App::Service = Other` written
		// inside any wrapper must still withdraw the root `App::Service`.
		"qualified target inside a wrapper": {"class Boot\n  App::Service = Other\nend\n",
			[]string{"Boot.App.Service|Service", "App.Service|Service"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := rubyIdentityHazards(parseRuby(t, tc.source))
			if len(got) != len(tc.want) {
				t.Fatalf("hazards = %#v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Fatalf("missing %s in %#v", w, got)
				}
			}
		})
	}
}

// A constant-mutation API called through a literal constant receiver names its
// target as exactly as a bare assignment does. Ignoring it because the receiver
// is "not self" left the same confidently wrong edge in place.
func TestRubyConstantIdentityLiteralReceiver(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		want   []string
	}{
		"root const_set":    {"App.const_set(:Service, Other)\n", []string{"App.Service|Service"}},
		"root remove_const": {"App.remove_const(:Service)\n", []string{"App.Service|Service"}},
		"root autoload":     {"App.autoload(:Service, \"service\")\n", []string{"App.Service|Service"}},
		"root string name":  {"App.const_set(\"Service\", Other)\n", []string{"App.Service|Service"}},
		// A qualified receiver names a constant under that path, not under its
		// first segment.
		"qualified receiver": {"App::Nested.const_set(:Service, Other)\n", []string{"App.Nested.Service|Service"}},
		"deep receiver":      {"App::Nested::Deep.const_set(:Service, Other)\n", []string{"App.Nested.Deep.Service|Service"}},
		// A leading `::` is exact: no owner-relative alternative belongs here.
		"absolute receiver":           {"module Boot\n  ::App.const_set(:Service, Other)\nend\n", []string{"App.Service|Service"}},
		"absolute qualified receiver": {"module Boot\n  ::App::Nested.const_set(:Service, Other)\nend\n", []string{"App.Nested.Service|Service"}},
		// A relative receiver inside a lexical owner could be that owner's
		// constant or the root one; both are recorded because a hazard only
		// ever withholds an edge.
		"relative receiver in one level": {"module Boot\n  App.const_set(:Service, Other)\nend\n",
			[]string{"Boot.App.Service|Service", "App.Service|Service"}},
		"relative receiver in three levels": {"module A\n  module B\n    class C\n      App.const_set(:Service, Other)\n    end\n  end\nend\n",
			[]string{"A.B.C.App.Service|Service", "A.B.App.Service|Service", "A.App.Service|Service", "App.Service|Service"}},
		// A qualified opening reaches the root boundary, so its body has one
		// lexical level, not two.
		"relative receiver in a qualified open": {"class App::Caller\n  Boot.const_set(:Service, Other)\nend\n",
			[]string{"App.Caller.Boot.Service|Service", "Boot.Service|Service"}},
		// The cref forms keep naming the current owner, exactly once.
		"bare in an owner": {"module App\n  const_set(:Service, Other)\nend\n", []string{"App.Service|Service"}},
		"self in an owner": {"module App\n  self.const_set(:Service, Other)\nend\n", []string{"App.Service|Service"}},
		// Deferred: nothing here names a constant from syntax.
		"value receiver":      {"target.const_set(:Service, Other)\n", nil},
		"chained receiver":    {"factory.module.const_set(:Service, Other)\n", nil},
		"send indirection":    {"App.send(:const_set, :Service, Other)\n", nil},
		"dynamic name":        {"App.const_set(name, Other)\n", nil},
		"interpolated name":   {"App.const_set(\"#{p}Service\", Other)\n", nil},
		"no arguments":        {"App.const_set\n", nil},
		"unrelated method":    {"App.configure(:Service)\n", nil},
		"root cref const_set": {"const_set(:Service, Other)\n", nil},
		"root self const_set": {"self.const_set(:Service, Other)\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := rubyIdentityHazards(parseRuby(t, tc.source))
			if len(got) != len(tc.want) {
				t.Fatalf("hazards = %#v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Fatalf("missing %s in %#v", w, got)
				}
			}
		})
	}
}

// The lexical chain a relative path is measured against is the nesting the walk
// actually descended, so an assignment target gets the same candidate set.
func TestRubyRelativeQualifiedAssignmentUsesTheLexicalChain(t *testing.T) {
	p := parseRuby(t, "module A\n  module B\n    Foo::Bar = Other\n  end\nend\n")
	want := []string{"A.B.Foo.Bar|Bar", "A.Foo.Bar|Bar", "Foo.Bar|Bar"}
	got := rubyIdentityHazards(p)
	if len(got) != len(want) {
		t.Fatalf("hazards = %#v, want %v", got, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("missing %s in %#v", w, got)
		}
	}
}

// A mutation whose receiver is a proven constant path does not depend on the
// cref, so a `def` or an eigenclass body -- the shape an installer actually
// takes -- must not swallow it. The cref forms, by contrast, are only evidence
// where `self` really is a nameable class or module.
func TestRubyConstantIdentityInsideMethodBodies(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		want   []string
	}{
		"receiver mutation in a singleton method": {
			"module Boot\n  def self.install\n    App.const_set(:Service, Other)\n  end\nend\n",
			[]string{"Boot.App.Service|Service", "App.Service|Service"}},
		"receiver mutation in an instance method": {
			"module Boot\n  def install\n    App.const_set(:Service, Other)\n  end\nend\n",
			[]string{"Boot.App.Service|Service", "App.Service|Service"}},
		"receiver mutation in an eigenclass body": {
			"module App\n  class << self\n    Boot.const_set(:Service, Other)\n  end\nend\n",
			[]string{"App.Boot.Service|Service", "Boot.Service|Service"}},
		"absolute receiver mutation in a method": {
			"module Boot\n  def self.install\n    ::App.const_set(:Service, Other)\n  end\nend\n",
			[]string{"App.Service|Service"}},
		// `self` inside `def self.install` IS the module, so a bare const_set
		// there names the module's own constant.
		"cref mutation in a singleton method": {
			"module App\n  def self.install\n    const_set(:Service, Other)\n  end\nend\n",
			[]string{"App.Service|Service"}},
		"self cref mutation in a singleton method": {
			"module App\n  def self.install\n    self.const_set(:Service, Other)\n  end\nend\n",
			[]string{"App.Service|Service"}},
		// A `def` inside `class << self` is also a singleton method.
		"cref mutation in an eigenclass method": {
			"module App\n  class << self\n    def install\n      const_set(:Service, Other)\n    end\n  end\nend\n",
			[]string{"App.Service|Service"}},
		// `self` in an instance method is an instance: it has no const_set.
		"cref mutation in an instance method": {
			"module App\n  def install\n    const_set(:Service, Other)\n  end\nend\n", nil},
		// Directly inside `class << self` the cref is the singleton class, so
		// the container's own constants are untouched.
		"cref mutation directly in an eigenclass": {
			"module App\n  class << self\n    const_set(:Service, Other)\n  end\nend\n", nil},
		// Every constant assignment inside a `def` is a Ruby SyntaxError, bare
		// and qualified alike, so none of them is evidence.
		"bare assignment in a method": {
			"module App\n  def install\n    Service = Other\n  end\nend\n", nil},
		"qualified assignment in a method": {
			"module App\n  def install\n    Foo::Bar = Other\n  end\nend\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := rubyIdentityHazards(parseRuby(t, tc.source))
			if len(got) != len(tc.want) {
				t.Fatalf("hazards = %#v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Fatalf("missing %s in %#v", w, got)
				}
			}
		})
	}
}

// A relative qualified opening cannot name its own cref, but Ruby's nesting
// there is that unnameable frame plus the enclosing levels -- which are still
// candidates a relative receiver could resolve through.
func TestRubyConstantIdentityInRelativeQualifiedOpening(t *testing.T) {
	p := parseRuby(t, "module Boot\n  class Outer::Wrap\n    App.const_set(:Service, Other)\n    Service = Other\n  end\nend\n")
	want := []string{"Boot.App.Service|Service", "App.Service|Service"}
	got := rubyIdentityHazards(p)
	if len(got) != len(want) {
		t.Fatalf("hazards = %#v, want %v (the bare assignment has no nameable cref)", got, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("missing %s in %#v", w, got)
		}
	}
}

// One row per constant per file: repeating the mutation says nothing more, and
// a relative path whose candidate levels collide must not double up either.
func TestRubyConstantIdentityHazardsAreDeduped(t *testing.T) {
	rows := rubyIdentityHazardRows(parseRuby(t, `module App
  Service = Other
  const_set(:Service, Other)
  self.const_set(:Service, Other)
  Service = Third
end
`))
	if len(rows) != 1 || rows["App.Service|Service"] != 1 {
		t.Fatalf("rows = %#v, want exactly one App.Service row", rows)
	}
}

// Spellings that must keep behaving, as regression fences.
func TestRubyConstantIdentityReceiverSpellingFences(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		want   []string
	}{
		"paren-less arguments": {"App.const_set :Service, Other\n", []string{"App.Service|Service"}},
		"safe navigation":      {"App&.const_set(:Service, Other)\n", []string{"App.Service|Service"}},
		"scope operator call":  {"App::Nested::const_set(:Service, Other)\n", []string{"App.Nested.Service|Service"}},
		"superclass body":      {"class Foo < Bar\n  App.const_set(:Service, Other)\nend\n", []string{"Foo.App.Service|Service", "App.Service|Service"}},
		"public_send indirect": {"App.public_send(:const_set, :Service, Other)\n", nil},
		"send indirect":        {"App.send(:const_set, :Service, Other)\n", nil},
		// `Object`, `Kernel` and `BasicObject` are ordinary constant receivers:
		// each names its own constant, and this parser already makes those
		// qnames lexical candidates for a caller inside them.
		"Object receiver":      {"Object.const_set(:Service, Other)\n", []string{"Object.Service|Service"}},
		"Kernel receiver":      {"Kernel.const_set(:Service, Other)\n", []string{"Kernel.Service|Service"}},
		"BasicObject receiver": {"BasicObject.const_set(:Service, Other)\n", []string{"BasicObject.Service|Service"}},
		"absolute Kernel":      {"::Kernel.const_set(:Service, Other)\n", []string{"Kernel.Service|Service"}},
		// The ROOT constant table stays deferred, and that follows from a
		// single-segment qname being unreachable here rather than from the
		// receiver: a bare `Service` at the root is not a candidate.
		"root bare assignment": {"Service = Other\n", nil},
		"root cref const_set":  {"const_set(:Service, Other)\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := rubyIdentityHazards(parseRuby(t, tc.source))
			if len(got) != len(tc.want) {
				t.Fatalf("hazards = %#v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Fatalf("missing %s in %#v", w, got)
				}
			}
		})
	}
}
