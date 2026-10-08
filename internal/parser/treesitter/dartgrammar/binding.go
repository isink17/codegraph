//go:build cgo

// Package dartgrammar links the vendored UserNobody14/tree-sitter-dart parser
// against the go-tree-sitter runtime. See PROVENANCE.md.
package dartgrammar

//#include "tree_sitter/parser.h"
//const TSLanguage *tree_sitter_dart(void);
import "C"

import (
	"unsafe"

	sitter "github.com/smacker/go-tree-sitter"
)

// GetLanguage returns the Dart language.
func GetLanguage() *sitter.Language {
	return sitter.NewLanguage(unsafe.Pointer(C.tree_sitter_dart()))
}

// ABI is the tree-sitter language ABI the vendored parser was generated for.
// The go-tree-sitter runtime loads 13 and 14 only, and its SetLanguage drops
// the rejection silently, so this is the one place a mismatch is visible.
func ABI() uint32 {
	return uint32(C.tree_sitter_dart().abi_version)
}
