package heuristic

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
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

var heredocStartRE = regexp.MustCompile(`<<[-~]?['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
var heuristicCSharpStaticRE = regexp.MustCompile(`^static\s+`)
var heuristicCSharpNamespaceRE = regexp.MustCompile(`(?m)^\s*namespace\s+([A-Za-z_][A-Za-z0-9_.]*)\s*(?:;|\{)`)

type Adapter struct {
	language    string
	exts        map[string]struct{}
	imports     []importPattern
	symbols     []symbolPattern
	skipFuncSet map[string]struct{}
	cStyle      bool
	hashStyle   bool
	// rawImports matches the import patterns against the line as written,
	// because the stripped line has lost its string literals; only a line
	// that does not start inside a string or comment is read.
	rawImports bool
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
			{kind: "constructor", re: regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\([^;]*\)\s*\{`), nameGroup: 1},
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

func NewLua() *Adapter {
	return &Adapter{
		language: "lua",
		exts:     extSet(".lua"),
		imports:  []importPattern{{re: regexp.MustCompile(`require\s*\(\s*["']([^"']+)["']\s*\)`), nameGroup: 1}},
		symbols:  []symbolPattern{{kind: "function", re: regexp.MustCompile(`(?m)^\s*(?:local\s+)?function\s+([A-Za-z_][A-Za-z0-9_.:]*)\s*\(`), nameGroup: 1}},
	}
}

