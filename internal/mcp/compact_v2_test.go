package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/compactfmt"
	"github.com/isink17/codegraph/internal/detail"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/tokenest"
)

func compactFixtureCalls() map[string]map[string]any {
	return map[string]map[string]any{
		"find_symbol": {"query": "Area"}, "search_symbols": {"query": "Helper"},
		"find_callers": {"symbol": "Helper"}, "find_callees": {"symbol": "lib.AlsoUseHelper"},
		"get_impact_radius":  {"symbols": []any{"Helper", "missing,a"}},
		"find_related_tests": {"files": []any{"lib/widget.go", "missing,a", "missing,b"}},
		"find_dead_code":     {"limit": 5}, "list_files": {"limit": 5}, "trace_dependencies": {"symbol": "Helper"},
	}
}

// Goldens captured on 89574592102522969124ebba527c820cd66e7fd4 before v2 edits.
// Pins the old serializers before introducing v2: v1's ambiguous cells are a
// compatibility contract, not an encoding v2 may silently replace.
func TestCompactV1AndJSONGoldenCompatibility(t *testing.T) {
	// One file per scan pins database IDs without changing or normalizing wire bytes.
	s, ctx := detailFixtureWithStableIDs(t, true)
	for tool, args := range compactFixtureCalls() {
		t.Run(tool, func(t *testing.T) {
			original, failed := callToolRaw(t, s, ctx, tool, args)
			if failed {
				t.Fatal(original)
			}
			args["format"] = "json"
			explicit, failed := callToolRaw(t, s, ctx, tool, args)
			if failed || explicit != original {
				t.Fatal("default JSON changed", explicit)
			}
			for _, format := range []string{"json", "compact"} {
				args["format"] = format
				got, failed := callToolRaw(t, s, ctx, tool, args)
				if failed {
					t.Fatal(got)
				}
				path := filepath.Join("testdata", "compact_compat", tool+"."+format)
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if got != string(want) {
					t.Fatalf("%s bytes changed\ngot %s\nwant %s", format, got, want)
				}
			}
		})
	}
}

