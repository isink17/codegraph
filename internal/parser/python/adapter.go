package python

import (
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/texttoken"
)

var (
	// classRE demands the header go on after the name (`:`, bases, PEP 695
	// type parameters, or a backslash continuation), so a name cut short by a
	// rune the class does not know is not recorded as a shorter name.
	classRE = regexp.MustCompile(`^\s*class\s+(` + identifier + `)\s*[:(\[\\]`)
	defRE   = regexp.MustCompile(`^\s*(?:async\s+)?def\s+(` + identifier + `)\s*\(`)
	// callRE keeps the whole dotted receiver chain a call site actually wrote,
	// so `helpers.load()` stays distinguishable from a bare `load()`. The chain
	// is syntax, not a claim about what `helpers` is.
	callRE = regexp.MustCompile(`(` + identifier + `(?:\.` + identifier + `)*)\s*\(`)
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
	// Import syntax is read from the masked source before the line walk, so a
	// statement spanning several physical lines is still one binding and its
	// lines never reach call extraction. The bindings themselves are emitted
	// inside the walk, where the enclosing lexical scope is known.
	stmts, importLines := importStatements(maskedLines)
	seenModules := make(map[string]struct{}, len(stmts))
	// lastLine/lastLen track the most recent content line (not blank, not a
	// comment). A scope popped by a dedent ends at that line: blank lines,
	// comments and decorators between declarations belong to no body.
	lastLine, lastLen := 0, 0
	// open tracks bracket depth carried over from previous lines. A line that
	// continues an open bracket carries no indentation meaning -- the `):`
	// closing a signature split over several lines is not a dedent out of the
	// declaration it belongs to -- and it declares nothing of its own.
	open := 0

	for i, line := range lines {
		lineNo := i + 1
		line = strings.TrimSuffix(line, "\r")
		masked := strings.TrimSuffix(maskedLines[i], "\r")
		indent := lineIndent(line)
		trimmed := strings.TrimSpace(line)
		continued := open > 0
		open += bracketDelta(masked)
		if open < 0 {
			open = 0
		}
		if strings.TrimSpace(masked) == "" {
			continue
		}
		if continued {
			lastLine, lastLen = lineNo, len(line)
			continue
		}

		scopes = closeScopes(scopes, indent, lastLine, lastLen, pf.Symbols)
		lastLine, lastLen = lineNo, len(line)

		if m := classRE.FindStringSubmatch(masked); len(m) == 2 {
			name := NormalizeIdentifier(m[1])
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
			name := NormalizeIdentifier(m[1])
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

		if importLines[i] {
			// A class body is not a scope Python name lookup passes through, so
			// an import written in one reaches nothing this resolver can see.
			if len(scopes) > 0 && scopes[len(scopes)-1].kind == "class" {
				continue
			}
			for _, binding := range ImportBindings(stmts[i]) {
				binding.OwnerModule = strings.Join(scopeNames(scopes), ".")
				pf.Scope.Imports = append(pf.Scope.Imports, binding)
				if _, ok := seenModules[binding.SourceSpecifier]; !ok {
					seenModules[binding.SourceSpecifier] = struct{}{}
					pf.Imports = append(pf.Imports, binding.SourceSpecifier)
				}
			}
			continue
		}

		if lastFunction(scopes) < 0 {
			continue
		}
		scan := masked
		if from := patternSyntaxEnd(maskedLines, i); from > 0 {
			scan = strings.Repeat(" ", from) + masked[from:]
		}
		for _, loc := range callRE.FindAllStringSubmatchIndex(scan, -1) {
			if len(loc) != 4 {
				continue
			}
			// A chain whose head is itself a call or a subscript (`f().run()`,
			// `items[0]()`) has no name the parser can state truthfully, so it
			// emits nothing rather than the tail alone. Neither does a match
			// right after a non-ASCII rune: outside strings and comments that
			// rune can only be part of a name `identifier` does not know.
			if start := loc[2]; start > 0 && (strings.IndexByte(").]", scan[start-1]) >= 0 || scan[start-1] >= utf8.RuneSelf) {
				continue
			}
			name := NormalizeIdentifier(scan[loc[2]:loc[3]])
			if !strings.Contains(name, ".") && isPythonKeyword(name) {
				continue
			}
			pf.Edges = append(pf.Edges, graph.Edge{
				SrcSymbolID: 0,
				DstName:     name,
				Kind:        "calls",
				Evidence:    strings.TrimSpace(line),
				Line:        lineNo,
				Col:         loc[2] + 1,
			})
			pf.References = append(pf.References, graph.Reference{
				Kind:          "call",
				Name:          name,
				QualifiedName: name,
				Range: graph.Position{
					StartLine: lineNo,
					StartCol:  loc[2] + 1,
					EndLine:   lineNo,
					EndCol:    len(line) + 1,
				},
			})
		}
	}

	// EOF closes every open scope at the file's last content line.
	closeScopes(scopes, -1, lastLine, lastLen, pf.Symbols)
	addPythonLocalBindings(module, lines, &pf)

	return pf, nil
}

// addPythonLocalBindings records what each lexical scope binds itself: the
// module, and every function or method body now that their ranges are sealed.
// A method scans only its own body; class bodies are recorded by
// AddClassScopeBindings.
func addPythonLocalBindings(module string, lines []string, pf *graph.ParsedFile) {
	emit := func(owner, source string) {
		for _, binding := range LocalBindings(source, owner != "") {
			kind := graph.ScopeImportLocalBinding
			if binding.Declaration {
				kind = graph.ScopeImportNestedDeclaration
			}
			pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
				LocalName:   binding.Name,
				Kind:        kind,
				OwnerModule: binding.Owner(owner),
			})
		}
	}
	emit("", strings.Join(lines, "\n"))
	for _, sym := range pf.Symbols {
		if sym.Kind != "function" && sym.Kind != "method" {
			continue
		}
		start, end := sym.Range.StartLine-1, sym.Range.EndLine
		if start < 0 || end > len(lines) || start >= end {
			continue
		}
		emit(strings.TrimPrefix(sym.QualifiedName, module+"."), strings.Join(lines[start:end], "\n"))
	}
	AddClassScopeBindings(module, lines, pf)
}

