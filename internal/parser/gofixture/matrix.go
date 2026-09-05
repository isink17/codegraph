// Package gofixture holds the Go source fixtures both Go parser adapters are
// measured against.
//
// It exists so the native go/ast adapter and the tree-sitter adapter are pinned
// to the same tree and the same expectations rather than to two hand-copied
// ones that can drift apart. go_receiver_scope is a high-confidence strategy;
// an adapter that decided differently would make the confidence a lie.
package gofixture

// GoShadowMatrixSource is the shadow matrix both Go adapters must agree on.
//
// Every function here binds `store` -- the spelling of the file's own import
// alias -- in a different way, plus the controls where the alias is genuinely
// unshadowed and must still reach the imported package. It is exported so the
// tree-sitter adapter's test and the cross-adapter parity test measure the same
// tree rather than two hand-copied ones that can drift apart.
const GoShadowMatrixSource = `package main

import store "example.com/project/store"

type Thing struct{}

func (t *Thing) Get() {}

// A. parameter shadows package alias
func shadowParam(store *Thing) { store.Get() }

// B. method receiver shadows package alias
func (store *Thing) Recv() { store.Get() }

// C. named result shadows alias
func shadowResult() (store *Thing) { store.Get(); return nil }

// D. var with explicit pointer type
func shadowVar() {
	var store *Thing
	store.Get()
}

// E. short declaration from a call: local, but no proven type
func shadowShort() {
	store := factory()
	store.Get()
}

// E2. short declaration from a composite literal: type proven
func shadowComposite() {
	store := Thing{}
	store.Get()
	ptr := &Thing{}
	ptr.Get()
}

// F. range variable shadows alias
func shadowRange(items []Thing) {
	for _, store := range items {
		store.Get()
	}
}

// G. type-switch binding shadows alias
func shadowTypeSwitch(v any) {
	switch store := v.(type) {
	case *Thing:
		store.Get()
	}
}

// H. inner-block local shadows alias inside the block
func shadowBlock() {
	{
		var store *Thing
		store.Get()
	}
}

// I. same name outside any local scope still reaches the package import
func unshadowed() { store.Get() }

// J. a function literal's local does not leak to a sibling function
func shadowClosure() {
	f := func(store *Thing) { store.Get() }
	_ = f
}

func siblingAfterClosure() { store.Get() }

// K. import alias still works when genuinely unshadowed
func plainImport() { store.Get() }

func factory() *Thing { return nil }

// L. a function literal in an expression position (an if condition) still
// opens a scope. A whitelist of statement kinds misses this one.
func shadowClosureInCondition() {
	if check(func(store *Thing) bool {
		store.Get()
		return true
	}) {
		return
	}
}

func check(f func(*Thing) bool) bool { return false }

// M. parameter names inside a *type* bind nothing, so they must not shadow the
// import for the rest of the block.
func typeParamNamesBindNothing() {
	var fn func(store *Thing)
	_ = fn
	store.Get()
}
`

// GoShadowMatrixCalls is the destination spelling every call in the matrix must
// produce, keyed by source line. A local qualifier is kept verbatim; only a
// genuine import binding is rewritten to the import path.
var GoShadowMatrixCalls = map[int]string{
	10: "store.Get",                     // A parameter
	13: "store.Get",                     // B receiver
	16: "store.Get",                     // C named result
	21: "store.Get",                     // D var *Thing
	26: "factory",                       // E bare call, untouched
	27: "store.Get",                     // E short decl, local
	33: "store.Get",                     // E2 composite literal
	35: "ptr.Get",                       // E2 &composite literal
	41: "store.Get",                     // F range variable
	49: "store.Get",                     // G type switch
	57: "store.Get",                     // H inner block
	62: "example.com/project/store.Get", // I unshadowed -> import
	66: "store.Get",                     // J closure parameter
	70: "example.com/project/store.Get", // J sibling -> import, no leak
	73: "example.com/project/store.Get", // K unshadowed -> import
	80: "check",                         // L the call the literal is an argument to
	81: "store.Get",                     // L literal parameter binds inside an if condition
	95: "example.com/project/store.Get", // M a type's parameter name binds nothing
}

// GoShadowMatrixLocals is every lexical binding the matrix declares, rendered as
// `name [start-end] type ptr`. An empty type is the point of the case, not a
// gap: it vetoes the import rewrite and binds nothing.
var GoShadowMatrixLocals = []string{
	"f [65-68] : false",
	"f [88-88] : false",
	// `var fn func(store *Thing)` declares fn and nothing else: the parameter
	// name inside the type binds no local, so no `store` binding appears for
	// it and the import rewrite still reaches line 95.
	"fn [92-96] : false",
	"items [39-43] : false",
	"ptr [31-36] Thing: true",
	"store [10-10] Thing: true",
	"store [13-13] Thing: true",
	"store [16-16] Thing: true",
	"store [19-22] Thing: true",
	"store [25-28] : false",
	"store [31-36] Thing: false",
	"store [40-42] : false",
	"store [47-50] : false",
	"store [55-58] Thing: true",
	"store [66-66] Thing: true",
	"store [80-83] Thing: true",
	"t [7-7] Thing: true",
	"v [46-51] any: false",
}

// GoReceiverTypeSource covers the receiver-type shapes go_receiver_scope binds
// through, and the ones it deliberately refuses. Both adapters must project it
// identically.
const GoReceiverTypeSource = `package main

import store "example.com/project/store"

type Store struct{}
type Pair[K any, V any] struct{}

func (s *Store) Close()  {}
func (s Store) Value()   {}
func (p *Pair[K, V]) Do() {}

// proven: method receiver
func (s *Store) Run() { s.Close() }

// proven: typed parameter
func run(s *Store) { s.Close() }

// proven: value parameter reaching a pointer-receiver method (addressable)
func runValue(s Store) { s.Close() }

// proven: explicit var
func runVar() {
	var s Store
	s.Value()
}

// proven: generic receiver, type arguments stripped
func (p *Pair[K, V]) Also() { p.Do() }

// proven: qualified type resolves through the import
func runQualified(s *store.Store) { s.Close() }

// NOT proven: return type of a constructor
func runFactory() {
	s := NewStore()
	s.Close()
}

// NOT proven: field selection
func runField(w wrapper) {
	s := w.inner
	s.Close()
}

// NOT proven: interface value
func runIface(s closer) { s.Close() }

// NOT proven: a field chain is not a receiver call this evidence owns
func runChain(w wrapper) { w.inner.Close() }

type wrapper struct{ inner *Store }
type closer interface{ Close() }

func NewStore() *Store { return nil }
`
