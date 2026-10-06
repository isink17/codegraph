package python

import (
	"regexp"
	"regexp/syntax"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/isink17/codegraph/internal/graph"
)

var (
	globalNameWriteRE = regexp.MustCompile(`^\s*globals\s*\(\s*\)\s*\[\s*(?:"([^"\\]*)"|'([^'\\]*)')\s*\]\s*=`)
	setattrWriteRE    = regexp.MustCompile(`^\s*setattr\s*\(\s*([^\s(),]+)\s*,\s*(?:"([^"\\]*)"|'([^'\\]*)')\s*,`)
	memberWriteRE     = regexp.MustCompile(`^\s*([^\s.=]+)\.([^\s.=]+)\s*=([^=]|$)`)
)

// identStart and identContinue are the rune classes of a Python name (PEP 3131):
// an XID_Start rune or `_`, then XID_Continue runes. RE2 has no XID classes, so
// these are the general categories XID is built from plus its Other_ID_Start /
// Other_ID_Continue runes. That also admits 22 runes XID excludes (U+037A,
// U+2E2F, U+FC5E..U+FC63, ...); Python rejects every one of them in the position
// these classes would accept it, so on source Python compiles they read the same
// names. Runes assigned after Go's unicode.Version are not names here.
//
// This is the one definition of a Python name in the package: both adapters'
// declarations, calls, imports and local bindings are read with it.
const (
	identStart    = `_\p{L}\p{Nl}\x{1885}\x{1886}\x{2118}\x{212E}`
	identContinue = identStart + `\p{Mn}\p{Mc}\p{Nd}\p{Pc}` +
		`\x{00B7}\x{0387}\x{1369}-\x{1371}\x{19DA}\x{200C}\x{200D}\x{30FB}\x{FF65}`
	identifier = `[` + identStart + `][` + identContinue + `]*`
)

// startRanges and continueRanges are the same two classes as sorted [lo, hi]
// rune pairs, read from the regexp parser so a single rune is tested without
// running a regexp and without a second spelling of the class.
var (
	startRanges    = classRanges(identStart)
	continueRanges = classRanges(identContinue)
)

func classRanges(class string) []rune {
	re, err := syntax.Parse("["+class+"]", syntax.Perl)
	if err != nil || re.Op != syntax.OpCharClass {
		panic("python identifier class does not parse: " + class)
	}
	return re.Rune
}

func inRanges(r rune, ranges []rune) bool {
	i := sort.Search(len(ranges)/2, func(i int) bool { return ranges[2*i+1] >= r })
	return i < len(ranges)/2 && ranges[2*i] <= r
}

func isIdentifierRune(r rune) bool { return inRanges(r, continueRanges) }

// NormalizeIdentifier is the name CPython binds for an identifier spelled s:
// its NFKC form (PEP 3131), so `ｆｕｌｌ`, `ﬁnd` and `𝐟` are `full`, `find` and
// `f`. Validity is decided on the spelling, as CPython does, before this runs.
// It is for identifier tokens only (definitions, references, attribute and
// import names): string, comment and file-name text keep their spelling.
func NormalizeIdentifier(s string) string { return norm.NFKC.String(s) }

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
					SourceSpecifier: NormalizeIdentifier(module),
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
				SourceSpecifier: NormalizeIdentifier(module),
				ImportedName:    NormalizeIdentifier(fields[0]),
				LocalName:       NormalizeIdentifier(local),
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
			SourceSpecifier: NormalizeIdentifier(module),
			ImportedName:    NormalizeIdentifier(bound),
			LocalName:       NormalizeIdentifier(local),
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
	for i, r := range name {
		if !isIdentifierRune(r) || i == 0 && !inRanges(r, startRanges) {
			return false
		}
	}
	return name != ""
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
// loop target, `with`/`except` alias, lambda parameter, `case` capture and
// nested declaration in its body.
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
	return scopeBindings(src, isFunctionScope, false)
}

// ClassBodyBindings reports the names a class body binds, given the class's
// source: every assignment, loop target, alias, lambda parameter, `case`
// capture, import and nested declaration written directly in the body. Code
// that runs directly in the class body reads them before any enclosing scope;
// the methods and nested classes it declares do not, because a function body
// skips its class's scope. The header's bases and keywords are evaluated in the
// enclosing scope and bind nothing here.
func ClassBodyBindings(src string) []LocalBinding {
	return scopeBindings(src, true, true)
}

