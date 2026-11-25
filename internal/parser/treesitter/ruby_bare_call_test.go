//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"
)

// Every `helper` occurrence is marked with what Ruby makes of it and what the
// adapter emits: `call` is a method call with an edge; `local` is a local
// variable read; `withheld` is a call in Ruby that the adapter does not emit,
// because the name is a local somewhere else in the same method; `none` has no
// edge for another reason (no call, no modelled source method, or a receiver
// that is not provably self).
const rubyBareCallSource = `class Widget
  attr_reader :name

  def helper
    1
  end

  def bare_call
    helper # call
  end

  def local_variable
    helper = 1
    helper # local
  end

  def parameter(helper)
    helper # local
  end

  def assign_after_use
    helper # withheld
    helper = 2
    helper # local
  end

  def self_assignment
    helper = helper # local
  end

  def conditional_assignment
    helper = 1 if false
    helper # local
  end

  def block_param
    [1].each { |helper| helper } # local
    helper # withheld
  end

  def block_local_assignment
    [1].each { helper = 1 }
    helper # withheld
  end

  def block_sees_outer_call
    [1].each { helper } # call
  end

  def keyword_param(helper:)
    helper # local
  end

  def optional_param(other = helper) # call
    other
  end

  def splat_param(*helper)
    helper # local
  end

  def block_arg(&helper)
    helper # local
  end

  def multiple_assignment
    helper, other = 1, 2
    helper # local
  end

  def nested_destructuring
    (other, helper), more = [1, 2], 3
    helper # local
  end

  def operator_assignment
    helper ||= 1
    helper # local
  end

  def rescue_variable
    begin
      raise "x"
    rescue => helper
    end
    helper # local
  end

  def for_variable
    for helper in [1]
    end
    helper # local
  end

  def named_capture
    /(?<helper>\w+)/ =~ "x"
    helper # local
  end

  def lambda_param
    ->(helper) { helper } # local
  end

  def block_local_declaration
    [1].each { |x; helper| helper = x }
    helper # withheld
  end

  def array_pattern(value)
    case value
    in [helper]
      helper # local
    end
  end

  def hash_pattern(value)
    case value
    in {helper:}
      helper # local
    end
  end

  def rightward_pattern(value)
    value => helper
    helper # local
  end

  def implicit_block_parameter
    [1].each { it } # none
  end

  def defined_check
    defined?(helper) # none
  end

  def eval_does_not_declare
    eval("helper = 1")
    helper # call
  end

  def nested_scope_assignment
    class << helper = Object.new
    end
    helper # local
  end

  def nested_def_does_not_read
    def inner
      helper # none
    end
  end

  def endless = helper # call

  def paren_call
    helper() # call
  end

  def self_call
    self.helper # call
  end

  def bare_in_expression
    helper + 1 # call
  end

  def bare_as_argument
    puts helper # call
  end

  def bare_as_receiver
    helper.to_s # call
  end

  def bare_in_interpolation
    "#{helper}" # call
  end

  def bare_in_condition
    return 1 if helper # call
  end

  def self.singleton
    helper # call
  end

  def rebinding_block(other)
    other.instance_eval { helper } # none
  end

  def attribute
    name # call
  end
end

helper # none
`

func TestRubyBareIdentifierCallsFollowLocalVariableRule(t *testing.T) {
	pf, err := NewRuby().Parse(context.Background(), "widget.rb", []byte(rubyBareCallSource))
	if err != nil {
		t.Fatal(err)
	}
	watched := map[string]bool{"helper": true, "self.helper": true, "name": true, "it": true}
	edges := map[int]string{}
	for _, e := range pf.Edges {
		if watched[e.DstName] {
			edges[e.Line] = e.Evidence
		}
	}
	refs := map[int]bool{}
	for _, r := range pf.References {
		if r.Kind == "call" && watched[r.Name] {
			refs[r.Range.StartLine] = true
		}
	}
	checked := 0
	for i, line := range strings.Split(rubyBareCallSource, "\n") {
		_, marker, ok := strings.Cut(line, "# ")
		if !ok {
			continue
		}
		lineNo := i + 1
		want := marker == "call"
		evidence, got := edges[lineNo]
		if got != want || refs[lineNo] != want {
			t.Errorf("line %d %q (%s): edge=%v ref=%v, want %v", lineNo, strings.TrimSpace(line), marker, got, refs[lineNo], want)
		}
		if got && !strings.Contains(line, "self.") && evidence != "ruby:implicit_receiver" {
			t.Errorf("line %d: evidence %q, want ruby:implicit_receiver", lineNo, evidence)
		}
		checked++
	}
	if checked < 40 {
		t.Fatalf("only %d marked lines checked", checked)
	}
}

// `def !=(obj)` parses as an endless `def !` with body `(obj)`, and the rest of
// the file loses its nesting without an error node, so the parameter `obj` would
// read as a call.
func TestRubyBareIdentifierCallsSkipMisparsedFile(t *testing.T) {
	const src = `class Box
  def obj; end

  def !=(obj)
    return false if obj.equal?(self)
    obj
  end

  def other
    obj
  end
end
`
	pf, err := NewRuby().Parse(context.Background(), "box.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range pf.Edges {
		if e.DstName == "obj" {
			t.Fatalf("bare identifier emitted from a misparsed file: %+v", e)
		}
	}
}

func TestRubyBareIdentifierCallsSkipFileWithSyntaxError(t *testing.T) {
	const src = `class Box
  def helper; end

  def run
    helper
  end

  def broken(
end
`
	pf, err := NewRuby().Parse(context.Background(), "box.rb", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range pf.Edges {
		if e.DstName == "helper" {
			t.Fatalf("bare identifier emitted from a file with a syntax error: %+v", e)
		}
	}
}
