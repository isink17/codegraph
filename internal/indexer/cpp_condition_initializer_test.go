//go:build cgo

package indexer

import "testing"

func TestCppConditionDeclarationCallResolvesThroughIndex(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", `struct State {};
struct A {
  int next(State&) { return 0; }
  void apply(State& state) { while (int i = next(state)) { (void)i; } }
};
`)
	r.run("index")

	wantBound(t, r.boundTargets("next"), "A::next")
}
