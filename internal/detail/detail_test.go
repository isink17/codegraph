package detail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestParseLevel(t *testing.T) {
	for _, name := range Names() {
		got, err := Parse(name, Full)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", name, err)
		}
		if string(got) != name {
			t.Fatalf("Parse(%q) = %q, want %q", name, got, name)
		}
	}

	got, err := Parse("  ", Skeleton)
	if err != nil || got != Skeleton {
		t.Fatalf("Parse(blank) = %q, %v; want skeleton, nil", got, err)
	}

	// Synonyms an agent might reach for are rejected rather than mapped: one
	// vocabulary, and a typo must not silently change how much is returned.
	for _, bad := range []string{"brief", "verbose", "complete", "Card", "CARD", "tiny"} {
		if _, err := Parse(bad, Card); err == nil {
			t.Fatalf("Parse(%q) accepted an undefined level", bad)
		}
	}
}

func TestLevelOrdering(t *testing.T) {
	if !Full.AtLeast(Excerpt) || !Excerpt.AtLeast(Skeleton) || !Skeleton.AtLeast(Card) {
		t.Fatal("levels are not ordered card < skeleton < excerpt < full")
	}
	if Card.AtLeast(Skeleton) {
		t.Fatal("card must not satisfy skeleton")
	}
}

// fakeMembers records how many times it is consulted so a projection that
// degenerates into one query per symbol fails the test rather than merely
// running slower.
type fakeMembers struct {
	calls   int
	refs    []MemberRef
	members map[int64][]graph.Symbol
}

func (f *fakeMembers) SymbolMembers(_ context.Context, refs []MemberRef) (map[int64][]graph.Symbol, error) {
	f.calls++
	f.refs = append(f.refs, refs...)
	return f.members, nil
}

func sym(id int64, name, kind string, start, end int, file string) graph.Symbol {
	return graph.Symbol{
		ID:            id,
		FileID:        7,
		Language:      "go",
		Kind:          kind,
		Name:          name,
		QualifiedName: "pkg." + name,
		ContainerName: "pkg",
		Signature:     "func " + name + "()",
		Visibility:    "public",
		Range:         graph.Position{StartLine: start, StartCol: 1, EndLine: end, EndCol: 2},
		DocSummary:    name + " does something.",
		StableKey:     "go::" + file + "::" + name,
		FilePath:      file,
	}
}

func TestCardCarriesOnlyIdentityAndDrillDownSelectors(t *testing.T) {
	p := NewProjector(Card, t.TempDir(), nil, nil)
	got := p.Symbol(context.Background(), sym(11, "Widget", "type", 10, 40, "pkg/widget.go"))

	if got.Name != "Widget" || got.QualifiedName != "pkg.Widget" || got.Kind != "type" {
		t.Fatalf("card identity = %+v", got)
	}
	// Every selector the tools accept for a follow-up call must survive a card.
	if got.SymbolID != 11 || got.File != "pkg/widget.go" || got.Line != 10 || got.StableKey == "" {
		t.Fatalf("card lacks a drill-down selector: %+v", got)
	}
	if got.Signature != "" || got.DocSummary != "" || got.Visibility != "" || got.ContainerName != "" {
		t.Fatalf("card leaked skeleton fields: %+v", got)
	}
	if got.Source != nil || got.Range != nil || got.FileID != 0 || got.EndLine != 0 {
		t.Fatalf("card leaked excerpt or full fields: %+v", got)
	}
}

func TestSkeletonAddsDeclarationAndMembersInOneQuery(t *testing.T) {
	members := &fakeMembers{members: map[int64][]graph.Symbol{
		11: {
			sym(12, "Size", "function", 20, 25, "pkg/widget.go"),
			sym(13, "Name", "function", 12, 15, "pkg/widget.go"),
		},
	}}
	p := NewProjector(Skeleton, t.TempDir(), members, nil)

	got := p.Symbols(context.Background(), []graph.Symbol{
		sym(11, "Widget", "type", 10, 40, "pkg/widget.go"),
		sym(21, "Other", "type", 50, 80, "pkg/widget.go"),
		sym(31, "Run", "function", 90, 95, "pkg/widget.go"),
	})

	if members.calls != 1 {
		t.Fatalf("member lookups = %d, want exactly 1 for the whole page", members.calls)
	}
	// Function-like symbols are not containers and must not be asked about.
	if len(members.refs) != 2 {
		t.Fatalf("member refs = %d, want 2 container refs", len(members.refs))
	}
	if got[0].Signature == "" || got[0].Visibility == "" || got[0].DocSummary == "" || got[0].EndLine != 40 {
		t.Fatalf("skeleton missing declaration fields: %+v", got[0])
	}
	if len(got[0].Members) != 2 {
		t.Fatalf("members = %d, want 2", len(got[0].Members))
	}
	if got[0].Members[0].Name != "Name" || got[0].Members[1].Name != "Size" {
		t.Fatalf("members are not ordered by line: %+v", got[0].Members)
	}
	// A skeleton describes shape; it must never carry source.
	if got[0].Source != nil {
		t.Fatalf("skeleton returned source: %+v", got[0].Source)
	}
}

