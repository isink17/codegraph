package store

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// insertRangedSymbol inserts a symbol with an explicit line range, which is what
// member containment is decided by. The existing helpers pin every symbol to
// line 1, so nesting cannot be expressed with them.
func insertRangedSymbol(t *testing.T, ctx context.Context, s *Store, repoID, fileID int64, name, kind string, startLine, endLine int) int64 {
	t.Helper()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO symbols(
			repo_id, file_id, language, kind, name, qualified_name, container_name,
			start_line, start_col, end_line, end_col, stable_key, qualified_suffix, dot_tail2, dot_tail3
		)
		VALUES(?, ?, 'py', ?, ?, ?, '', ?, 1, ?, 1, ?, ?, ?, ?)
	`, repoID, fileID, kind, name, "m."+name, startLine, endLine,
		"m."+name+"|py", qualifiedSuffix("m."+name), dotTail2("m."+name), dotTail3("m."+name))
	if err != nil {
		t.Fatalf("insert symbol %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func namesOf(members []graph.Symbol) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Name)
	}
	return out
}

// TestSymbolMembersReturnsImmediateChildren pins the containment contract the
// skeleton level rests on. It builds the nesting directly rather than through a
// parser so that it runs in every build, including the CGO_ENABLED=0 one where
// no adapter records a body range at all.
func TestSymbolMembersReturnsImmediateChildren(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)

	fileID, err := insertTestFile(ctx, s, repoID, "shapes.py")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}

	//  1 class Shape:                 <- container, 1-12
	//  2     def area(self):          <- member, 2-6
	//  3         def rounded():       <- grandchild, 3-4
	//  6     def describe(self):      <- member, 7-9
	// 10     class Inner:             <- member and a container itself, 10-12
	// 11         def deep(self):      <- Inner's member, not Shape's
	// 14 def free_function():         <- outside, 14-15
	shape := insertRangedSymbol(t, ctx, s, repoID, fileID, "Shape", "class", 1, 12)
	insertRangedSymbol(t, ctx, s, repoID, fileID, "area", "function", 2, 6)
	insertRangedSymbol(t, ctx, s, repoID, fileID, "rounded", "function", 3, 4)
	insertRangedSymbol(t, ctx, s, repoID, fileID, "describe", "function", 7, 9)
	inner := insertRangedSymbol(t, ctx, s, repoID, fileID, "Inner", "class", 10, 12)
	insertRangedSymbol(t, ctx, s, repoID, fileID, "deep", "function", 11, 12)
	insertRangedSymbol(t, ctx, s, repoID, fileID, "free_function", "function", 14, 15)

	// A second file must not leak into either container's members.
	otherFileID, err := insertTestFile(ctx, s, repoID, "other.py")
	if err != nil {
		t.Fatalf("insertTestFile(other): %v", err)
	}
	insertRangedSymbol(t, ctx, s, repoID, otherFileID, "elsewhere", "function", 2, 6)

	got, err := s.SymbolMembers(ctx, repoID, []MemberRange{
		{SymbolID: shape, FileID: fileID, StartLine: 1, EndLine: 12},
		{SymbolID: inner, FileID: fileID, StartLine: 10, EndLine: 12},
	})
	if err != nil {
		t.Fatalf("SymbolMembers() error = %v", err)
	}

	shapeMembers := namesOf(got[shape])
	want := []string{"area", "describe", "Inner"}
	if len(shapeMembers) != len(want) {
		t.Fatalf("Shape members = %v, want %v", shapeMembers, want)
	}
	for i, name := range want {
		if shapeMembers[i] != name {
			t.Fatalf("Shape members = %v, want %v (ordered by line)", shapeMembers, want)
		}
	}

	// `deep` belongs to Inner, and both containers were asked about in the same
	// call, so it must appear under exactly one of them.
	innerMembers := namesOf(got[inner])
	if len(innerMembers) != 1 || innerMembers[0] != "deep" {
		t.Fatalf("Inner members = %v, want [deep]", innerMembers)
	}

	// `area` is itself not requested as a container, so its nested function is
	// nobody's member here -- it is not silently promoted to Shape's.
	for _, name := range shapeMembers {
		if name == "rounded" {
			t.Fatalf("Shape members include a grandchild: %v", shapeMembers)
		}
	}
}

func TestSymbolMembersExcludesTheContainerAndOutsiders(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)

	fileID, err := insertTestFile(ctx, s, repoID, "a.py")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	box := insertRangedSymbol(t, ctx, s, repoID, fileID, "Box", "class", 5, 10)
	// Starts on the container's own first line: not strictly inside.
	insertRangedSymbol(t, ctx, s, repoID, fileID, "sameLine", "function", 5, 6)
	// Starts inside but runs past the end: not contained.
	insertRangedSymbol(t, ctx, s, repoID, fileID, "overruns", "function", 8, 14)
	insertRangedSymbol(t, ctx, s, repoID, fileID, "before", "function", 1, 4)
	insertRangedSymbol(t, ctx, s, repoID, fileID, "inside", "function", 6, 7)

	got, err := s.SymbolMembers(ctx, repoID, []MemberRange{
		{SymbolID: box, FileID: fileID, StartLine: 5, EndLine: 10},
	})
	if err != nil {
		t.Fatalf("SymbolMembers() error = %v", err)
	}
	names := namesOf(got[box])
	if len(names) != 1 || names[0] != "inside" {
		t.Fatalf("Box members = %v, want [inside]", names)
	}
}

func TestSymbolMembersWithNoRangesDoesNotQuery(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)

	got, err := s.SymbolMembers(ctx, repoID, nil)
	if err != nil {
		t.Fatalf("SymbolMembers(nil) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("SymbolMembers(nil) = %v, want empty", got)
	}
}

// TestSymbolMembersHandlesContainersSharingARange covers two declarations with
// the same span -- which happens when a language emits a type and its underlying
// struct, or when two adapters both record a class. Ordering alone would put one
// of them inside the other and leave the loser with no members at all, and a
// memberless container is indistinguishable from an empty one on the wire.
func TestSymbolMembersHandlesContainersSharingARange(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)

	fileID, err := insertTestFile(ctx, s, repoID, "twins.py")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	first := insertRangedSymbol(t, ctx, s, repoID, fileID, "First", "class", 1, 20)
	second := insertRangedSymbol(t, ctx, s, repoID, fileID, "Second", "class", 1, 20)
	insertRangedSymbol(t, ctx, s, repoID, fileID, "shared", "function", 5, 6)

	got, err := s.SymbolMembers(ctx, repoID, []MemberRange{
		{SymbolID: first, FileID: fileID, StartLine: 1, EndLine: 20},
		{SymbolID: second, FileID: fileID, StartLine: 1, EndLine: 20},
	})
	if err != nil {
		t.Fatalf("SymbolMembers() error = %v", err)
	}
	for label, id := range map[string]int64{"First": first, "Second": second} {
		names := namesOf(got[id])
		if len(names) != 1 || names[0] != "shared" {
			t.Fatalf("%s members = %v, want [shared]", label, names)
		}
	}
}

// TestSymbolMembersDoesNotDuplicateAcrossChunks pins the interaction between the
// shared result map and chunking: the same container asked about twice, far
// enough apart to land in two chunks, must not have its members listed twice.
func TestSymbolMembersDoesNotDuplicateAcrossChunks(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)

	fileID, err := insertTestFile(ctx, s, repoID, "chunked.py")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	box := insertRangedSymbol(t, ctx, s, repoID, fileID, "Box", "class", 1, 40)
	insertRangedSymbol(t, ctx, s, repoID, fileID, "inside", "function", 5, 6)

	ranges := make([]MemberRange, 0, memberChunkSize+2)
	ranges = append(ranges, MemberRange{SymbolID: box, FileID: fileID, StartLine: 1, EndLine: 40})
	for i := 0; i < memberChunkSize; i++ {
		// Filler containers in a file that holds nothing, so they contribute no
		// members but do push the duplicate past the chunk boundary.
		ranges = append(ranges, MemberRange{SymbolID: box + int64(1000+i), FileID: fileID, StartLine: 10_000, EndLine: 10_001})
	}
	ranges = append(ranges, MemberRange{SymbolID: box, FileID: fileID, StartLine: 1, EndLine: 40})

	got, err := s.SymbolMembers(ctx, repoID, ranges)
	if err != nil {
		t.Fatalf("SymbolMembers() error = %v", err)
	}
	if names := namesOf(got[box]); len(names) != 1 {
		t.Fatalf("Box members = %v, want exactly one entry", names)
	}
}

// TestSymbolMembersQueryUsesTheFileStartIndex pins the access path. The whole
// reason the branches are UNION ALL rather than an OR chain is that SQLite
// stops decomposing an OR of more than a couple of terms and scans every symbol
// in the repository instead; nothing else in the test suite would notice that
// regression.
func TestSymbolMembersQueryUsesTheFileStartIndex(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)

	fileID, err := insertTestFile(ctx, s, repoID, "planned.py")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	ranges := make([]MemberRange, 0, 5)
	for i := 0; i < 5; i++ {
		id := insertRangedSymbol(t, ctx, s, repoID, fileID, "C"+string(rune('a'+i)), "class", 1+i*10, 9+i*10)
		ranges = append(ranges, MemberRange{SymbolID: id, FileID: fileID, StartLine: 1 + i*10, EndLine: 9 + i*10})
	}

	var query strings.Builder
	args := make([]any, 0, len(ranges)*4)
	for i, ref := range ranges {
		if i > 0 {
			query.WriteString("\nUNION ALL\n")
		}
		query.WriteString(`SELECT s.id FROM symbols s JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND s.file_id = ? AND s.start_line > ? AND s.end_line <= ?`)
		args = append(args, repoID, ref.FileID, ref.StartLine, ref.EndLine)
	}

	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query.String(), args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN error = %v", err)
	}
	defer rows.Close()

	branches := 0
	for rows.Next() {
		var id, parent, notUsed int
		var plan string
		if err := rows.Scan(&id, &parent, &notUsed, &plan); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if strings.Contains(plan, "symbols") && strings.Contains(plan, "idx_symbols_file_start") {
			branches++
		}
		if strings.Contains(plan, "SCAN s") {
			t.Fatalf("member lookup scans the symbols table: %s", plan)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if branches != len(ranges) {
		t.Fatalf("%d of %d branches used idx_symbols_file_start", branches, len(ranges))
	}
}