func TestCompactPresenceOriginalCollision(t *testing.T) {
	encode := func(values []string) string {
		d := compactfmt.NewDocument("test")
		writePresence(d, map[string]any{"missing": values})
		out, err := d.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, pair := range [][2][]string{{{"a,b", "c"}, {"a", "b,c"}}, {{}, {""}}} {
		if reflect.DeepEqual(pair[0], pair[1]) || encode(pair[0]) != encode(pair[1]) {
			t.Fatal("original v1 collision no longer reproduced")
		}
		a, _ := json.Marshal(pair[0])
		b, _ := json.Marshal(pair[1])
		if strings.EqualFold(string(a), string(b)) {
			t.Fatal("JSON lost boundaries")
		}
	}
}

func TestCompactV2EveryToolDirectGatewayAndShape(t *testing.T) {
	calls := compactFixtureCalls()
	if len(calls) != len(compactCapableTools) {
		t.Fatal("v2 fixture coverage differs from advertised tools")
	}
	for name := range compactCapableTools {
		if calls[name] == nil {
			t.Fatalf("missing complete v2 contract for %s", name)
		}
	}
	s, ctx := detailFixture(t)
	for tool, args := range compactFixtureCalls() {
		t.Run(tool, func(t *testing.T) {
			args["format"] = "json"
			jsonText, failed := callToolRaw(t, s, ctx, tool, args)
			if failed {
				t.Fatal(jsonText)
			}
			var envelope struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(jsonText), &envelope); err != nil {
				t.Fatal(err)
			}
			args["format"] = "compact"
			old, failed := callToolRaw(t, s, ctx, tool, args)
			if failed {
				t.Fatal(old)
			}
			v1, err := compactfmt.Decode(old)
			if err != nil {
				t.Fatal(err)
			}
			args["format"] = "compact-v2"
			text, failed := callToolRaw(t, s, ctx, tool, args)
			if failed {
				t.Fatal(text)
			}
			v2, err := compactfmt.DecodeV2(text)
			if err != nil {
				t.Fatal(err)
			}
			if v2.Tool != tool || len(v2.Sections) != len(v1.Sections) {
				t.Fatal("shape lost sections")
			}
			for i, sec := range v2.Sections {
				prior := v1.Sections[i]
				if sec.Name != prior.Name || len(sec.Rows) != len(prior.Rows) || len(sec.Columns) != len(prior.Columns) {
					t.Fatalf("section shape changed: %+v", sec)
				}
				for j, col := range sec.Columns {
					expected := prior.Columns[j]
					if col == "missing_json" {
						if expected != "missing" || sec.Types[j] != compactfmt.StringArray {
							t.Fatal("array declaration")
						}
					} else if col != expected || sec.Types[j] != compactfmt.Scalar {
						t.Fatal("scalar declaration")
					}
					for k, row := range sec.Rows {
						if col == "missing_json" {
							var missing []string
							fields := envelope.Data
							if seed, ok := fields["seed_presence"]; ok {
								if err := json.Unmarshal(seed, &fields); err != nil {
									t.Fatal(err)
								}
							}
							if raw := fields["missing"]; raw != nil {
								if err := json.Unmarshal(raw, &missing); err != nil {
									t.Fatal(err)
								}
							}
							if missing == nil {
								missing = []string{}
							}
							if !reflect.DeepEqual(row[j].Array, missing) {
								t.Fatalf("array != JSON: %#v / %#v", row[j].Array, missing)
							}
						} else if row[j].Scalar != prior.Rows[k][j] {
							t.Fatalf("scalar changed: %q / %q", row[j].Scalar, prior.Rows[k][j])
						}
					}
				}
			}
			again, failed := callToolRaw(t, s, ctx, tool, args)
			if failed || again != text {
				t.Fatal("nondeterministic v2")
			}
			if err := s.SetToolMode(ToolModeGateway); err != nil {
				t.Fatal(err)
			}
			forwarded, failed := callToolRaw(t, s, ctx, "tool_call", map[string]any{"name": tool, "arguments": args})
			if failed || forwarded != text {
				t.Fatal("gateway altered v2", forwarded)
			}
			if err := s.SetToolMode(ToolModeFull); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompactV2PresenceAndImpactArrays(t *testing.T) {
	values := [][]string{nil, {}, {""}, {"a", ""}, {"a"}, {"a,b", "c"}, {"a", "b,c"}, {"\t", "\r\n", `\`, "雪"}, {"@section injected", "b", "b"}}
	for _, missing := range values {
		for _, shape := range []string{"presence", "impact-typed", "impact-map"} {
			d := compactfmt.NewDocumentV2("test")
			switch shape {
			case "presence":
				writePresence(d, map[string]any{"requested": 3, "found": 1, "missing": missing})
			default:
				seed := any(store.ImpactSeedPresence{Requested: 3, Found: 1, Missing: missing})
				if shape == "impact-map" {
					seed = map[string]any{"requested": 3, "found": 1, "missing": missing}
				}
				data := map[string]any{"symbols": []detail.Symbol{}, "files": []string{}, "summary": map[string]any{"affected_symbols": 0, "affected_files": 0, "returned_symbols": 0, "returned_files": 0, "unresolved_edges": 0, "unresolved_names": 0, "offset": 0, "truncated": false}, "seed_presence": seed}
				if err := writeImpactSections(d, data); err != nil {
					t.Fatal(err)
				}
			}
			text, err := d.Encode()
			if err != nil {
				t.Fatal(err)
			}
			out, err := compactfmt.DecodeV2(text)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := out.Section("presence").Cell(0, "missing_json")
			if !ok {
				t.Fatal(text)
			}
			want := missing
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(got.Array, want) {
				t.Fatal(got.Array, want)
			}
		}
	}
}

func TestCompactV2LimitsAndUnsupportedFormats(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"graph_stats", map[string]any{"format": "compact-v2"}},
		{"search_semantic", map[string]any{"query": "x", "format": "compact-v2"}},
		{"search_symbols", map[string]any{"query": "x", "format": "compact-v3"}},
		{"search_symbols", map[string]any{"query": "x", "format": "compact-v2", "detail": "full"}},
		{"search_symbols", map[string]any{"query": "x", "format": "compact-v2", "limit": 501}},
		{"get_impact_radius", map[string]any{"symbols": make([]string, 501), "format": "compact-v2"}},
	} {
		raw, _ := json.Marshal(tc.args)
		if err := validateToolArguments(tc.tool, raw); err == nil {
			t.Fatal("accepted unsupported request", tc)
		}
	}
	s, ctx := capabilityFixture(t, true)
	text, failed := callToolRaw(t, s, ctx, "find_callers", map[string]any{"symbol": "Helper", "format": "compact-v2"})
	if failed {
		t.Fatal(text)
	}
	d, err := compactfmt.DecodeV2(text)
	if err != nil {
		t.Fatal(err)
	}
	if sec := d.Section("limitations"); sec == nil || len(sec.Rows) == 0 {
		t.Fatal("lost limitations")
	}
	text, failed = callToolRaw(t, s, ctx, "trace_dependencies", map[string]any{"symbol": "Helper", "offset": 10000, "format": "compact-v2"})
	if failed {
		t.Fatal(text)
	}
	d, err = compactfmt.DecodeV2(text)
	if err != nil {
		t.Fatal(err)
	}
	presence := d.Section("presence")
	offset, _ := presence.Cell(0, "offset")
	total, _ := presence.Cell(0, "total")
	truncated, _ := presence.Cell(0, "truncated")
	if offset.Scalar != total.Scalar || truncated.Scalar != "false" || len(d.Section("dependencies").Rows) != 0 {
		t.Fatal("lost normalized past-end pagination", presence)
	}
}

func TestCompactV2UsageAndSizes(t *testing.T) {
	s := newBulkUsageTestServer(t)
	for _, format := range []string{"json", "compact", "compact-v2"} {
		args := map[string]any{"query": "Widget", "limit": 40, "format": format}
		failed, text := callResult(t, s, "search_symbols", args)
		if failed {
			t.Fatal(text)
		}
		report := usageReport(t, s, false)
		found := false
		for _, row := range report.Tools {
			if row.Name == "search_symbols" && row.Format == format {
				found = true
				if row.Calls != 1 || row.ResponseBytes != int64(len(text)) || row.ResponseEstimatedTokens != tokenest.FromBytes(len(text)) {
					t.Fatal("incorrect meter", row)
				}
			}
		}
		if !found {
			t.Fatal("missing format bucket", format)
		}
		t.Logf("40-symbol fixture %s: %d bytes, %d estimated tokens", format, len(text), tokenest.FromBytes(len(text)))
	}
	server, ctx := detailFixture(t)
	for tool, args := range compactFixtureCalls() {
		for _, format := range []string{"json", "compact", "compact-v2"} {
			args["format"] = format
			out, failed := callToolRaw(t, server, ctx, tool, args)
			if failed {
				t.Fatal(out)
			}
			t.Logf("%s %s: %d bytes, %d estimated tokens", tool, format, len(out), tokenest.FromBytes(len(out)))
		}
	}
}