func TestSkeletonReportsMissingBodyRangeInsteadOfPretending(t *testing.T) {
	members := &fakeMembers{}
	p := NewProjector(Skeleton, t.TempDir(), members, nil)
	// end_line == start_line is what the regex heuristic adapters record; no
	// member list can be derived from it.
	got := p.Symbol(context.Background(), sym(11, "Widget", "class", 10, 10, "pkg/widget.rb"))

	if len(members.refs) != 0 {
		t.Fatal("queried members for a symbol with no body range")
	}
	if got.SkeletonNote == "" {
		t.Fatal("skeleton silently returned an empty structure")
	}
	if len(got.Members) != 0 {
		t.Fatalf("members = %+v, want none", got.Members)
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func numberedLines(n int, sep string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d%s", i, sep)
	}
	return b.String()
}

func TestExcerptIsBoundedAndFullIsComplete(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "big.go", numberedLines(400, "\n"))
	ctx := context.Background()

	long := sym(1, "Long", "function", 100, 300, "big.go")

	excerpt := NewProjector(Excerpt, root, nil, nil).Symbol(ctx, long)
	if excerpt.Source == nil {
		t.Fatalf("excerpt returned no source: %+v", excerpt)
	}
	lines := excerpt.Source.EndLine - excerpt.Source.StartLine + 1
	if lines > ExcerptMaxLines {
		t.Fatalf("excerpt spans %d lines, over the %d ceiling", lines, ExcerptMaxLines)
	}
	if !excerpt.Source.Truncated {
		t.Fatal("excerpt of a 201-line symbol was not marked truncated")
	}
	// Truncation must hand back the range needed to ask for the rest.
	if excerpt.Source.AvailableStartLine != 100 || excerpt.Source.AvailableEndLine != 300 {
		t.Fatalf("truncated excerpt hid the real range: %+v", excerpt.Source)
	}

	full := NewProjector(Full, root, nil, nil).Symbol(ctx, long)
	if full.Source == nil || full.Source.Truncated {
		t.Fatalf("full source was truncated: %+v", full.Source)
	}
	// Full applies the same context lines as excerpt and a far higher ceiling,
	// so it always spans at least what excerpt spanned.
	if full.Source.StartLine != 100-ContextLines || full.Source.EndLine != 300+ContextLines {
		t.Fatalf("full source range = %d-%d, want %d-%d",
			full.Source.StartLine, full.Source.EndLine, 100-ContextLines, 300+ContextLines)
	}
	if full.Range == nil || full.FileID == 0 {
		t.Fatalf("full dropped legacy fields: %+v", full)
	}
}

