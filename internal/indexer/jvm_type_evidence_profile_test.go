//go:build cgo

package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestJVMTypeEvidenceProfileUpgradeReparsesStoredFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "Types.kt"), "package api\ntypealias Local = kotlin.String\nclass Token\nfun use(value: Token): Token = value\n")
	writeProfileFile(t, filepath.Join(root, "Aliases.kt"), "package api\ntypealias OnlyAlias = kotlin.String\n")
	writeProfileFile(t, filepath.Join(root, "Types.java"), "package api; class JavaType<T> { String use(Token value) { return null; } }")
	s := newProfileStore(t)
	idx := New(s.Store, lifecycleRegistry(), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if _, err := s.raw(t).ExecContext(ctx, `UPDATE files SET parser_profile=CASE language WHEN 'kotlin' THEN 'treesitter:kotlin:v11' ELSE 'treesitter:java:v11' END WHERE repo_id=?; DELETE FROM jvm_type_evidence WHERE repo_id=?`, repo, repo); err != nil {
		t.Fatal(err)
	}
	summary, err := idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesIndexed != 3 || !strings.Contains(strings.Join(summary.ParserProfileLanguages, ","), "java") || !strings.Contains(strings.Join(summary.ParserProfileLanguages, ","), "kotlin") {
		t.Fatalf("profile upgrade summary = %+v", summary)
	}
	facts, err := s.JVMTypeEvidence(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	var java, kotlin, alias bool
	for _, fact := range facts {
		java = java || fact.SourceLanguage == "java" && fact.Kind == "class_declaration"
		kotlin = kotlin || fact.SourceLanguage == "kotlin" && fact.Kind == "class"
		alias = alias || fact.Kind == "typealias" && fact.SyntaxState == "unknown"
	}
	if !java || !kotlin || !alias {
		t.Fatalf("upgraded source facts: %+v", facts)
	}
}
