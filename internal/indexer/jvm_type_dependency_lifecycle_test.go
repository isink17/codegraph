//go:build cgo

package indexer

import "testing"

func TestJVMTypeDependencyProviderUpdateKeepsUnknownScopeFailClosed(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Types.kt":  "package api\nclass Token\n",
		"Caller.kt": "package api\nfun use(value: Token): Token = value\n",
	})
	read := func() (int, int) {
		t.Helper()
		deps, err := r.store.JVMTypeDependencies(r.ctx, r.repoID)
		if err != nil {
			t.Fatal(err)
		}
		var consumerCount, unknownCount int
		for _, dep := range deps {
			if dep.ConsumerPath == "Caller.kt" {
				consumerCount++
				if dep.Kind == "unknown" && dep.State == "current" {
					unknownCount++
				}
			}
		}
		return consumerCount, unknownCount
	}
	if n, unknown := read(); n == 0 || unknown != n {
		t.Fatalf("initial scope should remain unknown: dependencies=%d unknown=%d", n, unknown)
	}

	r.write(t, "Types.kt", "package api\n@JvmInline value class Token(val value: kotlin.String)\n")
	summary := r.update(t)
	if summary.FilesChanged != 1 || summary.FilesIndexed != 1 {
		t.Fatalf("provider update summary = %+v", summary)
	}
	if n, unknown := read(); n == 0 || unknown != n {
		t.Fatalf("provider update escaped unknown scope: dependencies=%d unknown=%d", n, unknown)
	}
	noop := r.update(t)
	if noop.FilesChanged != 0 || noop.FilesIndexed != 0 {
		t.Fatalf("second update was not a no-op: %+v", noop)
	}
}