// TestFullContainsExcerptForEveryShape is the ordering guarantee stated in the
// package doc, checked across the shapes that used to break it: a short body, a
// body longer than the excerpt ceiling, and a declaration-only range.
func TestFullContainsExcerptForEveryShape(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", numberedLines(2000, "\n"))
	ctx := context.Background()

	cases := map[string]graph.Symbol{
		"short body":          sym(1, "Short", "function", 10, 19, "a.go"),
		"over excerpt cap":    sym(2, "Long", "function", 100, 400, "a.go"),
		"declaration only":    sym(3, "Decl", "class", 500, 500, "a.go"),
		"single line at file": sym(4, "Edge", "function", 1, 1, "a.go"),
	}
	for name, symbol := range cases {
		t.Run(name, func(t *testing.T) {
			excerpt := NewProjector(Excerpt, root, nil, nil).Symbol(ctx, symbol)
			full := NewProjector(Full, root, nil, nil).Symbol(ctx, symbol)
			if excerpt.Source == nil || full.Source == nil {
				t.Fatalf("missing source: excerpt=%+v full=%+v", excerpt.Source, full.Source)
			}
			if full.Source.StartLine > excerpt.Source.StartLine || full.Source.EndLine < excerpt.Source.EndLine {
				t.Fatalf("full (%d-%d) does not contain excerpt (%d-%d)",
					full.Source.StartLine, full.Source.EndLine,
					excerpt.Source.StartLine, excerpt.Source.EndLine)
			}
			if !strings.Contains(full.Source.Text, excerpt.Source.Text) {
				t.Fatalf("full text does not contain excerpt text\nfull:\n%s\nexcerpt:\n%s",
					full.Source.Text, excerpt.Source.Text)
			}
		})
	}
}

func TestSmallSymbolExcerptIsNotMarkedTruncated(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "small.go", numberedLines(40, "\n"))

	got := NewProjector(Excerpt, root, nil, nil).
		Symbol(context.Background(), sym(1, "Tiny", "function", 10, 12, "small.go"))

	if got.Source == nil {
		t.Fatal("no source for a small symbol")
	}
	if got.Source.Truncated {
		t.Fatalf("small symbol marked truncated: %+v", got.Source)
	}
	if got.Source.StartLine != 10-ExcerptContextLines || got.Source.EndLine != 12+ExcerptContextLines {
		t.Fatalf("excerpt window = %d-%d, want %d-%d",
			got.Source.StartLine, got.Source.EndLine, 10-ExcerptContextLines, 12+ExcerptContextLines)
	}
}

func TestCRLFSourceMatchesLFSource(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "unix.go", numberedLines(30, "\n"))
	writeFile(t, root, "win.go", numberedLines(30, "\r\n"))
	ctx := context.Background()

	unix := NewProjector(Full, root, nil, nil).Symbol(ctx, sym(1, "A", "function", 5, 9, "unix.go"))
	win := NewProjector(Full, root, nil, nil).Symbol(ctx, sym(2, "A", "function", 5, 9, "win.go"))

	if unix.Source == nil || win.Source == nil {
		t.Fatal("missing source")
	}
	if unix.Source.Text != win.Source.Text {
		t.Fatalf("CRLF source differs from LF source:\n%q\n%q", win.Source.Text, unix.Source.Text)
	}
	if strings.Contains(win.Source.Text, "\r") {
		t.Fatal("carriage returns survived into the rendered source")
	}
	if win.Source.StartLine != 5-ContextLines || win.Source.EndLine != 9+ContextLines {
		t.Fatalf("CRLF line numbering = %d-%d, want %d-%d",
			win.Source.StartLine, win.Source.EndLine, 5-ContextLines, 9+ContextLines)
	}
}

func TestSourceFailuresAreReportedNotFatal(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "short.go", numberedLines(5, "\n"))
	ctx := context.Background()
	p := NewProjector(Full, root, nil, nil)

	deleted := p.Symbol(ctx, sym(1, "Gone", "function", 1, 10, "deleted.go"))
	if deleted.Source != nil || deleted.SourceNote == "" {
		t.Fatalf("deleted file did not degrade cleanly: %+v", deleted)
	}

	// Line metadata can outlive the lines it describes.
	stale := p.Symbol(ctx, sym(2, "Stale", "function", 90, 120, "short.go"))
	if stale.Source != nil || stale.SourceNote == "" {
		t.Fatalf("stale range did not degrade cleanly: %+v", stale)
	}

	// A range that starts inside the file but runs past its end is clamped
	// rather than refused -- and the clamp is reported, because returning less
	// source than the symbol has without saying so is the silent truncation
	// this model exists to avoid.
	clamped := p.Symbol(ctx, sym(3, "Clamped", "function", 3, 99, "short.go"))
	if clamped.Source == nil || clamped.Source.EndLine != 5 {
		t.Fatalf("range past EOF was not clamped: %+v", clamped.Source)
	}
	if !clamped.Source.Truncated {
		t.Fatalf("clamping to the real file was silent: %+v", clamped.Source)
	}
	if clamped.Source.AvailableEndLine != 99 {
		t.Fatalf("clamped source hid the indexed range: %+v", clamped.Source)
	}
}

