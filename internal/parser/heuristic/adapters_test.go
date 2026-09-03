package heuristic

import (
	"context"
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

func hasSymbolName(parsed graph.ParsedFile, name string) bool {
	for _, sym := range parsed.Symbols {
		if sym.Name == name {
			return true
		}
	}
	return false
}