func NewScala() *Adapter {
	return &Adapter{
		language: "scala",
		exts:     extSet(".scala"),
		imports:  []importPattern{{re: regexp.MustCompile(`^\s*import\s+([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)`), nameGroup: 1}},
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`\b(class|trait|object|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 2},
			{kind: "function", re: regexp.MustCompile(`^\s*(?:(?:private|protected|override|final|implicit|inline|transparent|abstract|sealed)(?:\[[^\]]*\])?\s+)*def\s+([A-Za-z_][A-Za-z0-9_]*)`), nameGroup: 1},
		},
		cStyle: true,
	}
}

// NewDart is the non-CGO Dart fallback: classes, mixins, enums, extensions,
// typedefs and typed function or method headers, plus directive URIs. It
// builds no call graph, like the CGO Dart adapter.
func NewDart() *Adapter {
	return &Adapter{
		language: "dart",
		exts:     extSet(".dart"),
		imports: []importPattern{
			{re: regexp.MustCompile(`^\s*(?:import|export|part\s+of|part)\s+r?'([^'$]+)'`), nameGroup: 1},
			{re: regexp.MustCompile(`^\s*(?:import|export|part\s+of|part)\s+r?"([^"$]+)"`), nameGroup: 1},
		},
		rawImports: true,
		symbols: []symbolPattern{
			{kind: "type", re: regexp.MustCompile(`^\s*(?:(?:abstract|sealed|base|interface|final|mixin)\s+)*(?:class|mixin|enum)\s+([A-Za-z_$][\w$]*)`), nameGroup: 1},
			{kind: "type", re: regexp.MustCompile(`^\s*extension\s+type\s+(?:const\s+)?([A-Za-z_$][\w$]*)`), nameGroup: 1},
			{kind: "type", re: regexp.MustCompile(`^\s*extension\s+([A-Za-z_$][\w$]*)(?:\s*<[^>]*>)?\s+on\b`), nameGroup: 1},
			{kind: "type", re: regexp.MustCompile(`^\s*typedef\s+([A-Za-z_$][\w$]*)\s*(?:<[^>]*>)?\s*=`), nameGroup: 1},
			// A header needs a return type (void, a builtin, or a capitalised
			// type), so a call such as `setState(() {` is not a declaration.
			{kind: "function", re: regexp.MustCompile(`^\s*(?:(?:static|external|abstract)\s+)*(?:void|int|double|num|bool|dynamic|[A-Z][\w$]*(?:<[^()=]*>)?\??)\s+([A-Za-z_$][\w$]*)\s*(?:<[^()]*>)?\s*\([^;]*\)\s*(?:async\*?\s*|sync\*\s*)?(?:\{|=>)`), nameGroup: 1},
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

type stripState struct {
	inBlockComment bool
	// nestedComment records a `/*` inside a block comment. Only Kotlin nests
	// them, and the stripper does not, so its package reading fails closed.
	nestedComment  bool
	inString       bool
	stringQuote    byte
	stringDelim    int
	stringRaw      bool
	stringVerbatim bool
	escaped        bool
	heredocTerm    string
}

func (a *Adapter) Parse(_ context.Context, path string, content []byte) (graph.ParsedFile, error) {
	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	lines := strings.Split(string(content), "\n")
	pf := graph.ParsedFile{
		Language:   a.language,
		FileTokens: texttoken.Weights(content),
	}
	if a.language == "kotlin" {
		pf.Scope.Package = heuristicKotlinPackage(content)
		module = pf.Scope.Package
	}
	if a.language == "csharp" {
		module = heuristicCSharpModule(content)
		pf.Scope.Package = module
	}

	depth := 0
	classScopes := []classScope{}
	state := stripState{}

	for i, line := range lines {
		lineNo := i + 1
		line = strings.TrimSuffix(line, "\r")
		lineStartsInCode := !state.inString && !state.inBlockComment && state.heredocTerm == ""
		normalized, nextState := stripForHeuristic(line, state, a.cStyle, a.hashStyle)
		state = nextState
		trimmed := strings.TrimSpace(normalized)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#!") {
			depth += braceDelta(normalized)
			continue
		}
		for len(classScopes) > 0 && depth < classScopes[len(classScopes)-1].depth {
			classScopes = classScopes[:len(classScopes)-1]
		}

		if a.language == "kotlin" && !state.nestedComment && strings.HasPrefix(trimmed, "import ") {
			addHeuristicKotlinScope(trimmed, &pf.Scope.Imports)
		}
		for _, imp := range a.imports {
			importSource := normalized
			if a.rawImports {
				if !lineStartsInCode {
					continue
				}
				importSource = line
			}
			if m := imp.re.FindStringSubmatch(importSource); len(m) > imp.nameGroup {
				val := strings.TrimSpace(m[imp.nameGroup])
				if val != "" {
					pf.Imports = append(pf.Imports, val)
					if a.language == "csharp" {
						pf.Scope.Imports = append(pf.Scope.Imports, heuristicCSharpImport(val))
					}
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
			if sym.kind == "constructor" && (a.language != "java" || len(classScopes) == 0 || classScopes[len(classScopes)-1].name != name) {
				continue
			}
			container := module
			if len(classScopes) > 0 {
				names := make([]string, 0, len(classScopes))
				for _, scope := range classScopes {
					names = append(names, scope.name)
				}
				container = strings.Join(names, ".")
			}
			qualified := heuristicQualified(module, name)
			if a.language == "kotlin" {
				qualified = heuristicQualified(module, name)
			}
			if container != module {
				if a.language == "kotlin" {
					qualified = heuristicQualified(module, container+"."+name)
				} else {
					qualified = heuristicQualified(module, container+"."+name)
				}
			}
			stablePrefix := "func"
			emittedKind := sym.kind
			if sym.kind == "constructor" {
				emittedKind = "function"
			}
			if emittedKind == "type" {
				stablePrefix = "type"
			}
			stableKey := stablePrefix + ":" + a.language + ":" + module + ":" + name
			if container != module {
				stableKey = stablePrefix + ":" + a.language + ":" + module + ":" + container + ":" + name
			}
			pf.Symbols = append(pf.Symbols, graph.Symbol{
				Language:      a.language,
				Kind:          emittedKind,
				Name:          name,
				QualifiedName: qualified,
				ContainerName: container,
				Signature:     heuristicCallableSignature(sym.kind, trimmed),
				Visibility:    visibility(name),
				Range: graph.Position{
					StartLine: lineNo,
					StartCol:  1,
					EndLine:   lineNo,
					EndCol:    len(line) + 1,
				},
				StableKey: stableKey,
			})
			if sym.kind == "type" {
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

func heuristicQualified(pkg, name string) string {
	return strings.Trim(strings.TrimSpace(pkg)+"."+strings.TrimSpace(name), ".")
}

var heuristicKotlinPackageRE = regexp.MustCompile(`^package\s+([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*;?$`)

// heuristicKotlinPackage applies the grammar's header rule to comment- and
// string-stripped lines: after a shebang and @file: annotations, the first
// significant text must be the whole `package a.b` line, with no second header.
// A package spelled in a comment or string, anywhere later, or after a nested
// block comment is no
// package.
func heuristicKotlinPackage(content []byte) string {
	state := stripState{}
	pkg := ""
	depth := 0 // open ( and [ of a @file: annotation spanning lines
	for _, line := range strings.Split(string(content), "\n") {
		var normalized string
		normalized, state = stripForHeuristic(strings.TrimSuffix(line, "\r"), state, true, false)
		if state.nestedComment {
			return ""
		}
		rest := strings.TrimSpace(normalized)
		if pkg != "" {
			if fields := strings.Fields(rest); len(fields) > 0 && fields[0] == "package" {
				return "" // a second header is ambiguous, even if its name is invalid
			}
			continue
		}
		if strings.HasPrefix(rest, "#!") {
			continue
		}
		for depth > 0 || strings.HasPrefix(rest, "@file:") {
			if depth == 0 {
				rest = strings.TrimLeftFunc(rest[len("@file:"):], func(r rune) bool {
					return r == '_' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r)
				})
				rest = strings.TrimSpace(rest)
				if !strings.HasPrefix(rest, "(") && !strings.HasPrefix(rest, "[") {
					continue
				}
			}
			end := len(rest)
			for i := 0; i < len(rest); i++ {
				switch rest[i] {
				case '(', '[':
					depth++
				case ')', ']':
					depth--
				}
				if depth == 0 {
					end = i + 1
					break
				}
			}
			rest = strings.TrimSpace(rest[end:])
			if depth > 0 {
				break
			}
		}
		if rest == "" {
			continue
		}
		if m := heuristicKotlinPackageRE.FindStringSubmatch(rest); len(m) == 2 {
			pkg = m[1]
			continue
		}
		return ""
	}
	return pkg
}

func addHeuristicKotlinScope(line string, out *[]graph.ScopeImport) {
	parts := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "import")))
	if len(parts) == 0 {
		return
	}
	path := parts[0]
	wildcard := strings.HasSuffix(path, ".*")
	name := strings.TrimSuffix(path, ".*")
	imported := name
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		imported = name[i+1:]
	}
	local := imported
	if wildcard {
		imported, local = "", ""
	}
	if len(parts) >= 3 && parts[1] == "as" {
		local = parts[2]
	}
	*out = append(*out, graph.ScopeImport{SourceSpecifier: name, ImportedName: imported, LocalName: local, Kind: graph.ScopeImportNamed, Wildcard: wildcard})
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
	var r rune
	for _, v := range name {
		r = v
		break
	}
	if unicode.IsUpper(r) {
		return "public"
	}
	if strings.HasPrefix(name, "_") {
		return "private"
	}
	return "module"
}

func heuristicSignature(declaration string) string {
	end := len(declaration)
	if close := strings.LastIndex(declaration, ")"); close >= 0 {
		end = close + 1
		if tail := declaration[end:]; strings.HasPrefix(strings.TrimSpace(tail), ":") {
			if eq := strings.Index(tail, "="); eq >= 0 {
				end += eq
			}
		}
		if brace := strings.IndexAny(declaration[end:], "{"); brace >= 0 {
			end += brace
		}
	}
	return strings.TrimSpace(declaration[:end])
}

func heuristicCallableSignature(kind, declaration string) string {
	if kind != "function" && kind != "constructor" {
		return ""
	}
	return heuristicSignature(declaration)
}

func stripForHeuristic(line string, state stripState, cStyle, hashStyle bool) (string, stripState) {
	if line == "" {
		return "", state
	}
	if state.heredocTerm != "" {
		if strings.TrimSpace(line) == state.heredocTerm {
			state.heredocTerm = ""
		}
		return "", state
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); i++ {
		ch := line[i]
		next := byte(0)
		if i+1 < len(line) {
			next = line[i+1]
		}
		if state.inBlockComment {
			if cStyle && ch == '*' && next == '/' {
				state.inBlockComment = false
				i++
			} else if ch == '/' && next == '*' {
				state.nestedComment = true
			}
			continue
		}
		if state.inString {
			if state.stringRaw {
				if ch == state.stringQuote && quoteRun(line, i) >= state.stringDelim {
					i += state.stringDelim - 1
					state.inString = false
					state.stringRaw = false
					state.stringQuote = 0
					state.stringDelim = 0
				}
				continue
			}
			if state.escaped {
				state.escaped = false
				continue
			}
			if ch == '\\' {
				state.escaped = true
				continue
			}
			if state.stringVerbatim && ch == state.stringQuote && next == state.stringQuote {
				i++ // doubled quote inside a C# verbatim string
				continue
			}
			if ch == state.stringQuote {
				state.inString = false
				state.stringQuote = 0
				state.stringDelim = 0
				state.stringVerbatim = false
			}
			continue
		}
		if cStyle && ch == '/' && next == '*' {
			state.inBlockComment = true
			i++
			continue
		}
		if cStyle && ch == '/' && next == '/' {
			break
		}
		if hashStyle && ch == '#' {
			break
		}
		if quote, start, delim, raw, verbatim, ok := heuristicStringStart(line, i, cStyle); ok {
			state.inString = true
			state.stringQuote = quote
			state.stringDelim = delim
			state.stringRaw = raw
			state.stringVerbatim = verbatim
			i = start
			continue
		}
		if hashStyle && ch == '<' && next == '<' {
			if m := heredocStartRE.FindStringSubmatch(line[i:]); len(m) == 2 {
				state.heredocTerm = m[1]
				break
			}
		}
		b.WriteByte(ch)
	}
	return b.String(), state
}

func heuristicStringStart(line string, i int, cStyle bool) (quote byte, quoteStart, delimiter int, raw, verbatim, ok bool) {
	start := i
	if cStyle {
		for i < len(line) && line[i] == '$' {
			i++
		}
		if i < len(line) && line[i] == '@' {
			verbatim = true
			i++
		}
		if i == start && line[i] == '@' {
			verbatim = true
			i++
			for i < len(line) && line[i] == '$' {
				i++
			}
		}
	}
	if i >= len(line) || (line[i] != '"' && line[i] != '\'' && line[i] != '`') {
		return 0, start, 0, false, false, false
	}
	quote = line[i]
	delimiter = 1
	if quote == '"' {
		delimiter = quoteRun(line, i)
		raw = delimiter >= 3
	}
	return quote, i, delimiter, raw, verbatim, true
}

func quoteRun(line string, i int) int {
	end := i
	for end < len(line) && line[end] == '"' {
		end++
	}
	return end - i
}

// Profile identifies the heuristic fallback for this adapter's language. These
// adapters extract symbols and imports from regular expressions and emit NO
// call edges at all -- which is exactly why the indexer must refuse to replace
// a call-capable graph with one of them. See parser.Profile.
func (a *Adapter) Profile() parser.Profile {
	version := "v1"
	// v4 handles raw and verbatim strings without leaking scope text.
	if a.language == "kotlin" {
		version = "v4"
	}
	// C# v5 recognises `using static` followed by any whitespace.
	if a.language == "csharp" {
		version = "v5"
	}
	return parser.NewProfile(a.language, "heuristic:"+a.language+":"+version, false)
}

func heuristicCSharpImport(value string) graph.ScopeImport {
	value = strings.TrimSpace(value)
	// `static` is a keyword only when whitespace of any kind follows it;
	// `staticns` is a namespace name.
	if m := heuristicCSharpStaticRE.FindString(value); m != "" {
		return graph.ScopeImport{SourceSpecifier: strings.TrimSpace(value[len(m):]), Kind: "static", Static: true}
	}
	if i := strings.Index(value, "="); i >= 0 {
		source := strings.TrimSpace(value[i+1:])
		return graph.ScopeImport{SourceSpecifier: source, ImportedName: source, LocalName: strings.TrimSpace(value[:i]), Kind: "alias"}
	}
	local := value
	if i := strings.LastIndexByte(value, '.'); i >= 0 {
		local = value[i+1:]
	}
	return graph.ScopeImport{SourceSpecifier: value, ImportedName: value, LocalName: local, Kind: "namespace"}
}

func heuristicCSharpModule(content []byte) string {
	state := stripState{}
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		lines[i], state = stripForHeuristic(line, state, true, false)
	}
	text := strings.Join(lines, "\n")
	matches := heuristicCSharpNamespaceRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) != 1 {
		return ""
	}
	m := matches[0]
	name := text[m[2]:m[3]]
	declaration := text[m[0]:m[1]]
	if strings.Contains(declaration, ";") {
		return name
	}
	prefix := strings.TrimSpace(text[:m[0]])
	if prefix != "" {
		return ""
	}
	open := strings.IndexByte(text[m[0]:m[1]], '{')
	if open < 0 {
		return ""
	}
	open += m[0]
	depth := 0
	close := -1
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				close = i
			}
		}
		if close >= 0 {
			break
		}
	}
	if close < 0 || strings.TrimSpace(text[close+1:]) != "" {
		return ""
	}
	return name
}
