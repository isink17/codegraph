package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestRubyPathsChangedOrAcrossBatches(t *testing.T) {
	ctx := context.Background()
	limit := sqliteBatchSize(1, 1)
	paths := func(first, last bool) []string {
		out := make([]string, 0, limit+3)
		if first {
			out = append(out, "app/early.rb")
		}
		for i := 0; len(out) <= limit; i++ {
			out = append(out, fmt.Sprintf("go/%04d.go", i))
		}
		if last {
			out = append(out, "app/late.rb")
		}
		return out
	}
	expanded := func(in []string) int {
		n := 0
		for _, path := range in {
			n += len(storedPathVariants(CanonicalRelPath(path)))
		}
		return n
	}

	for _, tc := range []struct {
		name, hit         string
		first, last, want bool
	}{
		{"early hit", "app/early.rb", true, false, true},
		{"late hit", "app/late.rb", false, true, true},
		{"all false", "", false, false, false},
		{"multiple hits", "app/early.rb", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRubyFixture(t)
			if tc.first {
				f.rb(t, "app/early.rb")
			}
			if tc.last {
				f.rb(t, "app/late.rb")
			}
			paths := paths(tc.first, tc.last)
			if got := expanded(paths); got <= limit {
				t.Fatalf("expanded path count = %d, want > batch limit %d", got, limit)
			}
			got, err := f.store.rubyPathsChanged(ctx, f.repoID, paths)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("rubyPathsChanged = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRubyPathsChangedBatchInvalidatesBoundPath(t *testing.T) {
	f := newRubyFixture(t)
	file := f.rb(t, "app/caller.rb")
	f.nested(t, file, "type", "App", "")
	f.nested(t, file, "type", "App.API", "App")
	f.nested(t, file, "class", "App.API.Service", "App.API")
	f.singleton(t, file, "App.API.Service.run", "public")
	f.nested(t, file, "class", "App.Caller", "App")
	caller := f.method(t, file, "App.Caller.f", false)
	edge := f.call(t, file, srcOf(caller), "API::Service.run", rubyConstantReceiver, 1)
	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if got := f.binding(t, edge); got == "<unresolved>" {
		t.Fatal("precondition: path edge did not bind")
	}
	f.scopeFact(t, file, graph.ScopeImportRubyConstantVisibility, "App.API", "Service", "private", true)
	limit := sqliteBatchSize(1, 1)
	paths := []string{"app/caller.rb"}
	for i := 0; len(paths) <= limit; i++ {
		paths = append(paths, fmt.Sprintf("go/%04d.go", i))
	}
	if got, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, paths, nil); err != nil {
		t.Fatal(err)
	} else if got.InvalidatedBindings == 0 {
		t.Fatalf("invalidated bindings = 0, want bound Ruby path invalidated")
	}
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("stale path binding = %s, want unresolved", got)
	}
}
