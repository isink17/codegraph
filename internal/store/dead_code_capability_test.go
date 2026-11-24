package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type deadCodeSeedFile struct {
	path, language, profile string
	callEdges               bool
}

// seedDeadCodeFile inserts a file with the parser provenance an index would
// persist for it.
func seedDeadCodeFile(t *testing.T, s *Store, repoID int64, f deadCodeSeedFile) int64 {
	t.Helper()
	callEdges := 0
	if f.callEdges {
		callEdges = 1
	}
	res, err := s.db.ExecContext(context.Background(),
		`INSERT INTO files(repo_id, path, language, indexed_at, parser_profile, parser_call_edges) VALUES(?, ?, ?, '', ?, ?)`,
		repoID, f.path, f.language, f.profile, callEdges)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedDeadCodeSymbol(t *testing.T, s *Store, repoID, fileID int64, language, name string) int64 {
	t.Helper()
	id, err := insertTestSymbolLang(context.Background(), s, repoID, fileID, name, name, language)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedDeadCodeReference(t *testing.T, s *Store, repoID, fileID int64, name string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO references_tbl(repo_id, file_id, ref_kind, name, start_line, start_col, end_line, end_col) VALUES(?, ?, 'call', ?, 1, 1, 1, 2)`,
		repoID, fileID, name); err != nil {
		t.Fatal(err)
	}
}

func seedDeadCodeCall(t *testing.T, s *Store, repoID, fileID, srcID, dstID int64, dstName string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, file_id, line) VALUES(?, ?, ?, ?, 'calls', ?, 1)`,
		repoID, srcID, dstID, dstName, fileID); err != nil {
		t.Fatal(err)
	}
}

func deadCodeNames(items []map[string]any) string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, fmt.Sprint(item["name"]))
	}
	return strings.Join(names, ",")
}

