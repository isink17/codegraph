package heuristic

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/texttoken"
)

type importPattern struct {
	re        *regexp.Regexp
	nameGroup int
}

type symbolPattern struct {
	kind      string
	re        *regexp.Regexp
	nameGroup int
}

type Adapter struct {
	language    string
	exts        map[string]struct{}
	imports     []importPattern
	symbols     []symbolPattern
	skipFuncSet map[string]struct{}
	cStyle      bool
	hashStyle   bool
}

func NewJava() *Adapter {
	return &Adapter{
		language: "java",
		exts:     extSet(".java"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*import\s+([^;]+);`), nameGroup: 1},
		},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`\b(class|interface|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 2},
			{kind: "function", re: regexp.MustCompile(`^\s*(?:public|protected|private|static|final|native|synchronized|abstract|\s)*[A-Za-z0-9_<>\[\], ?]+\s+([A-Za-z_][A-Za-z0-9_]*)\s*\([^;]*\)\s*(?:\{|throws|$)`), nameGroup: 1},
		},
		cStyle: true,
	}
}

func NewKotlin() *Adapter {
	return &Adapter{
		language: "kotlin",
		exts:     extSet(".kt", ".kts"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*import\s+([A-Za-z0-9_.*]+)`), nameGroup: 1},
		},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`\b(class|interface|object|enum\s+class)\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 2},
			{kind: "function", re: regexp.MustCompile(`^\s*fun\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), nameGroup: 1},
		},
		cStyle: true,
	}
}

func NewCSharp() *Adapter {
	return &Adapter{
		language: "csharp",
		exts:     extSet(".cs"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*using\s+([^;]+);`), nameGroup: 1},
		},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`\b(class|interface|struct|enum|record)\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 2},
			{kind: "function", re: regexp.MustCompile(`^\s*(?:public|private|protected|internal|static|virtual|override|async|sealed|new|partial|\s)+[A-Za-z0-9_<>\[\],?.]+\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), nameGroup: 1},
		},
		cStyle: true,
	}
}

func NewTypeScriptJavaScript() *Adapter {
	return &Adapter{
		language: "typescript",
		exts:     extSet(".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*import\s+.*\s+from\s+['"]([^'"]+)['"]`), nameGroup: 1},
			{re: regexp.MustCompile(`^\s*import\s+['"]([^'"]+)['"]`), nameGroup: 1},
			{re: regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`), nameGroup: 1},
		},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`\bclass\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 1},
			{kind: "function", re: regexp.MustCompile(`\bfunction\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), nameGroup: 1},
			{kind: "function", re: regexp.MustCompile(`\b(?:const|let|var)\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(?:async\s*)?\([^)]*\)\s*=>`), nameGroup: 1},
		},
		cStyle: true,
	}
}

func NewRust() *Adapter {
	return &Adapter{
		language: "rust",
		exts:     extSet(".rs"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*use\s+([^;]+);`), nameGroup: 1},
		},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`\b(struct|enum|trait|impl)\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 2},
			{kind: "function", re: regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), nameGroup: 1},
		},
		cStyle: true,
	}
}

func NewRuby() *Adapter {
	return &Adapter{
		language: "ruby",
		exts:     extSet(".rb"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*(?:require|require_relative)\s+['"]([^'"]+)['"]`), nameGroup: 1},
		},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`^\s*(?:class|module)\s+([A-Za-z_][A-Za-z0-9_:]*)`), nameGroup: 1},
			{kind: "function", re: regexp.MustCompile(`^\s*def\s+([A-Za-z_][A-Za-z0-9_!?=]*)`), nameGroup: 1},
		},
		hashStyle: true,
	}
}

func NewSwift() *Adapter {
	return &Adapter{
		language: "swift",
		exts:     extSet(".swift"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*import\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 1},
		},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`\b(class|struct|enum|protocol|actor)\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 2},
			{kind: "function", re: regexp.MustCompile(`^\s*func\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), nameGroup: 1},
		},
		cStyle: true,
	}
}

