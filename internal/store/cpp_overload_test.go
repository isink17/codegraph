package store

import "testing"

func TestCppNormalizedSignatureCallableIdentity(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"parameter_name", "(int value)", "(int)"},
		{"default_literal", "(int value = 42)", "(int)"},
		{"nested_default", "(int value = factory(1, 2))", "(int)"},
		{"template_parameter", "(const std::vector<int>& values)", "(conststd::vector<int>&)"},
		{"distinct_type", "(const char*)", "(constchar*)"},
		{"const_qualifier", "() const", "()const"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cppNormalizedSignature(tt.in); got != tt.want {
				t.Fatalf("cppNormalizedSignature(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

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