func scopeBindings(src string, isFunctionScope, classBody bool) []LocalBinding {
	lines := maskPythonLines(strings.Split(src, "\n"))
	rawLines := strings.Split(src, "\n")
	var out []LocalBinding
	seen := map[string]struct{}{}
	moduleSetattrBound := false
	escaped := map[string]bool{}
	var escapedOrder []string
	var noteEscapes func(stmt string)
	noteEscapes = func(line string) {
		for _, stmt := range splitTopLevel(line, ';') {
			stmt = strings.TrimSpace(stmt)
			for _, keyword := range []string{"global ", "nonlocal "} {
				if rest, ok := strings.CutPrefix(stmt, keyword); ok {
					for _, name := range splitTopLevel(rest, ',') {
						if name = NormalizeIdentifier(strings.TrimSpace(name)); validIdentifier(name) && !escaped[name] {
							escaped[name] = true
							escapedOrder = append(escapedOrder, name)
						}
					}
				}
			}
			if rest, ok := pythonInlineBody(stmt); ok {
				noteEscapes(rest)
			}
		}
	}
	decorated := false
	add := func(name string) {
		addBinding(&out, seen, LocalBinding{Name: name})
		if !isFunctionScope && !classBody && name == "setattr" {
			moduleSetattrBound = true
		}
	}

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
		if strings.HasPrefix(trimmed, "@") {
			decorated = true
			continue
		}
		wasDecorated := decorated
		decorated = false
		header := strings.TrimPrefix(trimmed, "async ")
		isDecl := strings.HasPrefix(header, "def ") || strings.HasPrefix(header, "class ")
		if strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "from ") {
			// An import that rebinds a name is recorded as an import with its
			// own lexical owner. Reporting it here as well would make every
			// import shadow itself. A class body's imports are recorded nowhere
			// else, so there they are bindings like any other.
			if base < 0 {
				base = indent
			}
			if classBody {
				for _, b := range ImportBindings(trimmed) {
					add(b.LocalName)
				}
			}
			if !isFunctionScope && !classBody {
				for _, binding := range ImportBindings(trimmed) {
					if binding.LocalName == "setattr" {
						moduleSetattrBound = true
					}
				}
			}
			continue
		}
		if base < 0 {
			base = indent
			if isFunctionScope && isDecl && starts[i] == 0 {
				// The scope's own header: its parameters are its bindings, and
				// its body is not a nested scope. A lambda in a default value
				// is no owner either, so its parameters are reported as this
				// function's, as they are for a lambda in the body. A class
				// header's bases and keywords bind nothing in the body, but a
				// lambda among them has no owner of its own either. A body
				// written on the header line binds like any other.
				colon := headerColon(header)
				if colon < 0 {
					colon = len(header)
				}
				if !classBody {
					addPythonParameters(header[:colon], add)
				}
				addLambdaParameters(header[:colon], add)
				if colon == len(header) {
					continue
				}
				noteEscapes(header[colon+1:])
				for _, stmt := range splitTopLevel(header[colon+1:], ';') {
					stmt = strings.TrimSpace(stmt)
					if strings.HasPrefix(stmt, "import ") || strings.HasPrefix(stmt, "from ") {
						// A function's imports are recorded as imports; only
						// a class body's are recorded nowhere else.
						if classBody {
							for _, b := range ImportBindings(stmt) {
								add(b.LocalName)
							}
						}
						continue
					}
					addPythonAssignedNames(stmt, add)
				}
				continue
			}
		}
		if isDecl {
			if !isFunctionScope && !classBody && (strings.HasPrefix(header, "def ") || strings.HasPrefix(header, "class ")) && declaredName(header) == "setattr" {
				moduleSetattrBound = true
			}
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
				// A decorator rebinds the name to whatever it returns, so a
				// decorated class, or a decorated member of a class body
				// (`@property`), is not provably what it declares.
				addBinding(&out, seen, LocalBinding{Name: declaredName(header), Declaration: !(wasDecorated && (classBody || strings.HasPrefix(header, "class ")))})
			} else if wasDecorated && strings.HasPrefix(header, "def ") {
				// A module decorator binds its result, not necessarily this def.
				add(declaredName(header))
			}
			continue
		}
		if !isFunctionScope && !classBody && starts[i] < len(rawLines) {
			stmt := rawLines[starts[i]]
			if name := moduleMutationBinding(stmt, !moduleSetattrBound); name != "" {
				addMutationBinding(&out, seen, name)
			}
		}
		noteEscapes(trimmed)
		addPythonAssignedNames(trimmed, add)
	}
	// A `global`/`nonlocal` name is rebindable from here by any statement form
	// (assignment, import, def, class, del), so every call to it in the file is
	// withheld: the owning scope is not known, the module sees it.
	if !classBody {
		for _, name := range escapedOrder {
			if i := slices.IndexFunc(out, func(b LocalBinding) bool { return b.Name == name }); i >= 0 {
				out[i].Declaration, out[i].Escapes = false, true
				continue
			}
			out = append(out, LocalBinding{Name: name, Escapes: true})
		}
	}
	return out
}

