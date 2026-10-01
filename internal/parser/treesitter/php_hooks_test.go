//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"
)

func TestPHPHooksAndConditionalDeclarations(t *testing.T) {
	src := []byte(`<?php
namespace App;
class Request {
 public string $name { get => $this->value(); set { $this->save($value); } }
 public function toArray() { return []; }
 public static function create() { return new self; }
}
if (!function_exists('helper')) { function helper() { return new Request; } }
if (true) { class Conditional { public function run() {} } }
`)
	p, err := NewPHP().Parse(context.Background(), "hooks.php", src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"App.Request": false, "App.Request.toArray": false, "App.Request.create": false, "App.helper": false, "App.Conditional": false, "App.Conditional.run": false}
	for _, s := range p.Symbols {
		if _, ok := want[s.QualifiedName]; !ok {
			t.Errorf("unexpected/re-homed symbol %s", s.QualifiedName)
			continue
		}
		want[s.QualifiedName] = true
		if s.Name == "toArray" && (s.Static == nil || *s.Static) {
			t.Errorf("toArray lacks instance method evidence: %+v", s)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing %s", name)
		}
	}
}

func TestPHPHookShapes(t *testing.T) {
	for _, hook := range []string{
		`public string $p { get => "value"; }`,
		`public string $p { set { $this->save($value); } }`,
		`public string $p { get { return $this->read(); } set => $value; }`,
		`public string $p { get; set; }`,
		`public private(set) string $p;`,
	} {
		t.Run(hook, func(t *testing.T) {
			p, err := NewPHP().Parse(context.Background(), "hooks.php", []byte(`<?php class C { `+hook+` public function after() {} }`))
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Symbols) != 2 || p.Symbols[1].QualifiedName != "C.after" || p.Symbols[1].Static == nil {
				t.Fatalf("symbols: %+v", p.Symbols)
			}
		})
	}
}

func TestPHPAsymmetricPromotedParameters(t *testing.T) {
	for _, modifier := range []string{"public private(set)", "public protected(set)", "protected private(set)"} {
		t.Run(modifier, func(t *testing.T) {
			p, err := NewPHP().Parse(context.Background(), "promotion.php", []byte(`<?php namespace App; class C { public function __construct(`+modifier+` Service $service) {} public function after() {} }`))
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Symbols) != 3 || p.Symbols[1].QualifiedName != "App.C.__construct" || p.Symbols[1].Static == nil {
				t.Fatalf("constructor lost or re-homed: %+v", p.Symbols)
			}
			for _, fact := range p.Scope.Imports {
				if fact.OwnerModule == "App.C" && fact.LocalName == "service" && fact.SourceSpecifier == "Service" && fact.Kind == "typed_binding" {
					return
				}
			}
			t.Fatalf("promoted property lost type evidence: %+v", p.Scope.Imports)
		})
	}
}

func TestPHPMalformedSyntaxFailsClosed(t *testing.T) {
	for _, fixture := range []struct {
		source string
		forbid []string
	}{
		{`<?php namespace App; class C { public string $p { set { } public function leaked() {} }`, []string{"App.C.leaked", "p"}},
		{`<?php class C { public function leaked( { } }`, []string{"C.leaked"}},
		{`<?php function caller() { $x-> (); }`, []string{"$x->"}},
		{`<?php class C { public function __construct(public private(nope) Service $service) {} }`, []string{"C.__construct"}},
		{`<?php class C { public function __construct(private public(set) Service $service) {} }`, []string{"C.__construct"}},
		{`<?php class C { public function __construct(public private(set) Service $service, broken( ) {} }`, []string{"C.__construct"}},
		{`<?php class C { public function f(public private(set) Service $service) {} }`, []string{"C.f"}},
	} {
		p, err := NewPHP().Parse(context.Background(), "broken.php", []byte(fixture.source))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range fixture.forbid {
			for _, s := range p.Symbols {
				if s.QualifiedName == forbidden {
					t.Errorf("malformed declaration emitted %s", forbidden)
				}
			}
			for _, i := range p.Scope.Imports {
				if i.LocalName == forbidden {
					t.Errorf("malformed property emitted scope fact %s", forbidden)
				}
			}
			for _, e := range p.Edges {
				if e.DstName == forbidden || strings.HasPrefix(forbidden, "$x->") && strings.Contains(e.DstName, "$x->") {
					t.Errorf("malformed call emitted %s", forbidden)
				}
			}
		}
	}
}

