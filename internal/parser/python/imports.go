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
		// `import a.b` binds `a`, but the only spelling that can name anything
		// inside `a.b` is the full dotted path, so that is what is recorded as
		// the local binding.
		local := module
		if len(fields) >= 3 && fields[1] == "as" {
			local = fields[2]
			if !validIdentifier(local) {
				continue
			}
		}
		if !validDottedName(module) {
			continue
		}
		out = append(out, graph.ScopeImport{
			SourceSpecifier: module,
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
// following parenthesised and backslash continuations, and reports which
// physical lines each statement consumed. Consumed lines are excluded from call
// extraction: `from x import (a, b)` is import syntax, not a call to `import`.
func importStatements(masked []string) (stmts []string, consumed []bool) {
	consumed = make([]bool, len(masked))
	for i := 0; i < len(masked); i++ {
		line := strings.TrimSpace(strings.TrimSuffix(masked[i], "\r"))
		if !strings.HasPrefix(line, "import ") && !strings.HasPrefix(line, "from ") {
			continue
		}
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
		stmts = append(stmts, stmt)
	}
	return stmts, consumed
}