// moduleMutationBinding returns statically named module/member writes. Dotted
// receivers are only acted on after the store matches them to visible imports.
func moduleMutationBinding(stmt string, builtinSetattr bool) string {
	if match := globalNameWriteRE.FindStringSubmatch(stmt); len(match) == 3 {
		name := match[1]
		if name == "" {
			name = match[2]
		}
		if validIdentifier(name) {
			return NormalizeIdentifier(name)
		}
	}
	var receiver, member string
	if match := setattrWriteRE.FindStringSubmatch(stmt); len(match) == 4 && builtinSetattr {
		receiver, member = match[1], match[2]
		if member == "" {
			member = match[3]
		}
	} else if match := memberWriteRE.FindStringSubmatch(stmt); len(match) == 4 {
		receiver, member = match[1], match[2]
	}
	if validIdentifier(receiver) && validIdentifier(member) {
		return NormalizeIdentifier(receiver) + "." + NormalizeIdentifier(member)
	}
	return ""
}

func addMutationBinding(out *[]LocalBinding, seen map[string]struct{}, name string) {
	parts := strings.Split(name, ".")
	for i, part := range parts {
		if !validIdentifier(part) {
			return
		}
		parts[i] = NormalizeIdentifier(part)
	}
	name = strings.Join(parts, ".")
	if _, ok := seen[name]; ok {
		return
	}
	seen[name] = struct{}{}
	*out = append(*out, LocalBinding{Name: name})
}

// DefHeaderEnd returns how many lines after its first the `def` header that
// starts src runs on: the index of the header's last physical line.
func DefHeaderEnd(src string) int {
	_, end := defHeader(src)
	return end
}

// DefHeaderNames reports which of names the `def` header starting src spells
// after its name -- in a parameter default or an annotation -- outside strings
// and comments. A spelling matches by its NFKC form, the name CPython binds.
func DefHeaderNames(src string, names []LocalBinding) []LocalBinding {
	header, _ := defHeader(src)
	open := strings.IndexByte(header, '(')
	if open < 0 {
		return nil
	}
	spelled := map[string]struct{}{}
	for _, word := range strings.FieldsFunc(header[open:], func(r rune) bool { return !isIdentifierRune(r) }) {
		spelled[NormalizeIdentifier(word)] = struct{}{}
	}
	var out []LocalBinding
	for _, b := range names {
		if _, ok := spelled[b.Name]; ok {
			out = append(out, b)
		}
	}
	return out
}

func defHeader(src string) (string, int) {
	return pythonLogicalLine(maskPythonLines(strings.Split(src, "\n")), 0)
}

// LocalBinding is one name a lexical scope binds itself. Declaration marks the
// names a nested `def` or `class` binds, which shadow an import like any other
// local but also name a symbol this graph holds.
type LocalBinding struct {
	Name        string
	Declaration bool
	// Escapes marks a `global`/`nonlocal` name: the scope that owns it is not
	// known here, so it is recorded at module level where every call sees it.
	Escapes bool
}

func addBinding(out *[]LocalBinding, seen map[string]struct{}, b LocalBinding) {
	if !validIdentifier(b.Name) {
		return
	}
	b.Name = NormalizeIdentifier(b.Name)
	if isPythonKeyword(b.Name) {
		return
	}
	if _, ok := seen[b.Name]; ok {
		if !b.Declaration {
			// A plain rebinding of a declared name (`C = make()` after
			// `class C`) makes the name something other than the declaration.
			for i := range *out {
				if (*out)[i].Name == b.Name {
					(*out)[i].Declaration = false
				}
			}
		}
		return
	}
	seen[b.Name] = struct{}{}
	*out = append(*out, b)
}

