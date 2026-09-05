package python

import (
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// ImportBindings converts one logical Python import statement into scope import
// evidence. It records what the syntax says and nothing more: the module
// spelling exactly as written (leading dots kept, so a relative import stays
// distinguishable from an absolute one), the imported name when the statement
// names one, and the local name the statement binds. Mapping either onto a
// repository file is the resolver's job, not the parser's.
//
// Both Python adapters route their import syntax through this function so the
// regex and tree-sitter paths cannot disagree about what a statement binds.
func ImportBindings(stmt string) []graph.ScopeImport {
	text := strings.Join(strings.Fields(stmt), " ")
	if rest, ok := strings.CutPrefix(text, "from "); ok {
		module, clause, found := strings.Cut(rest, " import ")
		module = strings.TrimSpace(module)
		if !found || !validModuleSpecifier(module) {
			return nil
		}
		var out []graph.ScopeImport
		for _, item := range strings.Split(strings.Trim(clause, "() "), ",") {
			fields := strings.Fields(item)
			if len(fields) == 0 {
				continue
			}
			if fields[0] == "*" {
				out = append(out, graph.ScopeImport{
					SourceSpecifier: module,
					Kind:            graph.ScopeImportNamed,
					Wildcard:        true,
				})
				continue
			}
			local := fields[0]
			if len(fields) >= 3 && fields[1] == "as" {
				local = fields[2]
			}
			if !validIdentifier(fields[0]) || !validIdentifier(local) {
				continue
			}
			out = append(out, graph.ScopeImport{
				SourceSpecifier: module,
				ImportedName:    fields[0],
				LocalName:       local,
				Kind:            graph.ScopeImportNamed,
			})
		}
		return out
	}
	rest, ok := strings.CutPrefix(text, "import ")
	if !ok {
		return nil
	}
	var out []graph.ScopeImport
	for _, item := range strings.Split(rest, ",") {
		fields := strings.Fields(item)
		if len(fields) == 0 {
			continue
		}
		module := fields[0]
		if !validDottedName(module) {
			continue
		}
		// `import a.b` binds `a`, not `a.b`: the statement imports `a.b` but the
		// name it puts in scope is the top package. `import a.b as x` binds `x`,
		// and `x` is the module `a.b` itself. LocalName is therefore the bound
		// name and ImportedName the module that name refers to -- which is what
		// separates the two forms, including `import a.b as a`.
		local := module
		bound := module
		if i := strings.IndexByte(module, '.'); i >= 0 {
			local = module[:i]
			bound = local
		}
		if len(fields) >= 3 && fields[1] == "as" {
			if !validIdentifier(fields[2]) {
				continue
			}
			local, bound = fields[2], module
		}
		out = append(out, graph.ScopeImport{
			SourceSpecifier: module,
			ImportedName:    bound,
			LocalName:       local,
			Kind:            graph.ScopeImportNamespace,
		})
	}
	return out
}

// validModuleSpecifier accepts the `from` operand: a dotted name, any number of
// leading dots for a relative import, or dots alone (`from . import x`).
func validModuleSpecifier(spec string) bool {
	trimmed := strings.TrimLeft(spec, ".")
	if len(trimmed) == len(spec) && trimmed == "" {
		return false
	}
	return trimmed == "" || validDottedName(trimmed)
}

func validDottedName(name string) bool {
	if name == "" {
		return false
	}
	for _, segment := range strings.Split(name, ".") {
		if !validIdentifier(segment) {
			return false
		}
	}
	return true
}

func validIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// importStatements folds the masked source into logical import statements,
// following parenthesised and backslash continuations. It reports the physical
// line each statement started on -- so the caller can attribute it to the
// lexical scope it was written in -- and which physical lines it consumed.
// Consumed lines are excluded from call extraction: `from x import (a, b)` is
// import syntax, not a call to `import`.
func importStatements(masked []string) (stmts map[int]string, consumed []bool) {
	stmts = map[int]string{}
	consumed = make([]bool, len(masked))
	for i := 0; i < len(masked); i++ {
		line := strings.TrimSpace(strings.TrimSuffix(masked[i], "\r"))
		if !strings.HasPrefix(line, "import ") && !strings.HasPrefix(line, "from ") {
			continue
		}
		start := i
		stmt := line
		consumed[i] = true
		depth := strings.Count(line, "(") - strings.Count(line, ")")
		open := strings.HasSuffix(line, "\\")
		for (depth > 0 || open) && i+1 < len(masked) {
			i++
			consumed[i] = true
			next := strings.TrimSpace(strings.TrimSuffix(masked[i], "\r"))
			stmt = strings.TrimSuffix(stmt, "\\") + " " + next
			depth += strings.Count(next, "(") - strings.Count(next, ")")
			open = strings.HasSuffix(next, "\\")
		}
		stmts[start] = stmt
	}
	return stmts, consumed
}

// LocalBindings reports the names one Python lexical scope binds itself, given
// that scope's source: its `def` header's parameters and every assignment,
// loop target, `with`/`except` alias and nested declaration in its body.
//
// A nested declaration's name counts only inside a function scope, where it is
// a local of the enclosing function. Module-level declarations are symbols this
// graph holds and are not negative evidence.
//
// It is negative evidence, and it is deliberately a superset. Nested scopes are
// skipped by indentation so a function's own locals stay its own, but anything
// the scan is unsure of is reported as bound: over-reporting only costs an
// unresolved edge, while under-reporting would let an import bind a name the
// call site had already given a different meaning.
//
// Import statements are not reported here. An import that rebinds a name is
// already recorded as an import with its own lexical owner, and reporting it as
// a local binding too would make every import shadow itself.
//
// Both Python adapters call this with the same text, so neither can decide a
// shadow the other does not see.
func LocalBindings(src string, isFunctionScope bool) []LocalBinding {
	lines := maskPythonLines(strings.Split(src, "\n"))
	var out []LocalBinding
	seen := map[string]struct{}{}
	add := func(name string) { addBinding(&out, seen, LocalBinding{Name: name}) }

	logical, starts := pythonLogicalLines(lines)
	base := -1
	skipUntil := -1
	for i, line := range logical {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := lineIndent(line)
		if skipUntil >= 0 {
			if indent > skipUntil {
				continue
			}
			skipUntil = -1
		}
		header := strings.TrimPrefix(trimmed, "async ")
		isDecl := strings.HasPrefix(header, "def ") || strings.HasPrefix(header, "class ")
		if strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "from ") {
			// An import that rebinds a name is recorded as an import with its
			// own lexical owner. Reporting it here as well would make every
			// import shadow itself.
			if base < 0 {
				base = indent
			}
			continue
		}
		if base < 0 {
			base = indent
			if isFunctionScope && isDecl && starts[i] == 0 {
				// The scope's own header: its parameters are its bindings, and
				// its body is not a nested scope.
				addPythonParameters(header, add)
				continue
			}
		}
		if isDecl {
			// A nested declaration owns everything indented below it, and its
			// body is not part of this scope.
			skipUntil = indent
			if isFunctionScope {
				// Inside a function, a nested `def`/`class` binds its name in
				// the enclosing function's namespace for the whole function --
				// it shadows an outer import whether it is written before or
				// after the call. At module scope the same name is a symbol
				// this graph already holds, and reporting it here would make a
				// module-level `def` shadow every call to itself.
				addBinding(&out, seen, LocalBinding{Name: declaredName(header), Declaration: true})
			}
			continue
		}
		addPythonAssignedNames(trimmed, add)
	}
	return out
}

