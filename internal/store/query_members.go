package store

import (
	"context"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// memberChunkSize bounds how many containers one statement asks about. Each
// container contributes three bind parameters, so 200 stays far below SQLite's
// default variable limit while keeping a normal result page to a single query.
const memberChunkSize = 200

// MemberRange is the source span of a container whose nested declarations are
// wanted. It lives here rather than in the rendering layer so that persistence
// does not depend on presentation.
type MemberRange struct {
	SymbolID  int64
	FileID    int64
	StartLine int
	EndLine   int
}

// SymbolMembers returns the immediate declarations nested inside each requested
// container, keyed by the container's symbol id.
//
// Membership is decided by line containment within the same file rather than by
// the free-text `container_name` column: containment is served by
// idx_symbols_file_start(file_id, start_line), whereas container_name has no
// index and would scan the repository's symbols once per page. It is also the
// only rule that holds across languages, since container_name means different
// things to different adapters (a package for Go functions, a receiver for Go
// methods, a class elsewhere).
//
// Only immediate children are returned. A skeleton describes one level of
// structure; listing every transitive descendant would grow it back toward the
// size of the body it exists to replace.
//
// Callers pass only containers whose range actually spans a body; a parser that
// recorded end_line == start_line yields no members here, which is why the
// projection reports that case rather than silently showing an empty structure.
func (s *Store) SymbolMembers(ctx context.Context, repoID int64, ranges []MemberRange) (map[int64][]graph.Symbol, error) {
	if len(ranges) == 0 {
		return nil, nil
	}
	// Deduplicate by symbol id. `out` accumulates across chunks, so a container
	// requested twice -- once in each of two chunks -- would otherwise have its
	// members appended twice.
	unique := make([]MemberRange, 0, len(ranges))
	seenRange := make(map[int64]bool, len(ranges))
	for _, ref := range ranges {
		if seenRange[ref.SymbolID] {
			continue
		}
		seenRange[ref.SymbolID] = true
		unique = append(unique, ref)
	}

	out := make(map[int64][]graph.Symbol, len(unique))
	for start := 0; start < len(unique); start += memberChunkSize {
		end := min(start+memberChunkSize, len(unique))
		chunk := unique[start:end]

		candidates, err := s.nestedSymbols(ctx, repoID, chunk)
		if err != nil {
			return nil, err
		}
		assignImmediateMembers(out, chunk, candidates)
	}
	return out, nil
}

// nestedSymbols fetches every symbol strictly inside any of the given ranges.
//
// The branches are UNION ALL rather than an OR chain because SQLite abandons
// OR-decomposition once there are more than a couple of terms and falls back to
// scanning every symbol in the repository, which makes the cost grow with the
// repository instead of with the page. Each UNION ALL branch plans as
// `SEARCH s USING INDEX idx_symbols_file_start (file_id=? AND start_line>?)`.
func (s *Store) nestedSymbols(ctx context.Context, repoID int64, ranges []MemberRange) ([]graph.Symbol, error) {
	var query strings.Builder
	args := make([]any, 0, len(ranges)*4)
	for i, ref := range ranges {
		if i > 0 {
			query.WriteString("\nUNION ALL\n")
		}
		// Strictly inside: a container is not its own member, and a symbol that
		// merely starts on the container's first line but runs past its end is
		// not nested in it.
		query.WriteString(`
			SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
			       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND s.file_id = ? AND s.start_line > ? AND s.end_line <= ?`)
		args = append(args, repoID, ref.FileID, ref.StartLine, ref.EndLine)
	}

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	candidates, err := scanSymbols(rows)
	if err != nil {
		return nil, err
	}

	// UNION ALL keeps duplicates, and it must: DISTINCT over sixteen wide
	// columns costs more than folding a handful of repeats here. A symbol is
	// repeated only when it is nested in more than one requested container.
	seen := make(map[int64]bool, len(candidates))
	deduped := candidates[:0]
	for _, cand := range candidates {
		if seen[cand.ID] {
			continue
		}
		seen[cand.ID] = true
		deduped = append(deduped, cand)
	}
	return deduped, nil
}

// assignImmediateMembers attributes each candidate to its nearest enclosing
// requested container.
//
// It is a single ordered sweep per file rather than a containment test of every
// candidate against every other one: a hub page can ask about hundreds of
// containers holding thousands of nested declarations, and a cross product over
// those would cost more than the queries that produced them.
//
// Sorting by (start_line ASC, end_line DESC) puts an enclosing declaration
// immediately before everything it encloses, so a stack of "still open"
// declarations has the nearest container on top at all times.
func assignImmediateMembers(out map[int64][]graph.Symbol, ranges []MemberRange, candidates []graph.Symbol) {
	type element struct {
		symbolID   int64
		fileID     int64
		start, end int
		// symbol is nil for a requested container that was not itself fetched as
		// somebody else's nested declaration. Such an element takes part in the
		// sweep -- it can be a parent -- but is nobody's member.
		symbol *graph.Symbol
	}

	byFile := map[int64][]element{}
	requested := make(map[int64]bool, len(ranges))
	// slot records where a requested container's element lives, so a container
	// that also turns up as somebody's nested declaration can be given its
	// payload instead of being added to the sweep twice.
	type slot struct {
		fileID int64
		index  int
	}
	slots := make(map[int64]slot, len(ranges))
	for _, ref := range ranges {
		requested[ref.SymbolID] = true
		slots[ref.SymbolID] = slot{fileID: ref.FileID, index: len(byFile[ref.FileID])}
		byFile[ref.FileID] = append(byFile[ref.FileID], element{
			symbolID: ref.SymbolID, fileID: ref.FileID,
			start: ref.StartLine, end: ref.EndLine,
		})
	}
	for i := range candidates {
		cand := candidates[i]
		if requested[cand.ID] {
			// The identity check guards against a caller whose range named a
			// different file than the symbol actually lives in: the slot would
			// then point at an unrelated element, or past the end of a shorter
			// slice.
			if at, ok := slots[cand.ID]; ok && at.index < len(byFile[at.fileID]) &&
				byFile[at.fileID][at.index].symbolID == cand.ID {
				byFile[at.fileID][at.index].symbol = &candidates[i]
				continue
			}
		}
		byFile[cand.FileID] = append(byFile[cand.FileID], element{
			symbolID: cand.ID, fileID: cand.FileID,
			start: cand.Range.StartLine, end: cand.Range.EndLine,
			symbol: &candidates[i],
		})
	}

	fileIDs := make([]int64, 0, len(byFile))
	for id := range byFile {
		fileIDs = append(fileIDs, id)
	}
	sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })

	// frame is one nesting level of the sweep. Declarations that share a range
	// exactly -- two adapters emitting the same class, a type and its underlying
	// struct -- occupy one frame together, so a symbol nested in that range is a
	// member of every one of them rather than of whichever happened to be
	// ordered last.
	type frame struct {
		start, end int
		symbolIDs  []int64
	}

	for _, fileID := range fileIDs {
		elems := byFile[fileID]
		sort.SliceStable(elems, func(i, j int) bool {
			if elems[i].start != elems[j].start {
				return elems[i].start < elems[j].start
			}
			if elems[i].end != elems[j].end {
				return elems[i].end > elems[j].end
			}
			return elems[i].symbolID < elems[j].symbolID
		})

		var stack []frame
		assign := func(parent frame, symbol *graph.Symbol) {
			if symbol == nil {
				return
			}
			for _, parentID := range parent.symbolIDs {
				if requested[parentID] {
					out[parentID] = append(out[parentID], *symbol)
				}
			}
		}

		for _, elem := range elems {
			// Pop until the top either encloses elem or shares its exact range.
			// Ordering guarantees top.start <= elem.start, so containment reduces
			// to these comparisons and each frame is pushed and popped once.
			sameRange := false
			for len(stack) > 0 {
				top := stack[len(stack)-1]
				if top.start == elem.start && top.end == elem.end {
					sameRange = true
					break
				}
				if elem.start > top.start && elem.end <= top.end {
					break
				}
				stack = stack[:len(stack)-1]
			}

			if sameRange {
				// elem is not nested in the frame it shares a range with; they are
				// the same nesting level. It joins that frame as a parent, and is
				// a member of whatever encloses them both.
				if len(stack) > 1 {
					assign(stack[len(stack)-2], elem.symbol)
				}
				top := &stack[len(stack)-1]
				top.symbolIDs = append(top.symbolIDs, elem.symbolID)
				continue
			}

			if len(stack) > 0 {
				assign(stack[len(stack)-1], elem.symbol)
			}
			stack = append(stack, frame{start: elem.start, end: elem.end, symbolIDs: []int64{elem.symbolID}})
		}
	}

	for id := range out {
		members := out[id]
		sort.SliceStable(members, func(i, j int) bool {
			if members[i].Range.StartLine != members[j].Range.StartLine {
				return members[i].Range.StartLine < members[j].Range.StartLine
			}
			return members[i].ID < members[j].ID
		})
	}
}
