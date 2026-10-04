//go:build cgo

package indexer

import "testing"

// rustPrivateVisibilityCases pin Rust's private visibility: an item without
// `pub` (or with `pub(self)`) is visible in its own module and every module
// nested inside it, and nowhere else. Each case was checked against rustc.
//
// The table runs in a Cargo `src/` layout and in a flat one. In the flat
// layout the parser can only guess a nested file's module from its last path
// segment, and a guess is not a fact, so cases marked guessed stay unresolved
// there.
var rustPrivateVisibilityCases = []struct {
	name      string
	files     tree
	src, call string
	target    string // "" means the call must stay unresolved
	guessed   bool
}{
	{"same module", tree{
		"lib.rs": "fn helper() {}\npub fn caller() {\n    helper();\n}\n",
	}, "lib.rs", "helper", "lib.rs:crate::helper", false},
	{"same file in a child module", tree{
		"lib.rs": "mod m;",
		"m.rs":   "fn inner() {}\npub fn outer() {\n    inner();\n}\n",
	}, "m.rs", "inner", "m.rs:crate::m::inner", false},
	{"pub(self) in the same module", tree{
		"lib.rs": "pub(self) fn helper() {}\npub fn caller() {\n    helper();\n}\n",
	}, "lib.rs", "helper", "lib.rs:crate::helper", false},
	{"child reaches parent through super", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "pub fn outer() {\n    super::helper();\n}\n",
	}, "m.rs", "super::helper", "lib.rs:crate::helper", false},
	{"child reaches parent through crate", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "pub fn outer() {\n    crate::helper();\n}\n",
	}, "m.rs", "crate::helper", "lib.rs:crate::helper", false},
	{"child imports parent item", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "use super::helper;\npub fn outer() {\n    helper();\n}\n",
	}, "m.rs", "helper", "lib.rs:crate::helper", false},
	{"grandchild reaches root", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "mod n;",
		"m/n.rs": "pub fn deep() {\n    crate::helper();\n}\n",
	}, "m/n.rs", "crate::helper", "lib.rs:crate::helper", true},
	// A bare name does not see the parent's items without an import.
	{"child bare name", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "pub fn outer() {\n    helper();\n}\n",
	}, "m.rs", "helper", "", false},
	{"sibling denied", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", "", false},
	{"sibling denied pub(self)", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "pub(self) fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", "", false},
	{"parent denied a child's item", tree{
		"lib.rs": "mod a;\npub fn caller() {\n    a::helper();\n}\n",
		"a.rs":   "fn helper() {}",
	}, "lib.rs", "a::helper", "", false},
	{"pub(crate) sibling", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "pub(crate) fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", "a.rs:crate::a::helper", false},
	{"pub(super) sibling", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "pub(super) fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", "a.rs:crate::a::helper", false},
	// `pub(in path)` is recorded but not modelled, so it stays fail-closed.
	{"pub(in crate) sibling", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "pub(in crate) fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", "", false},
	// Inside an impl method `super` starts from the impl's module, not its
	// type (rustc: crate::a::helper).
	{"super from an impl method", tree{
		"lib.rs": "mod a; fn helper() {}",
		"a.rs":   "mod m; fn helper() {}",
		"a/m.rs": "pub struct S;\nimpl S {\n    pub fn go() {\n        super::helper();\n    }\n}\n",
	}, "a/m.rs", "super::helper", "a.rs:crate::a::helper", true},
	// Paths in an impl method resolve from the impl's module, however the
	// type is written. rustc: crate::a::helper in both, the free fn (not the
	// associated one) in the third.
	{"super from an impl of a crate-qualified type", tree{
		"lib.rs": "mod a; pub struct S; fn helper() {}",
		"a.rs":   "mod m; fn helper() {}",
		"a/m.rs": "impl crate::S {\n    pub fn go() {\n        super::helper();\n    }\n}\n",
	}, "a/m.rs", "super::helper", "a.rs:crate::a::helper", true},
	{"super from an impl of its own module's qualified type", tree{
		"lib.rs": "mod a;",
		"a.rs":   "mod m; fn helper() {}",
		"a/m.rs": "pub struct S;\nfn helper() {}\nimpl crate::a::m::S {\n    pub fn go() {\n        super::helper();\n    }\n}\n",
	}, "a/m.rs", "super::helper", "a.rs:crate::a::helper", true},
	{"bare call from an impl of a qualified type", tree{
		"lib.rs": "mod b;",
		"b.rs":   "pub struct S;\nimpl S {\n    fn helper() {}\n}\nimpl crate::b::S {\n    pub fn go() {\n        helper();\n    }\n}\nfn helper() {}\n",
	}, "b.rs", "helper", "b.rs:crate::b::helper", false},
	{"super from a trait impl in a child of the root", tree{
		"lib.rs": "mod a; fn helper() {}",
		"a.rs":   "pub struct S;\nimpl Default for S {\n    fn default() -> Self {\n        super::helper();\n        S\n    }\n}\n",
	}, "a.rs", "super::helper", "lib.rs:crate::helper", false},
	{"crate path from an impl method", tree{
		"lib.rs": "mod a; fn helper() {}",
		"a.rs":   "pub struct S;\nimpl S {\n    pub fn go() {\n        crate::helper();\n    }\n}\n",
	}, "a.rs", "crate::helper", "lib.rs:crate::helper", false},
	// A parameter or local binding shadows a module fn of its name (rustc
	// calls the closure / the parameter).
	{"local closure named like a module fn", tree{
		"lib.rs": "fn f() {}\npub fn c() {\n    let f = || {};\n    f();\n}\n",
	}, "lib.rs", "f", "", false},
	{"fn parameter named like a module fn", tree{
		"lib.rs": "fn f() {}\npub fn c(f: fn()) {\n    f();\n}\n",
	}, "lib.rs", "f", "", false},
	{"closure parameter named like a module fn", tree{
		"lib.rs": "fn f() {}\npub fn c() {\n    let g = |f: fn()| f();\n    g(f);\n}\n",
	}, "lib.rs", "f", "", false},
	// Struct-pattern shorthand and other pattern forms bind too (rustc: the
	// binding in each).
	{"let struct shorthand binding", tree{
		"lib.rs": "struct P { f: fn() }\nfn f() {}\npub fn c(p: P) {\n    let P { f } = p;\n    f();\n}\n",
	}, "lib.rs", "f", "", false},
	{"parameter struct shorthand binding", tree{
		"lib.rs": "struct P { f: fn() }\nfn f() {}\npub fn c(P { f }: P) {\n    f();\n}\n",
	}, "lib.rs", "f", "", false},
	{"match arm struct shorthand binding", tree{
		"lib.rs": "struct P { f: fn() }\nfn f() {}\npub fn c(p: P) {\n    match p {\n        P { f } => f(),\n    }\n}\n",
	}, "lib.rs", "f", "", false},
	{"ref mut struct shorthand binding", tree{
		"lib.rs": "struct P { f: fn() }\nfn f() {}\npub fn c(mut p: P) {\n    let P { ref mut f } = p;\n    f();\n}\n",
	}, "lib.rs", "f", "", false},
	{"renamed field binding", tree{
		"lib.rs": "struct P { g: fn() }\nfn f() {}\npub fn c(p: P) {\n    let P { g: f } = p;\n    f();\n}\n",
	}, "lib.rs", "f", "", false},
	{"captured and or-pattern bindings", tree{
		"lib.rs": "fn f() {}\npub fn c(x: Option<fn()>) {\n    match x {\n        Some(f @ _) | Some(f) => f(),\n        None => {}\n    }\n}\n",
	}, "lib.rs", "f", "", false},
	{"tuple and slice pattern bindings", tree{
		"lib.rs": "fn f() {}\npub fn c(t: (u8, fn()), s: &[fn()]) {\n    let (_, f) = t;\n    if let [f, ..] = s {\n        f();\n    }\n    f();\n}\n",
	}, "lib.rs", "f", "", false},
	// A local binding is a value, not a module: `x::f()` is unaffected.
	{"let binding does not shadow a path head", tree{
		"lib.rs": "mod m;\npub fn c() {\n    let m = 1;\n    m::f();\n}\n",
		"m.rs":   "pub fn f() {}",
	}, "lib.rs", "m::f", "m.rs:crate::m::f", false},
	// A trait impl's items have the trait's visibility, which the parser
	// does not know, so they never answer; rustc calls the trait method.
	{"single trait impl associated call", tree{
		"lib.rs": "struct Cfg;\nimpl Default for Cfg {\n    fn default() -> Self {\n        Cfg\n    }\n}\npub fn c() {\n    Cfg::default();\n}\n",
	}, "lib.rs", "Cfg::default", "", false},
	// A private inherent method is what `Type::m` finds first.
	{"private inherent method", tree{
		"lib.rs": "struct S;\nimpl S {\n    fn m() {}\n}\npub fn c() {\n    S::m();\n}\n",
	}, "lib.rs", "S::m", "lib.rs:crate::S::m", false},
	{"private inherent method beside a trait method", tree{
		"lib.rs": "struct S;\nimpl S {\n    fn m() {}\n}\ntrait T {\n    fn m();\n}\nimpl T for S {\n    fn m() {}\n}\npub fn c() {\n    S::m();\n}\n",
	}, "lib.rs", "S::m", "lib.rs:crate::S::m", false},
	// A trait impl method is recorded under the type as the impl writes it,
	// but `Type::m` finds an inherent method first, wherever its impl is.
	// rustc calls the inherent nfa.rs NFA::swap in all three; the parser
	// cannot tell a trait impl from an inherent one, so each stays unresolved.
	{"trait impl method of a module-qualified type", tree{
		"lib.rs": "mod nfa; mod r;",
		"nfa.rs": "pub struct NFA;\nimpl NFA {\n    pub(crate) fn swap(&mut self) {}\n}\n",
		"r.rs":   "use crate::nfa;\npub trait R {\n    fn swap(&mut self);\n}\nimpl R for nfa::NFA {\n    fn swap(&mut self) {\n        nfa::NFA::swap(self);\n    }\n}\n",
	}, "r.rs", "nfa::NFA::swap", "", false},
	{"free fn beside a trait impl of a module-qualified type", tree{
		"lib.rs": "mod nfa; mod r;",
		"nfa.rs": "pub struct NFA;\nimpl NFA {\n    pub(crate) fn swap(&mut self) {}\n}\n",
		"r.rs":   "use crate::nfa;\npub trait R {\n    fn swap(&mut self);\n}\nimpl R for nfa::NFA {\n    fn swap(&mut self) {}\n}\npub fn go(n: &mut nfa::NFA) {\n    nfa::NFA::swap(n);\n}\n",
	}, "r.rs", "nfa::NFA::swap", "", false},
	{"trait impl method of an imported type", tree{
		"lib.rs": "mod nfa; mod r;",
		"nfa.rs": "pub struct NFA;\nimpl NFA {\n    pub(crate) fn swap(&mut self) {}\n}\n",
		"r.rs":   "use crate::nfa::NFA;\npub trait R {\n    fn swap(&mut self);\n}\nimpl R for NFA {\n    fn swap(&mut self) {\n        NFA::swap(self);\n    }\n}\n",
	}, "r.rs", "NFA::swap", "", false},
	// An item declared in a block shadows the module's item of that name
	// throughout the block, nested items included; it is not recorded, so the
	// call stays unresolved. rustc calls the block's item in each.
	{"fn-body fn shadows a module fn", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    fn helper() {}\n    helper();\n}\n",
	}, "lib.rs", "helper", "", false},
	{"fn-body fn seen from a nested fn", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    fn helper() {}\n    fn other() {\n        helper();\n    }\n    other();\n}\n",
	}, "lib.rs", "helper", "", false},
	{"fn-body use shadows a module fn", tree{
		"lib.rs": "mod a; fn helper() {}\npub fn outer() {\n    use crate::a::helper;\n    helper();\n}\n",
		"a.rs":   "pub fn helper() {}",
	}, "lib.rs", "helper", "", false},
	{"fn-body glob use shadows a module fn", tree{
		"lib.rs": "mod a; fn helper() {}\npub fn outer() {\n    use crate::a::*;\n    helper();\n}\n",
		"a.rs":   "pub fn helper() {}",
	}, "lib.rs", "helper", "", false},
	{"fn-body mod shadows a module", tree{
		"lib.rs": "mod m;\npub fn outer() {\n    mod m {\n        pub fn f() {}\n    }\n    m::f();\n}\n",
		"m.rs":   "pub fn f() {}",
	}, "lib.rs", "m::f", "", false},
	// rustc: the module's helper; the block's item ends with its block.
	{"inner-block fn does not shadow outside its block", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    {\n        fn helper() {}\n    }\n    helper();\n}\n",
	}, "lib.rs", "helper", "lib.rs:crate::helper", false},
	// The parser cannot see an inherent impl through a type alias, in a
	// `const _` block, in a fn body or in a `#[path]` module, or a trait
	// default or blanket method; the trait impl method never answers. rustc
	// calls the inherent / other trait's method in each.
	{"default method of another trait", tree{
		"lib.rs": "mod t { pub trait Foo { fn m(); } }\ntrait Bar { fn m() {} }\nstruct S;\nimpl t::Foo for S {\n    fn m() {}\n}\nimpl Bar for S {}\npub fn caller() {\n    S::m();\n}\n",
	}, "lib.rs", "S::m", "", false},
	{"default method of another trait, trait in a child file", tree{
		"lib.rs": "mod t;\ntrait Bar { fn m() {} }\nstruct S;\nimpl t::Foo for S {\n    fn m() {}\n}\nimpl Bar for S {}\npub fn caller() {\n    S::m();\n}\n",
		"t.rs":   "pub trait Foo { fn m(); }\n",
	}, "lib.rs", "S::m", "", false},
	{"blanket trait in scope", tree{
		"lib.rs": "mod t; mod u;\nuse u::Bar;\nstruct S;\nimpl t::Foo for S {\n    fn m() {}\n}\npub fn caller() {\n    S::m();\n}\n",
		"t.rs":   "pub trait Foo { fn m(); }\n",
		"u.rs":   "pub trait Bar { fn m(); }\nimpl<T> Bar for T {\n    fn m() {}\n}\n",
	}, "lib.rs", "S::m", "", false},
	{"inherent impl through a type alias", tree{
		"lib.rs": "struct S;\ntype A = S;\nimpl A {\n    fn m() {}\n}\ntrait T { fn m(); }\nimpl T for S {\n    fn m() {}\n}\npub fn caller() {\n    S::m();\n}\n",
	}, "lib.rs", "S::m", "", false},
	{"inherent impl in a const block", tree{
		"lib.rs": "struct S;\ntrait T { fn m(); }\nimpl T for S {\n    fn m() {}\n}\nconst _: () = {\n    impl S {\n        fn m() {}\n    }\n};\npub fn caller() {\n    S::m();\n}\n",
	}, "lib.rs", "S::m", "", false},
	{"inherent impl in a path-attribute module", tree{
		"lib.rs":     "#[path = \"x/impls.rs\"]\nmod impls;\npub struct S;\ntrait T { fn m(); }\nimpl T for S {\n    fn m() {}\n}\npub fn caller() {\n    S::m();\n}\n",
		"x/impls.rs": "impl super::S {\n    pub(crate) fn m() {}\n}\n",
	}, "lib.rs", "S::m", "", false},
	{"inherent impl in a fn body", tree{
		"lib.rs": "struct S;\ntrait T { fn m(); }\nimpl T for S {\n    fn m() {}\n}\nfn setup() {\n    impl S {\n        fn m() {}\n    }\n}\npub fn caller() {\n    S::m();\n}\n",
	}, "lib.rs", "S::m", "", false},
	{"Self call inside a trait impl", tree{
		"lib.rs": "mod t { pub trait Foo { fn m(); fn n(); } }\nstruct S;\nimpl t::Foo for S {\n    fn m() {}\n    fn n() { Self::m(); }\n}\npub fn caller() {}\n",
	}, "lib.rs", "Self::m", "", false},
	// A `mod` declared in a block is not the file's module: a call in it
	// names that module's items (rustc: m::helper in each).
	{"call inside a fn-body mod", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    mod m {\n        pub fn helper() {}\n        pub fn g() {\n            helper();\n        }\n    }\n    m::g();\n}\n",
	}, "lib.rs", "helper", "", false},
	{"call inside a fn-body mod, public module fn", tree{
		"lib.rs": "pub fn helper() {}\npub fn outer() {\n    mod m {\n        pub fn helper() {}\n        pub fn g() {\n            helper();\n        }\n    }\n    m::g();\n}\n",
	}, "lib.rs", "helper", "", false},
	{"self path inside a fn-body mod", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    mod m {\n        fn helper() {}\n        pub fn g() {\n            self::helper();\n        }\n    }\n    m::g();\n}\n",
	}, "lib.rs", "self::helper", "", false},
	{"crate path inside a fn-body mod", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    mod m {\n        pub fn g() {\n            crate::helper();\n        }\n    }\n    m::g();\n}\n",
	}, "lib.rs", "crate::helper", "lib.rs:crate::helper", false},
	// `self::` skips the block's items (rustc: the module's helper).
	{"self path past a block fn", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    fn helper() {}\n    self::helper();\n}\n",
	}, "lib.rs", "self::helper", "lib.rs:crate::helper", false},
	// A statement macro may declare the block's own helper (rustc: the
	// macro's); std expression macros declare nothing.
	{"statement macro in the block", tree{
		"lib.rs": "fn helper() {}\nmacro_rules! mk { () => { fn helper() {} } }\npub fn outer() {\n    mk!();\n    helper();\n}\n",
	}, "lib.rs", "helper", "", false},
	{"std expression macro in the block", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    println!(\"x\");\n    helper();\n}\n",
	}, "lib.rs", "helper", "lib.rs:crate::helper", false},
	{"file macro shadowing a std macro name", tree{
		"lib.rs": "fn helper() {}\nmacro_rules! println { () => { fn helper() {} } }\npub fn outer() {\n    println!();\n    helper();\n}\n",
	}, "lib.rs", "helper", "", false},
	{"unsafe block item", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    unsafe {\n        fn helper() {}\n        helper();\n    }\n}\n",
	}, "lib.rs", "helper", "", false},
	{"closure body block item", tree{
		"lib.rs": "fn helper() {}\npub fn outer() {\n    let c = || {\n        fn helper() {}\n        helper();\n    };\n    c();\n}\n",
	}, "lib.rs", "helper", "", false},
	{"match arm after a block item", tree{
		"lib.rs": "fn helper() {}\npub fn outer(x: u8) {\n    fn helper() {}\n    match x { _ => helper() }\n}\n",
	}, "lib.rs", "helper", "", false},
	// Another crate root's private `crate::helper` is not the caller's.
	{"other crate root", tree{
		"lib.rs":  "fn helper() {}",
		"main.rs": "fn main() {\n    helper();\n}\n",
	}, "main.rs", "helper", "", false},
	// a/sub.rs and sub.rs are different modules even where the parser's
	// path guess gives both the same name.
	{"same guessed module name in another file", tree{
		"lib.rs":   "mod a; mod sub;",
		"a.rs":     "mod sub;",
		"a/sub.rs": "pub fn caller() {\n    f();\n}\n",
		"sub.rs":   "fn f() {}",
	}, "a/sub.rs", "f", "", false},
	// An inline module's item is not the enclosing file module's: here
	// `String` is std's.
	{"inline module item in a non-root file", tree{
		"lib.rs": "mod m;",
		"m.rs":   "mod x {\n    pub struct String;\n    impl String {\n        fn new() {}\n    }\n}\npub fn caller() {\n    String::new();\n}\n",
	}, "m.rs", "String::new", "", false},
	// A re-export passes on only what the re-exporting module sees. c is
	// outside a, so its glob does not import a::x's pub(super) id and
	// std::process::id answers (rustc: the glob is unused, a::x::id never
	// used), although the caller itself could see a::x::id.
	{"pub(super) item behind a glob re-export from outside its parent", tree{
		"lib.rs": "mod a; mod c;",
		"a.rs":   "pub mod x; pub mod y;",
		"a/x.rs": "pub(super) fn id() -> u32 { 7 }",
		"a/y.rs": "pub fn caller() {\n    crate::c::id();\n}\n",
		"c.rs":   "pub use crate::a::x::*;\npub use std::process::*;\n",
	}, "a/y.rs", "crate::c::id", "", false},
	// a itself sees a::x's pub(super) id, so its glob re-export passes it on
	// to a's children (rustc: crate::a::x::id).
	{"pub(super) item behind a glob re-export from its parent", tree{
		"lib.rs": "mod a;",
		"a.rs":   "pub mod x; pub mod y;\npub use x::*;\n",
		"a/x.rs": "pub(super) fn id() -> u32 { 7 }",
		"a/y.rs": "pub fn caller() {\n    crate::a::id();\n}\n",
	}, "a/y.rs", "crate::a::id", "a/x.rs:crate::a::x::id", true},
	// A descendant of a sees a::x's pub(super) id too, so its glob
	// re-export passes it on (rustc: crate::a::x::id).
	{"pub(super) item behind a descendant's glob re-export", tree{
		"lib.rs": "mod a;",
		"a.rs":   "pub mod x; pub mod y;",
		"a/x.rs": "pub(super) fn id() -> u32 { 7 }",
		"a/y.rs": "pub use super::x::*;\npub fn caller() {\n    crate::a::y::id();\n}\n",
	}, "a/y.rs", "crate::a::y::id", "a/x.rs:crate::a::x::id", true},
	// Every hop of a re-export chain is judged from its own module: a passes
	// id on, but e is outside a, so std::process::id answers (rustc).
	{"pub(super) item behind a chained glob re-export from outside its parent", tree{
		"lib.rs": "mod a; mod e;",
		"a.rs":   "pub mod x;\npub use x::*;\n",
		"a/x.rs": "pub(super) fn id() -> u32 { 7 }",
		"e.rs":   "pub use crate::a::*;\npub use std::process::*;\npub fn outside() {\n    crate::e::id();\n}\n",
	}, "e.rs", "crate::e::id", "", false},
	// A glob re-export never passes on a private item: `exit` is std's.
	{"private item behind a glob re-export", tree{
		"lib.rs": "mod a; mod c;",
		"a.rs":   "fn exit(_code: i32) {}\npub fn caller() {\n    crate::c::exit(0);\n}\n",
		"c.rs":   "pub use crate::a::*;\npub use std::process::exit;\n",
	}, "a.rs", "crate::c::exit", "", false},
}

