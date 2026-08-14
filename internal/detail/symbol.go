package detail

import (
	"context"
	"fmt"
	"sort"

	"github.com/isink17/codegraph/internal/graph"
)

// Response bounds. Progressive disclosure only helps if the expensive levels
// stay bounded too: an unbounded traversal rendered at full detail would undo
// the whole point.
const (
	// MaxSkeletonMembers caps the member list of one container. A three-thousand
	// line class must not produce a "skeleton" larger than its own excerpt.
	MaxSkeletonMembers = 40
	// MaxSkeletonContainers caps how many containers in one response get a member
	// list. Like MaxSourceSymbols this is a backstop for the tools with no result
	// limit of their own, set well above any paged request.
	MaxSkeletonContainers = 200
	// MaxSourceSymbols caps how many symbols in one response carry rendered
	// source. It is a backstop for the tools that have no result limit of their
	// own -- an impact radius can reach thousands of symbols, and rendering all
	// of them at full detail would read the repository -- and is set well above
	// any paged request so that a caller who asked for a hundred results and
	// paid for full detail still gets source for all hundred.
	MaxSourceSymbols = 200
)

// Symbol is the canonical projected representation of an indexed symbol. Every
// MCP tool that returns a symbol returns this shape, so a field means the same
// thing wherever an agent meets it.
//
// The levels are cumulative and are expressed purely by which fields are
// populated -- there is one struct, not four, so a field can never acquire a
// different meaning in a different tool:
//
//	card      name, qualified_name, kind, language, file, line, symbol_id, stable_key
//	skeleton  + container_name, signature, visibility, doc_summary, end_line, members
//	excerpt   + source (a window around the symbol, capped tightly)
//	full      + file_id, range, source (the same window, capped far higher)
//
// `qualified_name`, `symbol_id`, and `file`+`line` are all present from card
// onward precisely so a card is sufficient to make the follow-up call: they are
// the selectors find_symbol, find_callers, and find_callees already accept.
//
// The compatibility rule for full, stated exactly because it is the escape hatch
// the whole model rests on: every field the pre-P13 payload carried is present at
// full detail under the same JSON name and with the same value -- unless that
// value is empty, in which case the key is omitted, as it is at every other
// level. graph.Symbol's tags carried no `omitempty`, so it did emit
// `"language":""` for a symbol without one. Buying those keys back would mean
// either a per-symbol custom marshaler, which measurably raises allocation on
// card -- the default and by far the hottest path -- or empty placeholders in
// every card. Neither is worth a key whose value says nothing, and an indexed
// symbol always has a language, a file id, a symbol id and a stable key, so on
// real data the two shapes agree key for key.
type Symbol struct {
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Language      string `json:"language,omitempty"`
	File          string `json:"file,omitempty"`
	Line          int    `json:"line,omitempty"`
	SymbolID      int64  `json:"symbol_id,omitempty"`
	StableKey     string `json:"stable_key,omitempty"`

	// Skeleton and above.
	ContainerName string   `json:"container_name,omitempty"`
	Signature     string   `json:"signature,omitempty"`
	Visibility    string   `json:"visibility,omitempty"`
	DocSummary    string   `json:"doc_summary,omitempty"`
	EndLine       int      `json:"end_line,omitempty"`
	Members       []Member `json:"members,omitempty"`
	// MemberCount is the number of members the container actually has, present
	// only when the list above was capped.
	MemberCount int `json:"member_count,omitempty"`
	// SkeletonNote explains why a structural view is thinner than requested --
	// for example when the indexing parser recorded no body range for this
	// symbol. A thinner honest answer is reported; it is never padded with the
	// full body instead.
	SkeletonNote string `json:"skeleton_note,omitempty"`

	// Excerpt and above.
	Source *Source `json:"source,omitempty"`
	// SourceNote explains an absent or degraded Source: file deleted since
	// indexing, range past the end of the file, a parser that recorded only the
	// declaration line, drift between disk and index, or the per-response
	// source budget.
	SourceNote string `json:"source_note,omitempty"`

	// Full only. Carried so that detail=full is a superset of the pre-P13
	// response: `range` and `file_id` are the two fields the old graph.Symbol
	// payload had that the projected shape otherwise drops.
	FileID int64           `json:"file_id,omitempty"`
	Range  *graph.Position `json:"range,omitempty"`
}

// Member is one declaration nested inside a container symbol, as shown by a
// skeleton. It is deliberately narrower than Symbol: a skeleton describes the
// shape of its members, and an agent that wants a member's own body asks for
// that member by name.
type Member struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Signature string `json:"signature,omitempty"`
	Line      int    `json:"line"`
}

// MemberRef identifies a container whose nested declarations a skeleton needs.
type MemberRef struct {
	SymbolID  int64
	FileID    int64
	StartLine int
	EndLine   int
}