func TestPHPConditionalBranchesAndChainedCalls(t *testing.T) {
	p, err := NewPHP().Parse(context.Background(), "branches.php", []byte(`<?php
namespace App;
if (true) { function helper() {} } elseif (false) { function helper() {} } else { function helper() {} }
function caller() { new Request()->toArray(); }
`))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, s := range p.Symbols {
		if s.QualifiedName == "App.helper" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("conditional duplicates: %d", count)
	}
	for _, e := range p.Edges {
		if e.DstName == "toArray" {
			t.Fatal("chained member became bare function")
		}
	}
	if len(p.Edges) != 1 {
		t.Fatalf("calls: %+v", p.Edges)
	}
}

func TestPHPConditionalDeclarationBodies(t *testing.T) {
	for _, src := range []string{
		`<?php if (true) function helper() {}`,
		`<?php if (true): function helper() {} elseif(false): function other() {} else: class C {} endif;`,
		`<?php namespace App { if (true) { if (false) { function helper() {} } } }`,
	} {
		p, err := NewPHP().Parse(context.Background(), "conditional.php", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Symbols) == 0 {
			t.Fatalf("conditional declarations lost: %s", src)
		}
	}
}

func TestPHPHookPreservesTypedPropertyEvidence(t *testing.T) {
	p, err := NewPHP().Parse(context.Background(), "hooks.php", []byte(`<?php namespace App; class C { public Service $service { get => $this->service; } public function f() { $this->service->run(); } }`))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range p.Scope.Imports {
		if i.OwnerModule == "App.C" && i.LocalName == "service" && i.SourceSpecifier == "Service" && i.Kind == "typed_binding" {
			return
		}
	}
	t.Fatalf("hook property lost type evidence: %+v", p.Scope.Imports)
}

// Reduced from Symfony's valid ImportMapConfigReader.php heredoc and
// EmojiTransliterator.php chained assignment. Both grammars recover inside a
// method body; that must not erase the proven class and method declarations.
func TestPHPRecoveryPreservesValidProductionDeclarations(t *testing.T) {
	fixtures := []struct {
		name, source string
		methods      int
	}{
		{"heredoc", `<?php namespace App;
class Reader {
 public function dump($map) {
  $this->write(<<<EOF
<?php
/**
 * @return array<string, array{
 *     path: string,
 * }|array{
 *     version: string,
 * }>
 */
return $map;
EOF);
 }
 public function one(){} public function two(){} public function three(){}
 public function four(){} public function five(){} public function six(){} public function seven(){}
}`, 8},
		{"chained null assignment", `<?php namespace App;
class Emoji {
 public function create($transliterator) { $this->transliterator ??= clone $transliterator ??= \Transliterator::createFromRules('x'); }
 public function one(){} public function two(){} public function three(){}
 public function four(){} public function five(){}
}`, 6},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			p, err := NewPHP().Parse(context.Background(), "valid.php", []byte(fixture.source))
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Symbols) != fixture.methods+1 {
				t.Fatalf("valid declarations lost: got %d symbols, want %d", len(p.Symbols), fixture.methods+1)
			}
			for _, s := range p.Symbols {
				if s.Kind == "function" && (s.Static == nil || s.ContainerName == "App") {
					t.Fatalf("re-homed method: %+v", s)
				}
			}
		})
	}
}
