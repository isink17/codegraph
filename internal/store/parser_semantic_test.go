package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
)

func TestPlanParserSemanticEpochs(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	current := map[string]int{"java": 1, "python": 2}
	all := func(string) bool { return true }
	if got, err := s.PlanParserSemanticEpochs(ctx, 1, current, all); err != nil || len(got) != 2 {
		t.Fatalf("missing marker plan = %v, %v", got, err)
	}
	if err := s.StampParserSemanticEpochs(ctx, 1, current, []string{"java", "python"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PlanParserSemanticEpochs(ctx, 1, current, all); err != nil || len(got) != 0 {
		t.Fatalf("current marker plan = %v, %v", got, err)
	}
	set := func(lang, value string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, value, parserSemanticKey(1, lang)); err != nil {
			t.Fatal(err)
		}
	}
	set("java", "2")
	if _, err := s.PlanParserSemanticEpochs(ctx, 1, current, all); !errors.Is(err, ErrParserSemanticNewer) {
		t.Fatalf("future marker error = %v", err)
	}
	set("java", "v2")
	if _, err := s.PlanParserSemanticEpochs(ctx, 1, current, all); !errors.Is(err, ErrParserSemanticMalformed) {
		t.Fatalf("malformed marker error = %v", err)
	}
	if err := s.StampParserSemanticEpochs(ctx, 1, current, []string{"java"}); !errors.Is(err, ErrParserSemanticMalformed) {
		t.Fatalf("stamp over malformed marker error = %v", err)
	}
	set("java", "3")
	if err := s.StampParserSemanticEpochs(ctx, 1, current, []string{"java"}); !errors.Is(err, ErrParserSemanticNewer) {
		t.Fatalf("stale stamp over future marker error = %v", err)
	}
	if _, err := s.PlanParserSemanticEpochs(ctx, 1, current, func(lang string) bool { return lang == "python" }); err != nil {
		t.Fatalf("unaffected malformed Java marker blocked Python-only operation: %v", err)
	}
}

func TestParserSemanticPendingIsResumableButFutureStateRefuses(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	current := map[string]int{"java": parser.SemanticEpochs()["java"]}
	all := func(string) bool { return true }
	if err := s.BeginParserSemanticTransitions(ctx, 9, current, []string{"java"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PlanParserSemanticEpochs(ctx, 9, current, all); err != nil || len(got) != 1 || got[0] != "java" {
		t.Fatalf("pending plan=%v, %v", got, err)
	}
	if err := s.CheckParserSemanticGraph(ctx, 9); !errors.Is(err, ErrParserSemanticIncomplete) {
		t.Fatalf("graph check=%v, want incomplete", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, fmt.Sprintf("v1:0:%d", current["java"]+1), parserSemanticPendingKey(9, "java")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlanParserSemanticEpochs(ctx, 9, current, all); !errors.Is(err, ErrParserSemanticNewer) {
		t.Fatalf("future pending plan=%v, want newer", err)
	}
	if err := s.CheckParserSemanticGraph(ctx, 9); !errors.Is(err, ErrParserSemanticNewer) {
		t.Fatalf("future pending query=%v, want newer", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, "broken", parserSemanticPendingKey(9, "java")); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckParserSemanticGraph(ctx, 9); !errors.Is(err, ErrParserSemanticMalformed) {
		t.Fatalf("malformed pending query=%v, want malformed", err)
	}
}

func TestParserSemanticStampChecksFileGenerationsTransactionally(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO files(repo_id,path,language,size_bytes,mtime_unix_ns,content_sha256,parse_state) VALUES(?,?,?,?,?,?,?)`, repo.ID, "A.java", "java", 1, 1, "hash", "indexed"); err != nil {
		t.Fatal(err)
	}
	current := map[string]int{"java": 2}
	if err := s.StampParserSemanticEpochs(ctx, repo.ID, current, []string{"java"}); !errors.Is(err, ErrParserSemanticIncomplete) {
		t.Fatalf("stamp with old indexed file error = %v", err)
	}
	var markerCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, parserSemanticKey(repo.ID, "java")).Scan(&markerCount); err != nil || markerCount != 0 {
		t.Fatalf("failed stamp wrote marker: count=%d err=%v", markerCount, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET parser_semantic_epoch=2 WHERE repo_id=?`, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.StampParserSemanticEpochs(ctx, repo.ID, current, []string{"java"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.ExistingFiles(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ReplaceFileGraphsBatch(ctx, repo.ID, 1, []ReplaceFileGraphInput{{Path: "A.java", Language: "java", ContentHash: "stale", Parsed: graph.ParsedFile{Language: "java"}, ParserProfile: "treesitter:java:v1", ParserSemanticEpoch: 1}})
	if !errors.Is(err, ErrParserSemanticNewer) {
		t.Fatalf("stale concurrent write error = %v", err)
	}
	after, err := s.ExistingFiles(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before["A.java"] != after["A.java"] {
		t.Fatalf("stale write changed file metadata: before=%+v after=%+v", before["A.java"], after["A.java"])
	}
	// A future file can become durable before its repository marker. An old
	// writer must still refuse while the marker is missing.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, parserSemanticKey(repo.ID, "java")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET parser_semantic_epoch=3 WHERE repo_id=?`, repo.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReplaceFileGraphsBatch(ctx, repo.ID, 1, []ReplaceFileGraphInput{{Path: "A.java", Language: "java", ContentHash: "stale-again", Parsed: graph.ParsedFile{Language: "java"}, ParserProfile: "treesitter:java:v1", ParserSemanticEpoch: 2}})
	if !errors.Is(err, ErrParserSemanticNewer) {
		t.Fatalf("stale write over future file epoch without marker error = %v", err)
	}
}
