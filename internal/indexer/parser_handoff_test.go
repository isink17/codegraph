package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

type recordingParserAdapter struct {
	language string
	parsed   []string
}

func (a *recordingParserAdapter) Language() string     { return a.language }
func (a *recordingParserAdapter) Supports(string) bool { return true }
func (a *recordingParserAdapter) Parse(_ context.Context, path string, _ []byte) (graph.ParsedFile, error) {
	a.parsed = append(a.parsed, path)
	return graph.ParsedFile{Language: a.language}, nil
}

func TestProcessFileTaskParserPathOwnership(t *testing.T) {
	dir := t.TempDir()
	nativePath := filepath.Join(dir, "source.ts")
	if err := os.WriteFile(nativePath, []byte("function run() {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(nativePath)
	if err != nil {
		t.Fatal(err)
	}

	run := func(language, rel string) *recordingParserAdapter {
		adapter := &recordingParserAdapter{language: language}
		result := processFileTask(context.Background(), fileTask{
			path: nativePath, rel: rel, info: info, adapter: adapter, language: language,
		}, store.ExistingFileMeta{}, false, false, 0, nil, "fail", nil, nil)
		if result.err != nil || result.action != "replace" {
			t.Fatalf("%s result = action %q, err %v", language, result.action, result.err)
		}
		if len(adapter.parsed) != 1 {
			t.Fatalf("%s Parse calls = %d, want 1", language, len(adapter.parsed))
		}
		return adapter
	}

	typescript := run("typescript", `pkg/x\y.ts`)
	if got := typescript.parsed[0]; got != `pkg/x\y.ts` {
		t.Fatalf("TypeScript Parse path = %q, want logical rel", got)
	}
	nonTypeScript := run("python", `pkg/x\y.py`)
	if got := nonTypeScript.parsed[0]; got != nativePath {
		t.Fatalf("non-TypeScript Parse path = %q, want native path %q", got, nativePath)
	}
}
