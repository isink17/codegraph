//go:build cgo

package soliditygrammar

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
)

func TestGeneratedArtifactsMatchApprovedHashes(t *testing.T) {
	for path, want := range map[string]string{
		"parser.c":             "77120ebcf942b8890e889edabe8f050caffd6113f8acb9a33195e31b9c32ae15",
		"tree_sitter/parser.h": "180b893c8734778fd32f372dfbc27bd6ad1cd2221f26150b31256ff6716320d2",
		"LICENSE":              "8844f0cc9b76b9c8a9d0251904eb7536b6bb9976e0ec577e8f27ab96d42523ef",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
			t.Errorf("%s SHA-256 = %s, want %s", path, got, want)
		}
	}
}

func TestABIV14LoadsAndParses(t *testing.T) {
	if got := ABI(); got != 14 {
		t.Fatalf("grammar ABI = %d, want 14", got)
	}
	if !LanguageAccepted() {
		t.Fatal("pinned Tree-sitter runtime rejected the ABI 14 language")
	}
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(GetLanguage())
	tree, err := parser.ParseCtx(context.Background(), nil, []byte("pragma solidity ^0.8.20; contract C { function f(uint x) public returns (uint) { return x + 1; } }"))
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if tree.RootNode().HasError() {
		t.Fatalf("valid fixture parsed with errors: %s", tree.RootNode().String())
	}
}
