//go:build cgo

// Package soliditygrammar links the ABI 14 parser regenerated from the
// pinned JoranHonig/tree-sitter-solidity grammar. See PROVENANCE.md.
package soliditygrammar

/*
#include "tree_sitter/parser.h"
typedef struct TSParser TSParser;
TSParser *ts_parser_new(void);
bool ts_parser_set_language(TSParser *, const TSLanguage *);
void ts_parser_delete(TSParser *);
const TSLanguage *tree_sitter_solidity(void);
static bool solidity_language_accepted(void) {
	TSParser *parser = ts_parser_new();
	if (!parser) return false;
	bool accepted = ts_parser_set_language(parser, tree_sitter_solidity());
	ts_parser_delete(parser);
	return accepted;
}
*/
import "C"

import (
	"unsafe"

	sitter "github.com/smacker/go-tree-sitter"
)

func GetLanguage() *sitter.Language {
	return sitter.NewLanguage(unsafe.Pointer(C.tree_sitter_solidity()))
}

func ABI() uint32 { return uint32(C.tree_sitter_solidity().abi_version) }

func LanguageAccepted() bool { return bool(C.solidity_language_accepted()) }
