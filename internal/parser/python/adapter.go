package python

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

var (
	classRE = regexp.MustCompile(`^\s*class\s+([A-Za-z_][A-Za-z0-9_]*)`)
	defRE   = regexp.MustCompile(`^\s*def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	callRE  = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

type scope struct {
	name   string
	indent int
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
		FileTokens: tokenWeights(content),
	}
	funcByStable := map[string]graph.Symbol{}
	var classStack []scope
	var funcStack []scope

	for i, line := range lines {
		lineNo := i + 1
		indent := lineIndent(line)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		for len(classStack) > 0 && indent <= classStack[len(classStack)-1].indent {
			classStack = classStack[:len(classStack)-1]
		}
		for len(funcStack) > 0 && indent <= funcStack[len(funcStack)-1].indent {
			funcStack = funcStack[:len(funcStack)-1]
		}

		if m := classRE.FindStringSubmatch(line); len(m) == 2 {
			name := m[1]
			qualified := module + "." + name
			sym := graph.Symbol{
				Language:      "python",
				Kind:          "class",
				Name:          name,
				QualifiedName: qualified,
				ContainerName: module,
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
			classStack = append(classStack, scope{name: name, indent: indent})
			continue
		}

		if m := defRE.FindStringSubmatch(line); len(m) == 2 {
			name := m[1]
			container := module
			qualified := module + "." + name
			stableKey := "func:" + module + "::" + name
			if len(classStack) > 0 {
				container = classStack[len(classStack)-1].name
				qualified = module + "." + container + "." + name
				stableKey = "func:" + module + ":" + container + ":" + name
			}
			sig := strings.TrimSpace(trimmed)
			sym := graph.Symbol{
				Language:      "python",
				Kind:          "function",
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
			funcByStable[stableKey] = sym
			funcStack = append(funcStack, scope{name: stableKey, indent: indent})
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

		if len(funcStack) == 0 {
			continue
		}
		srcStable := funcStack[len(funcStack)-1].name
		if _, ok := funcByStable[srcStable]; !ok {
			continue
		}
		for _, m := range callRE.FindAllStringSubmatch(line, -1) {
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

	return pf, nil
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

func tokenWeights(content []byte) map[string]float64 {
	text := strings.ToLower(string(content))
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '/' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	weights := map[string]float64{}
	for _, field := range fields {
		if len(field) < 2 {
			continue
		}
		weights[field] += 1
	}
	return weights
}
