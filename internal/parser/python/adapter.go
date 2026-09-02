package python

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/texttoken"
)

var (
	classRE = regexp.MustCompile(`^\s*class\s+([A-Za-z_][A-Za-z0-9_]*)`)
	defRE   = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	callRE  = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

type scope struct {
	name   string
	indent int
	kind   string
	// symIdx is the index of the scope's declaration in ParsedFile.Symbols,
	// so closing the scope can seal that symbol's body range.
	symIdx int
}

type Adapter struct{}

func New() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Language() string {
	return "python"
}

func (a *Adapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".py")
}

func (a *Adapter) Extensions() []string {
	return []string{".py"}
}

func (a *Adapter) Parse(_ context.Context, path string, content []byte) (graph.ParsedFile, error) {
	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	lines := strings.Split(string(content), "\n")
	pf := graph.ParsedFile{
		Language:   "python",
		FileTokens: texttoken.Weights(content),
	}
	var scopes []scope
	maskedLines := maskPythonLines(lines)
	// lastLine/lastLen track the most recent content line (not blank, not a
	// comment). A scope popped by a dedent ends at that line: blank lines,
	// comments and decorators between declarations belong to no body.
	lastLine, lastLen := 0, 0

	for i, line := range lines {
		lineNo := i + 1
		line = strings.TrimSuffix(line, "\r")
		masked := strings.TrimSuffix(maskedLines[i], "\r")
		indent := lineIndent(line)
		trimmed := strings.TrimSpace(line)
		if strings.TrimSpace(masked) == "" {
			continue
		}

		scopes = closeScopes(scopes, indent, lastLine, lastLen, pf.Symbols)
		lastLine, lastLen = lineNo, len(line)

		if m := classRE.FindStringSubmatch(masked); len(m) == 2 {
			name := m[1]
			path := scopeNames(scopes)
			qualified := qualifiedName(module, append(path, name))
			container := module
			if len(path) > 0 {
				container = strings.Join(path, ".")
			}
			sym := graph.Symbol{
				Language:      "python",
				Kind:          "class",
				Name:          name,
				QualifiedName: qualified,
				ContainerName: container,
				Visibility:    visibility(name),
				Range: graph.Position{
					StartLine: lineNo,
					StartCol:  indent + 1,
					EndLine:   lineNo,
					EndCol:    len(line) + 1,
				},
				StableKey: "class:" + module + ":" + name,
			}
			pf.Symbols = append(pf.Symbols, sym)
			scopes = append(scopes, scope{name: name, indent: indent, kind: "class", symIdx: len(pf.Symbols) - 1})
			continue
		}

		if m := defRE.FindStringSubmatch(masked); len(m) == 2 {
			name := m[1]
			path := scopeNames(scopes)
			container := module
			kind := "function"
			if len(path) > 0 {
				container = strings.Join(path, ".")
			}
			if len(scopes) > 0 && scopes[len(scopes)-1].kind == "class" {
				kind = "method"
			}
			qualified := qualifiedName(module, append(path, name))
			stableKey := "func:" + module + "::" + name
			if len(path) > 0 {
				stableKey = "func:" + module + ":" + strings.Join(path, ".") + ":" + name
			}
			sig := strings.TrimSpace(trimmed)
			sym := graph.Symbol{
				Language:      "python",
				Kind:          kind,
				Name:          name,
				QualifiedName: qualified,
				ContainerName: container,
				Signature:     sig,
				Visibility:    visibility(name),
				Range: graph.Position{
					StartLine: lineNo,
					StartCol:  indent + 1,
					EndLine:   lineNo,
					EndCol:    len(line) + 1,
				},
				StableKey: stableKey,
			}
			pf.Symbols = append(pf.Symbols, sym)
			scopes = append(scopes, scope{name: name, indent: indent, kind: "function", symIdx: len(pf.Symbols) - 1})
			continue
		}

		if strings.HasPrefix(trimmed, "import ") {
			imports := strings.Split(strings.TrimPrefix(trimmed, "import "), ",")
			for _, imp := range imports {
				imp = strings.TrimSpace(imp)
				if imp != "" {
					pf.Imports = append(pf.Imports, imp)
				}
			}
		}
		if strings.HasPrefix(trimmed, "from ") && strings.Contains(trimmed, " import ") {
			parts := strings.SplitN(strings.TrimPrefix(trimmed, "from "), " import ", 2)
			if len(parts) == 2 {
				modulePath := strings.TrimSpace(parts[0])
				if modulePath != "" {
					pf.Imports = append(pf.Imports, modulePath)
				}
			}
		}

		if lastFunction(scopes) < 0 {
			continue
		}
		for _, m := range callRE.FindAllStringSubmatch(masked, -1) {
			if len(m) != 2 {
				continue
			}
			name := m[1]
			if isPythonKeyword(name) {
				continue
			}
			pf.Edges = append(pf.Edges, graph.Edge{
				SrcSymbolID: 0,
				DstName:     name,
				Kind:        "calls",
				Evidence:    strings.TrimSpace(line),
				Line:        lineNo,
			})
			pf.References = append(pf.References, graph.Reference{
				Kind:          "call",
				Name:          name,
				QualifiedName: name,
				Range: graph.Position{
					StartLine: lineNo,
					StartCol:  indent + 1,
					EndLine:   lineNo,
					EndCol:    len(line) + 1,
				},
			})
		}
	}

	// EOF closes every open scope at the file's last content line.
	closeScopes(scopes, -1, lastLine, lastLen, pf.Symbols)

	return pf, nil
}