// MemberLoader resolves the nested declarations of several containers at once.
//
// It is one call for a whole page by construction: projecting N symbols must
// never become N queries, which is what makes skeleton safe to use on a
// traversal result.
type MemberLoader interface {
	SymbolMembers(ctx context.Context, refs []MemberRef) (map[int64][]graph.Symbol, error)
}

// Projector renders indexed symbols at one detail level.
//
// A Projector is single-use and not safe for concurrent use: it owns a source
// cache scoped to the response it is building, so two responses never share
// file contents or projections.
type Projector struct {
	level        Level
	members      MemberLoader
	files        FileStater
	source       *sourceReader
	sourceBudget int
	// membersFor is the set of symbols a member lookup was actually issued for,
	// so a container that was skipped is distinguishable from one that genuinely
	// has no members.
	membersFor map[int64]bool
}

// NewProjector builds a projector for one response. repoRoot is the resolved
// repository root; source rendering is disabled when it is empty. members and
// files may be nil, in which case skeletons carry declarations but no member
// list, and rendered source carries no staleness note.
func NewProjector(level Level, repoRoot string, members MemberLoader, files FileStater) *Projector {
	if !level.Valid() {
		level = Card
	}
	return &Projector{
		level:        level,
		members:      members,
		files:        files,
		source:       newSourceReader(repoRoot),
		sourceBudget: MaxSourceSymbols,
	}
}

// Level reports the level this projector renders at.
func (p *Projector) Level() Level { return p.level }

// hasBody reports whether the indexed range plausibly spans a body rather than
// just a declaration line.
//
// The check is on the range, not the language, on purpose: the same database
// can be written by the tree-sitter registry or by the regex heuristic registry
// depending on how the binary was built, and nothing records which one ran. A
// heuristic symbol has end_line == start_line, so the range itself is the only
// honest signal available.
func hasBody(sym graph.Symbol) bool {
	return sym.Range.EndLine > sym.Range.StartLine
}

// containerKinds is every symbol kind any adapter emits for a declaration that
// can contain other declarations. Kinds outside this set are treated as leaves,
// so a kind missing here would silently produce a memberless skeleton -- which
// is why the set is exhaustive over the parser vocabulary rather than a
// shortlist of the common cases.
var containerKinds = map[string]bool{
	"class":     true,
	"type":      true,
	"struct":    true,
	"interface": true,
	"enum":      true,
	"module":    true,
	"namespace": true,
	"trait":     true,
	"object":    true,
	"protocol":  true,
	"actor":     true,
	"record":    true,
}

func isContainerKind(kind string) bool { return containerKinds[kind] }

// Symbols projects a page of symbols. Member lookup for the whole page is a
// single batched call, file freshness is one more, and each file is read at
// most once regardless of how many symbols on the page come from it.
func (p *Projector) Symbols(ctx context.Context, syms []graph.Symbol) []Symbol {
	out := make([]Symbol, 0, len(syms))
	if len(syms) == 0 {
		return out
	}

	memberIndex, memberErr := p.loadMembers(ctx, syms)
	p.loadFileStates(ctx, syms)

	for _, sym := range syms {
		out = append(out, p.project(sym, memberIndex[sym.ID], memberErr))
	}
	return out
}

// Symbol projects a single symbol. It exists so that a one-symbol response does
// not have to build a slice; it shares every rule with Symbols.
func (p *Projector) Symbol(ctx context.Context, sym graph.Symbol) Symbol {
	projected := p.Symbols(ctx, []graph.Symbol{sym})
	return projected[0]
}

// loadMembers fetches nested declarations for every container on the page in
// one query. The error is returned rather than swallowed so that a lookup
// failure can be reported on the affected symbols instead of appearing as a
// container that genuinely has no members.
func (p *Projector) loadMembers(ctx context.Context, syms []graph.Symbol) (map[int64][]graph.Symbol, error) {
	if !p.level.AtLeast(Skeleton) || p.members == nil {
		return nil, nil
	}
	refs := make([]MemberRef, 0, len(syms))
	p.membersFor = make(map[int64]bool, len(syms))
	for _, sym := range syms {
		if !isContainerKind(sym.Kind) || !hasBody(sym) || sym.FileID == 0 {
			continue
		}
		if len(refs) >= MaxSkeletonContainers {
			break
		}
		p.membersFor[sym.ID] = true
		refs = append(refs, MemberRef{
			SymbolID:  sym.ID,
			FileID:    sym.FileID,
			StartLine: sym.Range.StartLine,
			EndLine:   sym.Range.EndLine,
		})
	}
	if len(refs) == 0 {
		return nil, nil
	}
	index, err := p.members.SymbolMembers(ctx, refs)
	if err != nil {
		return nil, err
	}
	return index, nil
}