// LocalBinding is one name a lexical scope binds itself. Declaration marks the
// names a nested `def` or `class` binds, which shadow an import like any other
// local but also name a symbol this graph holds.
type LocalBinding struct {
	Name        string
	Declaration bool
}

func addBinding(out *[]LocalBinding, seen map[string]struct{}, b LocalBinding) {
	if !validIdentifier(b.Name) || isPythonKeyword(b.Name) {
		return
	}
	if _, ok := seen[b.Name]; ok {
		return
	}
	seen[b.Name] = struct{}{}
	*out = append(*out, b)
}

// declaredName reads the name a `def`/`class` header declares, with any
// `async ` prefix already stripped.
func declaredName(header string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(header, "def"), "class"))
	end := 0
	for end < len(rest) && isIdentifierByte(rest[end]) {
		end++
	}
	return rest[:end]
}

// pythonLogicalLines folds bracket and backslash continuations, reporting the
// physical line each logical line started on.
func pythonLogicalLines(lines []string) (logical []string, starts []int) {
	depth := 0
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		start := i
		stmt := line
		depth = bracketDelta(line)
		for (depth > 0 || strings.HasSuffix(strings.TrimSpace(stmt), "\\")) && i+1 < len(lines) {
			i++
			next := strings.TrimSuffix(lines[i], "\r")
			stmt = strings.TrimSuffix(strings.TrimRight(stmt, " \t"), "\\") + " " + strings.TrimSpace(next)
			depth += bracketDelta(next)
		}
		logical = append(logical, stmt)
		starts = append(starts, start)
	}
	return logical, starts
}

func bracketDelta(line string) int {
	delta := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '(', '[', '{':
			delta++
		case ')', ']', '}':
			delta--
		}
	}
	return delta
}

// addPythonParameters extracts the parameter names from a `def` header. Default
// values and annotations are skipped by taking only the text before the first
// `=` or `:` of each comma-separated item at the top bracket level.
func addPythonParameters(header string, add func(string)) {
	open := strings.IndexByte(header, '(')
	if open < 0 {
		return
	}
	close := strings.LastIndexByte(header, ')')
	if close <= open {
		return
	}
	for _, item := range splitTopLevel(header[open+1:close], ',') {
		item = strings.TrimSpace(item)
		item = strings.TrimLeft(item, "*")
		if cut := strings.IndexAny(item, "=:"); cut >= 0 {
			item = item[:cut]
		}
		add(strings.TrimSpace(item))
	}
}