// fakeStates reports one indexed file state and counts its calls, so a
// per-symbol freshness check would fail the test rather than merely cost more.
type fakeStates struct {
	calls  int
	states map[string]FileState
}

func (f *fakeStates) FileStates(_ context.Context, _ []string) (map[string]FileState, error) {
	f.calls++
	return f.states, nil
}

func TestSourceReportsDriftFromTheIndex(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", numberedLines(40, "\n"))
	ctx := context.Background()

	// The index recorded a different size, which is what happens when the file
	// is edited after indexing: the stored line numbers no longer describe the
	// bytes on disk.
	stale := &fakeStates{states: map[string]FileState{"a.go": {SizeBytes: 1}}}
	got := NewProjector(Excerpt, root, nil, stale).Symbols(ctx, []graph.Symbol{
		sym(1, "A", "function", 5, 9, "a.go"),
		sym(2, "B", "function", 20, 24, "a.go"),
	})
	if stale.calls != 1 {
		t.Fatalf("freshness lookups = %d, want exactly 1 for the whole page", stale.calls)
	}
	for _, s := range got {
		if s.Source == nil {
			t.Fatalf("drift withheld the source entirely: %+v", s)
		}
		if !strings.Contains(s.SourceNote, "changed since it was indexed") {
			t.Fatalf("drift was not reported: %q", s.SourceNote)
		}
	}

	// A file whose recorded size matches gets no note.
	info, err := os.Stat(filepath.Join(root, "a.go"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	fresh := &fakeStates{states: map[string]FileState{
		"a.go": {SizeBytes: info.Size(), MtimeUnixNs: info.ModTime().UnixNano()},
	}}
	clean := NewProjector(Excerpt, root, nil, fresh).Symbol(ctx, sym(1, "A", "function", 5, 9, "a.go"))
	if clean.SourceNote != "" {
		t.Fatalf("unchanged file was reported as drifted: %q", clean.SourceNote)
	}
}

// TestFullIsNeverSmallerThanExcerpt covers the case the released CGO_ENABLED=0
// binary hits constantly: the regex heuristic adapters record end_line ==
// start_line, so a symbol's "complete source" would be a single line, and full
// would return less than excerpt did.
func TestFullIsNeverSmallerThanExcerpt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "heuristic.rb", numberedLines(40, "\n"))
	ctx := context.Background()
	declarationOnly := sym(1, "Widget", "class", 10, 10, "heuristic.rb")

	excerpt := NewProjector(Excerpt, root, nil, nil).Symbol(ctx, declarationOnly)
	full := NewProjector(Full, root, nil, nil).Symbol(ctx, declarationOnly)

	if excerpt.Source == nil || full.Source == nil {
		t.Fatalf("missing source: excerpt=%+v full=%+v", excerpt.Source, full.Source)
	}
	if len(full.Source.Text) < len(excerpt.Source.Text) {
		t.Fatalf("full (%d bytes) is smaller than excerpt (%d bytes)",
			len(full.Source.Text), len(excerpt.Source.Text))
	}
	if full.SourceNote == "" {
		t.Fatal("full returned a window instead of a body without saying why")
	}
}

func TestSkeletonReportsAFailedMemberLookup(t *testing.T) {
	p := NewProjector(Skeleton, t.TempDir(), failingMembers{}, nil)
	got := p.Symbol(context.Background(), sym(11, "Widget", "class", 10, 40, "pkg/widget.rb"))

	if len(got.Members) != 0 {
		t.Fatalf("members = %+v, want none", got.Members)
	}
	if !strings.Contains(got.SkeletonNote, "lookup failed") {
		t.Fatalf("a failed lookup was indistinguishable from an empty container: %q", got.SkeletonNote)
	}
}

type failingMembers struct{}

func (failingMembers) SymbolMembers(context.Context, []MemberRef) (map[int64][]graph.Symbol, error) {
	return nil, errFakeLookup
}

var errFakeLookup = fmt.Errorf("member lookup exploded")