func TestRustPrivateVisibility(t *testing.T) {
	for _, layout := range []string{"src/", ""} {
		for _, tc := range rustPrivateVisibilityCases {
			files := tree{}
			for path, content := range tc.files {
				files[layout+path] = content
			}
			src, target := layout+tc.src, tc.target
			if target != "" {
				target = layout + target
			}
			if layout == "" && tc.guessed {
				target = ""
			}
			runRustCallEntrypoints(t, "layout="+layout+"/"+tc.name, files, src, tc.call, target)
		}
	}
}

// runRustCallEntrypoints indexes files and requires the fresh index and every
// resolver entrypoint, each starting from an unresolved edge, to decide the
// call the same way.
func runRustCallEntrypoints(t *testing.T, name string, files tree, src, call, target string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		r := newLifecycleRepo(t, files)
		assertRustCallTarget(t, r, "fresh", src, call, target)
		for _, ep := range []struct {
			name string
			run  func() error
		}{
			{"repo", func() error { _, err := r.store.ResolveEdges(r.ctx, r.repoID); return err }},
			{"paths", func() error { return r.store.ResolveEdgesForPaths(r.ctx, r.repoID, []string{src}) }},
			{"names", func() error {
				_, err := r.store.ResolveEdgesForNamesWithStats(r.ctx, r.repoID, []string{call})
				return err
			}},
			{"paths+names", func() error {
				_, err := r.store.ResolveEdgesForPathsAndNames(r.ctx, r.repoID, []string{src}, []string{call})
				return err
			}},
		} {
			if _, err := r.raw(t).ExecContext(r.ctx, `UPDATE edges SET dst_symbol_id=NULL,resolution_strategy='',resolution_confidence='' WHERE repo_id=? AND dst_name=?`, r.repoID, call); err != nil {
				t.Fatal(err)
			}
			if err := ep.run(); err != nil {
				t.Fatalf("%s: %v", ep.name, err)
			}
			assertRustCallTarget(t, r, ep.name, src, call, target)
		}
	})
}

