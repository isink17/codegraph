//go:build cgo

package luagrammar

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
)

func shape(t *testing.T, src string) string {
	p := sitter.NewParser()
	p.SetLanguage(GetLanguage())
	tree, err := p.ParseCtx(context.Background(), nil, []byte(src))
	if err != nil {
		t.Error(err)
		return ""
	}
	var b strings.Builder
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		fmt.Fprintf(&b, "(%s %d %d", n.Type(), n.StartByte(), n.EndByte())
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
		b.WriteByte(')')
	}
	walk(tree.RootNode())
	return b.String()
}

func TestParsesLua54Syntax(t *testing.T) {
	src := "local x <const>, y <close> = 1, nil\ngoto done\n::done::\nlocal s = [==[a]]b]==]\n"
	if got := shape(t, src); strings.Contains(got, "ERROR") || strings.Contains(got, "MISSING") {
		t.Fatalf("tree has errors: %s", got)
	}
}

// The external scanner keeps its state per parser, so trees parsed at the
// same time cannot change each other.
func TestConcurrentParsesMatchSerial(t *testing.T) {
	var sources []string
	for i := range 8 {
		sources = append(sources, strings.Repeat(fmt.Sprintf("--[%s[ c ]%s]\nlocal s%d = [[a\nb]] .. \"q\" -- x\n", strings.Repeat("=", i), strings.Repeat("=", i), i), 40))
	}
	want := make([]string, len(sources))
	for i, src := range sources {
		want[i] = shape(t, src)
	}
	var wg sync.WaitGroup
	for range 20 {
		for i, src := range sources {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if shape(t, src) != want[i] {
					t.Errorf("source %d: concurrent tree differs from serial", i)
				}
			}()
		}
	}
	wg.Wait()
}