func TestSkeletonMembersAreCapped(t *testing.T) {
	many := make([]graph.Symbol, 0, MaxSkeletonMembers*2)
	for i := 0; i < MaxSkeletonMembers*2; i++ {
		many = append(many, sym(int64(100+i), fmt.Sprintf("m%03d", i), "function", 11+i, 11+i, "big.rb"))
	}
	members := &fakeMembers{members: map[int64][]graph.Symbol{11: many}}

	got := NewProjector(Skeleton, t.TempDir(), members, nil).
		Symbol(context.Background(), sym(11, "Huge", "class", 10, 500, "big.rb"))

	if len(got.Members) != MaxSkeletonMembers {
		t.Fatalf("members = %d, want the cap of %d", len(got.Members), MaxSkeletonMembers)
	}
	if got.MemberCount != len(many) {
		t.Fatalf("member_count = %d, want the true total %d", got.MemberCount, len(many))
	}
	if got.SkeletonNote == "" {
		t.Fatal("the member list was capped silently")
	}
	// The cap keeps the first members by line, not an arbitrary subset.
	if got.Members[0].Name != "m000" {
		t.Fatalf("cap did not keep the first members by line: %+v", got.Members[0])
	}
}

func TestSourceBudgetIsBoundedAndReported(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", numberedLines((MaxSourceSymbols+5)*4+10, "\n"))
	syms := make([]graph.Symbol, 0, MaxSourceSymbols+5)
	for i := 0; i < MaxSourceSymbols+5; i++ {
		syms = append(syms, sym(int64(i+1), fmt.Sprintf("s%02d", i), "function", 1+i*4, 3+i*4, "a.go"))
	}

	got := NewProjector(Full, root, nil, nil).Symbols(context.Background(), syms)

	rendered := 0
	for _, s := range got {
		if s.Source != nil {
			rendered++
		}
	}
	if rendered != MaxSourceSymbols {
		t.Fatalf("rendered %d symbols with source, want the budget of %d", rendered, MaxSourceSymbols)
	}
	for _, s := range got[MaxSourceSymbols:] {
		if s.Source != nil {
			t.Fatalf("budget exceeded: %+v", s)
		}
		if !strings.Contains(s.SourceNote, "source budget") {
			t.Fatalf("omitted source was not explained: %q", s.SourceNote)
		}
	}
}

// TestContainerKindsCoverTheParserVocabulary fails when an adapter starts
// emitting a container kind the projection does not know about, which would
// otherwise show up as a silently memberless skeleton.
func TestContainerKindsCoverTheParserVocabulary(t *testing.T) {
	for _, kind := range []string{"class", "type", "struct", "interface", "enum", "module", "namespace", "trait", "object", "protocol", "actor", "record"} {
		if !isContainerKind(kind) {
			t.Fatalf("%q is emitted by an adapter but is not treated as a container", kind)
		}
	}
	for _, kind := range []string{"function", "method", "value", "field", "constant"} {
		if isContainerKind(kind) {
			t.Fatalf("%q is a leaf kind but is treated as a container", kind)
		}
	}
}

func TestSourceCannotEscapeTheRepositoryRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	writeFile(t, root, "in.go", "package p\n")
	writeFile(t, filepath.Dir(root), "secret.txt", "classified\n")

	got := NewProjector(Full, root, nil, nil).
		Symbol(context.Background(), sym(1, "Escape", "function", 1, 1, "../secret.txt"))

	if got.Source != nil {
		t.Fatalf("read outside the repository root: %+v", got.Source)
	}
	if got.SourceNote == "" {
		t.Fatal("path rejection was not reported")
	}
}

