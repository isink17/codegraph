package texttoken

import "testing"

func TestWeightsString(t *testing.T) {
	got := WeightsString("Foo/bar foo_bar foo.bar! x a")
	if got["foo/bar"] != 1 {
		t.Fatalf("foo/bar = %v, want 1", got["foo/bar"])
	}
	if got["foo_bar"] != 1 {
		t.Fatalf("foo_bar = %v, want 1", got["foo_bar"])
	}
	if got["foo.bar"] != 1 {
		t.Fatalf("foo.bar = %v, want 1", got["foo.bar"])
	}
	if _, ok := got["x"]; ok {
		t.Fatalf("single-letter token x should be filtered")
	}
	if _, ok := got["a"]; ok {
		t.Fatalf("single-letter token a should be filtered")
	}
}
