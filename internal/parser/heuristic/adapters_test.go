package heuristic

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestTypeScriptIgnoresMultilineTemplateSymbols(t *testing.T) {
	adapter := NewTypeScriptJavaScript()
	content := []byte("const tpl = `\nclass FakeType {}\nfunction fakeCall() {}\n`;\nfunction RealFn() {}\n")
	parsed, err := adapter.Parse(context.Background(), "sample.ts", content)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if hasSymbolName(parsed, "FakeType") {
		t.Fatalf("unexpected symbol FakeType parsed from template literal")
	}
	if hasSymbolName(parsed, "fakeCall") {
		t.Fatalf("unexpected symbol fakeCall parsed from template literal")
	}
	if !hasSymbolName(parsed, "RealFn") {
		t.Fatalf("expected symbol RealFn to be parsed")
	}
}

func TestRubyIgnoresHeredocSymbols(t *testing.T) {
	adapter := NewRuby()
	content := []byte("query = <<~SQL\nclass FakeClass\n  def fake_method\n  end\nSQL\nclass RealClass\n  def real_method\n  end\nend\n")
	parsed, err := adapter.Parse(context.Background(), "sample.rb", content)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if hasSymbolName(parsed, "FakeClass") {
		t.Fatalf("unexpected symbol FakeClass parsed from heredoc")
	}
	if hasSymbolName(parsed, "fake_method") {
		t.Fatalf("unexpected symbol fake_method parsed from heredoc")
	}
	if !hasSymbolName(parsed, "RealClass") {
		t.Fatalf("expected symbol RealClass to be parsed")
	}
	if !hasSymbolName(parsed, "real_method") {
		t.Fatalf("expected symbol real_method to be parsed")
	}
}

