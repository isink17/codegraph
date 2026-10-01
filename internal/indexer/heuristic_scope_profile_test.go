package indexer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/parser/heuristic"
)

func TestHeuristicScopeProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, source := range map[string]string{
		"A.kt": "package real\n/*\nimport fake.Target\n*/\nimport real.Target as Alias\nval raw = \"\"\"\n text \" interior\nimport raw.Fake\n\"\"\"\nfun run() {}\n",
		"A.cs": "using Real.Target;\n/*\nnamespace Fake;\n*/\nnamespace Real;\nclass A { string raw = \"\"\"\nnamespace Fake.Name;\n text \" interior\nusing Fake.Name;\n\"\"\"; }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	registry := parser.NewRegistry(heuristic.NewKotlin(), heuristic.NewCSharp())
	s := newProfileStore(t)
	idx := New(s.Store, registry, nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	raw := s.raw(t)
	snapshot := func() []string {
		rows, err := raw.Query(`SELECT f.path||'|'||e.package_name||'|'||COALESCE(i.source_specifier,'')||'|'||COALESCE(i.local_name,'') FROM files f JOIN file_scope_evidence e ON e.file_id=f.id LEFT JOIN scope_import_evidence i ON i.file_id=f.id WHERE f.repo_id=? ORDER BY f.path,i.source_specifier`, repo)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	fresh := snapshot()
	if !reflect.DeepEqual(fresh, []string{"A.cs|Real|Real.Target|Target", "A.kt|real|real.Target|Alias"}) {
		t.Fatalf("fresh scope = %v", fresh)
	}
	// Simulate persisted v3 scope evidence while preserving source bytes.
	for _, sql := range []string{
		"UPDATE files SET parser_profile='heuristic:'||language||':v3'",
		"UPDATE file_scope_evidence SET package_name='Fake' WHERE language='csharp'",
		"UPDATE scope_import_evidence SET source_specifier='fake.Target' WHERE language='kotlin'",
	} {
		if _, err := raw.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesIndexed != 2 || len(summary.ParserProfileLanguages) != 2 {
		t.Fatalf("profile update = %+v", summary)
	}
	if got := snapshot(); !reflect.DeepEqual(got, fresh) {
		t.Fatalf("upgraded %v != fresh %v", got, fresh)
	}
	again, err := idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v", again)
	}
}
