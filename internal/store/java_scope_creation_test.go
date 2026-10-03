package store

import "testing"

// A construction's source text starts with `new` unless it is a qualified
// creation, whose qualifier comes first.
func TestJavaUnqualifiedCreation(t *testing.T) {
	for evidence, want := range map[string]bool{
		"new Box()":              true,
		"new/* c */Box()":        true,
		"new@A Box()":            true,
		"new\n\tBox()":           true,
		"new a.b.Box<>()":        true,
		"outer.new Inner()":      false,
		"this.new Inner()":       false,
		"Outer.this.new Inner()": false,
		"newOuter.new Inner()":   false,
		"new_.new Inner()":       false,
		"new$x.new Inner()":      false,
		"newé.new Inner()":       false,
		"make().new Inner()":     false,
		"":                       false,
	} {
		if got := javaUnqualifiedCreation(evidence); got != want {
			t.Errorf("javaUnqualifiedCreation(%q) = %v, want %v", evidence, got, want)
		}
	}
}
