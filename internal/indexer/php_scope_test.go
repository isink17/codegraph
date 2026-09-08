//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	ts "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// phpRepo is one indexed PHP fixture tree with its store, so a test can edit
// files, run incremental updates and read the resulting edge truth.
type phpRepo struct {
	t      *testing.T
	root   string
	dbPath string
	s      *store.Store
	idx    *Indexer
	repoID int64
}

func newPHPRepo(t *testing.T, files map[string]string) *phpRepo {
	t.Helper()
	r := &phpRepo{t: t, root: t.TempDir(), dbPath: filepath.Join(t.TempDir(), "graph.db")}
	for path, content := range files {
		r.write(path, content)
	}
	s, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r.s = s
	r.idx = New(s, parser.NewRegistry(ts.NewPHP()), nil)
	if _, err := r.idx.Index(context.Background(), Options{RepoRoot: r.root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(context.Background(), r.root)
	if err != nil {
		t.Fatal(err)
	}
	r.repoID = repo.ID
	return r
}

func (r *phpRepo) write(path, content string) {
	r.t.Helper()
	full := filepath.Join(r.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *phpRepo) remove(path string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.root, path)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *phpRepo) update(paths ...string) store.ScanSummary {
	r.t.Helper()
	summary, err := r.idx.Update(context.Background(), Options{RepoRoot: r.root, ScanKind: "update", Paths: paths})
	if err != nil {
		r.t.Fatal(err)
	}
	return summary
}

func (r *phpRepo) edges() map[string]store.ExportEdge {
	r.t.Helper()
	edges, err := r.s.ExportEdgesPage(context.Background(), r.repoID, 100000, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	got := make(map[string]store.ExportEdge, len(edges))
	for _, e := range edges {
		got[e.FilePath+":"+e.DstName] = e
	}
	return got
}

func (r *phpRepo) edge(file, dst string) store.ExportEdge {
	r.t.Helper()
	all := r.edges()
	e, ok := all[file+":"+dst]
	if !ok {
		keys := make([]string, 0, len(all))
		for k := range all {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		r.t.Fatalf("missing edge %s:%s; have %v", file, dst, keys)
	}
	return e
}

func (r *phpRepo) assertTarget(file, dst, wantQName, wantStrategy string) {
	r.t.Helper()
	e := r.edge(file, dst)
	if e.DstSymbolID == nil || e.DstQualifiedName != wantQName || e.ResolutionStrategy != wantStrategy || e.ResolutionConfidence != "high" {
		r.t.Fatalf("%s:%s = target=%q strategy=%q confidence=%q, want %q via %q/high", file, dst, e.DstQualifiedName, e.ResolutionStrategy, e.ResolutionConfidence, wantQName, wantStrategy)
	}
	r.assertReferenceMatchesEdge(file, dst, e)
}

func (r *phpRepo) assertUnresolved(file, dst string) {
	r.t.Helper()
	e := r.edge(file, dst)
	if e.DstSymbolID != nil || e.ResolutionStrategy != "" || e.ResolutionConfidence != "" {
		r.t.Fatalf("%s:%s resolved to %q via %q/%q, want unresolved", file, dst, e.DstQualifiedName, e.ResolutionStrategy, e.ResolutionConfidence)
	}
	r.assertReferenceMatchesEdge(file, dst, e)
}

// assertReferenceMatchesEdge checks that the call reference sharing the edge's
// line and spelling carries the same target (or none) and the caller as context.
func (r *phpRepo) assertReferenceMatchesEdge(file, dst string, e store.ExportEdge) {
	r.t.Helper()
	raw := r.raw()
	defer raw.Close()
	var symbolID, contextID sql.NullInt64
	var n int
	if err := raw.QueryRowContext(context.Background(), `
		SELECT COUNT(*), MIN(r.symbol_id), MIN(r.context_symbol_id)
		FROM references_tbl r JOIN files f ON f.id = r.file_id
		WHERE r.repo_id = ? AND r.ref_kind = 'call' AND f.path = ? AND r.name = ? AND r.start_line = ?`,
		r.repoID, file, dst, e.Line).Scan(&n, &symbolID, &contextID); err != nil {
		r.t.Fatal(err)
	}
	if n != 1 {
		r.t.Fatalf("%s:%s line %d: %d call references, want 1", file, dst, e.Line, n)
	}
	if e.DstSymbolID == nil {
		if symbolID.Valid {
			r.t.Fatalf("%s:%s unresolved edge but reference symbol_id=%d", file, dst, symbolID.Int64)
		}
		return
	}
	if !symbolID.Valid || symbolID.Int64 != *e.DstSymbolID {
		r.t.Fatalf("%s:%s edge target %d, reference symbol_id %v", file, dst, *e.DstSymbolID, symbolID)
	}
	if !contextID.Valid || contextID.Int64 != e.SrcSymbolID {
		r.t.Fatalf("%s:%s edge source %d, reference context %v", file, dst, e.SrcSymbolID, contextID)
	}
}

func (r *phpRepo) raw() *sql.DB {
	r.t.Helper()
	raw, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		r.t.Fatal(err)
	}
	return raw
}

// projection renders every PHP edge semantically -- spelling, target qualified
// name, strategy, confidence -- so fresh and incremental graphs can be compared
// without row ids.
func phpProjection(t *testing.T, s *store.Store, repoID int64) []string {
	t.Helper()
	edges, err := s.ExportEdgesPage(context.Background(), repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, fmt.Sprintf("%s:%d:%s -> %s|%s|%s", e.FilePath, e.Line, e.DstName, e.DstQualifiedName, e.ResolutionStrategy, e.ResolutionConfidence))
	}
	sort.Strings(out)
	return out
}

func (r *phpRepo) projection() []string {
	r.t.Helper()
	return phpProjection(r.t, r.s, r.repoID)
}

// assertFreshParity indexes the current tree from scratch and requires the
// incremental graph to be indistinguishable from it.
func (r *phpRepo) assertFreshParity() {
	r.t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(r.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(r.root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
	fresh := newPHPRepo(r.t, files)
	if got, want := strings.Join(r.projection(), "\n"), strings.Join(fresh.projection(), "\n"); got != want {
		r.t.Fatalf("fresh/incremental mismatch\nfresh:\n%s\nincremental:\n%s", want, got)
	}
}

// -- BASE reproduction / acceptance -------------------------------------------

const phpAcceptanceFixture = `<?php

namespace Vendor {
    class Service {
        public static function run() {}
        private static function hidden() {}
        public function instanceRun() {}
    }
}

namespace App {
    use Vendor\Service as S;

    class Local {
        public static function run() {}
    }

    class Caller {
        private static function helper() {}

        public function f() {
            Local::run();
            S::run();
            \Vendor\Service::run();
            namespace\Local::run();
            self::helper();

            S::hidden();
            S::instanceRun();

            static::helper();
            parent::helper();

            $service->run();
            $service?->run();
        }
    }
}
`

func TestPHPStaticScopeAcceptance(t *testing.T) {
	r := newPHPRepo(t, map[string]string{"Acceptance.php": phpAcceptanceFixture})
	for _, line := range r.projection() {
		t.Log(line)
	}
	r.assertTarget("Acceptance.php", "Local::run", "App.Local.run", "php_type_scope")
	r.assertTarget("Acceptance.php", "S::run", "Vendor.Service.run", "php_alias_static")
	r.assertTarget("Acceptance.php", `\Vendor\Service::run`, "Vendor.Service.run", "php_type_scope")
	r.assertTarget("Acceptance.php", `namespace\Local::run`, "App.Local.run", "php_type_scope")
	r.assertTarget("Acceptance.php", "self::helper", "App.Caller.helper", "php_self_static")
	for _, dst := range []string{"S::hidden", "S::instanceRun", "static::helper", "parent::helper", "$service->run", "$service?->run"} {
		r.assertUnresolved("Acceptance.php", dst)
	}
	// Every PHP edge is either one of the five proven binds or unresolved:
	// zero wrong targets.
	resolved := 0
	for _, e := range r.edges() {
		if e.DstSymbolID != nil {
			resolved++
		}
	}
	if resolved != 5 {
		t.Fatalf("resolved PHP edges = %d, want exactly 5", resolved)
	}
	assertCallers(t, r.s, r.repoID, "Vendor.Service.run", "App.Caller.f")
	assertCallees(t, r.s, r.repoID, "App.Caller.f", "App.Caller.helper", "App.Local.run", "Vendor.Service.run")
	r.assertFreshParity()
}

func TestPHPInstancePropertyScopeAcceptance(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Service.php": `<?php
namespace Vendor;
class Service { public function run() {} public static function staticRun() {} private function hidden() {} }
`,
		"Caller.php": `<?php
namespace App;
use Vendor\Service as S;
class Caller {
 private S $service;
 private self $peer;
 private $unknown;
	 private function helper() {}
	 public function f() {
  $this->helper();
  $this->service->run();
  $this->service->staticRun();
  $this->service->hidden();
  $this->peer->helper();
  $this->unknown->run();
  $this?->helper();
	  $this->service?->run();
	  $service->run();
	  $service = new S();
	  $service->run();
 }
 public static function sf() { $this->helper(); }
}

`,
	})
	r.assertTarget("Caller.php", "$this->service->run", "Vendor.Service.run", "php_typed_property")
	r.assertTarget("Caller.php", "$this->peer->helper", "App.Caller.helper", "php_typed_property")
	for _, dst := range []string{"$this->service->staticRun", "$this->service->hidden", "$this->unknown->run", "$this?->helper", "$this->service?->run", "$service->run"} {
		r.assertUnresolved("Caller.php", dst)
	}
	// Static source method must not be rescued by its containing type.
	all, err := r.s.ExportEdgesPage(context.Background(), r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if e.SrcQualifiedName == "App.Caller.f" && e.DstName == "$this->helper" && (e.DstQualifiedName != "App.Caller.helper" || e.ResolutionStrategy != "php_this_instance") {
			t.Fatalf("direct this = %+v", e)
		}
	}
	for _, e := range all {
		if e.SrcQualifiedName == "App.Caller.sf" && e.DstName == "$this->helper" && e.DstSymbolID != nil {
			t.Fatalf("static source bound: %+v", e)
		}
	}
	r.assertFreshParity()
}

func TestPHPTraitInstanceScopeVeto(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Service.php": `<?php
namespace App;
class Service { public function run() {} }
`,
		"Trait.php": `<?php
namespace App;
trait T { function f() { $this->run(); } }
`,
		"Class.php": `<?php
namespace App;
class C {
 function f() { $this->run(); }
 function run() {}
}
`,
	})
	r.assertUnresolved("Trait.php", "$this->run")
	r.assertTarget("Class.php", "$this->run", "App.C.run", "php_this_instance")
	r.assertFreshParity()
}

func TestPHPThisDoesNotLeakIntoStaticClosure(t *testing.T) {
	r := newPHPRepo(t, map[string]string{"Service.php": `<?php namespace App; class Service { public function run() {} }`, "C.php": `<?php
namespace App;
class C {
 private function directTarget() {}
 private function closureTarget() {}
 private function ordinaryTarget() {}
 private function arrowTarget() {}
 private function nestedTarget() {}
 private Service $service;
 public function f() {
  $this->directTarget();
  $cb = static function () { $this->closureTarget(); };
  $cb = function () { $this->ordinaryTarget(); $this->service->run(); };
  $cb = fn() => $this->arrowTarget();
  function inner() { $this->nestedTarget(); }
 }
}
`})
	all, err := r.s.ExportEdgesPage(context.Background(), r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		switch e.DstName {
		case "$this->directTarget":
			if e.DstQualifiedName != "App.C.directTarget" || e.ResolutionStrategy != "php_this_instance" {
				t.Fatalf("direct = %+v", e)
			}
		case "$this->closureTarget", "$this->ordinaryTarget", "$this->arrowTarget", "$this->nestedTarget", "$this->service->run":
			if e.DstSymbolID != nil {
				t.Fatalf("closure leaked = %+v", e)
			}
		}
	}
}

func TestPHPThisNestedScopeIncrementalParity(t *testing.T) {
	r := newPHPRepo(t, map[string]string{"C.php": `<?php
namespace App;
class C {
 private function run() {}
 public function f() { $this->run(); }
}
`})
	r.assertTarget("C.php", "$this->run", "App.C.run", "php_this_instance")
	r.assertFreshParity()
	r.write("C.php", `<?php
namespace App;
class C {
 private function run() {}
 public function f() { $cb = static function () { $this->run(); }; }
}
`)
	r.update("C.php")
	r.assertUnresolved("C.php", "$this->run")
	r.assertFreshParity()
	r.write("C.php", `<?php
namespace App;
class C {
 private function run() {}
 public function f() { $this->run(); }
}
`)
	r.update("C.php")
	r.assertTarget("C.php", "$this->run", "App.C.run", "php_this_instance")
	r.assertFreshParity()
}

func TestPHPStaticScopeBaseShapes(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Service.php": `<?php
namespace App;

class Service {
    public static function run() {}
}
`,
		"Caller.php": `<?php
namespace App;

class Caller {
    public function f() {
        Service::run();
    }
}
`,
		"Vendor.php": `<?php
namespace Vendor;

class Service {
    public static function run() {}
}
`,
		"Alias.php": `<?php
namespace Other;

use Vendor\Service as S;

class Alias {
    private static function helper() {}
    public function f() {
        S::run();
        self::helper();
    }
}
`,
	})
	r.assertTarget("Caller.php", "Service::run", "App.Service.run", "php_type_scope")
	r.assertTarget("Alias.php", "S::run", "Vendor.Service.run", "php_alias_static")
	r.assertTarget("Alias.php", "self::helper", "Other.Alias.helper", "php_self_static")
	r.assertFreshParity()
}

// -- namespace semantics --------------------------------------------------------

func TestPHPStaticScopeNoGlobalClassFallbackInsideNamespace(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Global.php": `<?php
class Service {
    public static function run() {}
}
`,
		"Caller.php": `<?php
namespace App;

class Caller {
    public function f() {
        Service::run();
    }
}
`,
	})
	r.assertUnresolved("Caller.php", "Service::run")
	r.assertFreshParity()
}

func TestPHPStaticScopeGlobalNamespaceControl(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Global.php": `<?php
class Service {
    public static function run() {}
}

function f() {
    Service::run();
}
`,
	})
	r.assertTarget("Global.php", "Service::run", "Service.run", "php_type_scope")
	r.assertFreshParity()
}

func TestPHPStaticScopeNoParentNamespaceWalk(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"AppService.php": `<?php
namespace App;

class Service {
    public static function run() {}
}
`,
		"Sub.php": `<?php
namespace App\Sub;

class Caller {
    public function f() {
        Service::run();
        Foo\Bar::run();
    }
}
`,
		"AppFooBar.php": `<?php
namespace App\Foo;

class Bar {
    public static function run() {}
}
`,
	})
	r.assertUnresolved("Sub.php", "Service::run")
	r.assertUnresolved("Sub.php", `Foo\Bar::run`)
	r.assertFreshParity()
}

func TestPHPStaticScopeRelativeQualifiedAndAliasPrefix(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Types.php": `<?php
namespace Internal {
    class Service {
        public static function run() {}
    }
}

namespace App\Internal {
    class Service {
        public static function run() {}
    }
}

namespace Vendor\Package {
    class Service {
        public static function run() {}
    }
}

namespace App {
    class Caller {
        function f() {
            Internal\Service::run();
        }
    }
}

namespace App\Aliased {
    use Vendor\Package as Internal;

    class Caller {
        function f() {
            Internal\Service::run();
        }
    }
}

namespace App\Prefixed {
    use Vendor\Package as P;

    class Caller {
        function f() {
            P\Service::run();
            \Internal\Service::run();
        }
    }
}
`,
	})
	edges, err := r.s.ExportEdgesPage(context.Background(), r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.ExportEdge{}
	for _, e := range edges {
		got[e.SrcQualifiedName+":"+e.DstName] = e
	}
	check := func(src, dst, want, strategy string) {
		t.Helper()
		e, ok := got[src+":"+dst]
		if !ok || e.DstQualifiedName != want || e.ResolutionStrategy != strategy {
			t.Fatalf("%s:%s = %#v, want %s via %s", src, dst, e, want, strategy)
		}
	}
	check("App.Caller.f", `Internal\Service::run`, "App.Internal.Service.run", "php_type_scope")
	check("App.Aliased.Caller.f", `Internal\Service::run`, "Vendor.Package.Service.run", "php_alias_static")
	check("App.Prefixed.Caller.f", `P\Service::run`, "Vendor.Package.Service.run", "php_alias_static")
	check("App.Prefixed.Caller.f", `\Internal\Service::run`, "Internal.Service.run", "php_type_scope")
	r.assertFreshParity()
}

func TestPHPStaticScopeAbsoluteAndNamespaceKeywordBeatAliases(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Types.php": `<?php
namespace Vendor {
    class Service {
        public static function run() {}
    }
}

namespace Other {
    class Service {
        public static function run() {}
    }
}

namespace App {
    use Other\Service as Vendor;
    use Other\Service as Service;

    class Service {
        public static function run() {}
    }

    class Caller {
        function f() {
            \Vendor\Service::run();
            namespace\Service::run();
            NAMESPACE\Service::run();
        }
    }
}
`,
	})
	r.assertTarget("Types.php", `\Vendor\Service::run`, "Vendor.Service.run", "php_type_scope")
	r.assertTarget("Types.php", `namespace\Service::run`, "App.Service.run", "php_type_scope")
	r.assertTarget("Types.php", `NAMESPACE\Service::run`, "App.Service.run", "php_type_scope")
	r.assertFreshParity()
}

func TestPHPStaticScopeNamespaceKeywordInGlobalScopeFailsClosed(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Global.php": `<?php
class Service {
    public static function run() {}
}

function f() {
    namespace\Service::run();
}
`,
	})
	r.assertUnresolved("Global.php", `namespace\Service::run`)
}

// -- imports --------------------------------------------------------------------

func TestPHPStaticScopeAliasIsAuthoritative(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Caller.php": `<?php
namespace App;

use Missing\Service as S;

class S {
    public static function run() {}
}

class Caller {
    function f() {
        S::run();
    }
}
`,
	})
	r.assertUnresolved("Caller.php", "S::run")
	r.assertFreshParity()
}

func TestPHPStaticScopeAliasCaseCollisionFailsClosed(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Types.php": `<?php
namespace App;

class s {
    public static function run() {}
}

namespace App\p;

class Service {
    public static function run() {}
}

namespace Vendor;

class Service {
    public static function run() {}
}
`,
		"Caller.php": `<?php
namespace App;

use Vendor\Service as S;
use Vendor\Package as P;

class Caller {
    public function f() {
        s::run();
        S::run();
        p\Service::run();
    }
}
`,
	})
	r.assertUnresolved("Caller.php", "s::run")
	r.assertTarget("Caller.php", "S::run", "Vendor.Service.run", "php_alias_static")
	r.assertUnresolved("Caller.php", `p\Service::run`)
	r.assertFreshParity()
}

func TestPHPStaticScopeAliasCaseCollisionControls(t *testing.T) {
	caller := func(className, use string, call string) string {
		return "<?php\nnamespace App;\n\n" + use + "\nclass " + className + " {\n    function f() {\n        " + call + "\n    }\n}\n"
	}
	r := newPHPRepo(t, map[string]string{
		"Types.php": `<?php
namespace App;
class s { public static function run() {} }
namespace Vendor;
class Service { public static function run() {} }
`,
		"NoImport.php": caller("NoImport", "", "s::run();"),
		"Function.php": caller("FunctionAlias", "use function Vendor\\Service as S;", "s::run();"),
		"Const.php":    caller("ConstAlias", "use const Vendor\\Service as S;", "s::run();"),
	})
	r.assertTarget("NoImport.php", "s::run", "App.s.run", "php_type_scope")
	r.assertTarget("Function.php", "s::run", "App.s.run", "php_type_scope")
	r.assertTarget("Const.php", "s::run", "App.s.run", "php_type_scope")
	r.assertFreshParity()
}

func TestPHPStaticScopeAliasCaseCollisionImportTransitions(t *testing.T) {
	caller := func(use string) string {
		return "<?php\nnamespace App;\n\n" + use + "\nclass Caller {\n    function f() {\n        s::run();\n    }\n}\n"
	}
	r := newPHPRepo(t, map[string]string{
		"Types.php": `<?php
namespace App;
class s { public static function run() {} }
namespace Vendor;
class Service { public static function run() {} }
`,
		"Caller.php": caller(""),
	})
	r.assertTarget("Caller.php", "s::run", "App.s.run", "php_type_scope")

	r.write("Caller.php", caller("use Vendor\\Service as S;"))
	r.update("Caller.php")
	r.assertUnresolved("Caller.php", "s::run")
	r.assertFreshParity()

	r.write("Caller.php", caller(""))
	r.update("Caller.php")
	r.assertTarget("Caller.php", "s::run", "App.s.run", "php_type_scope")
	r.assertFreshParity()
}

func TestPHPStaticScopeImportOwnerIsExactNamespace(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Vendor.php": `<?php
namespace X {
    class Service {
        public static function run() {}
    }
}
namespace Y {
    class Service {
        public static function run() {}
    }
}
`,
		"Callers.php": `<?php
namespace A {
    use X\Service as S;

    class Caller {
        function f() {
            S::run();
        }
    }
}

namespace B {
    use Y\Service as S;

    class Caller {
        function f() {
            S::run();
        }
    }
}

namespace C {
    class Caller {
        function f() {
            S::run();
        }
    }
}
`,
	})
	edges, err := r.s.ExportEdgesPage(context.Background(), r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range edges {
		if e.DstName == "S::run" {
			got[e.SrcQualifiedName] = e.DstQualifiedName
		}
	}
	if got["A.Caller.f"] != "X.Service.run" || got["B.Caller.f"] != "Y.Service.run" || got["C.Caller.f"] != "" {
		t.Fatalf("S::run targets by caller = %v", got)
	}
	r.assertFreshParity()
}

func TestPHPStaticScopeGlobalImportDoesNotLeakIntoNamespace(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Vendor.php": `<?php
namespace X;
class Service {
    public static function run() {}
}
`,
		"Caller.php": `<?php
use X\Service as S;

namespace App;

class Caller {
    function f() {
        S::run();
    }
}
`,
	})
	// The `use` above the namespace declaration is owned by the global
	// namespace; a caller inside App does not see it.
	r.assertUnresolved("Caller.php", "S::run")
}

func TestPHPStaticScopeFunctionAndConstImportsAreNotTypes(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Vendor.php": `<?php
namespace Vendor {
    class Service {
        public static function run() {}
    }
}
namespace Config {
    class Service {
        public static function run() {}
    }
}
`,
		"Caller.php": `<?php
namespace App;

use function Vendor\Service as S;
use const Config\Service as T;

class Caller {
    function f() {
        S::run();
        T::run();
    }
}
`,
	})
	r.assertUnresolved("Caller.php", "S::run")
	r.assertUnresolved("Caller.php", "T::run")
	r.assertFreshParity()
}

func TestPHPStaticScopeRequireIsNotTypeEvidence(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Service.php": `<?php
namespace Lib;
class Service {
    public static function run() {}
}
`,
		"Caller.php": `<?php
namespace App;

require 'Service.php';

class Caller {
    function f() {
        Service::run();
    }
}
`,
	})
	r.assertUnresolved("Caller.php", "Service::run")
}

// -- type identity before member lookup -------------------------------------

func TestPHPStaticScopeMemberExistenceNeverPicksType(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"A.php": `<?php
namespace A;
class Service {
    public static function run() {}
}
`,
		"B.php": `<?php
namespace B;
class Service {
    public static function other() {}
}
`,
		"Caller.php": `<?php
namespace App;

use A\Service as S;
use B\Service as S;

class Service {
    public static function runWithArg($x) {}
}

class Caller {
    function f() {
        S::run();
        Service::run();
    }
}
`,
	})
	r.assertUnresolved("Caller.php", "S::run")
	r.assertUnresolved("Caller.php", "Service::run")
	r.assertFreshParity()
}

func TestPHPStaticScopeDuplicateTypeIsNotPartial(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"One.php": `<?php
namespace Vendor;
class Service {
    public static function run() {}
}
`,
		"Two.php": `<?php
namespace Vendor;
class Service {
    public static function run() {}
}
`,
		"Caller.php": `<?php
namespace App;
use Vendor\Service;
class Caller {
    function f() {
        Service::run();
    }
}
`,
	})
	r.assertUnresolved("Caller.php", "Service::run")
}

// -- staticness, visibility, reserved qualifiers, dynamic spellings -----------

func TestPHPStaticScopeStaticnessAndVisibility(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Service.php": `<?php
namespace App;

class Service {
    public static function pub() {}
    protected static function prot() {}
    private static function priv() {}
    public function inst() {}

    function f() {
        Service::priv();
        Service::prot();
        SELF::priv();
        Self::inst();
    }
}

class Caller {
    private function instHelper() {}

    function f() {
        Service::pub();
        Service::prot();
        Service::priv();
        Service::inst();
        self::instHelper();
        STATIC::instHelper();
        Parent::instHelper();
        $class::pub();
        Service::$method();
        Service::{$expr}();
        Service::CONST::pub();
        Service::pub()->chain();
        $this->instHelper();
    }
}
`,
	})
	edges, err := r.s.ExportEdgesPage(context.Background(), r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.ExportEdge{}
	for _, e := range edges {
		got[e.SrcQualifiedName+":"+e.DstName] = e
	}
	want := map[string]string{
		"App.Service.f:Service::priv":        "App.Service.priv",
		"App.Service.f:Service::prot":        "App.Service.prot",
		"App.Service.f:SELF::priv":           "App.Service.priv",
		"App.Service.f:Self::inst":           "",
		"App.Caller.f:Service::pub":          "App.Service.pub",
		"App.Caller.f:Service::prot":         "",
		"App.Caller.f:Service::priv":         "",
		"App.Caller.f:Service::inst":         "",
		"App.Caller.f:self::instHelper":      "",
		"App.Caller.f:STATIC::instHelper":    "",
		"App.Caller.f:Parent::instHelper":    "",
		"App.Caller.f:$class::pub":           "",
		"App.Caller.f:Service::$method":      "",
		"App.Caller.f:Service::$expr":        "",
		"App.Caller.f:Service::CONST::pub":   "",
		"App.Caller.f:Service::pub()->chain": "",
		"App.Caller.f:$this->instHelper":     "App.Caller.instHelper",
	}
	for key, target := range want {
		e, ok := got[key]
		if !ok {
			t.Fatalf("missing edge %s", key)
		}
		if e.DstQualifiedName != target {
			t.Errorf("%s = %q, want %q", key, e.DstQualifiedName, target)
		}
		if target == "" && (e.DstSymbolID != nil || e.ResolutionStrategy != "") {
			t.Errorf("%s bound via %q", key, e.ResolutionStrategy)
		}
	}
	r.assertFreshParity()
}

func TestPHPStaticScopeSelfNeedsContainingType(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Top.php": `<?php
namespace App;

class Top {
    public static function run() {}
}

function f() {
    self::run();
}
`,
	})
	r.assertUnresolved("Top.php", "self::run")
}

func TestPHPStaticScopeTopLevelFunctionSource(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Vendor.php": `<?php
namespace Vendor;
class Service {
    public static function run() {}
}
`,
		"Fn.php": `<?php
namespace App;

use Vendor\Service;

function f() {
    Service::run();
}
`,
	})
	r.assertTarget("Fn.php", "Service::run", "Vendor.Service.run", "php_alias_static")
	r.assertFreshParity()
}

func TestPHPStaticScopeMultiNamespaceFileUsesSourceNamespace(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Vendor.php": `<?php
namespace Vendor {
    class One {
        public static function run() {}
    }
    class Two {
        public static function run() {}
    }
}
`,
		"Callers.php": `<?php
namespace A {
    use Vendor\One as S;

    class Caller {
        function f() {
            S::run();
        }
    }
}

namespace B {
    use Vendor\Two as S;

    class Caller {
        function f() {
            S::run();
        }
    }
}
`,
	})
	edges, err := r.s.ExportEdgesPage(context.Background(), r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range edges {
		if e.DstName == "S::run" {
			got[e.SrcQualifiedName] = e.DstQualifiedName
		}
	}
	if got["A.Caller.f"] != "Vendor.One.run" || got["B.Caller.f"] != "Vendor.Two.run" {
		t.Fatalf("S::run targets = %v", got)
	}
	r.assertFreshParity()
}

// -- incremental transitions ---------------------------------------------------

func TestPHPStaticScopeCallerImportTransitions(t *testing.T) {
	vendor := `<?php
namespace A {
    class Service {
        public static function run() {}
    }
}
namespace B {
    class Service {
        public static function run() {}
    }
}
`
	caller := func(uses string) string {
		return "<?php\nnamespace App;\n" + uses + "\nclass Caller {\n    function f() {\n        S::run();\n    }\n}\n"
	}
	r := newPHPRepo(t, map[string]string{
		"Vendor.php": vendor,
		"Caller.php": caller("use A\\Service as S;"),
	})
	r.assertTarget("Caller.php", "S::run", "A.Service.run", "php_alias_static")

	r.write("Caller.php", caller("use B\\Service as S;"))
	r.update("Caller.php")
	r.assertTarget("Caller.php", "S::run", "B.Service.run", "php_alias_static")
	r.assertFreshParity()

	r.write("Caller.php", caller("use A\\Service as S;"))
	r.update("Caller.php")
	r.assertTarget("Caller.php", "S::run", "A.Service.run", "php_alias_static")
	r.assertFreshParity()

	// Import removal: no App.S exists, so nothing may bind -- not the stale A
	// target and not a global fallback.
	r.write("Caller.php", caller(""))
	r.update("Caller.php")
	r.assertUnresolved("Caller.php", "S::run")
	r.assertFreshParity()

	// Import ambiguity: two persisted imports under one local name.
	r.write("Caller.php", caller("use A\\Service as S;\nuse B\\Service as S;"))
	r.update("Caller.php")
	r.assertUnresolved("Caller.php", "S::run")
	r.assertFreshParity()

	r.write("Caller.php", caller("use B\\Service as S;"))
	r.update("Caller.php")
	r.assertTarget("Caller.php", "S::run", "B.Service.run", "php_alias_static")
	r.assertFreshParity()
}

func TestPHPStaticScopeDestinationTransitions(t *testing.T) {
	service := func(decl string) string {
		return "<?php\nnamespace App;\n\nclass Service {\n    " + decl + " function run() {}\n}\n"
	}
	r := newPHPRepo(t, map[string]string{
		"Service.php": service("public static"),
		"Caller.php": `<?php
namespace App;

class Caller {
    function f() {
        Service::run();
    }
}
`,
	})
	r.assertTarget("Caller.php", "Service::run", "App.Service.run", "php_type_scope")

	// Staticness: instance method is not a `::` target.
	r.write("Service.php", service("public"))
	r.update("Service.php")
	r.assertUnresolved("Caller.php", "Service::run")
	r.assertFreshParity()

	r.write("Service.php", service("public static"))
	r.update("Service.php")
	r.assertTarget("Caller.php", "Service::run", "App.Service.run", "php_type_scope")
	r.assertFreshParity()

	// Visibility across types.
	r.write("Service.php", service("private static"))
	r.update("Service.php")
	r.assertUnresolved("Caller.php", "Service::run")
	r.assertFreshParity()

	r.write("Service.php", service("protected static"))
	r.update("Service.php")
	r.assertUnresolved("Caller.php", "Service::run")
	r.assertFreshParity()

	r.write("Service.php", service("public static"))
	r.update("Service.php")
	r.assertTarget("Caller.php", "Service::run", "App.Service.run", "php_type_scope")
	r.assertFreshParity()

	// Delete / restore the destination file.
	r.remove("Service.php")
	r.update("Service.php")
	r.assertUnresolved("Caller.php", "Service::run")
	r.assertFreshParity()

	r.write("Service.php", service("public static"))
	r.update("Service.php")
	r.assertTarget("Caller.php", "Service::run", "App.Service.run", "php_type_scope")
	r.assertFreshParity()

	// Type ambiguity: a second active row for App.Service.
	r.write("Duplicate.php", service("public static"))
	r.update("Duplicate.php")
	r.assertUnresolved("Caller.php", "Service::run")
	r.assertFreshParity()

	r.remove("Duplicate.php")
	r.update("Duplicate.php")
	r.assertTarget("Caller.php", "Service::run", "App.Service.run", "php_type_scope")
	r.assertFreshParity()

	// A duplicate type that declares no method still makes the type identity
	// ambiguous, and its removal must rebind.
	r.write("Empty.php", "<?php\nnamespace App;\n\nclass Service {}\n")
	r.update("Empty.php")
	r.assertUnresolved("Caller.php", "Service::run")
	r.assertFreshParity()

	r.remove("Empty.php")
	r.update("Empty.php")
	r.assertTarget("Caller.php", "Service::run", "App.Service.run", "php_type_scope")
	r.assertFreshParity()

	// Source-side ambiguity: a duplicate `class Caller` makes the calling
	// method's namespace unprovable; the binding must clear and come back.
	r.write("DupCaller.php", "<?php\nnamespace App;\n\nclass Caller {}\n")
	r.update("DupCaller.php")
	r.assertUnresolved("Caller.php", "Service::run")
	r.assertFreshParity()

	r.remove("DupCaller.php")
	r.update("DupCaller.php")
	r.assertTarget("Caller.php", "Service::run", "App.Service.run", "php_type_scope")
	r.assertFreshParity()
}

// -- upgrade repair -------------------------------------------------------------

// TestPHPStaticScopeUpgradeRepairConvergesWithoutReparse simulates a database
// indexed by a P22.43 binary: parser facts present, scoped calls unresolved,
// and one PHP-owned edge carrying a generic target the PHP resolver refuses.
// A plain update over an unchanged tree must converge without any reparse and
// without --force, and a second run must be a no-op.
func TestPHPStaticScopeUpgradeRepairConvergesWithoutReparse(t *testing.T) {
	r := newPHPRepo(t, map[string]string{"Acceptance.php": phpAcceptanceFixture})
	before := r.projection()
	ctx := context.Background()
	raw := r.raw()
	defer raw.Close()
	// Old state: every PHP-owned edge unresolved, plus one owned edge wrongly
	// bound by a generic strategy (`static::helper` -> App.Caller.helper).
	if _, err := raw.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = NULL, resolution_strategy = '', resolution_confidence = '' WHERE repo_id = ?`, r.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = (SELECT id FROM symbols WHERE repo_id = ? AND qualified_name = 'App.Caller.helper'), resolution_strategy = 'exact_qualified', resolution_confidence = 'high' WHERE repo_id = ? AND dst_name = 'static::helper'`, r.repoID, r.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE references_tbl SET symbol_id = NULL, context_symbol_id = NULL WHERE repo_id = ?`, r.repoID); err != nil {
		t.Fatal(err)
	}
	key := "resolver.php_scope_repaired.v1." + strconv.FormatInt(r.repoID, 10)
	if _, err := raw.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		t.Fatal(err)
	}
	var marker string
	if err := raw.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key = ?), '')`, key).Scan(&marker); err != nil || marker != "" {
		t.Fatalf("marker before repair = %q err=%v", marker, err)
	}

	summary := r.update()
	t.Logf("summary=%+v", summary)
	if got, want := strings.Join(r.projection(), "\n"), strings.Join(before, "\n"); got != want {
		t.Fatalf("repair did not converge\nwant:\n%s\ngot:\n%s", want, got)
	}
	r.assertUnresolved("Acceptance.php", "static::helper")
	r.assertTarget("Acceptance.php", "S::run", "Vendor.Service.run", "php_alias_static")
	if err := raw.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker after repair = %q err=%v", marker, err)
	}

	// Second run: nothing to repair, nothing to parse, graph unchanged.
	if _, err := raw.ExecContext(ctx, `UPDATE edges SET resolution_confidence = 'probe' WHERE repo_id = ? AND dst_name = 'S::run'`, r.repoID); err != nil {
		t.Fatal(err)
	}
	r.update()
	if got := r.edge("Acceptance.php", "S::run").ResolutionConfidence; got != "probe" {
		t.Fatalf("second run rewrote the PHP edge (confidence=%q); repair was not a no-op", got)
	}
}

// -- downgrade safety -----------------------------------------------------------

func TestPHPStaticScopeNoCgoDowngradeStillRefused(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "A.php"), "<?php\nclass A {}\n")
	s := newProfileStore(t)
	capable := New(s.Store, parser.NewRegistry(callCapable("php", ".php", "treesitter:php:v2")), nil)
	if _, err := capable.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	before := graphSnapshot(t, s)
	degraded := New(s.Store, parser.NewRegistry(symbolsOnly("php", ".php", "heuristic:php:v1")), nil)
	if _, err := degraded.Update(ctx, Options{RepoRoot: root}); !errors.Is(err, ErrParserDowngradeRefused) {
		t.Fatalf("err = %v, want ErrParserDowngradeRefused", err)
	}
	if after := graphSnapshot(t, s); before != after {
		t.Fatalf("graph mutated by a refused downgrade")
	}
}

// -- performance fixture --------------------------------------------------------

func phpScaleFiles(n int) map[string]string {
	files := make(map[string]string, 2*n+2)
	files["Vendor.php"] = `<?php
namespace Vendor;
class Service {
    public static function run() {}
    public static function create() {}
}
class Factory {
    public static function build() {}
}
`
	files["Vendor2.php"] = `<?php
namespace Vendor2;
class Service {
    public static function run() {}
}
`
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("N%03d/Service.php", i)] = fmt.Sprintf(`<?php
namespace N%d;
class Service {
    public static function run() {}
    public function instance() {}
}
class Builder {
    public static function make() {}
}
`, i)
		uses := "use Vendor\\Service as V;\nuse Vendor\\Factory;"
		if i%5 == 0 {
			uses += "\nuse Vendor2\\Service as V;"
		}
		files[fmt.Sprintf("N%03d/Caller.php", i)] = fmt.Sprintf(`<?php
namespace N%d;
%s
class Caller {
    private static function helper() {}
    function f() {
        Service::run();
        Builder::make();
        V::run();
        Factory::build();
        \Vendor\Service::create();
        namespace\Service::run();
        self::helper();
        static::helper();
        $this->instance();
        $x?->run();
    }
}
`, i, uses)
	}
	return files
}

func BenchmarkPHPStaticScopeScale(b *testing.B) {
	files := phpScaleFiles(500)
	write := func(root string) {
		for path, content := range files {
			full := filepath.Join(root, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("fresh", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			root := b.TempDir()
			write(root)
			s, err := store.Open(filepath.Join(b.TempDir(), "graph.db"))
			if err != nil {
				b.Fatal(err)
			}
			if _, err := New(s, parser.NewRegistry(ts.NewPHP()), nil).Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
				b.Fatal(err)
			}
			_ = s.Close()
		}
	})
	root := b.TempDir()
	write(root)
	s, err := store.Open(filepath.Join(b.TempDir(), "graph.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(ts.NewPHP()), nil)
	if _, err := idx.Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		b.Fatal(err)
	}
	b.Run("caller-import-edit", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			alias := "Vendor"
			if i%2 == 1 {
				alias = "Vendor2"
			}
			content := strings.Replace(files["N001/Caller.php"], "use Vendor\\Service as V;", "use "+alias+"\\Service as V;", 1)
			if err := os.WriteFile(filepath.Join(root, "N001/Caller.php"), []byte(content), 0o644); err != nil {
				b.Fatal(err)
			}
			if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update", Paths: []string{"N001/Caller.php"}}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("destination-staticness-edit", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			content := files["N002/Service.php"]
			if i%2 == 1 {
				content = strings.Replace(content, "public static function run", "public function run", 1)
			}
			if err := os.WriteFile(filepath.Join(root, "N002/Service.php"), []byte(content), 0o644); err != nil {
				b.Fatal(err)
			}
			if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update", Paths: []string{"N002/Service.php"}}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestPHPStaticScopeScaleMetrics reports the before/after style counts on the
// deterministic scale fixture and requires zero wrong targets.
func TestPHPStaticScopeScaleMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("scale fixture")
	}
	r := newPHPRepo(t, phpScaleFiles(500))
	edges := r.edges()
	counts := map[string]int{}
	for _, e := range edges {
		counts["php_calls"]++
		switch {
		case strings.Contains(e.DstName, "->"):
			counts["member"]++
		case strings.Contains(e.DstName, "::"):
			counts["scoped"]++
		}
		if e.DstSymbolID != nil {
			counts["resolved"]++
			counts["strategy:"+e.ResolutionStrategy]++
		} else if strings.Contains(e.DstName, "::") {
			counts["scoped_unresolved"]++
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%s=%d", k, counts[k])
	}
	if counts["scoped"] < 1000 {
		t.Fatalf("scoped calls = %d, want >= 1000", counts["scoped"])
	}
	// Every N%d caller: Service::run, Builder::make, Factory::build,
	// \Vendor\Service::create, namespace\Service::run, self::helper bind;
	// V::run binds only where the alias is unique (4 of every 5 files).
	wantResolved := 500*6 + 400
	if counts["resolved"] != wantResolved {
		t.Fatalf("resolved = %d, want %d", counts["resolved"], wantResolved)
	}
	for key, e := range edges {
		if e.DstSymbolID == nil {
			continue
		}
		switch {
		case strings.HasSuffix(key, "Service::run") && !strings.HasPrefix(e.DstName, "V::"):
			if e.DstQualifiedName != strings.TrimSuffix(strings.TrimPrefix(e.SrcQualifiedName, ""), ".Caller.f")+".Service.run" {
				t.Fatalf("wrong target %s -> %s", key, e.DstQualifiedName)
			}
		case strings.HasSuffix(key, "V::run"):
			if e.DstQualifiedName != "Vendor.Service.run" {
				t.Fatalf("wrong target %s -> %s", key, e.DstQualifiedName)
			}
		}
	}
}
