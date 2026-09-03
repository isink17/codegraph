//go:build cgo

package treesitter

import (
	"context"
	"reflect"
	"testing"
)

func TestTypeScriptCallEvidenceUsesSemanticCalleeNames(t *testing.T) {
	const oneLine = `function run() { fn(); client.request("/health"); a.b.c(); factory()(); factory().run(); arr[0](); optional?.call(); }`
	const multiLine = "function run() {\n  client\n    .request(\"/health\");\n}"
	parse := func(src string) []string {
		pf, err := NewTypeScript().Parse(context.Background(), "fixture.ts", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, edge := range pf.Edges {
			got = append(got, edge.DstName)
		}
		return got
	}
	if got := parse(oneLine); len(got) != 5 || got[0] != "fn" || got[1] != "client.request" || got[2] != "a.b.c" || got[3] != "factory" || got[4] != "factory" {
		t.Fatalf("calls = %v, want [fn client.request a.b.c factory factory]", got)
	}
	if got := parse(multiLine); len(got) != 1 || got[0] != "client.request" {
		t.Fatalf("multiline calls = %v, want [client.request]", got)
	}
	pf, err := NewTypeScript().Parse(context.Background(), "fixture.ts", []byte(multiLine))
	if err != nil || len(pf.Edges) != 1 || pf.Edges[0].Line != 2 || len(pf.References) != 1 || pf.References[0].Range.StartLine != 2 {
		t.Fatalf("source attribution = edges %#v references %#v err=%v", pf.Edges, pf.References, err)
	}
}

func TestTypeScriptIntrinsicCallEvidencePreservesQualifiedNames(t *testing.T) {
	p, err := NewTypeScript().Parse(context.Background(), "intrinsics.ts", []byte(`function run(x: unknown) {
  JSON.stringify(x);
  Array.isArray(x);
  Object.keys(x as object);
  Math.max(1, 2);
  Promise.resolve(x);
}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, edge := range p.Edges {
		got = append(got, edge.DstName)
	}
	want := []string{"JSON.stringify", "Array.isArray", "Object.keys", "Math.max", "Promise.resolve"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("intrinsic calls = %v, want %v", got, want)
	}
}

func TestTypeScriptReExportEvidenceIsDistinctFromImports(t *testing.T) {
	src := `import { helper } from "./helper";
export { helper } from "./helper";
export { helper as publicHelper } from "./helper";
export * from "./helper";
export * as helpers from "./helper";
export { helper };`
	pf, err := NewTypeScript().Parse(context.Background(), "fixture.ts", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Imports) != 1 || pf.Imports[0] != "./helper" {
		t.Fatalf("imports = %#v", pf.Imports)
	}
	if len(pf.ReExports) != 4 {
		t.Fatalf("re-exports = %#v", pf.ReExports)
	}
	if pf.ReExports[0].Source != "./helper" || pf.ReExports[0].Name != "helper" || pf.ReExports[0].ExportedName != "helper" {
		t.Fatalf("named re-export = %#v", pf.ReExports[0])
	}
	if pf.ReExports[1].Name != "helper" || pf.ReExports[1].ExportedName != "publicHelper" {
		t.Fatalf("aliased re-export = %#v", pf.ReExports[1])
	}
	if !pf.ReExports[2].Wildcard || pf.ReExports[2].Source != "./helper" || pf.ReExports[2].Name != "" {
		t.Fatalf("wildcard re-export = %#v", pf.ReExports[2])
	}
	if !pf.ReExports[3].Wildcard || pf.ReExports[3].ExportedName != "helpers" {
		t.Fatalf("namespace re-export = %#v", pf.ReExports[3])
	}
}

func TestTypeScriptCallEvidenceJavaScriptParity(t *testing.T) {
	for _, path := range []string{"fixture.js", "fixture.ts"} {
		pf, err := NewTypeScript().Parse(context.Background(), path, []byte("function run() { client\n  .request('/health'); factory()(); }"))
		if err != nil {
			t.Fatal(err)
		}
		if len(pf.Edges) != 2 || pf.Edges[0].DstName != "client.request" || pf.Edges[1].DstName != "factory" {
			t.Fatalf("%s calls = %#v", path, pf.Edges)
		}
	}
	pf, err := NewTypeScript().Parse(context.Background(), "fixture.js", []byte(`export { helper } from "./helper"; export * from "./helper";`))
	if err != nil || len(pf.ReExports) != 2 || !pf.ReExports[1].Wildcard {
		t.Fatalf("javascript re-exports = %#v, err=%v", pf.ReExports, err)
	}
}
