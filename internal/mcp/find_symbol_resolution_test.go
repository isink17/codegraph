package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/compactfmt"
	"github.com/isink17/codegraph/internal/indexer"
)

func qualifiedLookupFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s, ctx := detailFixture(t)
	writeRepoFile(t, s.repoRoot, "a_wrapper.go", "package store\n\nfunc LookupSymbolShortName(value string) string { return lookupSymbolShortName(value) }\n")
	writeRepoFile(t, s.repoRoot, "z_impl.go", "package store\n\nfunc lookupSymbolShortName(value string) string { return value }\n")
	if _, err := s.indexer.Update(ctx, indexer.Options{RepoRoot: s.repoRoot}); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func BenchmarkFindSymbolQualifiedPreference(b *testing.B) {
	for _, tc := range []struct{ name, query string }{
		{"exact", "bench.BenchFn0"},
		{"fallback", "BenchFn"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			raw, err := json.Marshal(map[string]any{"query": tc.query, "limit": 20})
			if err != nil {
				b.Fatal(err)
			}
			benchHandle(b, false, "find_symbol", raw)
		})
	}
}

func TestFindSymbolPrefersExactQualifiedIdentity(t *testing.T) {
	s, ctx := qualifiedLookupFixture(t)
	const qualified = "store.lookupSymbolShortName"
	want, err := s.store.FindSymbolExactResult(ctx, s.repoID, qualified, 1, 0)
	if err != nil || len(want.Matches) != 1 {
		t.Fatalf("indexed exact identity = %+v, %v", want, err)
	}
	data, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": qualified, "limit": 1, "detail": "full"})
	got := recordsOf(t, data, "matches")
	t.Logf("find_symbol query=%s: matched=%v records=%v", qualified, data["matched"], got)
	if data["matched"] != true || len(got) != 1 || got[0]["qualified_name"] != qualified {
		t.Fatalf("qualified lookup returned %v, want exact %s", got, qualified)
	}
	identity := want.Matches[0]
	for key, value := range map[string]any{
		"symbol_id": float64(identity.ID), "stable_key": identity.StableKey,
		"kind": identity.Kind, "language": identity.Language, "file": identity.FilePath,
		"line": float64(identity.Range.StartLine),
	} {
		if got[0][key] != value {
			t.Errorf("%s = %v, want %v", key, got[0][key], value)
		}
	}
	source := got[0]["source"].(map[string]any)["text"].(string)
	if !strings.Contains(source, "return value") || strings.Contains(source, "return lookupSymbolShortName(value)") {
		t.Fatalf("source is not the exact implementation: %s", source)
	}
}

func TestFindSymbolQualifiedPreferenceAcrossGatewayAndFormats(t *testing.T) {
	s, ctx := qualifiedLookupFixture(t)
	for _, mode := range []ToolMode{ToolModeFull, ToolModeGateway} {
		if err := s.SetToolMode(mode); err != nil {
			t.Fatal(err)
		}
		for _, delegated := range []bool{false, true} {
			if mode == ToolModeFull && delegated {
				continue // tool_call is only advertised in gateway mode.
			}
			for _, format := range []string{"json", "compact", "compact-v2"} {
				args := map[string]any{"query": "store.lookupSymbolShortName", "limit": 1, "format": format}
				name := "find_symbol"
				if delegated {
					name, args = "tool_call", map[string]any{"name": name, "arguments": args}
				}
				text, failed := callToolRaw(t, s, ctx, name, args)
				if failed {
					t.Fatalf("%s %v %s: %s", mode, delegated, format, text)
				}
				var qualified string
				switch format {
				case "json":
					var envelope struct {
						Data struct {
							Matches []struct {
								QualifiedName string `json:"qualified_name"`
							} `json:"matches"`
						} `json:"data"`
					}
					if err := json.Unmarshal([]byte(text), &envelope); err != nil || len(envelope.Data.Matches) != 1 {
						t.Fatalf("JSON = %s, %v", text, err)
					}
					qualified = envelope.Data.Matches[0].QualifiedName
				case "compact":
					doc, err := compactfmt.Decode(text)
					if err != nil || len(doc.Section("matches").Rows) != 1 {
						t.Fatalf("compact = %s, %v", text, err)
					}
					qualified, _ = doc.Section("matches").Cell(0, "qualified_name")
				case "compact-v2":
					doc, err := compactfmt.DecodeV2(text)
					if err != nil || len(doc.Section("matches").Rows) != 1 {
						t.Fatalf("compact-v2 = %s, %v", text, err)
					}
					cell, _ := doc.Section("matches").Cell(0, "qualified_name")
					qualified = cell.Scalar
				}
				if qualified != "store.lookupSymbolShortName" {
					t.Fatalf("%s %v %s selected %q", mode, delegated, format, qualified)
				}
			}
		}
	}
}

func TestFindSymbolQualifiedPreferenceKeepsBroadSearch(t *testing.T) {
	s, ctx := qualifiedLookupFixture(t)
	for _, query := range []string{"lookupSymbolShortName", "lookupSym", "NoSuchSymbolAnywhere"} {
		find, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": query})
		search, _ := callToolJSON(t, s, ctx, "search_symbols", map[string]any{"query": query})
		if !reflect.DeepEqual(find, search) {
			t.Fatalf("unqualified/partial search changed for %q: %v / %v", query, find, search)
		}
	}
	search, _ := callToolJSON(t, s, ctx, "search_symbols", map[string]any{"query": "store.lookupSymbolShortName", "limit": 1})
	if got := recordsOf(t, search, "matches"); len(got) != 1 || got[0]["qualified_name"] != "store.LookupSymbolShortName" {
		t.Fatalf("search_symbols lost its original broad candidate order: %v", got)
	}
}