// closeScopes pops every scope whose indent the current line dedents to (or
// past) and seals the popped symbol's range at the last content line seen,
// which by construction is the final line of that scope's body. Ranges are
// inclusive on both ends. A declaration whose body never got a content line
// keeps its single-line range.
func closeScopes(stack []scope, indent, lastLine, lastLen int, symbols []graph.Symbol) []scope {
	for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
		top := stack[len(stack)-1]
		if r := &symbols[top.symIdx].Range; lastLine > r.EndLine {
			r.EndLine = lastLine
			r.EndCol = lastLen + 1
		}
		stack = stack[:len(stack)-1]
	}
	return stack
}

func scopeNames(scopes []scope) []string {
	names := make([]string, 0, len(scopes))
	for _, s := range scopes {
		names = append(names, s.name)
	}
	return names
}

func qualifiedName(module string, path []string) string {
	if len(path) == 0 {
		return module
	}
	return module + "." + strings.Join(path, ".")
}

func lastFunction(scopes []scope) int {
	for i := len(scopes) - 1; i >= 0; i-- {
		if scopes[i].kind == "function" {
			return i
		}
	}
	return -1
}

type pythonLexState struct {
	quote   byte
	triple  bool
	escaped bool
}

func maskPythonLines(lines []string) []string {
	state := pythonLexState{}
	masked := make([]string, len(lines))
	for i, line := range lines {
		masked[i] = maskPythonLine(line, &state)
	}
	return masked
}

func maskPythonLine(line string, state *pythonLexState) string {
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if state.quote != 0 {
			if state.triple {
				if i+2 < len(line) && line[i] == state.quote && line[i+1] == state.quote && line[i+2] == state.quote {
					b.WriteString("   ")
					i += 2
					state.quote, state.triple = 0, false
				} else {
					b.WriteByte(' ')
				}
				continue
			}
			if state.escaped {
				state.escaped = false
				b.WriteByte(' ')
				continue
			}
			if ch == '\\' {
				state.escaped = true
				b.WriteByte(' ')
				continue
			}
			b.WriteByte(' ')
			if ch == state.quote {
				state.quote = 0
			}
			continue
		}
		if ch == '#' {
			b.WriteString(strings.Repeat(" ", len(line)-i))
			break
		}
		if ch == '\'' || ch == '"' {
			state.quote = ch
			if i+2 < len(line) && line[i+1] == ch && line[i+2] == ch {
				state.triple = true
				b.WriteString("   ")
				i += 2
			} else {
				b.WriteByte(' ')
			}
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func lineIndent(line string) int {
	n := 0
	for _, r := range line {
		if r == ' ' {
			n++
			continue
		}
		if r == '\t' {
			n += 4
			continue
		}
		break
	}
	return n
}

func visibility(name string) string {
	if name == "" {
		return ""
	}
	if strings.HasPrefix(name, "_") {
		return "module"
	}
	return "public"
}

func isPythonKeyword(name string) bool {
	switch name {
	case "if", "for", "while", "return", "print", "with", "class", "def", "try", "except", "elif":
		return true
	default:
		return false
	}
}
