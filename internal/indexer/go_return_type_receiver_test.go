package indexer

import (
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
)

const goReturnTypeServer = `package srv

type Server struct{}

func (s *Server) Start() {}

func New() *Server { return nil }
`

const goReturnTypeTypes = `package app

type Server struct{}

func (s *Server) Start() {}
func (s *Server) Stop()  {}

type Other struct{}

func (o *Other) Start() {}

type Starter interface{ Start() }

func NewServer() *Server        { return nil }
func NewNamed() (s *Server)     { return nil }
func NewPair() (*Server, error) { return nil, nil }
func NewStarter() Starter       { return &Server{} }
func Make[T any](v T) *Server   { return nil }

func NewOther() *Other { return nil }

var NewValue = func() *Other { return nil }
`

// goReturnTypeScope redeclares x in an inner block: the use above the inner
// declaration still names the outer x.
const goReturnTypeScope = `package app

func scoped() {
	x := NewServer()
	x.Start()
	{
		x.Start()
		x := NewOther()
		x.Start()
	}
}
`

// goReturnTypeApp compiles with go vet apart from missing.Missing, which is the
// point of that case.
const goReturnTypeApp = `package app

import (
	"errors"

	"example.com/project/srv"
)

func use() {
	short := NewServer()
	short.Start()
	var declared = NewServer()
	declared.Stop()
	NewServer().Start()
	named := NewNamed()
	named.Start()
	remote := srv.New()
	remote.Start()
	pair, _ := NewPair()
	pair.Start()
	iface := NewStarter()
	iface.Start()
	generic := Make(1)
	generic.Start()
	value := NewValue()
	value.Start()
	missing := NewServer()
	missing.Missing()
	external := errors.New("x")
	_ = external.Error()
}

func shadowed(NewServer func() *Other) {
	local := NewServer()
	local.Start()
	NewServer().Start()
}
`

func TestGoReceiverThroughFunctionResult(t *testing.T) {
	runGoReceiverThroughFunctionResult(t, goNativeRegistry())
}

func runGoReceiverThroughFunctionResult(t *testing.T, reg *parser.Registry) {
	t.Helper()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod":       goShadowModule,
		"srv/srv.go":   goReturnTypeServer,
		"app/types.go": goReturnTypeTypes,
		"app/app.go":   goReturnTypeApp,
		"app/scope.go": goReturnTypeScope,
		"app/fixture_test.go": `package app

func NewFixture() *Server { return nil }

func helper() {
	f := NewFixture()
	f.Stop()
}
`,
		"app/prod.go": `package app

func prod() {
	p := NewFixture()
	p.Stop()
}
`,
	})
	const unresolved = "<unresolved> [/]"
	for _, c := range []struct{ dst, want string }{
		{"short.Start", "app/types.go:app.Server.Start [go_receiver_scope/high]"},
		{"declared.Stop", "app/types.go:app.Server.Stop [go_receiver_scope/high]"},
		{"NewServer().Start", "app/types.go:app.Server.Start [go_receiver_scope/high]"},
		{"named.Start", "app/types.go:app.Server.Start [go_receiver_scope/high]"},
		{"remote.Start", "srv/srv.go:srv.Server.Start [go_receiver_scope/high]"},
		{"pair.Start", unresolved},
		{"iface.Start", unresolved},
		{"generic.Start", unresolved},
		{"value.Start", unresolved},
		{"missing.Missing", unresolved},
		{"external.Error", unresolved},
		{"local.Start", unresolved},
	} {
		if got := r.edgeState(t, "app/app.go", c.dst); got != c.want {
			t.Errorf("%s: %s, want %s", c.dst, got, c.want)
		}
	}
	// A constructor declared in a test file answers only a test caller.
	if got, want := r.edgeState(t, "app/fixture_test.go", "f.Stop"), "app/types.go:app.Server.Stop [go_receiver_scope/high]"; got != want {
		t.Errorf("test caller of a test constructor: %s, want %s", got, want)
	}
	if got := r.edgeState(t, "app/prod.go", "p.Stop"); got != unresolved {
		t.Errorf("production caller of a test constructor: %s, want %s", got, unresolved)
	}
	scoped := map[string]int{}
	for _, line := range r.projection(t) {
		if strings.HasPrefix(line, `edge app/scope.go "x.Start" => `) {
			scoped[strings.TrimPrefix(line, `edge app/scope.go "x.Start" => `)]++
		}
	}
	if scoped["app/types.go:app.Server.Start [go_receiver_scope/high]"] != 2 || scoped["app/types.go:app.Other.Start [go_receiver_scope/high]"] != 1 {
		t.Errorf("inner redeclaration claimed a use above it: %v", scoped)
	}
	// A parameter named like the function owns the chained call, so it gets
	// no edge at all rather than one a strategy could bind to Server.Start.
	count := 0
	for _, line := range r.projection(t) {
		if line == `edge app/app.go "NewServer().Start" => app/types.go:app.Server.Start [go_receiver_scope/high]` {
			count++
		}
	}
	if count != 1 {
		t.Errorf("chained call edges bound = %d, want 1 (the shadowed one emits none)", count)
	}
}

