package store

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/terraform"
)

func TestHCLScopeVetoSQLMatchesGoTwin(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	files := map[string]int64{}
	for _, language := range []string{"hcl", "go"} {
		id, err := insertTestFileLang(ctx, s, repo.ID, "src."+language, language)
		if err != nil {
			t.Fatal(err)
		}
		files[language] = id
	}
	caller, err := insertTestSymbolLang(ctx, s, repo.ID, files["go"], "caller", "caller", "go")
	if err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"hcl", "go"} {
		for _, kind := range []string{EdgeKindReferences, EdgeKindCalls, EdgeKindCrossLanguageRef} {
			res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line) VALUES(?, ?, 'var.x', ?, '', ?, 1)`,
				repo.ID, caller, kind, files[language])
			if err != nil {
				t.Fatal(err)
			}
			id, _ := res.LastInsertId()
			var sqlOwned bool
			if err := s.db.QueryRowContext(ctx, `SELECT NOT (`+hclScopeVetoSQL+`) FROM edges JOIN files f ON f.id = edges.file_id WHERE edges.id = ?`, id).Scan(&sqlOwned); err != nil {
				t.Fatal(err)
			}
			goOwned := hclScopeOwned(edgeTarget{srcLanguage: language, edgeKind: kind})
			if want := language == "hcl"; sqlOwned != want || goOwned != want {
				t.Fatalf("%s/%s: SQL owned=%v, Go owned=%v, want %v", language, kind, sqlOwned, goOwned, want)
			}
		}
	}
}

// The pass binds a reference only to the single declaration of its address in
// the referring file's own directory.
func TestResolveHCLScopeBindsOnlyAUniqueSameDirectoryDeclaration(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	file := func(path string) int64 {
		id, err := insertTestFileLang(ctx, s, repo.ID, path, "hcl")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	symbol := func(file int64, kind, qname string) int64 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key) VALUES(?, ?, 'hcl', ?, ?, ?, 1, 1, 9, 1, ?)`,
			repo.ID, file, kind, qname, qname, "tf:"+kind+":"+qname)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	edge := func(file, src int64, dst, evidence string) int64 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line, start_col) VALUES(?, ?, ?, 'references', ?, ?, 2, 1)`,
			repo.ID, src, dst, evidence, file)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	ref := graph.HCLTerraformReferenceEvidence

	aMain, aOther, bMain := file("a/main.tf"), file("a/other.tf"), file("b/main.tf")
	region := symbol(aMain, terraform.KindVariable, "var.region")
	symbol(bMain, terraform.KindVariable, "var.region")
	symbol(aMain, terraform.KindResource, "aws_instance.web")
	symbol(aOther, terraform.KindResource, "aws_instance.web")
	output := symbol(aOther, terraform.KindOutput, "output.id")
	symbol(bMain, terraform.KindLocal, "local.only_in_b")
	// A generic .hcl file in the directory is not part of the module.
	symbol(file("a/packer.hcl"), terraform.KindVariable, "var.region")

	sameDir := edge(aOther, output, "var.region", ref)
	duplicate := edge(aOther, output, "aws_instance.web", ref)
	otherDir := edge(aOther, output, "local.only_in_b", ref)
	dynamic := edge(aOther, output, "var.region", graph.HCLTerraformDynamicEvidence)

	cMain := file("c/main.tf")
	cVar := symbol(cMain, terraform.KindVariable, "var.region")
	brokenDir := edge(cMain, cVar, "var.region", ref)
	symbol(file("c/broken.tf"), terraform.KindSyntaxError, terraform.KindSyntaxError)

	n, err := resolveHCLScope(ctx, s.db, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("bound %d edges, want 1", n)
	}
	for id, want := range map[int64]int64{sameDir: region, duplicate: 0, otherDir: 0, dynamic: 0, brokenDir: 0} {
		var got *int64
		var strategy string
		if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id, resolution_strategy FROM edges WHERE id = ?`, id).Scan(&got, &strategy); err != nil {
			t.Fatal(err)
		}
		if want == 0 && got != nil || want != 0 && (got == nil || *got != want || strategy != ResolutionStrategyTerraformModuleScope) {
			t.Fatalf("edge %d: dst=%v strategy=%q, want %d", id, got, strategy, want)
		}
	}
}
