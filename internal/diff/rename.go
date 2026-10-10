package diff

import "sort"

// FileRenames annotates file_removed/file_added pairs whose bytes are
// identical. It is derived from the full change set (not the page) and never
// replaces those changes: declarations in a renamed file still appear as
// removed and added, and risk still cites their dependents, because a path
// change can change package, module or import semantics.
type FileRenames struct {
	// Exact pairs one removed path with one added path: the content hash and
	// language match, no other removed or added file carries that hash, and no
	// file present in both revisions carries it (that would be a copy source).
	Exact []FileRename `json:"exact"`
	// Ambiguous lists hash groups that could be renames but cannot be paired
	// uniquely; their files stay plain additions and removals.
	Ambiguous []AmbiguousRename `json:"ambiguous"`
	// Limitation is set when pairing was not attempted.
	Limitation string `json:"limitation,omitempty"`
}

type FileRename struct {
	From        string `json:"from"`
	To          string `json:"to"`
	ContentHash string `json:"content_hash"`
	Language    string `json:"language,omitempty"`
}

type AmbiguousRename struct {
	ContentHash string   `json:"content_hash"`
	Removed     []string `json:"removed"`
	Added       []string `json:"added"`
	Reason      string   `json:"reason"`
}

func fileRenames(base, head []SourceFile, policiesMatch bool) FileRenames {
	r := FileRenames{Exact: []FileRename{}, Ambiguous: []AmbiguousRename{}}
	if !policiesMatch {
		r.Limitation = "renames not paired because effective indexing policies differ"
		return r
	}
	bm, hm := map[string]SourceFile{}, map[string]SourceFile{}
	for _, f := range base {
		bm[f.Path] = f
	}
	for _, f := range head {
		hm[f.Path] = f
	}
	type group struct {
		removed, added []SourceFile
		retained       bool
	}
	groups := map[string]*group{}
	get := func(h string) *group {
		if groups[h] == nil {
			groups[h] = &group{}
		}
		return groups[h]
	}
	for p, f := range bm {
		if f.ContentHash == "" {
			continue
		}
		if _, ok := hm[p]; ok {
			get(f.ContentHash).retained = true
		} else {
			get(f.ContentHash).removed = append(get(f.ContentHash).removed, f)
		}
	}
	for p, f := range hm {
		if f.ContentHash == "" {
			continue
		}
		if _, ok := bm[p]; ok {
			get(f.ContentHash).retained = true
		} else {
			get(f.ContentHash).added = append(get(f.ContentHash).added, f)
		}
	}
	for h, g := range groups {
		if len(g.removed) == 0 || len(g.added) == 0 {
			continue
		}
		var reason string
		switch {
		case len(g.removed) != 1 || len(g.added) != 1:
			reason = "several removed or added files share this content"
		case g.retained:
			reason = "a file present in both revisions shares this content, so the addition may be a copy"
		case g.removed[0].Language != g.added[0].Language:
			reason = "language differs between the removed and added file"
		}
		if reason == "" {
			r.Exact = append(r.Exact, FileRename{From: g.removed[0].Path, To: g.added[0].Path, ContentHash: h, Language: g.added[0].Language})
			continue
		}
		r.Ambiguous = append(r.Ambiguous, AmbiguousRename{ContentHash: h, Removed: paths(g.removed), Added: paths(g.added), Reason: reason})
	}
	sort.Slice(r.Exact, func(i, j int) bool { return r.Exact[i].From < r.Exact[j].From })
	sort.Slice(r.Ambiguous, func(i, j int) bool { return r.Ambiguous[i].ContentHash < r.Ambiguous[j].ContentHash })
	return r
}

func paths(files []SourceFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}
