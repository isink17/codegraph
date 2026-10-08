//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestLuaAdapterDeclarationsImportsAndCallReferences(t *testing.T) {
	const src = `local function localFn() end
function globalFn() localFn(); module.run(); obj:method(); unknown() end
function M.tableFn() end
function M:method() end
require("./provider")
require(moduleName)
`
	p, err := NewLua().Parse(context.Background(), "main.lua", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range p.Symbols {
		got[s.StableKey] = s.Kind
	}
	for key, kind := range map[string]string{
		"func:lua:local:localFn":    "function",
		"func:lua:global:globalFn":  "function",
		"func:lua:global:M.tableFn": "function",
		"func:lua:global:M.method":  "method",
	} {
		found := false
		for gotKey, gotKind := range got {
			if gotKind == kind && strings.HasPrefix(gotKey, key+":") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no symbol with declaration identity for %q and kind %q", key, kind)
		}
	}
	if len(p.Edges) != 1 || p.Edges[0].DstName != "localFn" || p.Edges[0].Evidence != graph.LuaLocalFunctionEvidence+"1:1" || p.Edges[0].Line != 2 || p.Edges[0].Col != 21 {
		t.Fatalf("edges = %+v, want only the proven localFn call", p.Edges)
	}
	references := map[string]bool{}
	for _, r := range p.References {
		references[r.Name] = true
	}
	for _, name := range []string{"localFn", "module.run", "obj:method", "unknown", "require"} {
		if !references[name] {
			t.Errorf("missing call reference %q", name)
		}
	}
	if len(p.Imports) != 1 || p.Imports[0] != "./provider" {
		t.Fatalf("imports = %v", p.Imports)
	}
}

func TestLuaAdapterRejectsNonLuaExtension(t *testing.T) {
	if NewLua().Supports("main.luau") || !NewLua().Supports("main.lua") {
		t.Fatal("unexpected extension support")
	}
}

func TestLuaAdapterSyntaxErrorDoesNotInventDeclarations(t *testing.T) {
	p, err := NewLua().Parse(context.Background(), "broken.lua", []byte("function broken(\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Symbols) != 0 || len(p.Edges) != 0 {
		t.Fatalf("syntax recovery invented graph facts: %+v", p)
	}
}

func TestLuaAdapterLiteralRequireForms(t *testing.T) {
	const src = `local a = require("paren")
local b = require "bare"
local c = require [[long]]
local d = require("x" .. y)
local e = require(name)
local f = require()
`
	p, err := NewLua().Parse(context.Background(), "req.lua", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.Imports, ","); got != "paren,bare,long" {
		t.Fatalf("imports = %s", got)
	}
}
