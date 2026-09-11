//go:build cgo

package treesitter

import "github.com/isink17/codegraph/internal/parser"

// Profiles for the tree-sitter adapters. Every one of them builds a call
// graph, which is the capability the non-cgo fallbacks lose for nine of these
// languages. See parser.Profile for when to bump a version suffix.

func tsProfile(language string) parser.Profile {
	return parser.Profile{ID: "treesitter:" + language + ":v1", EmitsCallEdges: true}
}

func (a *GoAdapter) Profile() parser.Profile     { return tsProfile("go") }
func (a *PythonAdapter) Profile() parser.Profile { return tsProfile("python") }
func (a *JavaAdapter) Profile() parser.Profile   { return tsProfile("java") }
func (a *KotlinAdapter) Profile() parser.Profile { return tsProfile("kotlin") }
func (a *CSharpAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:csharp:v4", EmitsCallEdges: true}
}
func (a *TypeScriptAdapter) Profile() parser.Profile { return tsProfile("typescript") }
func (a *RustAdapter) Profile() parser.Profile       { return tsProfile("rust") }

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
	return parser.Profile{ID: "treesitter:ruby:v5", EmitsCallEdges: true}
}
func (a *SwiftAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:swift:v6", EmitsCallEdges: true}
}
func (a *PHPAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:php:v3", EmitsCallEdges: true}
}
func (a *CppAdapter) Profile() parser.Profile { return tsProfile("cpp") }