// loadFileStates fetches, in one call, what the index recorded about the files
// this response may render, so drift between disk and index can be reported.
func (p *Projector) loadFileStates(ctx context.Context, syms []graph.Symbol) {
	if !p.level.AtLeast(Excerpt) || p.files == nil || !p.source.enabled() {
		return
	}
	paths := make([]string, 0, len(syms))
	seen := make(map[string]bool, len(syms))
	for _, sym := range syms {
		if sym.FilePath == "" || seen[sym.FilePath] {
			continue
		}
		seen[sym.FilePath] = true
		paths = append(paths, sym.FilePath)
	}
	states, err := p.files.FileStates(ctx, paths)
	if err != nil {
		// Freshness is advisory. Losing it must not cost the caller the source
		// they asked for.
		return
	}
	p.source.noteIndexedState(states)
}

func (p *Projector) project(sym graph.Symbol, members []graph.Symbol, memberErr error) Symbol {
	out := Symbol{
		Name:          sym.Name,
		QualifiedName: sym.QualifiedName,
		Kind:          sym.Kind,
		Language:      sym.Language,
		File:          sym.FilePath,
		Line:          sym.Range.StartLine,
		SymbolID:      sym.ID,
		StableKey:     sym.StableKey,
	}
	if p.level == Card {
		return out
	}

	out.ContainerName = sym.ContainerName
	out.Signature = sym.Signature
	out.Visibility = sym.Visibility
	out.DocSummary = sym.DocSummary
	out.EndLine = sym.Range.EndLine
	if isContainerKind(sym.Kind) {
		switch {
		case !hasBody(sym):
			out.SkeletonNote = "no member list: the indexing parser recorded no body range for this symbol"
		case memberErr != nil:
			out.SkeletonNote = "no member list: the member lookup failed"
		case p.members != nil && !p.membersFor[sym.ID]:
			// The page held more containers than one response looks up. Saying so
			// is the difference between "this container is empty" and "this
			// response did not check".
			out.SkeletonNote = "no member list: this response reached its container budget; request this symbol directly"
		case len(members) > 0:
			out.Members, out.MemberCount = projectMembers(members)
			if out.MemberCount > 0 {
				out.SkeletonNote = fmt.Sprintf(
					"member list truncated to the first %d of %d by line; search_symbols scoped to %q reaches the rest",
					len(out.Members), out.MemberCount, sym.QualifiedName)
			}
		}
	}

	if p.level.AtLeast(Excerpt) {
		p.attachSource(&out, sym)
	}

	if p.level == Full {
		out.FileID = sym.FileID
		position := sym.Range
		out.Range = &position
	}
	return out
}

// attachSource renders the symbol's source. At excerpt the window is bounded by
// the rules in source.go; at full the symbol's exact indexed range is returned.
func (p *Projector) attachSource(out *Symbol, sym graph.Symbol) {
	if sym.FilePath == "" {
		out.SourceNote = "no indexed file path for this symbol"
		return
	}
	if p.sourceBudget <= 0 {
		out.SourceNote = "source omitted: this response reached its source budget; request this symbol directly"
		return
	}

	// A parser that recorded only the declaration line gives full nothing to
	// return but a couple of lines, which is not the body the level promises.
	// The window is the same either way; the note says why the answer is a
	// window rather than a body.
	declarationOnly := p.level == Full && !hasBody(sym)
	maxLines := FullMaxLines
	if p.level == Excerpt || declarationOnly {
		maxLines = ExcerptMaxLines
	}

	src, reason := p.source.slice(sym.FilePath, sym.Range.StartLine, sym.Range.EndLine, ContextLines, maxLines)
	if reason != "" {
		out.SourceNote = reason
		return
	}
	p.sourceBudget--
	out.Source = src

	notes := make([]string, 0, 2)
	if declarationOnly {
		notes = append(notes, "indexed range covers the declaration line only; the parser that indexed this file recorded no body range, so a bounded window around it is shown instead")
	}
	if stale := p.source.staleNote(sym.FilePath); stale != "" {
		notes = append(notes, stale)
	}
	switch len(notes) {
	case 0:
	case 1:
		out.SourceNote = notes[0]
	default:
		out.SourceNote = notes[0] + "; " + notes[1]
	}
}

// projectMembers renders a container's members, capped. The second return value
// is the container's true member count, and is zero when nothing was dropped.
func projectMembers(members []graph.Symbol) ([]Member, int) {
	// The loader already orders its results, but the cap below makes ordering
	// part of *which* members are shown rather than only how they are listed,
	// so the projection does not take the loader's word for it.
	ordered := make([]graph.Symbol, len(members))
	copy(ordered, members)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Range.StartLine != ordered[j].Range.StartLine {
			return ordered[i].Range.StartLine < ordered[j].Range.StartLine
		}
		return ordered[i].Name < ordered[j].Name
	})

	total := len(ordered)
	capped := ordered
	truncated := 0
	if total > MaxSkeletonMembers {
		capped = ordered[:MaxSkeletonMembers]
		truncated = total
	}
	out := make([]Member, 0, len(capped))
	for _, m := range capped {
		out = append(out, Member{
			Name:      m.Name,
			Kind:      m.Kind,
			Signature: m.Signature,
			Line:      m.Range.StartLine,
		})
	}
	return out, truncated
}
