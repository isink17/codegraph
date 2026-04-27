package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

func TestJSONAndDOTIncludeGraphData(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "main.go"), `package main
func helper() {}
func main() { helper() }
`)

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	svc := New(query.New(s, nil))
	jsonOut, err := svc.JSON(ctx, repo.ID)
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if !strings.Contains(string(jsonOut), `"symbols"`) {
		t.Fatalf("JSON output missing symbols section: %s", string(jsonOut))
	}
	if !strings.Contains(string(jsonOut), `"edges"`) {
		t.Fatalf("JSON output missing edges section: %s", string(jsonOut))
	}

	dotOut, err := svc.DOT(ctx, repo.ID, "", 0)
	if err != nil {
		t.Fatalf("DOT() error = %v", err)
	}
	if !strings.Contains(string(dotOut), "->") {
		t.Fatalf("DOT output missing edge rendering: %s", string(dotOut))
	}
}

// TestJSONPagedNoFocusUsesPagingHelpers verifies that the bounded-page
// no-focus path (`symbol == "" && limit > 0`) returns at most `limit`
// symbols/edges and that subsequent pages cover disjoint ranges. This is the
// path that previously materialized the entire repo via GraphSnapshot before
// slicing client-side.
func TestJSONPagedNoFocusUsesPagingHelpers(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	for i := 0; i < 8; i++ {
		writeFile(t, filepath.Join(repoRoot, fmt.Sprintf("file_%d.go", i)),
			fmt.Sprintf("package main\nfunc helper%d() {}\nfunc main%d() { helper%d() }\n", i, i, i))
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	svc := New(query.New(s, nil))

	page0, err := svc.JSONPaged(ctx, repo.ID, "", 0, 4, 0)
	if err != nil {
		t.Fatalf("JSONPaged(page0) error = %v", err)
	}
	page1, err := svc.JSONPaged(ctx, repo.ID, "", 0, 4, 4)
	if err != nil {
		t.Fatalf("JSONPaged(page1) error = %v", err)
	}

	parsed0 := struct {
		Symbols []map[string]any `json:"symbols"`
	}{}
	parsed1 := struct {
		Symbols []map[string]any `json:"symbols"`
	}{}
	if err := json.Unmarshal(page0, &parsed0); err != nil {
		t.Fatalf("Unmarshal page0: %v", err)
	}
	if err := json.Unmarshal(page1, &parsed1); err != nil {
		t.Fatalf("Unmarshal page1: %v", err)
	}
	if len(parsed0.Symbols) == 0 || len(parsed0.Symbols) > 4 {
		t.Fatalf("page0 symbol count = %d, want 1..4", len(parsed0.Symbols))
	}
	if len(parsed1.Symbols) > 4 {
		t.Fatalf("page1 symbol count = %d, want <=4", len(parsed1.Symbols))
	}
	// Pages must not overlap on stable_key.
	keys := map[string]bool{}
	for _, sym := range parsed0.Symbols {
		if k, ok := sym["stable_key"].(string); ok && k != "" {
			keys[k] = true
		}
	}
	for _, sym := range parsed1.Symbols {
		if k, ok := sym["stable_key"].(string); ok && k != "" && keys[k] {
			t.Fatalf("page1 overlaps page0 on stable_key %q", k)
		}
	}
}

// TestJSONStreamMatchesJSONShape validates that the writer-based unbounded
// path emits the same top-level shape as the byte-slice JSON() path
// (`repo`, `stats`, `symbols`, `edges`) and the same row identities. Peak
// memory drops from O(repo) to O(pageSize) is structural — not measured
// here, just the output equivalence is asserted.
func TestJSONStreamMatchesJSONShape(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	for i := 0; i < 6; i++ {
		writeFile(t, filepath.Join(repoRoot, fmt.Sprintf("file_%d.go", i)),
			fmt.Sprintf("package main\nfunc helper%d() {}\nfunc main%d() { helper%d() }\n", i, i, i))
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	svc := New(query.New(s, nil))

	jsonOut, err := svc.JSON(ctx, repo.ID)
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	var buf bytes.Buffer
	// Force a tiny page size to exercise the page-boundary stitching path.
	if err := svc.JSONStream(ctx, &buf, repo.ID, 2); err != nil {
		t.Fatalf("JSONStream() error = %v", err)
	}

	type doc struct {
		Repo    string           `json:"repo"`
		Stats   map[string]any   `json:"stats"`
		Symbols []map[string]any `json:"symbols"`
		Edges   []map[string]any `json:"edges"`
	}
	var refDoc, streamDoc doc
	if err := json.Unmarshal(jsonOut, &refDoc); err != nil {
		t.Fatalf("Unmarshal(JSON output): %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &streamDoc); err != nil {
		t.Fatalf("Unmarshal(JSONStream output): %v\n%s", err, buf.String())
	}
	if refDoc.Repo != streamDoc.Repo || refDoc.Repo == "" {
		t.Fatalf("repo mismatch: ref=%q stream=%q", refDoc.Repo, streamDoc.Repo)
	}
	if len(refDoc.Symbols) == 0 || len(streamDoc.Symbols) != len(refDoc.Symbols) {
		t.Fatalf("symbols count mismatch: ref=%d stream=%d", len(refDoc.Symbols), len(streamDoc.Symbols))
	}
	if len(refDoc.Edges) == 0 || len(streamDoc.Edges) != len(refDoc.Edges) {
		t.Fatalf("edges count mismatch: ref=%d stream=%d", len(refDoc.Edges), len(streamDoc.Edges))
	}
	// Stable_key sets must match exactly (paged loader is ORDER BY id, JSON
	// path goes through GraphSnapshot — both end up covering all symbols).
	refKeys := map[string]bool{}
	for _, sym := range refDoc.Symbols {
		if k, ok := sym["stable_key"].(string); ok {
			refKeys[k] = true
		}
	}
	for _, sym := range streamDoc.Symbols {
		k, _ := sym["stable_key"].(string)
		if !refKeys[k] {
			t.Fatalf("stream symbol stable_key %q not in JSON() output", k)
		}
	}
}

// TestDOTStreamMatchesDOTShape pins down that the writer-based no-focus
// DOT path emits the same node set and the same digraph framing as the
// byte-slice DOT() path. Edge ordering differs (DOTStream pages via
// ExportEdgesPage's ORDER BY id; DOT() iterates GraphSnapshot's
// loadEdgesForExport result), so this test asserts node parity and
// edge-line set parity rather than byte equality.
func TestDOTStreamMatchesDOTShape(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	for i := 0; i < 6; i++ {
		writeFile(t, filepath.Join(repoRoot, fmt.Sprintf("file_%d.go", i)),
			fmt.Sprintf("package main\nfunc helper%d() {}\nfunc main%d() { helper%d() }\n", i, i, i))
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	svc := New(query.New(s, nil))

	refOut, err := svc.DOT(ctx, repo.ID, "", 0)
	if err != nil {
		t.Fatalf("DOT() error = %v", err)
	}
	var buf bytes.Buffer
	// Force a tiny page size so the stitching path runs.
	if err := svc.DOTStream(ctx, &buf, repo.ID, 2); err != nil {
		t.Fatalf("DOTStream() error = %v", err)
	}

	splitDOT := func(b []byte) (header string, nodes, edges []string, footer string) {
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) < 2 {
			t.Fatalf("DOT output too short:\n%s", b)
		}
		header = lines[0]
		footer = lines[len(lines)-1]
		for _, ln := range lines[1 : len(lines)-1] {
			trim := strings.TrimSpace(ln)
			switch {
			case strings.Contains(trim, "->"):
				edges = append(edges, trim)
			case strings.HasSuffix(trim, ";"):
				nodes = append(nodes, trim)
			}
		}
		return
	}
	refHdr, refNodes, refEdges, refFtr := splitDOT(refOut)
	strHdr, strNodes, strEdges, strFtr := splitDOT(buf.Bytes())

	if refHdr != strHdr || refHdr != "digraph codegraph {" {
		t.Fatalf("header mismatch: ref=%q stream=%q", refHdr, strHdr)
	}
	if refFtr != strFtr || refFtr != "}" {
		t.Fatalf("footer mismatch: ref=%q stream=%q", refFtr, strFtr)
	}
	if len(refNodes) == 0 || len(refEdges) == 0 {
		t.Fatalf("ref produced no nodes/edges; nodes=%d edges=%d", len(refNodes), len(refEdges))
	}
	if !reflect.DeepEqual(refNodes, strNodes) {
		t.Fatalf("node lines mismatch:\nref=%v\nstream=%v", refNodes, strNodes)
	}
	refEdgeSet := map[string]int{}
	for _, e := range refEdges {
		refEdgeSet[e]++
	}
	strEdgeSet := map[string]int{}
	for _, e := range strEdges {
		strEdgeSet[e]++
	}
	if !reflect.DeepEqual(refEdgeSet, strEdgeSet) {
		t.Fatalf("edge multiset mismatch:\nref=%v\nstream=%v", refEdgeSet, strEdgeSet)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