func TestCRLFRangesDoNotCountCarriageReturns(t *testing.T) {
	adapter := NewTypeScriptJavaScript()
	lf, err := adapter.Parse(context.Background(), "sample.ts", []byte("function RealFn() {\n}\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	crlf, err := adapter.Parse(context.Background(), "sample.ts", []byte("function RealFn() {\r\n}\r\n"))
	if err != nil {
		t.Fatalf("Parse(CRLF) error = %v", err)
	}
	if len(lf.Symbols) != 1 || len(crlf.Symbols) != 1 || crlf.Symbols[0].Range != lf.Symbols[0].Range {
		t.Fatalf("CRLF range = %+v, LF range = %+v", crlf.Symbols, lf.Symbols)
	}
}

func TestJVMHeuristicKeepsNestedContainersAndHeaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adapter *Adapter
		content string
		want    string
	}{
		{"java", NewJava(), "class Outer {\n class Inner {\n  void run(int value) {}\n }\n}\n", "sample.Outer.Inner.run"},
		{"kotlin", NewKotlin(), "class Outer {\n class Inner {\n  fun run(value: Int): Int = value\n }\n}\n", "Outer.Inner.run"},
	} {
		p, err := tc.adapter.Parse(context.Background(), "sample."+map[string]string{"java": "java", "kotlin": "kt"}[tc.name], []byte(tc.content))
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, s := range p.Symbols {
			if s.QualifiedName == tc.want {
				found = true
				if s.ContainerName != "Outer.Inner" || s.Signature == "" {
					t.Errorf("%s symbol = %+v", tc.name, s)
				}
			}
		}
		if !found {
			t.Errorf("%s missing %s: %+v", tc.name, tc.want, p.Symbols)
		}
	}
	p, err := NewJava().Parse(context.Background(), "Foo.java", []byte("class Foo {\n Foo(int x) {}\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Symbols) != 2 || p.Symbols[1].Kind != "function" || p.Symbols[1].Signature == "" {
		t.Fatalf("constructor = %+v", p.Symbols)
	}
}

func TestCSharpHeuristicV2KeepsNamespaceIdentity(t *testing.T) {
	p, err := NewCSharp().Parse(context.Background(), "Service.cs", []byte("namespace App.Core;\nclass Outer {\n class Inner {\n  void Run(int x) {}\n }\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.Package != "App.Core" || p.Symbols[len(p.Symbols)-1].QualifiedName != "App.Core.Outer.Inner.Run" {
		t.Fatalf("C# heuristic facts = package %q symbols %+v", p.Scope.Package, p.Symbols)
	}
	if NewCSharp().Profile().ID != "heuristic:csharp:v4" || NewCSharp().Profile().EmitsCallEdges {
		t.Fatalf("C# heuristic profile = %+v", NewCSharp().Profile())
	}
}

func TestCSharpHeuristicV2FailsClosedOnBlockNamespaceMix(t *testing.T) {
	for name, source := range map[string]string{
		"mixed.cs": `class Global {}
namespace App.Core { class Namespaced {} }`,
		"multiple.cs": `namespace A { class One {} }
namespace B { class Two {} }`,
	} {
		p, err := NewCSharp().Parse(context.Background(), name, []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		if p.Scope.Package != "" {
			t.Fatalf("%s package = %q, want empty", name, p.Scope.Package)
		}
		for _, symbol := range p.Symbols {
			if strings.Contains(symbol.QualifiedName, ".") {
				t.Fatalf("%s leaked namespace into %q", name, symbol.QualifiedName)
			}
		}
	}
}

func hasSymbolName(parsed graph.ParsedFile, name string) bool {
	for _, sym := range parsed.Symbols {
		if sym.Name == name {
			return true
		}
	}
	return false
}

// The fallback applies the header rule to stripped lines, so a comment or a
// string can never name the Kotlin package. Java has no heuristic package.
func TestKotlinHeuristicPackageIgnoresCommentsAndStrings(t *testing.T) {
	for src, want := range map[string]string{
		"package com.real\nfun a() {}\n":                                                         "com.real",
		"/*\npackage com.fake\n*/\npackage com.real\nfun a() {}\n":                               "com.real",
		"/**\n package com.fake\n */\npackage com.real\nfun a() {}\n":                            "com.real",
		"// package com.fake\npackage com.real\nfun a() {}\n":                                    "com.real",
		"#!/usr/bin/env kotlin\n@file:JvmName(\"F\")\npackage com.real\n":                        "com.real",
		"/*\npackage com.fake\n*/\nfun a() {}\n":                                                 "",
		"fun a() = \"\"\"\npackage com.fake\n\"\"\"\n":                                           "",
		"fun a() {}\npackage com.fake\n":                                                         "",
		"@file:JvmName(\"\"\"\npackage com.fake\n text \" interior\n\"\"\")\npackage com.real\n": "com.real",
		"@file:Suppress(\n    \"A\",\n    \"B\",\n)\npackage com.real\n":                         "com.real",
		"@file:[A B(\"x\")] @file:JvmSynthetic package com.real\n":                               "com.real",
		"/* /* nested */\npackage com.fake */\npackage com.real\n":                               "",
		"package com.`fake`\n":                                                                   "",
		"package com . real\n":                                                                   "",
	} {
		pf, err := NewKotlin().Parse(context.Background(), "A.kt", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if pf.Scope.Package != want {
			t.Errorf("%q: package = %q, want %q", src, pf.Scope.Package, want)
		}
		for _, s := range pf.Symbols {
			if strings.Contains(s.QualifiedName, "fake") {
				t.Errorf("%q: symbol %s carries a commented package", src, s.QualifiedName)
			}
		}
	}
	pf, err := NewJava().Parse(context.Background(), "A.java", []byte("/* package com.fake; */ package com.real;\nclass A {}\n"))
	if err != nil || pf.Scope.Package != "" {
		t.Fatalf("java heuristic package = %q, %v; want none", pf.Scope.Package, err)
	}
	if NewKotlin().Profile().ID != "heuristic:kotlin:v4" || NewJava().Profile().ID != "heuristic:java:v1" {
		t.Fatalf("profiles = %s, %s", NewKotlin().Profile().ID, NewJava().Profile().ID)
	}
}

func TestScopeEvidenceIgnoresCommentedDeclarations(t *testing.T) {
	pf, _ := NewKotlin().Parse(context.Background(), "A.kt", []byte("package com.real\npackage com.fake\nfun a() {}"))
	if pf.Scope.Package != "" {
		t.Fatalf("duplicate package = %q", pf.Scope.Package)
	}

	for _, src := range []string{
		"/*\nimport fake.Target\n*/\nimport real.Target as Alias\nfun a() {}",
		"val text = \"\"\"\nimport fake.Target\n\"\"\"\nimport real.Target as Alias\n",
	} {
		pf, err := NewKotlin().Parse(context.Background(), "A.kt", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if len(pf.Scope.Imports) != 1 || pf.Scope.Imports[0].SourceSpecifier != "real.Target" || pf.Scope.Imports[0].LocalName != "Alias" {
			t.Fatalf("imports = %+v", pf.Scope.Imports)
		}
	}
	for _, tc := range []struct{ src, want string }{
		{"/*\nnamespace Fake;\n*/\nclass A {}", ""},
		{"class A { string s = @\"\nnamespace Fake;\n\"; }", ""},
		{"/*\nnamespace Fake;\n*/\nnamespace Real;\nclass A {}", "Real"},
		{"// docs\nnamespace Real { class A { string s = \"}\"; } }", "Real"},
	} {
		pf, err := NewCSharp().Parse(context.Background(), "A.cs", []byte(tc.src))
		if err != nil {
			t.Fatal(err)
		}
		if pf.Scope.Package != tc.want {
			t.Fatalf("package = %q, want %q", pf.Scope.Package, tc.want)
		}
	}
}

func TestScopeEvidenceIgnoresRawAndVerbatimStrings(t *testing.T) {
	for _, src := range []string{
		"import real.Target\nval text = \"\"\"\n text \" interior\nimport fake.pkg.Name\n\"\"\"\nimport after.Target\n",
		"val text = \"\"\"\npackage fake.pkg\n text \" interior\npackage another.fake\n\"\"\"\npackage real.pkg\n",
		"val text = \"\"\" unterminated \" import fake.Target\nimport hidden.Target\n",
		`val text = "escaped \" quote import fake.Target"` + "\nimport real.Target\n",
	} {
		pf, err := NewKotlin().Parse(context.Background(), "A.kt", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(pf.Scope.Package, "fake") {
			t.Fatalf("raw string leaked package from %q: %+v", src, pf.Scope)
		}
		for _, imp := range pf.Scope.Imports {
			if strings.Contains(imp.SourceSpecifier, "fake") || strings.Contains(imp.SourceSpecifier, "hidden") {
				t.Fatalf("raw string leaked import from %q: %+v", src, pf.Scope.Imports)
			}
		}
	}

	for _, tc := range []struct{ src, wantPackage string }{
		{"var a = @\"\nnamespace Fake.Name;\n text \"\" interior\nusing Fake.Name;\n\";\nnamespace Real;\n", "Real"},
		{"var a = $\"namespace Fake.Name; using Fake.Name;\";\nnamespace Real;\n", "Real"},
		{`var a = $@"namespace Fake.Name; "" using Fake.Name;";` + "\nnamespace Real;\n", "Real"},
		{`var a = $""""` + "\n text \"\"\" fragment\nusing Fake.Name;\n" + `"""";` + "\nnamespace Real;\n", "Real"},
		{`var a = "escaped \" quote namespace Fake.Name;";` + "\nnamespace Real;\n", "Real"},
		{"var a = $\"\"\"\nnamespace Fake.Name;\n text \" interior\nusing Fake.Name;\n\"\"\";\nnamespace Real;\n", "Real"},
		{"var a = @\"unterminated namespace Fake.Name;\nnamespace Hidden;\n", ""},
	} {
		pf, err := NewCSharp().Parse(context.Background(), "A.cs", []byte(tc.src))
		if err != nil {
			t.Fatal(err)
		}
		if pf.Scope.Package != tc.wantPackage {
			t.Fatalf("C# string namespace from %q = %q, want %q", tc.src, pf.Scope.Package, tc.wantPackage)
		}
		for _, imp := range pf.Scope.Imports {
			if strings.Contains(imp.SourceSpecifier, "Fake") {
				t.Fatalf("C# string leaked import from %q: %+v", tc.src, pf.Scope.Imports)
			}
		}
	}
}