// addPythonAssignedNames reports the simple names a statement binds. Attribute
// and subscript targets (`self.x = 1`, `items[0] = 1`) bind nothing in the
// scope, so they are skipped rather than reported as their leading name.
func addPythonAssignedNames(stmt string, add func(string)) {
	addTargets := func(text string) {
		for _, target := range splitTopLevel(text, ',') {
			target = strings.TrimSpace(strings.Trim(strings.TrimSpace(target), "()[]"))
			target = strings.TrimLeft(target, "*")
			if cut := strings.IndexByte(target, ':'); cut >= 0 {
				target = target[:cut]
			}
			add(strings.TrimSpace(target))
		}
	}
	// `global x` / `nonlocal x` bind nothing here: they say the name belongs to
	// an outer scope, which is already recorded under that scope's own owner.
	for _, keyword := range []string{"global ", "nonlocal "} {
		if strings.HasPrefix(stmt, keyword) {
			return
		}
	}
	// A compound header carrying its body on one line (`if flag: h = 1`) binds
	// what the body binds. Without this the scan would read `if flag` as the
	// assignment target, drop it, and report no binding at all.
	if rest, ok := pythonInlineBody(stmt); ok {
		addPythonAssignedNames(rest, add)
	}
	// `for a, b in ...` and the same shape inside a comprehension.
	for _, segment := range splitKeyword(stmt, "for ") {
		if in := strings.Index(segment, " in "); in > 0 {
			addTargets(segment[:in])
		}
	}
	// `with x as a, y as b:` and `except E as err:`.
	for _, segment := range splitKeyword(stmt, " as ") {
		name := strings.TrimSpace(segment)
		if cut := strings.IndexAny(name, " ,:)"); cut >= 0 {
			name = name[:cut]
		}
		add(name)
	}
	// `name := value` anywhere in the statement.
	for i := 0; i+1 < len(stmt); i++ {
		if stmt[i] == ':' && stmt[i+1] == '=' {
			addTargets(lastIdentifierBefore(stmt[:i]))
		}
	}
	// `a = b = value`, `a += value`, `a: T = value`. Everything before the
	// first top-level `=` that is not part of a comparison operator.
	cut := topLevelAssignment(stmt)
	if cut < 0 {
		return
	}
	head := strings.TrimSpace(stmt[:cut])
	head = strings.TrimRight(head, "+-*/%&|^<>@")
	for _, part := range strings.Split(head, "=") {
		addTargets(part)
	}
}

// topLevelAssignment returns the index of the statement's assignment `=`, or -1
// when the statement assigns nothing. Comparison and inequality operators are
// not assignments, and an `=` inside brackets is a keyword argument.
func topLevelAssignment(stmt string) int {
	depth := 0
	for i := 0; i < len(stmt); i++ {
		switch stmt[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '=':
			if depth != 0 {
				continue
			}
			if i+1 < len(stmt) && stmt[i+1] == '=' {
				return -1
			}
			if i > 0 && strings.IndexByte("=!<>", stmt[i-1]) >= 0 {
				return -1
			}
			return i
		}
	}
	return -1
}

// splitTopLevel splits on a separator that is not inside brackets.
func splitTopLevel(text string, sep byte) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, text[start:i])
				start = i + 1
			}
		}
	}
	return append(out, text[start:])
}

// splitKeyword returns the text following each occurrence of a keyword.
func splitKeyword(stmt, keyword string) []string {
	var out []string
	for i := 0; i+len(keyword) <= len(stmt); i++ {
		if !strings.HasPrefix(stmt[i:], keyword) {
			continue
		}
		if keyword[0] != ' ' && i > 0 && isIdentifierByte(stmt[i-1]) {
			continue
		}
		out = append(out, stmt[i+len(keyword):])
	}
	return out
}

func lastIdentifierBefore(text string) string {
	end := len(text)
	for end > 0 && text[end-1] == ' ' {
		end--
	}
	start := end
	for start > 0 && isIdentifierByte(text[start-1]) {
		start--
	}
	return text[start:end]
}

// pythonInlineBody returns the statement a compound header carries on its own
// line, if any.
func pythonInlineBody(stmt string) (string, bool) {
	head, rest, found := strings.Cut(stmt, ":")
	if !found {
		return "", false
	}
	if bracketDelta(head) != 0 || strings.TrimSpace(rest) == "" {
		return "", false
	}
	word, _, _ := strings.Cut(strings.TrimSpace(head), " ")
	switch strings.TrimSuffix(word, ":") {
	case "if", "elif", "else", "for", "while", "with", "try", "except", "finally", "match", "case":
		return strings.TrimSpace(rest), true
	}
	return "", false
}

func isIdentifierByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
