//go:build cgo

package treesitter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
)

func TestSvelteScriptsPreserveSourceCoordinates(t *testing.T) {
	src := []byte("<!-- café -->\r\n<script context=\"module\" lang=\"ts\">export function same(x: number) { return x; }</script>\r\n<script>import { tick } from 'svelte';\r\n\tfunction same() { tick(); }\r\n</script>\r\n<button on:click={same()} use:action={same}> {#if same()}x{/if}</button>")
	got, err := NewSvelte().Parse(context.Background(), "ui/Panel.svelte", src)
	if err != nil {
		t.Fatal(err)
	}
	if got.Language != "svelte" || len(got.Symbols) != 2 || len(got.Imports) != 1 || got.Imports[0] != "svelte" || len(got.References) != 1 || len(got.Edges) != 0 {
		t.Fatalf("unexpected output: %+v", got)
	}
	if got.Symbols[0].StableKey == got.Symbols[1].StableKey {
		t.Fatal("script scopes collided")
	}
	if got.Symbols[1].Range.StartLine != 4 || got.Symbols[1].Range.StartCol != 2 {
		t.Fatalf("range = %+v", got.Symbols[1].Range)
	}
	if got.References[0].Name != "tick" || got.References[0].Range.StartLine != 4 {
		t.Fatalf("references = %+v", got.References)
	}
	// Compare every range and source spelling to the existing TS/JS parser,
	// using independently constructed byte-preserving masks.
	blocks := svelteScripts(src)
	for i, block := range blocks {
		mask := append([]byte(nil), src...)
		for j := range mask {
			if (j < block.start || j >= block.end) && mask[j] != '\r' && mask[j] != '\n' {
				mask[j] = ' '
			}
		}
		ext := ".js"
		if block.ts {
			ext = ".ts"
		}
		want, err := NewTypeScript().Parse(context.Background(), "fixture"+ext, mask)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Symbols[i].Range, want.Symbols[0].Range) || got.Symbols[i].Signature != want.Symbols[0].Signature {
			t.Fatalf("projection changed source coordinates or spelling: %+v / %+v", got.Symbols[i], want.Symbols[0])
		}
	}
}

func TestSvelteConservativeBoundaries(t *testing.T) {
	for _, src := range []string{
		`<div><script>function fake(){}</script></div>`,
		`<style>/* <script>function fake(){}</script> */</style>`,
		`<!-- <script>function fake(){}</script> -->`,
		`<script src="./other.js">function fake(){}</script>`,
		`<script lang="coffee">function fake(){}</script>`,
		`<script lang={language}>function fake(){}</script>`,
		`<script lang="js" lang="ts">function fake(){}</script>`,
		`<script>function fake(){}`, `<script>function fake(){}</script`,
		`<script>function a(){}</script><script>function b(){}</script>`,
		`<script module context="module">function fake(){}</script>`,
		`<script module="false">function fake(){}</script>`,
		`<scripture>function fake(){}</scripture>`,
		`{#if true}<script>function fake(){}</script>{/if}`,
	} {
		t.Run(src, func(t *testing.T) {
			got, err := NewSvelte().Parse(context.Background(), "x.svelte", []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Symbols)+len(got.References)+len(got.Edges)+len(got.Imports) != 0 {
				t.Fatalf("ambiguous source produced evidence: %+v", got)
			}
		})
	}
	got, err := NewSvelte().Parse(context.Background(), "x.svelte", []byte(`<script>function real(){}</script><div broken={fake() <script>function fake(){}</script>`))
	if err != nil || len(got.Symbols) != 1 || got.Symbols[0].Name != "real" || len(got.References) != 0 {
		t.Fatalf("malformed template contaminated script: %+v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSvelte().Parse(ctx, "x.svelte", nil); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestSveltePinnedLifecycleFixture(t *testing.T) {
	src, err := os.ReadFile("svelte/fixtures/Tick.svelte")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(src)); got != "484627c57f92e2ffc0d1b54799bef8ff8f456de9c1ac661171da04e6b59e5807" {
		t.Fatalf("upstream fixture changed: %s", got)
	}
	got, err := NewSvelte().Parse(context.Background(), "Tick.svelte", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Symbols) != 1 || got.Symbols[0].Name != "handleKeydown" || got.Symbols[0].Range.StartLine != 6 {
		t.Fatalf("symbols = %+v", got.Symbols)
	}
	if !reflect.DeepEqual(got.Imports, []string{"svelte"}) {
		t.Fatalf("imports = %v", got.Imports)
	}
	found := false
	for _, ref := range got.References {
		if ref.Name == "tick" {
			found = true
			if ref.Range.StartLine != 18 {
				t.Fatalf("tick range: %+v", ref.Range)
			}
		}
		if strings.Contains(ref.Name, "handleKeydown") {
			t.Fatalf("template binding became call: %+v", ref)
		}
	}
	if !found || len(got.Edges) != 0 || !reflect.DeepEqual(got.Scope, graph.ScopeEvidence{}) {
		t.Fatalf("unexpected lifecycle evidence: %+v", got)
	}
}

func TestSvelteLifecycleHooksRemainScriptReferences(t *testing.T) {
	parsed, err := NewSvelte().Parse(context.Background(), "Hooks.svelte", []byte(`<script module lang='ts'>export function setup(): void {}</script>
<script lang='js'>
import { onMount, onDestroy, beforeUpdate, afterUpdate } from 'svelte';
function mount() { onMount(() => {}); }
function cleanup() { onDestroy(() => {}); }
beforeUpdate(() => {}); afterUpdate(() => {});
</script>
<button on:click={mount} onclick={cleanup} use:setup bind:value={mount} transition:setup />`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Symbols) != 3 || len(parsed.References) != 4 || len(parsed.Edges) != 0 {
		t.Fatalf("lifecycle evidence: %+v", parsed)
	}
	for i, name := range []string{"onMount", "onDestroy", "beforeUpdate", "afterUpdate"} {
		if parsed.References[i].Name != name {
			t.Fatalf("reference %d: %+v", i, parsed.References[i])
		}
	}
}

func TestSvelteRegistryContract(t *testing.T) {
	r := parser.NewRegistry(NewSvelte(), NewTypeScript())
	if a := r.AdapterFor(`C:\ui\Panel.SVELTE`); a == nil || a.Language() != "svelte" {
		t.Fatalf("adapter = %v", a)
	}
	if !NewSvelte().Supports("Panel.SVELTE") || NewTypeScript().Supports("Panel.svelte") || len(r.ExtensionCollisions()) != 0 {
		t.Fatal("extension ownership")
	}
	p, ok := r.ProfileForLanguage("svelte")
	if !ok || p.ID != "treesitter:svelte:v1" || p.SemanticEpoch != 1 || p.EmitsCallEdges {
		t.Fatalf("profile = %+v", p)
	}
	r.SetNoCGOCapabilities(map[string]parser.NoCGOCapability{"svelte": {}})
	for _, c := range r.Capabilities() {
		if c.ID == "svelte" {
			if c.DisplayName != "Svelte" || c.Calls == nil || *c.Calls || c.NoCGOParser == nil || *c.NoCGOParser || c.NoCGOCallGraph == nil || *c.NoCGOCallGraph || c.CallResolution != nil {
				t.Fatalf("capability = %+v", c)
			}
		}
	}
}
