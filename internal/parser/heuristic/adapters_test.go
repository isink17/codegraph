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

func hasSymbolName(parsed graph.ParsedFile, name string) bool {
	for _, sym := range parsed.Symbols {
		if sym.Name == name {
			return true
		}
	}
	return false
}