// AddClassScopeBindings records what each class body binds, for the code that
// runs directly in it, from the symbols already parsed out of lines. Both
// Python adapters call it, so they record the same rows.
//
// A call written directly in a class body is attributed to the function the
// class is written in, so the body's names are recorded under that function as
// class-body bindings; a class outside every function has no such calls. A
// direct method's parameter defaults and annotations are evaluated in the
// class body too, but a call written in the method's `def` header is
// attributed to the method, so the body's names are also recorded under each
// direct method as header bindings, with the header's last line: they shadow
// only the calls on the header's lines, never the method body, which skips the
// class scope.
func AddClassScopeBindings(module string, lines []string, pf *graph.ParsedFile) {
	functions := map[string]bool{}
	classes := map[string][]LocalBinding{}
	for _, sym := range pf.Symbols {
		start, end := sym.Range.StartLine-1, sym.Range.EndLine
		switch {
		case sym.Kind == "function" || sym.Kind == "method":
			functions[sym.QualifiedName] = true
		case sym.Kind == "class" && start >= 0 && end <= len(lines) && start < end:
			// Classes sharing a qualified name (one per branch of an if/else
			// or try/except) cannot be told apart by their methods' or
			// children's parent name, so each answers for the union of their
			// bindings: a shared name only ever refuses more.
			for _, b := range ClassBodyBindings(strings.Join(lines[start:end], "\n")) {
				if !slices.ContainsFunc(classes[sym.QualifiedName], func(o LocalBinding) bool { return o.Name == b.Name }) {
					classes[sym.QualifiedName] = append(classes[sym.QualifiedName], b)
				}
			}
		}
	}
	// A function that assigns an attribute of a class written in a function
	// may replace that member with anything (`C.full = 3`). The row names
	// `receiver.attr` and is not a binding of the receiver name: it only tells
	// the resolver the member is not provably the one the class declares.
	localClasses := map[string]bool{}
	for _, sym := range pf.Symbols {
		if sym.Kind == "class" && strings.Count(strings.TrimPrefix(sym.QualifiedName, module+"."), ".") > 0 {
			localClasses[sym.Name] = true
		}
	}
	for _, sym := range pf.Symbols {
		start, end := sym.Range.StartLine-1, sym.Range.EndLine
		if (sym.Kind != "function" && sym.Kind != "method") || start < 0 || end > len(lines) || start >= end {
			continue
		}
		owner, seen := strings.TrimPrefix(sym.QualifiedName, module+"."), map[string]bool{}
		for _, target := range AttrAssignTargets(strings.Join(lines[start:end], "\n")) {
			if recv, _, _ := strings.Cut(target, "."); localClasses[recv] && !seen[target] {
				seen[target] = true
				pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
					LocalName:   target,
					Kind:        graph.ScopeImportLocalBinding,
					OwnerModule: owner,
				})
			}
		}
	}
	for _, sym := range pf.Symbols {
		start, end := sym.Range.StartLine-1, sym.Range.EndLine
		if start < 0 || end > len(lines) || start >= end {
			continue
		}
		parent := sym.QualifiedName[:max(strings.LastIndexByte(sym.QualifiedName, '.'), 0)]
		switch sym.Kind {
		case "class":
			// The innermost function the class is written in, if any.
			owner := parent
			for owner != "" && !functions[owner] {
				owner = owner[:max(strings.LastIndexByte(owner, '.'), 0)]
			}
			if owner == "" {
				continue
			}
			owner = strings.TrimPrefix(owner, module+".")
			for _, binding := range classes[sym.QualifiedName] {
				imported := ""
				if binding.Declaration {
					// A `def`/`class` the body declares; any other row is a
					// rebinding of the name.
					imported = "decl"
				}
				pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
					LocalName:    binding.Name,
					ImportedName: imported,
					Kind:         graph.ScopeImportClassBodyBinding,
					OwnerModule:  owner,
				})
			}
		case "method":
			bindings, ok := classes[parent]
			if !ok {
				continue
			}
			src := strings.Join(lines[start:end], "\n")
			last := strconv.Itoa(start + 1 + DefHeaderEnd(src))
			owner := strings.TrimPrefix(sym.QualifiedName, module+".")
			// Only the names the header spells: a class's every name under
			// its every method would grow with their product.
			for _, binding := range DefHeaderNames(src, bindings) {
				pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
					LocalName:    binding.Name,
					ImportedName: last,
					Kind:         graph.ScopeImportClassHeaderBinding,
					OwnerModule:  owner,
				})
			}
		}
	}
}

