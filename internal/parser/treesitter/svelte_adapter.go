//go:build cgo

package treesitter

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
)

// SvelteAdapter extracts inline scripts in the component preamble. Template
// syntax is opaque. Calls remain references until Svelte has a resolver contract.
type SvelteAdapter struct{}

func NewSvelte() *SvelteAdapter               { return &SvelteAdapter{} }
func (*SvelteAdapter) Language() string       { return "svelte" }
func (*SvelteAdapter) Extensions() []string   { return []string{".svelte"} }
func (*SvelteAdapter) Supports(p string) bool { return strings.EqualFold(filepath.Ext(p), ".svelte") }
func (*SvelteAdapter) Profile() parser.Profile {
	return parser.NewProfile("svelte", "treesitter:svelte:v1", false)
}

type svelteScript struct {
	start, end int
	module, ts bool
}

var svelteAttribute = regexp.MustCompile(`^([a-z]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'))?`)

// Only a whitespace/comment-separated script preamble is supported. Stopping
// before markup avoids guessing at Svelte expression and raw-text boundaries.
// Ambiguous attributes, duplicate scopes, and unterminated scripts fail closed.
func svelteScripts(content []byte) []svelteScript {
	var blocks []svelteScript
	seen := map[bool]bool{}
	for pos := 0; pos < len(content); {
		for pos < len(content) && strings.ContainsRune(" \t\r\n", rune(content[pos])) {
			pos++
		}
		if bytes.HasPrefix(content[pos:], []byte("<!--")) {
			end := bytes.Index(content[pos+4:], []byte("-->"))
			if end < 0 {
				return nil
			}
			pos += end + 7
			continue
		}
		if !bytes.HasPrefix(content[pos:], []byte("<script")) {
			break
		}
		begin := pos + 7
		if begin >= len(content) || (content[begin] != '>' && !strings.ContainsRune(" \t\r\n", rune(content[begin]))) {
			break
		}
		block := svelteScript{}
		attrs := map[string]bool{}
		p := begin
		for {
			for p < len(content) && strings.ContainsRune(" \t\r\n", rune(content[p])) {
				p++
			}
			if p >= len(content) {
				return nil
			}
			if content[p] == '>' {
				p++
				break
			}
			match := svelteAttribute.FindSubmatchIndex(content[p:])
			if match == nil {
				return nil
			}
			name := string(content[p+match[2] : p+match[3]])
			if attrs[name] {
				return nil
			}
			attrs[name] = true
			value := ""
			if match[4] >= 0 {
				value = string(content[p+match[4] : p+match[5]])
			}
			if match[6] >= 0 {
				value = string(content[p+match[6] : p+match[7]])
			}
			switch name {
			case "lang":
				if value != "ts" && value != "js" {
					return nil
				}
				block.ts = value == "ts"
			case "context":
				if value != "module" || attrs["module"] {
					return nil
				}
				block.module = true
			case "module":
				if value != "" || match[4] >= 0 || match[6] >= 0 || attrs["context"] {
					return nil
				}
				block.module = true
			default:
				return nil
			}
			p += match[1]
			if p < len(content) && content[p] != '>' && !strings.ContainsRune(" \t\r\n", rune(content[p])) {
				return nil
			}
		}
		end := bytes.Index(content[p:], []byte("</script>"))
		if end < 0 {
			return nil
		}
		block.start, block.end = p, p+end
		if seen[block.module] {
			return nil
		}
		seen[block.module] = true
		blocks = append(blocks, block)
		pos = block.end + len("</script>")
	}
	return blocks
}

func (a *SvelteAdapter) Parse(ctx context.Context, logicalPath string, content []byte) (graph.ParsedFile, error) {
	out := graph.ParsedFile{Language: a.Language(), FileTokens: computeFileTokens(content)}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	for _, block := range svelteScripts(content) {
		// Mask bytes, not runes: offsets and columns retain the original source
		// coordinates, including CRLF, tabs, and multibyte comment text.
		masked := make([]byte, len(content))
		for i, b := range content {
			if b == '\n' || b == '\r' {
				masked[i] = b
			} else {
				masked[i] = ' '
			}
		}
		copy(masked[block.start:block.end], content[block.start:block.end])
		scope, ext := "instance", ".js"
		if block.module {
			scope = "module"
		}
		if block.ts {
			ext = ".ts"
		}
		parsed, err := NewTypeScript().Parse(ctx, logicalPath+"#"+scope+ext, masked)
		if err != nil {
			return out, err
		}
		for _, symbol := range parsed.Symbols {
			symbol.Language = a.Language()
			symbol.StableKey = strings.Replace(symbol.StableKey, ":typescript:", ":svelte:", 1)
			out.Symbols = append(out.Symbols, symbol)
		}
		out.Imports = append(out.Imports, parsed.Imports...)
		out.References = append(out.References, parsed.References...)
	}
	return out, nil
}
