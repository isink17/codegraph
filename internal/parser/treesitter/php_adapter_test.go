//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestPHPNamespaceFacts(t *testing.T) {
	src := []byte(`<?php
namespace App\Services;
use A\Service;
use B\Service as Other;
use function Utils\run as helper;
use const Config\VALUE;
class Service {
    public static function run() {}
    protected function work() { Service::run(); \App\Services\Service::run(); self::run(); static::run(); parent::run(); $service->run(); $service?->run(); run(); Helpers\run(); \App\Helpers\run(); }
}
function helper() {}
`)
	p, err := NewPHP().Parse(context.Background(), "Service.php", src)
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.Package != "App.Services" {
		t.Fatalf("package = %q", p.Scope.Package)
	}
	want := map[string]struct {
		container, visibility string
		static                *bool
	}{
		"App.Services.Service":      {"App.Services", "public", nil},
		"App.Services.Service.run":  {"App.Services.Service", "public", boolPtr(true)},
		"App.Services.Service.work": {"App.Services.Service", "protected", boolPtr(false)},
		"App.Services.helper":       {"App.Services", "public", nil},
	}
	for _, s := range p.Symbols {
		w, ok := want[s.QualifiedName]
		if !ok || s.ContainerName != w.container || s.Visibility != w.visibility || !sameBool(s.Static, w.static) {
			t.Fatalf("symbol = %+v", s)
		}
		delete(want, s.QualifiedName)
		if s.StableKey != "type:php:"+s.QualifiedName && s.StableKey != "func:php:"+s.QualifiedName {
			t.Fatalf("stable key = %q", s.StableKey)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing symbols = %v", want)
	}

	imports := map[string]struct{ imported, local, kind, owner string }{}
	for _, i := range p.Scope.Imports {
		imports[i.SourceSpecifier] = struct{ imported, local, kind, owner string }{i.ImportedName, i.LocalName, i.Kind, i.OwnerModule}
	}
	for source, want := range map[string]struct{ imported, local, kind, owner string }{
		"A.Service":    {"Service", "Service", "php_type", "App.Services"},
		"B.Service":    {"Service", "Other", "php_type", "App.Services"},
		"Utils.run":    {"run", "helper", "php_function", "App.Services"},
		"Config.VALUE": {"VALUE", "VALUE", "php_const", "App.Services"},
	} {
		if imports[source] != want {
			t.Fatalf("import %s = %+v", source, imports[source])
		}
	}

	wantCalls := []string{"Service::run", `\App\Services\Service::run`, "self::run", "static::run", "parent::run", "$service->run", "$service?->run", "run", `Helpers\run`, `\App\Helpers\run`}
	got := make(map[string]int, len(p.Edges))
	for _, e := range p.Edges {
		if e.Kind == "calls" {
			got[e.DstName]++
		}
	}
	if len(got) != len(wantCalls) {
		t.Fatalf("calls = %v", got)
	}
	for _, want := range wantCalls {
		if got[want] != 1 {
			t.Fatalf("calls = %v", got)
		}
	}
	for _, ref := range p.References {
		if ref.Kind == "call" && got[ref.Name] != 1 || ref.QualifiedName != ref.Name {
			t.Fatalf("call correlation = %v refs=%v", got, p.References)
		}
	}
}

func TestPHPBracedNamespacesStayIsolated(t *testing.T) {
	src := []byte(`<?php
namespace A { use Vendor\One as X; use Vendor\Package\{Client, Service as S}; use function Vendor\Helpers\{run, stop as halt}; use const Vendor\Config\{VALUE}; class Caller {} function run() {} }
namespace B { use Vendor\Two as X; class Caller {} function run() {} }
`)
	p, err := NewPHP().Parse(context.Background(), "shared.php", src)
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.Package != "" {
		t.Fatalf("package = %q", p.Scope.Package)
	}
	for _, q := range []string{"A.Caller", "A.run", "B.Caller", "B.run"} {
		if !hasPHPQName(p, q) {
			t.Fatalf("missing %s", q)
		}
	}
	owners := map[string]string{}
	for _, i := range p.Scope.Imports {
		owners[i.SourceSpecifier] = i.OwnerModule
	}
	if owners["Vendor.One"] != "A" || owners["Vendor.Two"] != "B" || owners["Vendor.Package.Client"] != "A" || owners["Vendor.Package.Service"] != "A" || owners["Vendor.Helpers.run"] != "A" || owners["Vendor.Helpers.stop"] != "A" || owners["Vendor.Config.VALUE"] != "A" {
		t.Fatalf("owners = %v", owners)
	}
	for _, want := range []struct{ source, imported, local, kind string }{
		{"Vendor.Package.Client", "Client", "Client", "php_type"},
		{"Vendor.Package.Service", "Service", "S", "php_type"},
		{"Vendor.Helpers.run", "run", "run", "php_function"},
		{"Vendor.Helpers.stop", "stop", "halt", "php_function"},
		{"Vendor.Config.VALUE", "VALUE", "VALUE", "php_const"},
	} {
		found := false
		for _, i := range p.Scope.Imports {
			if i.SourceSpecifier == want.source && i.ImportedName == want.imported && i.LocalName == want.local && i.Kind == want.kind {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing group import %+v", want)
		}
	}
}

func TestPHPProfileV2(t *testing.T) {
	if got := NewPHP().Profile(); got.ID != "treesitter:php:v2" || !got.EmitsCallEdges {
		t.Fatalf("profile = %+v", got)
	}
}

func TestPHPGlobalAndSemicolonNamespaceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, source, qname, container string
	}{
		{"global", "<?php class Service {} function run() {}", "Service", ""},
		{"A", "<?php namespace A; class Service {} function run() {}", "A.Service", "A"},
		{"B", "<?php namespace B; class Service {} function run() {}", "B.Service", "B"},
	} {
		p, err := NewPHP().Parse(context.Background(), tc.name+".php", []byte(tc.source))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range p.Symbols {
			if s.QualifiedName == tc.qname && s.ContainerName == tc.container {
				found = true
			}
			if tc.name == "global" && strings.Contains(s.QualifiedName, "global") {
				t.Fatalf("filename leaked: %+v", s)
			}
		}
		if !found {
			t.Fatalf("%s symbols = %+v", tc.name, p.Symbols)
		}
	}
}

func boolPtr(v bool) *bool { return &v }
func sameBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func hasPHPQName(p graph.ParsedFile, q string) bool {
	for _, s := range p.Symbols {
		if s.QualifiedName == q {
			return true
		}
	}
	return false
}
