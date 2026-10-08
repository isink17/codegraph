package treesitter

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every Tree-sitter grammar linked into the binary carries an MIT (or
// similar) notice that must ship with release archives and the npm package.
// This fails when a grammar package is imported by non-test code, or a
// grammar is vendored here, without an entry in THIRD_PARTY_NOTICES.
func TestThirdPartyNoticesCoverGrammars(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	notices, err := os.ReadFile(filepath.Join(root, "THIRD_PARTY_NOTICES"))
	if err != nil {
		t.Fatal(err)
	}
	npmCopy, err := os.ReadFile(filepath.Join(root, "npm", "THIRD_PARTY_NOTICES"))
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may convert line endings; compare text, not bytes.
	notices = bytes.ReplaceAll(notices, []byte("\r\n"), []byte("\n"))
	npmCopy = bytes.ReplaceAll(npmCopy, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(notices, npmCopy) {
		t.Error("npm/THIRD_PARTY_NOTICES differs from THIRD_PARTY_NOTICES; copy it")
	}

	want := map[string]string{} // import path -> first file importing it
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range f.Imports {
			p, _ := strconv.Unquote(spec.Path.Value)
			if strings.HasPrefix(p, "github.com/smacker/go-tree-sitter") || strings.HasPrefix(p, "github.com/tree-sitter/") {
				if _, ok := want[p]; !ok {
					want[p] = path
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	vendored, err := filepath.Glob("*grammar")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range vendored {
		want["github.com/isink17/codegraph/internal/parser/treesitter/"+dir] = dir
	}
	if len(want) == 0 {
		t.Fatal("found no grammar imports; the scan is broken")
	}
	for p, from := range want {
		if !bytes.Contains(notices, []byte("\nGo package: "+p+"\n")) {
			t.Errorf("THIRD_PARTY_NOTICES has no \"Go package: %s\" entry (imported by %s)", p, from)
		}
	}
}