// TestRustPrivateVisibilityFollowsIncrementalChanges adds and removes a private
// item and narrows a crate-visible one, through both update shapes; every step
// must match a fresh index of the same tree.
func TestRustPrivateVisibilityFollowsIncrementalChanges(t *testing.T) {
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{
			"lib.rs": "mod m; mod a; mod b;",
			"m.rs":   "pub fn outer() {\n    super::helper();\n}\n",
			"a.rs":   "pub(crate) fn shared() {}",
			"b.rs":   "pub fn user() {\n    crate::a::shared();\n}\n",
		})
		update := func(paths ...string) {
			if scoped {
				r.update(t, paths...)
			} else {
				r.update(t)
			}
		}
		assertRustCallTarget(t, r, "no helper", "m.rs", "super::helper", "")
		r.write(t, "lib.rs", "mod m; mod a; mod b; fn helper() {}")
		update("lib.rs")
		r.assertFreshParity(t, "private helper added")
		assertRustCallTarget(t, r, "private helper added", "m.rs", "super::helper", "lib.rs:crate::helper")
		r.write(t, "lib.rs", "mod m; mod a; mod b;")
		update("lib.rs")
		r.assertFreshParity(t, "private helper removed")
		assertRustCallTarget(t, r, "private helper removed", "m.rs", "super::helper", "")

		assertRustCallTarget(t, r, "crate visible", "b.rs", "crate::a::shared", "a.rs:crate::a::shared")
		r.write(t, "a.rs", "fn shared() {}")
		update("a.rs")
		r.assertFreshParity(t, "narrowed to private")
		assertRustCallTarget(t, r, "narrowed to private", "b.rs", "crate::a::shared", "")
	}
}

