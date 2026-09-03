//go:build cgo

package treesitter

import (
	"regexp"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

var quotedSource = regexp.MustCompile(`["']([^"']+)["']`)

func packageEvidence(content []byte, language string) string {
	text := string(content)
	var re *regexp.Regexp
	switch language {
	case "java":
		re = regexp.MustCompile(`(?m)\bpackage\s+([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)\s*;`)
	case "kotlin":
		re = regexp.MustCompile(`(?m)^\s*package\s+([A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)*)`)
	}
	if re == nil {
		return ""
	}
	m := re.FindStringSubmatch(text)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func addJavaScope(text string, out *[]graph.ScopeImport) {
	text = strings.TrimSpace(strings.TrimSuffix(text, ";"))
	text = strings.TrimSpace(strings.TrimPrefix(text, "import"))
	static := strings.HasPrefix(text, "static ")
	text = strings.TrimSpace(strings.TrimPrefix(text, "static"))
	wildcard := strings.HasSuffix(text, ".*")
	name := strings.TrimSuffix(text, ".*")
	local := name
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		local = name[i+1:]
	}
	if wildcard {
		local = ""
	}
	*out = append(*out, graph.ScopeImport{SourceSpecifier: name, ImportedName: local, LocalName: local, Kind: graph.ScopeImportNamed, Wildcard: wildcard, Static: static})
}

func addKotlinScope(text string, out *[]graph.ScopeImport) {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "import"))
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return
	}
	path := parts[0]
	wildcard := strings.HasSuffix(path, ".*")
	name := strings.TrimSuffix(path, ".*")
	local := name
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		local = name[i+1:]
	}
	if len(parts) >= 3 && parts[1] == "as" {
		local = parts[2]
	}
	if wildcard {
		local = ""
	}
	*out = append(*out, graph.ScopeImport{SourceSpecifier: name, ImportedName: local, LocalName: local, Kind: graph.ScopeImportNamed, Wildcard: wildcard})
}

func addRustScope(text string, public bool, out *[]graph.ScopeImport) {
	text = strings.TrimSpace(strings.TrimSuffix(text, ";"))
	text = strings.TrimSpace(strings.TrimPrefix(text, "use"))
	if strings.HasPrefix(text, "pub ") {
		public = true
		text = strings.TrimSpace(strings.TrimPrefix(text, "pub"))
	}
	if i := strings.Index(text, "::{"); i >= 0 && strings.HasSuffix(text, "}") {
		prefix := text[:i]
		for _, item := range strings.Split(text[i+3:len(text)-1], ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			addRustScope(prefix+"::"+item, public, out)
		}
		return
	}
	wildcard := strings.HasSuffix(text, "::*")
	path := strings.TrimSuffix(text, "::*")
	alias := ""
	if i := strings.Index(path, " as "); i >= 0 {
		alias, path = strings.TrimSpace(path[i+4:]), strings.TrimSpace(path[:i])
	}
	local := path
	if i := strings.LastIndex(path, "::"); i >= 0 {
		local = path[i+2:]
	}
	if alias != "" {
		local = alias
	}
	if wildcard {
		local = ""
	}
	*out = append(*out, graph.ScopeImport{SourceSpecifier: path, ImportedName: local, LocalName: local, Kind: graph.ScopeImportUse, Wildcard: wildcard, ReExport: public})
}

func addTypeScriptImport(text string, out *[]graph.ScopeImport) {
	sourceMatch := quotedSource.FindStringSubmatch(text)
	if len(sourceMatch) != 2 {
		return
	}
	source := sourceMatch[1]
	head := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "import"))
	typeOnly := strings.HasPrefix(head, "type ")
	head = strings.TrimSpace(strings.TrimPrefix(head, "type"))
	from := strings.Index(head, "from")
	if from < 0 {
		*out = append(*out, graph.ScopeImport{SourceSpecifier: source, Kind: graph.ScopeImportSideEffect, TypeOnly: typeOnly})
		return
	}
	clause := strings.TrimSpace(strings.TrimSuffix(head[:from], "from"))
	if strings.HasPrefix(clause, "{") {
		for _, item := range strings.Split(strings.Trim(clause, "{} "), ",") {
			p := strings.Fields(strings.TrimSpace(item))
			if len(p) == 0 {
				continue
			}
			imported := p[0]
			local := imported
			if len(p) >= 3 && p[1] == "as" {
				local = p[2]
			}
			*out = append(*out, graph.ScopeImport{SourceSpecifier: source, ImportedName: imported, LocalName: local, Kind: graph.ScopeImportNamed, TypeOnly: typeOnly})
		}
	} else if strings.HasPrefix(clause, "*") {
		p := strings.Fields(clause)
		if len(p) >= 3 {
			*out = append(*out, graph.ScopeImport{SourceSpecifier: source, LocalName: p[2], Kind: graph.ScopeImportNamespace, TypeOnly: typeOnly})
		}
	} else if clause != "" {
		name := strings.TrimSpace(strings.Split(clause, ",")[0])
		*out = append(*out, graph.ScopeImport{SourceSpecifier: source, ImportedName: "default", LocalName: name, Kind: graph.ScopeImportDefault, TypeOnly: typeOnly})
	} else {
		*out = append(*out, graph.ScopeImport{SourceSpecifier: source, Kind: graph.ScopeImportSideEffect, TypeOnly: typeOnly})
	}
}

func addTypeScriptReExport(text string, out *[]graph.ScopeImport) {
	sm := quotedSource.FindStringSubmatch(text)
	if len(sm) != 2 {
		return
	}
	source := sm[1]
	if strings.Contains(text, "export * as ") {
		p := strings.Fields(text)
		if len(p) >= 4 {
			*out = append(*out, graph.ScopeImport{SourceSpecifier: source, LocalName: p[3], Kind: graph.ScopeImportNamespace, ReExport: true, Wildcard: true, NamespaceExport: true})
		}
		return
	}
	if strings.Contains(text, "export *") {
		*out = append(*out, graph.ScopeImport{SourceSpecifier: source, Kind: graph.ScopeImportNamed, ReExport: true, Wildcard: true})
		return
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return
	}
	for _, item := range strings.Split(text[start+1:end], ",") {
		p := strings.Fields(strings.TrimSpace(item))
		if len(p) == 0 {
			continue
		}
		imported := p[0]
		exported := imported
		if len(p) >= 3 && p[1] == "as" {
			exported = p[2]
		}
		*out = append(*out, graph.ScopeImport{SourceSpecifier: source, ImportedName: imported, LocalName: exported, Kind: graph.ScopeImportNamed, ReExport: true})
	}
}
