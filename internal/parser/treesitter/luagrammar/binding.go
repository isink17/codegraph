//go:build cgo

// Package luagrammar links the vendored tree-sitter-grammars/tree-sitter-lua
// v0.3.0 parser against the go-tree-sitter runtime. See PROVENANCE.md.
package luagrammar

//#include "tree_sitter/parser.h"
//const TSLanguage *tree_sitter_lua(void);
import "C"

import (
	"unsafe"

	sitter "github.com/smacker/go-tree-sitter"
)

// GetLanguage returns the Lua language.
func GetLanguage() *sitter.Language {
	return sitter.NewLanguage(unsafe.Pointer(C.tree_sitter_lua()))
}
