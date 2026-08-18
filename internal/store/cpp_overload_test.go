package store

import "testing"

func TestCppEvidenceRefusesUnmodeledOverloadChoice(t *testing.T) {
	tests := []struct {
		name         string
		declarations []string
		definitions  []string
		wantBound    bool
	}{
		{name: "distinct_signatures_refused", declarations: []string{"(int)", "(const char*)"}, definitions: []string{"(int)", "(const char*)"}},
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
