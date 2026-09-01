package store

import "testing"

func TestCppEvidenceRefusesUnmodeledOverloadChoice(t *testing.T) {
	tests := []struct {
		name         string
		declarations []string
		definitions  []string
		wantBound    bool
	}{
		{name: "distinct_signatures_refused", declarations: []string{"(int)", "(const char*)"}, definitions: []string{"(int)"}},
		{name: "one_signature_resolves", declarations: []string{"(int)"}, definitions: []string{"(int)"}, wantBound: true},
		{name: "duplicate_same_signature_resolves", declarations: []string{"(int)", "(int)"}, definitions: []string{"(int)"}, wantBound: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newGateFixture(t)
			file := f.file(t, "caller.cpp", "cpp")
			caller := f.symbolKind(t, file, "caller", "caller", "function", "cpp")
			for _, signature := range tt.declarations {
				id := f.symbolKind(t, file, "foo", "foo", "declaration", "cpp")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature = ? WHERE id = ?`, signature, id); err != nil {
					t.Fatalf("set declaration signature: %v", err)
				}
			}
			for _, signature := range tt.definitions {
				id := f.symbolKind(t, file, "foo", "foo", "function", "cpp")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature = ? WHERE id = ?`, signature, id); err != nil {
					t.Fatalf("set definition signature: %v", err)
				}
			}
			edge := f.edge(t, file, caller, "foo")
			if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
				t.Fatalf("ResolveEdges() error = %v", err)
			}
			_, bound := f.dstSymbolID(t, edge)
			if bound != tt.wantBound {
				t.Fatalf("bound = %v, want %v", bound, tt.wantBound)
			}
		})
	}
}

func TestCppEvidenceRefusesQualifiedOverloadChoice(t *testing.T) {
	f := newGateFixture(t)
	file := f.file(t, "caller.cpp", "cpp")
	caller := f.symbolKind(t, file, "caller", "caller", "function", "cpp")
	for _, sig := range []string{"(int value)", "(const char* text)"} {
		id := f.symbolKind(t, file, "foo", "A::foo", "declaration", "cpp")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature = ? WHERE id = ?`, sig, id); err != nil {
			t.Fatal(err)
		}
	}
	id := f.symbolKind(t, file, "foo", "A::foo", "function", "cpp")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature = ? WHERE id = ?`, "(int renamed)", id); err != nil {
		t.Fatal(err)
	}
	edge := f.edge(t, file, caller, "A::foo")
	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, bound := f.dstSymbolID(t, edge); bound {
		t.Fatal("qualified overloaded call resolved without argument evidence")
	}
}

func TestCppQualifiedOverloadParityAcrossEntrypoints(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newGateFixture(t)
			file := f.file(t, "caller.cpp", "cpp")
			caller := f.symbolKind(t, file, "caller", "caller", "function", "cpp")
			for _, sig := range []string{"(int)", "(const char*)"} {
				id := f.symbolKind(t, file, "foo", "A::foo", "declaration", "cpp")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature = ? WHERE id = ?`, sig, id); err != nil {
					t.Fatal(err)
				}
			}
			id := f.symbolKind(t, file, "foo", "A::foo", "function", "cpp")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature = ? WHERE id = ?`, "(int)", id); err != nil {
				t.Fatal(err)
			}
			edge := f.edge(t, file, caller, "A::foo")
			switch entry {
			case "full":
				if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
					t.Fatal(err)
				}
			case "paths":
				if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"caller.cpp"}); err != nil {
					t.Fatal(err)
				}
			case "names":
				if _, err := f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{"A::foo"}); err != nil {
					t.Fatal(err)
				}
			case "paths+names":
				if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"caller.cpp"}, []string{"A::foo"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, bound := f.dstSymbolID(t, edge); bound {
				t.Fatal("qualified overloaded call resolved without argument evidence")
			}
		})
	}
}

func TestCppQualifiedPositiveParityAcrossEntrypoints(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newGateFixture(t)
			file := f.file(t, "caller.cpp", "cpp")
			caller := f.symbolKind(t, file, "caller", "caller", "function", "cpp")
			decl := f.symbolKind(t, file, "foo", "A::foo", "declaration", "cpp")
			def := f.symbolKind(t, file, "foo", "A::foo", "function", "cpp")
			for _, id := range []int64{decl, def} {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature = ? WHERE id = ?`, "(int)", id); err != nil {
					t.Fatal(err)
				}
			}
			edge := f.edge(t, file, caller, "A::foo")
			switch entry {
			case "full":
				if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
					t.Fatal(err)
				}
			case "paths":
				if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"caller.cpp"}); err != nil {
					t.Fatal(err)
				}
			case "names":
				if _, err := f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{"A::foo"}); err != nil {
					t.Fatal(err)
				}
			case "paths+names":
				if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"caller.cpp"}, []string{"A::foo"}); err != nil {
					t.Fatal(err)
				}
			}
			if got, bound := f.dstSymbolID(t, edge); !bound || got != def {
				t.Fatalf("qualified positive bound = (%d, %v), want (%d, true)", got, bound, def)
			}
		})
	}
}
