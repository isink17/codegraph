package store

import (
	"sort"
	"strings"
)

// resolverScope narrows a repo-wide resolve to the edges of some source
// languages. The nil scope is the whole repository.
//
// Narrowing the clear alone is not enough: every strategy binds any unresolved
// edge in the repository, so a pass for one language would otherwise decide
// another's -- including edges an older resolver left unresolved, or a newer
// binary decided. A scope therefore gates every write.
type resolverScope struct {
	languages map[string]struct{}
	sqlList   string
}

func newResolverScope(languages []string) *resolverScope {
	if len(languages) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(languages))
	quoted := make([]string, 0, len(languages))
	for _, language := range languages {
		if _, dup := set[language]; dup {
			continue
		}
		set[language] = struct{}{}
		quoted = append(quoted, "'"+strings.ReplaceAll(language, "'", "''")+"'")
	}
	sort.Strings(quoted)
	return &resolverScope{languages: set, sqlList: strings.Join(quoted, ",")}
}

// has reports whether edges of the language are in scope.
func (r *resolverScope) has(language string) bool {
	if r == nil {
		return true
	}
	_, ok := r.languages[language]
	return ok
}

// noEdges is an edge-id filter that matches nothing: edge ids start at 1.
var noEdges = map[int64]struct{}{0: {}}

// only is the targeted-pass filter for a language's own pass. An in-scope
// language runs unfiltered (nil); an out-of-scope one runs over no edges, so it
// still creates the veto tables later gates read but binds nothing.
func (r *resolverScope) only(language string) map[int64]struct{} {
	if r.has(language) {
		return nil
	}
	return noEdges
}

// writeGate is the conjunct every repo-wide strategy UPDATE appends. It needs
// the calling edge's source file joined as `f`.
func (r *resolverScope) writeGate() string {
	if r == nil {
		return ""
	}
	return ` AND f.language IN (` + r.sqlList + `)`
}