// declaredName reads the name a `def`/`class` header declares, with any
// `async ` prefix already stripped.
func declaredName(header string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(header, "def"), "class"))
	name := rest[:len(rest)-len(strings.TrimLeftFunc(rest, isIdentifierRune))]
	if len(name) < len(rest) && rest[len(name)] >= utf8.RuneSelf {
		// The name goes on with a rune Go's tables do not know yet: the
		// prefix read so far is not the name.
		return ""
	}
	return name
}

// pythonLogicalLines folds bracket and backslash continuations, reporting the
// physical line each logical line started on.
func pythonLogicalLines(lines []string) (logical []string, starts []int) {
	for i := 0; i < len(lines); i++ {
		stmt, end := pythonLogicalLine(lines, i)
		logical = append(logical, stmt)
		starts = append(starts, i)
		i = end
	}
	return logical, starts
}

// pythonLogicalLine folds the bracket and backslash continuations of the
// statement starting at lines[i], returning it and the index of its last line.
func pythonLogicalLine(lines []string, i int) (string, int) {
	stmt := strings.TrimSuffix(lines[i], "\r")
	depth := bracketDelta(stmt)
	for (depth > 0 || strings.HasSuffix(strings.TrimSpace(stmt), "\\")) && i+1 < len(lines) {
		i++
		next := strings.TrimSuffix(lines[i], "\r")
		stmt = strings.TrimSuffix(strings.TrimRight(stmt, " \t"), "\\") + " " + strings.TrimSpace(next)
		depth += bracketDelta(next)
	}
	return stmt, i
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
	// `del x` makes x local to the scope for its whole body.
	if rest, ok := strings.CutPrefix(stmt, "del "); ok {
		for _, target := range splitTopLevel(rest, ',') {
			target = strings.TrimSpace(strings.Trim(strings.TrimSpace(target), "()"))
			if !strings.ContainsAny(target, ".[") {
				add(target)
			}
		}
		return
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
	addLambdaParameters(stmt, add)
	// `case <pattern> [if guard]:` binds every capture name in the pattern, in
	// the enclosing function for the whole function.
	// The soft keyword may be followed directly by a bracket or a tab
	// (`case(x):`, `case[x]:`), as the call-side pattern scan accepts.
	if rest, ok := strings.CutPrefix(stmt, "case"); ok && rest != "" && strings.IndexByte(" \t([{-", rest[0]) >= 0 {
		if colon := topLevelIndex(rest, ':'); colon >= 0 {
			pattern, _, _ := strings.Cut(rest[:colon], " if ")
			addPatternCaptures(pattern, add)
		}
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

// addLambdaParameters reports the parameters of every lambda in a statement:
// `lambda a, *b, c=1, **d: ...`. A lambda is not a scope this graph owns, so,
// like a comprehension target, its parameters are reported as the enclosing
// scope's: that refuses more calls than CPython would, never fewer.
func addLambdaParameters(stmt string, add func(string)) {
	for _, segment := range splitKeyword(stmt, "lambda") {
		if r, _ := utf8.DecodeRuneInString(segment); isIdentifierRune(r) {
			continue
		}
		if colon := lambdaColon(segment); colon >= 0 {
			for _, param := range splitTopLevel(segment[:colon], ',') {
				param = strings.TrimLeft(strings.TrimSpace(param), "*")
				param, _, _ = strings.Cut(param, "=")
				add(strings.TrimSpace(param))
			}
		}
	}
}

// addPatternCaptures reports the capture names of a match-case pattern: every
// name that is not a dotted value (`Color.RED`), a class name (`Point(...)`), a
// keyword-pattern attribute (`x=`), a mapping key, the wildcard `_` or a
// literal. A string's prefix (`b"k"`) may read as a capture; that only refuses.
func addPatternCaptures(pattern string, add func(string)) {
	for i := 0; i < len(pattern); {
		r, size := utf8.DecodeRuneInString(pattern[i:])
		if !isIdentifierRune(r) {
			i += size
			continue
		}
		start := i
		for i < len(pattern) {
			r, size := utf8.DecodeRuneInString(pattern[i:])
			if !isIdentifierRune(r) {
				break
			}
			i += size
		}
		name := pattern[start:i]
		before := strings.TrimRight(pattern[:start], " ")
		after := strings.TrimLeft(pattern[i:], " ")
		if strings.HasSuffix(before, ".") || after != "" && strings.IndexByte(".(=:", after[0]) >= 0 {
			continue
		}
		switch name {
		case "_", "None", "True", "False":
			continue
		}
		add(name) // a number or a non-name is refused by addBinding
	}
}

// lambdaColon returns the index of the colon that ends the parameters of the
// lambda whose text follows the keyword, skipping the colon of every lambda
// written in a default value, or -1 when there is none.
func lambdaColon(text string) int {
	depth, nested := 0, 0
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth--; depth < 0 {
				return -1
			}
		case depth != 0:
		case c == ':':
			if nested == 0 {
				return i
			}
			nested--
		case strings.HasPrefix(text[i:], "lambda") && isKeywordAt(text, i, len("lambda")):
			nested++
			i += len("lambda") - 1
		}
	}
	return -1
}

// isKeywordAt reports whether text[i:i+n] is a whole word.
func isKeywordAt(text string, i, n int) bool {
	before, _ := utf8.DecodeLastRuneInString(text[:i])
	after, _ := utf8.DecodeRuneInString(text[i+n:])
	return (i == 0 || !isIdentifierRune(before)) && (i+n == len(text) || !isIdentifierRune(after))
}

// topLevelIndex returns the index of the first sep outside brackets, or -1 when
// the text closes a bracket it did not open first.
func topLevelIndex(text string, sep byte) int {
	depth := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth--; depth < 0 {
				return -1
			}
		case sep:
			if depth == 0 {
				return i
			}
		}
	}
	return -1
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
		if r, _ := utf8.DecodeLastRuneInString(stmt[:i]); keyword[0] != ' ' && i > 0 && isIdentifierRune(r) {
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
	start := len(strings.TrimRightFunc(text[:end], isIdentifierRune))
	if start > 0 && text[start-1] >= utf8.RuneSelf {
		// A rune Go's tables do not know yet starts the name.
		return ""
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

// Owner is the lexical scope this binding is recorded under: the scope that
// wrote it, or the module for a `global`/`nonlocal` name.
func (b LocalBinding) Owner(written string) string {
	if b.Escapes {
		return ""
	}
	return written
}

// AttrAssignTargets reports the `name.attr` targets src assigns to, in the
// forms `a.x = v`, `a.x: T = v`, `a.x += v` and chained `a.x = b.y = v`. A
// target inside a tuple, loop or `with` is not read. The receiver is whatever
// name the text spells: the caller decides which receivers it cares about.
func AttrAssignTargets(src string) []string {
	var out []string
	logical, _ := pythonLogicalLines(maskPythonLines(strings.Split(src, "\n")))
	var scan func(stmt string)
	scan = func(line string) {
		for _, stmt := range splitTopLevel(line, ';') {
			stmt = strings.TrimSpace(stmt)
			if rest, ok := pythonInlineBody(stmt); ok {
				scan(rest)
				continue
			}
			depth, last := 0, 0
			var segments []string
			for i := 0; i < len(stmt); i++ {
				switch stmt[i] {
				case '(', '[', '{':
					depth++
				case ')', ']', '}':
					depth--
				case '=':
					if depth != 0 || i+1 < len(stmt) && stmt[i+1] == '=' || i > 0 && strings.IndexByte("=!<>:", stmt[i-1]) >= 0 {
						continue
					}
					seg := stmt[last:i]
					if n := len(seg); n > 0 && strings.IndexByte("+-*/%&|^@", seg[n-1]) >= 0 {
						seg = strings.TrimRight(seg, "+-*/%&|^@<>")
					}
					segments = append(segments, seg)
					last = i + 1
				}
			}
			for _, seg := range segments {
				if cut := topLevelIndex(seg, ':'); cut >= 0 {
					seg = seg[:cut]
				}
				recv, attr, ok := strings.Cut(strings.TrimSpace(seg), ".")
				recv, attr = strings.TrimSpace(recv), strings.TrimSpace(attr)
				if ok && validIdentifier(recv) && validIdentifier(attr) {
					out = append(out, NormalizeIdentifier(recv)+"."+NormalizeIdentifier(attr))
				}
			}
		}
	}
	for _, line := range logical {
		scan(strings.TrimSpace(line))
	}
	return out
}