// TestFullCarriesEveryLegacyFieldValue pins the compatibility rule stated above
// the Symbol type: at full detail every field the pre-P13 payload carried is
// present with the same value, and a field is omitted only when its value is
// empty -- which for an indexed symbol never happens.
func TestFullCarriesEveryLegacyFieldValue(t *testing.T) {
	populated := sym(42, "Widget", "type", 10, 40, "pkg/widget.go")

	got := NewProjector(Full, "", nil, nil).Symbol(context.Background(), populated)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	// Every JSON name graph.Symbol serialized, with the value it held.
	for field, want := range map[string]any{
		"symbol_id":      float64(42),
		"file_id":        float64(7),
		"language":       "go",
		"kind":           "type",
		"name":           "Widget",
		"qualified_name": "pkg.Widget",
		"container_name": "pkg",
		"signature":      "func Widget()",
		"visibility":     "public",
		"doc_summary":    "Widget does something.",
		"stable_key":     "go::pkg/widget.go::Widget",
		"file":           "pkg/widget.go",
	} {
		if decoded[field] != want {
			t.Fatalf("detail=full has %q = %v, want %v", field, decoded[field], want)
		}
	}
	rng, ok := decoded["range"].(map[string]any)
	if !ok {
		t.Fatalf("detail=full dropped range: %s", encoded)
	}
	for field, want := range map[string]any{
		"start_line": float64(10), "start_col": float64(1),
		"end_line": float64(40), "end_col": float64(2),
	} {
		if rng[field] != want {
			t.Fatalf("range has %q = %v, want %v", field, rng[field], want)
		}
	}

	// A symbol with no language, file id, symbol id or stable key -- which the
	// store never produces -- omits those keys rather than emitting empty
	// placeholders. That is the one documented difference from the old shape.
	sparse := NewProjector(Full, "", nil, nil).Symbol(context.Background(), graph.Symbol{
		Kind: "function", Name: "Orphan", QualifiedName: "pkg.Orphan",
		Range: graph.Position{StartLine: 1, EndLine: 2},
	})
	sparseEncoded, err := json.Marshal(sparse)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(sparseEncoded), `"language"`) {
		t.Fatalf("empty value was emitted as a placeholder: %s", sparseEncoded)
	}
	if !strings.Contains(string(sparseEncoded), `"range"`) {
		t.Fatalf("detail=full dropped range: %s", sparseEncoded)
	}
}

func TestProjectionsDoNotShareState(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", numberedLines(50, "\n"))
	members := &fakeMembers{members: map[int64][]graph.Symbol{
		1: {sym(9, "Member", "function", 12, 14, "a.go")},
	}}

	got := NewProjector(Excerpt, root, members, nil).Symbols(context.Background(), []graph.Symbol{
		sym(1, "First", "type", 10, 20, "a.go"),
		sym(2, "Second", "type", 30, 40, "a.go"),
	})

	if len(got[0].Members) != 1 || len(got[1].Members) != 0 {
		t.Fatalf("members bled between results: %+v / %+v", got[0].Members, got[1].Members)
	}
	if got[0].Source.Text == got[1].Source.Text {
		t.Fatal("two symbols in one file rendered identical source")
	}
}

func TestContainerBudgetIsBoundedAndReported(t *testing.T) {
	syms := make([]graph.Symbol, 0, MaxSkeletonContainers+3)
	for i := 0; i < MaxSkeletonContainers+3; i++ {
		syms = append(syms, sym(int64(i+1), fmt.Sprintf("C%03d", i), "class", 1+i*10, 9+i*10, "big.py"))
	}
	members := &fakeMembers{members: map[int64][]graph.Symbol{}}

	got := NewProjector(Skeleton, t.TempDir(), members, nil).Symbols(context.Background(), syms)

	if len(members.refs) != MaxSkeletonContainers {
		t.Fatalf("container refs = %d, want the budget of %d", len(members.refs), MaxSkeletonContainers)
	}
	// The containers that were looked up and genuinely have no members say
	// nothing; the ones that were skipped must say they were skipped, so an
	// agent cannot read "no members" as "empty".
	for _, s := range got[:MaxSkeletonContainers] {
		if strings.Contains(s.SkeletonNote, "container budget") {
			t.Fatalf("a looked-up container claimed it was skipped: %+v", s)
		}
	}
	for _, s := range got[MaxSkeletonContainers:] {
		if !strings.Contains(s.SkeletonNote, "container budget") {
			t.Fatalf("a skipped container was silent: %+v", s)
		}
	}
}

func TestFullSourceIsCappedAndReported(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "huge.go", numberedLines(FullMaxLines*2, "\n"))

	got := NewProjector(Full, root, nil, nil).
		Symbol(context.Background(), sym(1, "Huge", "function", 1, FullMaxLines+500, "huge.go"))

	if got.Source == nil {
		t.Fatal("no source")
	}
	lines := got.Source.EndLine - got.Source.StartLine + 1
	if lines > FullMaxLines {
		t.Fatalf("full source spans %d lines, over the %d ceiling", lines, FullMaxLines)
	}
	if !got.Source.Truncated || got.Source.AvailableEndLine != FullMaxLines+500 {
		t.Fatalf("full source was cut without reporting the real range: %+v", got.Source)
	}
}
