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
	return parser.Profile{ID: "treesitter:csharp:v2", EmitsCallEdges: true}
}
func (a *TypeScriptAdapter) Profile() parser.Profile { return tsProfile("typescript") }
func (a *RustAdapter) Profile() parser.Profile       { return tsProfile("rust") }
func (a *RubyAdapter) Profile() parser.Profile       { return tsProfile("ruby") }
func (a *SwiftAdapter) Profile() parser.Profile      { return tsProfile("swift") }
func (a *PHPAdapter) Profile() parser.Profile        { return tsProfile("php") }
func (a *CppAdapter) Profile() parser.Profile        { return tsProfile("cpp") }