// TestRustShadowingFollowsIncrementalChanges turns an inherent impl into a
// trait impl and adds and removes a block item and a parameter that shadow a
// module fn, through both update shapes; every step must match a fresh index
// of the same tree.
func TestRustShadowingFollowsIncrementalChanges(t *testing.T) {
	src := func(impl, body, param string) string {
		return "mod a;\nstruct Cfg;\ntrait Make {\n    fn make();\n}\n" + impl + " {\n    fn make() {}\n}\nfn helper() {}\npub fn c(" + param + ") {\n" + body + "    Cfg::make();\n    helper();\n}\n"
	}
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{"lib.rs": src("impl Cfg", "", ""), "a.rs": "pub fn f() {}"})
		update := func(paths ...string) {
			if scoped {
				r.update(t, paths...)
			} else {
				r.update(t)
			}
		}
		step := func(name, impl, body, param, make, help string) {
			r.write(t, "lib.rs", src(impl, body, param))
			update("lib.rs")
			r.assertFreshParity(t, name)
			assertRustCallTarget(t, r, name, "lib.rs", "Cfg::make", make)
			assertRustCallTarget(t, r, name, "lib.rs", "helper", help)
		}
		assertRustCallTarget(t, r, "initial", "lib.rs", "Cfg::make", "lib.rs:crate::Cfg::make")
		assertRustCallTarget(t, r, "initial", "lib.rs", "helper", "lib.rs:crate::helper")
		step("trait impl", "impl Make for Cfg", "", "", "", "lib.rs:crate::helper")
		step("inherent impl again", "impl Cfg", "", "", "lib.rs:crate::Cfg::make", "lib.rs:crate::helper")
		step("block fn added", "impl Cfg", "    fn helper() {}\n", "", "lib.rs:crate::Cfg::make", "")
		step("block fn removed", "impl Cfg", "", "", "lib.rs:crate::Cfg::make", "lib.rs:crate::helper")
		step("parameter added", "impl Cfg", "", "helper: fn()", "lib.rs:crate::Cfg::make", "")
		step("parameter removed", "impl Cfg", "", "", "lib.rs:crate::Cfg::make", "lib.rs:crate::helper")
	}
}