func NewPHP() *Adapter {
	return &Adapter{
		language: "php",
		exts:     extSet(".php"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*use\s+([^;]+);`), nameGroup: 1},
			{re: regexp.MustCompile(`^\s*require(?:_once)?\s*\(?\s*['"]([^'"]+)['"]`), nameGroup: 1},
		},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`\b(class|interface|trait|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 2},
			{kind: "function", re: regexp.MustCompile(`^\s*(?:public|private|protected|static|\s)*function\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), nameGroup: 1},
		},
		cStyle:    true,
		hashStyle: true,
	}
}

func NewCAndCpp() *Adapter {
	return &Adapter{
		language: "cpp",
		exts:     extSet(".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx", ".ipp"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*#include\s+[<"]([^>"]+)[>"]`), nameGroup: 1},
		},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`^\s*(class|struct|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 2},
			{kind: "function", re: regexp.MustCompile(`^\s*[A-Za-z_][A-Za-z0-9_:\<\>\*\&\s]*\s+([A-Za-z_][A-Za-z0-9_]*)\s*\([^;]*\)\s*(?:\{|$)`), nameGroup: 1},
		},
		skipFuncSet: map[string]struct{}{
			"if": {}, "for": {}, "while": {}, "switch": {}, "catch": {}, "sizeof": {}, "return": {},
		},
		cStyle: true,
	}
}

func (a *Adapter) Language() string {
	return a.language
}

func (a *Adapter) Supports(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	_, ok := a.exts[ext]
	return ok
}

func (a *Adapter) Extensions() []string {
	out := make([]string, 0, len(a.exts))
	for ext := range a.exts {
		out = append(out, ext)
	}
	return out
}

type classScope struct {
	name  string
	depth int
}

func (a *Adapter) Parse(_ context.Context, path string, content []byte) (graph.ParsedFile, error) {
	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	lines := strings.Split(string(content), "\n")
	pf := graph.ParsedFile{
		Language:   a.language,
		FileTokens: texttoken.Weights(content),
	}

	depth := 0
	classScopes := []classScope{}
	inBlockComment := false

	for i, line := range lines {
		lineNo := i + 1
		normalized, blockState := stripForHeuristic(line, inBlockComment, a.cStyle, a.hashStyle)
		inBlockComment = blockState
		trimmed := strings.TrimSpace(normalized)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#!") {
			depth += braceDelta(normalized)
			continue
		}
		for len(classScopes) > 0 && depth < classScopes[len(classScopes)-1].depth {
			classScopes = classScopes[:len(classScopes)-1]
		}

		for _, imp := range a.imports {
			if m := imp.re.FindStringSubmatch(normalized); len(m) > imp.nameGroup {
				val := strings.TrimSpace(m[imp.nameGroup])
				if val != "" {
					pf.Imports = append(pf.Imports, val)
				}
			}
		}

		for _, sym := range a.symbols {
			m := sym.re.FindStringSubmatch(normalized)
			if len(m) <= sym.nameGroup {
				continue
			}
			name := strings.TrimSpace(m[sym.nameGroup])
			if name == "" {
				continue
			}
			if sym.kind == "function" {
				if _, skip := a.skipFuncSet[name]; skip {
					continue
				}
			}
			container := module
			if len(classScopes) > 0 {
				container = classScopes[len(classScopes)-1].name
			}
			qualified := module + "." + name
			if container != module {
				qualified = module + "." + container + "." + name
			}
			stablePrefix := "func"
			if sym.kind != "function" {
				stablePrefix = "type"
			}
			stableKey := stablePrefix + ":" + a.language + ":" + module + ":" + name
			pf.Symbols = append(pf.Symbols, graph.Symbol{
				Language:      a.language,
				Kind:          sym.kind,
				Name:          name,
				QualifiedName: qualified,
				ContainerName: container,
				Visibility:    visibility(name),
				Range: graph.Position{
					StartLine: lineNo,
					StartCol:  1,
					EndLine:   lineNo,
					EndCol:    len(line) + 1,
				},
				StableKey: stableKey,
			})
			if sym.kind != "function" {
				opens := strings.Count(line, "{")
				scopeDepth := depth + opens
				if scopeDepth <= depth {
					scopeDepth = depth + 1
				}
				classScopes = append(classScopes, classScope{name: name, depth: scopeDepth})
			}
			break
		}

		depth += braceDelta(normalized)
		if depth < 0 {
			depth = 0
		}
	}
	return pf, nil
}

func extSet(exts ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, ext := range exts {
		out[strings.ToLower(ext)] = struct{}{}
	}
	return out
}

func braceDelta(line string) int {
	return strings.Count(line, "{") - strings.Count(line, "}")
}

func visibility(name string) string {
	if name == "" {
		return ""
	}
	r, _ := utf8FirstRune(name)
	if unicode.IsUpper(r) {
		return "public"
	}
	if strings.HasPrefix(name, "_") {
		return "private"
	}
	return "module"
}

func utf8FirstRune(s string) (rune, int) {
	for _, r := range s {
		return r, 1
	}
	return rune(0), 0
}

func stripForHeuristic(line string, inBlockComment, cStyle, hashStyle bool) (string, bool) {
	if line == "" {
		return "", inBlockComment
	}
	var b strings.Builder
	b.Grow(len(line))
	inString := false
	stringQuote := byte(0)
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		next := byte(0)
		if i+1 < len(line) {
			next = line[i+1]
		}
		if inBlockComment {
			if cStyle && ch == '*' && next == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == stringQuote {
				inString = false
				stringQuote = 0
			}
			continue
		}
		if cStyle && ch == '/' && next == '*' {
			inBlockComment = true
			i++
			continue
		}
		if cStyle && ch == '/' && next == '/' {
			break
		}
		if hashStyle && ch == '#' {
			break
		}
		if ch == '"' || ch == '\'' || ch == '`' {
			inString = true
			stringQuote = ch
			continue
		}
		b.WriteByte(ch)
	}
	return b.String(), inBlockComment
}