// TestGoReceiverThroughFunctionResultIncrementalParity changes only the file
// declaring the constructor; the caller's bytes never change, and every step
// must equal a fresh index of the same tree.
func TestGoReceiverThroughFunctionResultIncrementalParity(t *testing.T) {
	reg := goNativeRegistry()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod": goShadowModule,
		"a/types.go": `package a

type Server struct{}
type Other struct{}
type Starter interface{ Start() }

func (s *Server) Start() {}
func (o *Other) Start()  {}
`,
		"a/ctor.go": `package a

func NewServer() *Server { return nil }
`,
		"a/use.go": `package a

func use() {
	s := NewServer()
	s.Start()
	NewServer().Start()
}
`,
	})
	check := func(step, want string) {
		t.Helper()
		for _, dst := range []string{"s.Start", "NewServer().Start"} {
			if got := r.edgeState(t, "a/use.go", dst); got != want {
				t.Errorf("%s: %s: %s, want %s", step, dst, got, want)
			}
		}
		r.assertFreshParity(t, reg, step)
	}
	check("baseline", "a/types.go:a.Server.Start [go_receiver_scope/high]")
	if summary, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"}); err != nil {
		t.Fatal(err)
	} else if summary.FilesIndexed != 0 {
		t.Fatalf("no-op update indexed %d files", summary.FilesIndexed)
	}
	check("no-op update", "a/types.go:a.Server.Start [go_receiver_scope/high]")

	r.write(t, "a/ctor.go", "package a\n\nfunc NewServer() *Other { return nil }\n")
	r.update(t)
	check("result type changes", "a/types.go:a.Other.Start [go_receiver_scope/high]")

	r.write(t, "a/ctor.go", "package a\n\nfunc NewServer() Starter { return nil }\n")
	r.update(t)
	check("result becomes an interface", "<unresolved> [/]")

	r.write(t, "a/ctor.go", "package a\n\nfunc NewServer() *Server { return nil }\n")
	r.update(t)
	check("result restored", "a/types.go:a.Server.Start [go_receiver_scope/high]")

	r.write(t, "a/dup.go", "package a\n\nfunc NewServer() *Other { return nil }\n")
	r.update(t)
	check("second declaration of the function", "<unresolved> [/]")

	r.remove(t, "a/dup.go")
	r.update(t)
	check("second declaration removed", "a/types.go:a.Server.Start [go_receiver_scope/high]")

	r.remove(t, "a/ctor.go")
	r.update(t)
	check("function deleted", "<unresolved> [/]")
}

// TestGoReceiverThroughImportedFunctionResultIncrementalParity changes only the
// imported constructor's file. An unresolved caller must become bound when the
// result turns into a concrete type, exactly as a fresh index binds it.
func TestGoReceiverThroughImportedFunctionResultIncrementalParity(t *testing.T) {
	reg := goNativeRegistry()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod": goShadowModule,
		"srv/types.go": `package srv

type Server struct{}
type Other struct{}
type Starter interface{ Start() }

func (s *Server) Start() {}
func (o *Other) Start()  {}
`,
		"srv/ctor.go": "package srv\n\nfunc New() Starter { return nil }\n",
		"app/use.go": `package app

import "example.com/project/srv"

func use() {
	s := srv.New()
	s.Start()
}
`,
	})
	for _, step := range []struct{ name, result, want string }{
		{"interface result", "", "<unresolved> [/]"},
		{"result becomes *Server", "*Server", "srv/types.go:srv.Server.Start [go_receiver_scope/high]"},
		{"result becomes *Other", "*Other", "srv/types.go:srv.Other.Start [go_receiver_scope/high]"},
		{"result back to the interface", "Starter", "<unresolved> [/]"},
		{"result becomes *Server again", "*Server", "srv/types.go:srv.Server.Start [go_receiver_scope/high]"},
	} {
		if step.result != "" {
			r.write(t, "srv/ctor.go", "package srv\n\nfunc New() "+step.result+" { return nil }\n")
			r.update(t)
		}
		if got := r.edgeState(t, "app/use.go", "s.Start"); got != step.want {
			t.Errorf("%s: %s, want %s", step.name, got, step.want)
		}
		r.assertFreshParity(t, reg, step.name)
	}
}
