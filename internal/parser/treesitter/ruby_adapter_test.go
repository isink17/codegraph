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
