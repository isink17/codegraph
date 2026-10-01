//go:build cgo

package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	ts "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// Replay the measured v3 defects for controlled fixtures: hooks re-home a
// method, conditional helpers disappear, and the chained call loses its receiver.
type phpV3FixtureAdapter struct{ *ts.PHPAdapter }

func (phpV3FixtureAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:php:v3", EmitsCallEdges: true}
}
func (a phpV3FixtureAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	p, err := a.PHPAdapter.Parse(ctx, path, content)
	if err != nil {
		return p, err
	}
	switch filepath.Base(path) {
	case "Request.php":
		for _, s := range p.Symbols {
			if s.Name == "toArray" {
				s.QualifiedName = "App.toArray"
				s.ContainerName = "App"
				s.Static = nil
				s.StableKey = "func:php:App.toArray"
				p.Symbols = []graph.Symbol{s}
				break
			}
		}
	case "Helpers.php":
		p.Symbols = nil
	case "Caller.php":
		for i := range p.Edges {
			if strings.Contains(p.Edges[i].DstName, "->toArray") {
				p.Edges[i].DstName = "toArray"
				p.Edges[i].Evidence = "toArray"
			}
		}
		for i := range p.References {
			if strings.Contains(p.References[i].Name, "->toArray") {
				p.References[i].Name = "toArray"
				p.References[i].QualifiedName = "toArray"
			}
		}
	}
	return p, nil
}

func TestPHPParserV4UpgradeConvergesUnchangedSources(t *testing.T) {
	files := map[string]string{
		"Request.php": `<?php namespace App;
class Request {
 public string $p { get => "x"; set { $this->save($value); } }
 public function toArray() {}
}`,
		"Helpers.php": `<?php namespace App;
if (!function_exists('helper')) { function helper() {} }`,
		"Caller.php": `<?php namespace App;
function caller() {
 helper();
 (new Request())->toArray();
}`,
	}
	r := &phpRepo{t: t, root: t.TempDir(), dbPath: filepath.Join(t.TempDir(), "graph.db")}
	for path, src := range files {
		r.write(path, src)
	}
	var err error
	r.s, err = store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.s.Close() })
	old := New(r.s, parser.NewRegistry(phpV3FixtureAdapter{ts.NewPHP()}), nil)
	if _, err = old.Index(context.Background(), Options{RepoRoot: r.root}); err != nil {
		t.Fatal(err)
	}
	repo, err := r.s.UpsertRepo(context.Background(), r.root)
	if err != nil {
		t.Fatal(err)
	}
	r.repoID = repo.ID
	r.assertTarget("Caller.php", "toArray", "App.toArray", "exact_name")
	r.idx = New(r.s, parser.NewRegistry(ts.NewPHP()), nil)
	summary := r.update("Caller.php")
	if summary.FilesIndexed != 3 || summary.FilesChanged != 3 {
		t.Fatalf("unchanged language transition: %+v", summary)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "php" {
		t.Fatalf("transition languages: %+v", summary)
	}
	if _, ok := r.edges()["Caller.php:toArray"]; ok {
		t.Fatal("stale bare chained call survived")
	}
	r.assertTarget("Caller.php", "helper", "App.helper", "exact_name")
	r.assertFreshParity()
	if summary := r.update(); summary.FilesIndexed != 0 || summary.FilesChanged != 0 {
		t.Fatalf("second update not no-op: %+v", summary)
	}
	// A malformed edit discards its invalid declaration and retains proven peers.
	r.write("Request.php", files["Request.php"]+" public function leaked( {")
	r.update("Request.php")
	r.assertFreshParity()
	raw := r.raw()
	defer raw.Close()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM symbols s JOIN files f ON f.id=s.file_id WHERE f.path='Request.php' AND f.is_deleted=0 AND s.name='leaked'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("malformed facts: n=%d err=%v", n, err)
	}
	r.write("Request.php", files["Request.php"])
	r.update("Request.php")
	r.assertFreshParity()
}

func TestPHPConditionalDuplicateDeclarationsFailClosed(t *testing.T) {
	r := newPHPRepo(t, map[string]string{
		"Helpers.php": `<?php
if (true) { function helper() {} } else { function helper() {} }
function caller() {
 helper();
}`,
	})
	r.assertUnresolved("Helpers.php", "helper")
	raw := r.raw()
	defer raw.Close()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM symbols WHERE name='helper'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("duplicate facts: n=%d err=%v", n, err)
	}
	r.write("Helpers.php", `<?php
if (true) { function helper() {} }
function caller() {
 helper();
}`)
	r.update("Helpers.php")
	r.assertTarget("Helpers.php", "helper", "helper", "exact_qualified")
	r.assertFreshParity()
}
