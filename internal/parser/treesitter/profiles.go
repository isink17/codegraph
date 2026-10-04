//go:build cgo

package treesitter

import "github.com/isink17/codegraph/internal/parser"

// Profiles for the tree-sitter adapters. Every one of them builds a call
// graph, which is the capability the non-cgo fallbacks lose for nine of these
// languages. See parser.Profile for when to bump a version suffix.

func tsProfile(language string) parser.Profile {
	return parser.Profile{ID: "treesitter:" + language + ":v1", EmitsCallEdges: true}
}

func (a *GoAdapter) Profile() parser.Profile { return tsProfile("go") }

// Python v2 reads Unicode names (PEP 3131) in the import and local-binding
// evidence it shares with the non-cgo adapter: Unicode imports now bind, Unicode
// locals now shadow, and a nested `def café` no longer binds the fragment `caf`.
// v3 records defs and classes nested under compound statements (if, try, with,
// for, while, match), which v2 skipped along with the calls inside them. v4
// records every name in its NFKC form, the name CPython binds (PEP 3131). v5
// records lambda parameters and match-case captures as local bindings. v6
// records lambdas in a def header's defaults, bodies on a header line and
// class-body bindings. v7 matches a method header's names by their NFKC form.  v8 records global/nonlocal names, `del` targets, rebound and decorated class-body names.
func (a *PythonAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:python:v8", EmitsCallEdges: true}
}

// Java v7 marks a construction of a member type the calling class declares
// (graph.JavaOwnMemberTypeEvidence), which binds that member type.
// v6 spells a generic construction's class without its type arguments
// (`new Box<>()` constructs `Box`), so it binds like a raw construction.
// v5 marks bare and `this.` calls inside anonymous, local and
// enum-constant class bodies, whose own members shadow the enclosing class's.
// v4 persists AST argument counts on constructor-call edges and AST
// parameter counts on constructor declarations, so commas inside nested
// calls, literals, lambdas or generic types no longer change a constructor's
// arity. v3 reads the package from the package_declaration node instead of the
// first `package x;` spelled anywhere in the raw text, so a comment or string
// can no longer name it; the package prefixes every qualified name and stable
// key. v2 persists direct AST argument counts on method-invocation edges.
func (a *JavaAdapter) Profile() parser.Profile {
	if a.legacyPackage {
		return parser.Profile{ID: "treesitter:java:v2", EmitsCallEdges: true}
	}
	if a.legacyArity {
		return parser.Profile{ID: "treesitter:java:v3", EmitsCallEdges: true}
	}
	if a.legacyNestedScope {
		return parser.Profile{ID: "treesitter:java:v4", EmitsCallEdges: true}
	}
	if a.legacyGenericConstruction {
		return parser.Profile{ID: "treesitter:java:v5", EmitsCallEdges: true}
	}
	if a.legacyOwnMember {
		return parser.Profile{ID: "treesitter:java:v6", EmitsCallEdges: true}
	}
	return parser.Profile{ID: "treesitter:java:v7", EmitsCallEdges: true}
}

// Kotlin v11 refuses duplicate package headers recovered as expressions.
// v10 reads the package from the package_header node instead of the
// first raw-text line starting with `package x`, so a comment or a multi-line
// string can no longer name it; the package prefixes every qualified name and
// stable key. v9 structurally recovers one swallowed declaration: an annotated
// top-level `private fun N() { ... }` the grammar parsed as an expression
// becomes an ordinary private function whose file keeps its facade, and the
// two call nodes spelling its head are no longer extracted as calls. v8
// derives declaration visibility from the structured visibility_modifier
// children of the declaration's modifiers, rather than from source text an
// annotation argument could truncate (`@A(x = 1) private fun`) or contaminate
// (`@Suppress("private") fun`). v7 structurally recovers a proven detached
// top-level annotation run before modifier-less fun/object declarations: the
// file keeps its facade and the declaration persists the recovered
// annotations in its signature and its JVM name and arity evidence. v6
// persisted declaration-level @JvmName evidence and suppressed a .kt facade
// whose root holds anything but preamble and declaration syntax. Unchanged
// Kotlin source can now persist different facts.
func (a *KotlinAdapter) Profile() parser.Profile {
	switch a.legacy {
	case 6:
		return parser.Profile{ID: "treesitter:kotlin:v6", EmitsCallEdges: true}
	case 8:
		return parser.Profile{ID: "treesitter:kotlin:v8", EmitsCallEdges: true}
	case 9:
		return parser.Profile{ID: "treesitter:kotlin:v9", EmitsCallEdges: true}
	}
	return parser.Profile{ID: "treesitter:kotlin:v11", EmitsCallEdges: true}
}
func (a *CSharpAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:csharp:v4", EmitsCallEdges: true}
}
func (a *TypeScriptAdapter) Profile() parser.Profile { return tsProfile("typescript") }

// Rust v4 marks a call whose first path segment a block around it declares
// (graph.RustCallBlockScopeEvidence); unchanged bytes persist a different
// edge evidence, so every Rust file has to reach the parser again.
// Rust v3 extracts nested use groups recursively from the syntax tree and
// refuses malformed declarations instead of persisting guessed import paths.
// Rust v2 changes rust_module_evidence.external_path from a checkout-absolute
// candidate path to the candidate stem relative to the declaring file's
// directory. Unchanged bytes persist a different row, and the resolver joins
// the new spelling exactly, so every Rust file has to reach the parser again.
func (a *RustAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:rust:v4", EmitsCallEdges: true}
}

// Ruby v6 records a `def` passed as the only argument of a visibility modifier
// (`private def run`, `private_class_method def self.build`, `module_function
// def tool`) as the method it defines, with its body's calls, and no longer
// emits the wrapper call as an edge of that method. Unchanged bytes now persist
// new symbols, edges and visibility facts, so every Ruby file has to reach the
// parser again.
// Ruby v5 adds constant identity and visibility facts. Ruby v4 adds P22.48 singleton visibility: `def self.run` and a `class <<
// self` body under the default state are stated public, a bare `private` /
// `protected` inside `class << self` is carried on the method symbol, and
// `private_class_method` / `public_class_method` / a named `private` inside
// `class << self` are persisted as `ruby_singleton_visibility` scope facts,
// with `ruby_singleton_visibility_unknown` for dynamic argument forms and
// `module_function`. Those are new facts for the same bytes -- a repository
// indexed under v3 has no visibility at all and would let a constant receiver
// bind a private singleton method -- so the profile has to say so.
//
// Ruby v3 added P22.47 call-edge safety on top of the v2 semantic identities
// (module/class/method qnames, lexical-parent facts, receiver evidence and
// operator spelling, nested-eigenclass fail-closed): an assignment target is no
// longer emitted as a call to the reader it is spelled after, a `def` nested in
// a block no longer contributes calls to the enclosing method, and a block that
// rebinds `self` no longer contributes lexical-self calls. Those are fewer,
// different call edges for the same bytes, so the profile has to say so --
// otherwise a repository indexed under v2 would keep edges this parser refuses
// to emit and never reparse.
func (a *RubyAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:ruby:v6", EmitsCallEdges: true}
}
func (a *SwiftAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:swift:v7", EmitsCallEdges: true}
}
func (a *PHPAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:php:v4", EmitsCallEdges: true}
}
func (a *CppAdapter) Profile() parser.Profile { return tsProfile("cpp") }
