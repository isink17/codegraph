package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDataset(t *testing.T, ds map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(ds)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ds.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func validDataset() map[string]any {
	return map[string]any{
		"schema": DatasetSchema,
		"id":     "t",
		"corpora": []any{map[string]any{
			"name": "cobra", "commit": "c1", "tree": "t1",
			"files": 1, "symbols": 2, "references": 3, "edges": 4,
		}},
		"queries": []any{map[string]any{
			"id": "q1", "corpus": "cobra", "category": "execution_path",
			"truth": TruthPartial, "query": "trace",
			"relevant": []any{map[string]any{"file": "a.go", "grade": 3, "evidence": "a.go:1"}},
		}},
	}
}

func TestLoadDatasetRecordsItsOwnDigest(t *testing.T) {
	path := writeDataset(t, validDataset())
	ds, err := LoadDataset(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.SHA256) != 64 {
		t.Fatalf("dataset digest = %q", ds.SHA256)
	}
	again, err := LoadDataset(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.SHA256 != ds.SHA256 {
		t.Fatalf("digest is not stable: %s vs %s", again.SHA256, ds.SHA256)
	}
}

func TestLoadDatasetRejects(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"unknown schema": func(m map[string]any) { m["schema"] = "other/v9" },
		"no queries":     func(m map[string]any) { m["queries"] = []any{} },
		"unpinned corpus": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["corpus"] = "ghost"
		},
		"unknown truth": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["truth"] = "mostly"
		},
		"grade out of range": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["relevant"].([]any)[0].(map[string]any)["grade"] = 9
		},
		"label without evidence": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["relevant"].([]any)[0].(map[string]any)["evidence"] = " "
		},
		"corpus without identity": func(m map[string]any) {
			m["corpora"].([]any)[0].(map[string]any)["tree"] = ""
		},
		"no relevant labels": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["relevant"] = []any{}
		},
		"file labelled both ways": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["irrelevant"] = []any{
				map[string]any{"file": "a.go", "evidence": "also irrelevant"}}
		},
		"unknown field": func(m map[string]any) { m["weights"] = "tuned" },
		"absolute label path": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["relevant"].([]any)[0].(map[string]any)["file"] = "/src/a.go"
		},
		"unclean label path": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["relevant"].([]any)[0].(map[string]any)["file"] = "./a.go"
		},
		"backslash label path": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["relevant"].([]any)[0].(map[string]any)["file"] = `src\a.go`
		},
		"graded irrelevant label": func(m map[string]any) {
			m["queries"].([]any)[0].(map[string]any)["irrelevant"] = []any{
				map[string]any{"file": "b.go", "grade": 3, "evidence": "misfiled"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := validDataset()
			mutate(m)
			if _, err := LoadDataset(writeDataset(t, m)); err == nil {
				t.Fatalf("expected refusal for %s", name)
			}
		})
	}
}

func TestLoadDatasetRejectsDuplicateQueryID(t *testing.T) {
	m := validDataset()
	first := m["queries"].([]any)[0]
	m["queries"] = []any{first, first}
	_, err := LoadDataset(writeDataset(t, m))
	if err == nil || !strings.Contains(err.Error(), "duplicate query id") {
		t.Fatalf("err = %v, want duplicate query id", err)
	}
}

func TestShippedDatasetIsValid(t *testing.T) {
	ds, err := LoadDataset("datasets/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Queries) == 0 {
		t.Fatal("shipped dataset has no queries")
	}
	heldOut := 0
	categories := map[string]bool{}
	for _, q := range ds.Queries {
		categories[q.Category] = true
		if q.HeldOut {
			heldOut++
		}
	}
	// Pinned to the shape the evidence and the commit message describe, so a
	// silent dataset edit fails here instead of quietly changing a baseline.
	if len(ds.Queries) != 14 {
		t.Fatalf("shipped dataset has %d queries, want 14", len(ds.Queries))
	}
	if heldOut != 4 {
		t.Fatalf("shipped dataset holds out %d queries, want 4", heldOut)
	}
	if len(categories) != 6 {
		t.Fatalf("shipped dataset covers %d categories, want 6", len(categories))
	}
}