// A symbol of a language whose persisted graph holds no call edges is never
// declared dead: nothing could have recorded its callers. Only call-capable
// languages are classified; every other language is reported as skipped with
// the capability that kept it out.
func TestFindDeadCodeSkipsLanguagesWithoutCallGraph(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)

	goFile := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"a/lib.go", "go", "treesitter:go:v6", true})
	helper := seedDeadCodeSymbol(t, s, repo.ID, goFile, "go", "Helper")
	run := seedDeadCodeSymbol(t, s, repo.ID, goFile, "go", "Run")
	seedDeadCodeSymbol(t, s, repo.ID, goFile, "go", "unusedGo")
	seedDeadCodeCall(t, s, repo.ID, goFile, run, helper, "Helper")

	sol := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"b/Token.sol", "solidity", "treesitter:solidity:v1", false})
	seedDeadCodeSymbol(t, s, repo.ID, sol, "solidity", "transfer")
	seedDeadCodeSymbol(t, s, repo.ID, sol, "solidity", "_move")
	seedDeadCodeReference(t, s, repo.ID, sol, "_move")

	java := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"c/Widget.java", "java", "heuristic:java:v1", false})
	seedDeadCodeSymbol(t, s, repo.ID, java, "java", "size")

	// Unrecorded provenance: whether this file's calls were extracted is unknown.
	lua := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"d/m.lua", "lua", "", false})
	seedDeadCodeSymbol(t, s, repo.ID, lua, "lua", "luaFn")

	// One Kotlin file lost its call graph: the language is mixed, so a missing
	// caller may sit in the symbols-only file.
	kt1 := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"e/A.kt", "kotlin", "treesitter:kotlin:v15", true})
	seedDeadCodeSymbol(t, s, repo.ID, kt1, "kotlin", "ktFn")
	kt2 := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"e/B.kt", "kotlin", "heuristic:kotlin:v1", false})
	seedDeadCodeSymbol(t, s, repo.ID, kt2, "kotlin", "ktOther")

	// Scala records calls only to local defs of the calling file, so a member
	// called from another file never has a caller.
	scala := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"g/A.scala", "scala", "treesitter:scala:v4", true})
	seedDeadCodeSymbol(t, s, repo.ID, scala, "scala", "member")

	// The approximate Python fallback emits call edges: it stays classified, and
	// its limitation is disclosed separately (GraphLimitations).
	py := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"f/m.py", "python", "python-regex:python:v10", true})
	seedDeadCodeSymbol(t, s, repo.ID, py, "python", "pyFn")

	result, err := s.FindDeadCodeResult(ctx, repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := deadCodeNames(result.Items), "Run,unusedGo,pyFn"; got != want {
		t.Fatalf("dead code = %s, want %s", got, want)
	}
	wantSkipped := []DeadCodeSkippedLanguage{
		{Language: "java", Capability: GraphSymbolsOnly, Reason: DeadCodeNoCallEdges},
		{Language: "kotlin", Capability: GraphMixed, Reason: DeadCodeMixedParserProfiles},
		{Language: "lua", Capability: GraphUnknownProvenance, Reason: DeadCodeUnknownParserProvenance},
		{Language: "scala", Capability: GraphCallCapable, Reason: DeadCodeSameFileCallsOnly},
		{Language: "solidity", Capability: GraphSymbolsOnly, Reason: DeadCodeNoCallEdges},
	}
	if !reflect.DeepEqual(result.Skipped, wantSkipped) {
		t.Fatalf("skipped = %+v, want %+v", result.Skipped, wantSkipped)
	}

	items, err := s.FindDeadCode(ctx, repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(items, result.Items) {
		t.Fatalf("FindDeadCode = %v, FindDeadCodeResult.Items = %v", items, result.Items)
	}
}

// A call from a file whose language has no call graph is recorded only as an
// unbound reference. Such a reference naming a symbol of a call-capable
// language keeps that symbol out of the answer: the use is unproven, but so is
// the absence of one. A reference from a call-capable file is not this signal;
// its language's resolver already had the chance to bind it.
func TestFindDeadCodeWithholdsSymbolsNamedFromLanguagesWithoutCallGraph(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	ts := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"web/util.ts", "typescript", "treesitter:typescript:v2", true})
	seedDeadCodeSymbol(t, s, repo.ID, ts, "typescript", "formatName")
	seedDeadCodeSymbol(t, s, repo.ID, ts, "typescript", "trimAll")
	seedDeadCodeSymbol(t, s, repo.ID, ts, "typescript", "unusedTs")
	seedDeadCodeSymbol(t, s, repo.ID, ts, "typescript", "localOnly")
	seedDeadCodeReference(t, s, repo.ID, ts, "localOnly")

	svelte := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"web/App.svelte", "svelte", "treesitter:svelte:v1", false})
	seedDeadCodeSymbol(t, s, repo.ID, svelte, "svelte", "greet")
	seedDeadCodeReference(t, s, repo.ID, svelte, "formatName")
	seedDeadCodeReference(t, s, repo.ID, svelte, "utils.trimAll")

	result, err := s.FindDeadCodeResult(ctx, repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := deadCodeNames(result.Items), "localOnly,unusedTs"; got != want {
		t.Fatalf("dead code = %s, want %s", got, want)
	}
	if want := []DeadCodeSkippedLanguage{{Language: "svelte", Capability: GraphSymbolsOnly, Reason: DeadCodeNoCallEdges}}; !reflect.DeepEqual(result.Skipped, want) {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
}

// A graph that is call-capable everywhere answers exactly as before: no
// skipped languages, and the reference guard does not run.
func TestFindDeadCodeCallCapableGraphHasNoSkippedLanguages(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	goFile := seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"a/lib.go", "go", "treesitter:go:v6", true})
	seedDeadCodeSymbol(t, s, repo.ID, goFile, "go", "Exported")
	seedDeadCodeSymbol(t, s, repo.ID, goFile, "go", "main")
	seedDeadCodeSymbol(t, s, repo.ID, goFile, "go", "TestThing")
	result, err := s.FindDeadCodeResult(ctx, repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := deadCodeNames(result.Items); got != "Exported" {
		t.Fatalf("dead code = %s, want Exported", got)
	}
	if result.Skipped != nil {
		t.Fatalf("skipped = %+v, want nil", result.Skipped)
	}

	// A file whose parse failed holds no parser evidence and no provenance. It
	// does not make Go's provenance unknown.
	seedDeadCodeFile(t, s, repo.ID, deadCodeSeedFile{"a/broken.go", "go", "", false})
	result, err = s.FindDeadCodeResult(ctx, repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := deadCodeNames(result.Items); got != "Exported" {
		t.Fatalf("dead code = %s, want Exported", got)
	}
	if result.Skipped != nil {
		t.Fatalf("skipped = %+v, want nil", result.Skipped)
	}

	empty, repo2 := openBudgetStore(t)
	result, err = empty.FindDeadCodeResult(ctx, repo2.ID, 100, 0)
	if err != nil || result.Items != nil || result.Skipped != nil {
		t.Fatalf("empty graph = %+v, %v", result, err)
	}
}

// markFilesCallCapable records every file as written by a call-capable parser
// profile of its language, the provenance an index of a call-capable build
// persists. Raw-seeded files otherwise have unknown provenance.
func markFilesCallCapable(t *testing.T, s *Store, repoID int64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE files SET parser_profile = 'test:' || language || ':v1', parser_call_edges = 1 WHERE repo_id = ?`, repoID); err != nil {
		t.Fatal(err)
	}
}