// patternSyntaxEnd returns how much of masked line i is soft-keyword syntax
// rather than expressions that can call: the `match` of a match statement (its
// subject is an expression), or a case pattern up to its guard or the header's
// colon. A pattern calls nothing -- `case Point(x=0):` is a class pattern --
// while a guard and a same-line body are expressions. `match` and `case`
// anywhere else are ordinary names.
//
// ponytail: a guard or header end on a continuation line is not seen; those
// lines are skipped for calls anyway.
func patternSyntaxEnd(masked []string, i int) int {
	line := strings.TrimSuffix(masked[i], "\r")
	trimmed := strings.TrimLeft(line, " \t")
	for _, keyword := range []string{"match", "case"} {
		rest, ok := strings.CutPrefix(trimmed, keyword)
		if !ok || rest == "" || strings.IndexByte(" \t([{-", rest[0]) < 0 {
			continue
		}
		stmt, _ := pythonLogicalLine(masked, i)
		header := headerColon(stmt)
		if header < 0 {
			return 0
		}
		start := len(line) - len(rest)
		if keyword == "match" {
			return start
		}
		end := min(header, len(line))
		if guard := guardKeyword(line[start:end]); guard >= 0 {
			return start + guard
		}
		return end
	}
	return 0
}

// headerColon is the index of the colon ending a compound statement header:
// the first `:` outside brackets that is not a walrus.
func headerColon(stmt string) int {
	depth := 0
	for i := 0; i < len(stmt); i++ {
		switch stmt[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ':':
			if depth == 0 && (i+1 == len(stmt) || stmt[i+1] != '=') {
				return i
			}
		}
	}
	return -1
}

// guardKeyword finds the `if` starting a case guard: the word itself, however
// it is spaced (`if(`, `)if`, a tab before it), never part of a longer name.
func guardKeyword(s string) int {
	for from := 0; ; {
		i := strings.Index(s[from:], "if")
		if i < 0 {
			return -1
		}
		i += from
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[i+2:])
		if (i == 0 || !isIdentifierRune(before)) && (i+2 == len(s) || !isIdentifierRune(after)) {
			return i
		}
		from = i + 2
	}
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
	case "if", "for", "while", "return", "print", "with", "as", "class", "def", "try", "except", "elif",
		"and", "or", "not", "in", "is", "lambda", "yield", "await", "assert", "del", "raise",
		"global", "nonlocal", "pass", "else", "finally", "import", "from":
		return true
	default:
		return false
	}
}

// Profile identifies the dedicated non-cgo Python adapter, which is
// regex-driven but does build a call graph. See parser.Profile. v2 reads
// Unicode names in declarations, calls, imports and local bindings. v3 stops
// reading `as (`, match statements and case patterns as calls. v4 records
// every name in its NFKC form, the name CPython binds (PEP 3131). v5 records
// lambda parameters and match-case captures as local bindings. v6 records
// lambdas in a def header's defaults, bodies on a header line and class-body
// bindings. v7 matches a method header's names by their NFKC form. v8 records global/nonlocal names, `del` targets, rebound and decorated class-body names. v9 records decorated module functions and literal module/member writes as negative binding evidence.
func (a *Adapter) Profile() parser.Profile {
	return parser.NewProfile("python", "python-regex:python:v9", true)
}
