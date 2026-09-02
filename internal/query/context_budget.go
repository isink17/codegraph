package query

import (
	"errors"
	"fmt"
	"math"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/tokenest"
)

const (
	// DefaultContextMaxTokens is the budget a caller that says nothing gets. It
	// is large enough to carry the direct matches and their immediate graph
	// neighbours for a normal task, and small enough that an agent can afford the
	// call without planning for it.
	DefaultContextMaxTokens = 4000
	// MaxContextMaxTokens bounds what a caller may ask for. context_for_task is a
	// selector, not a source dump: past this point the answer is find_symbol with
	// detail=excerpt, not a larger context page.
	MaxContextMaxTokens = 200_000
)

// ErrContextBudgetTooSmall is returned when max_tokens cannot pay for a useful
// response: either the fixed response envelope does not fit, or it fits but
// leaves no room for a single context symbol while candidates remain. Reporting
// an empty page with has_more set would be a continuation an agent could never
// make progress on.
var ErrContextBudgetTooSmall = errors.New("max_tokens too small for a context page")

// normalizeContextTokens applies the token-budget contract: negative is
// rejected, zero or omitted takes the default, and an absurd request is clamped
// to the defensive bound rather than honoured.
func normalizeContextTokens(maxTokens int) (int, error) {
	switch {
	case maxTokens < 0:
		return 0, fmt.Errorf("max_tokens must not be negative, got %d", maxTokens)
	case maxTokens == 0:
		return DefaultContextMaxTokens, nil
	case maxTokens > MaxContextMaxTokens:
		return MaxContextMaxTokens, nil
	default:
		return maxTokens, nil
	}
}

// budgetInput is everything the budget needs: the ordered candidate stream, the
// offset a cursor resumed at, the budget, and the cursor state to stamp a
// continuation with.
type budgetInput struct {
	task       string
	candidates []contextCandidate
	offset     int
	maxTokens  int
	cursor     contextCursorState
}

// buildBudgetedContext fills the budget with the highest-ranked candidates from
// offset onwards and reports the exact size of what it returns.
//
// Selection uses a cheap per-item byte estimate to avoid re-serializing a
// growing document once per candidate, and then measures the assembled response
// exactly. If the exact measurement overshoots -- the estimate ignores nothing
// today, but a future field could make it optimistic -- the last item is dropped
// and the response is measured again, so the advertised budget is never
// silently exceeded.
func buildBudgetedContext(in budgetInput) (*graph.TaskContext, error) {
	total := len(in.candidates)
	if in.offset > total {
		in.offset = total
	}
	budgetBytes := in.maxTokens * tokenest.BytesPerToken

	// Worst-case envelope: has_more set, a continuation cursor at the largest
	// offset this stream can produce, and the widest counters. Measuring the
	// reserve rather than the eventual values keeps a page from overshooting
	// because the cursor it had to carry was longer than the one it measured.
	reserveCursor, err := encodeContextCursor(cursorAt(in.cursor, total))
	if err != nil {
		return nil, err
	}
	envelope := &graph.TaskContext{
		Task:             in.task,
		MaxTokens:        in.maxTokens,
		EstimatedTokens:  in.maxTokens,
		HasMore:          true,
		NextCursor:       reserveCursor,
		ReturnedSymbols:  total,
		RemainingSymbols: total,
	}
	_, envelopeBytes, err := tokenest.OfJSON(envelope)
	if err != nil {
		return nil, err
	}
	if tokenest.FromBytes(envelopeBytes) > in.maxTokens {
		return nil, fmt.Errorf("%w: the response envelope alone needs %d tokens, max_tokens is %d",
			ErrContextBudgetTooSmall, tokenest.FromBytes(envelopeBytes), in.maxTokens)
	}

	// Cheap fill pass.
	usedBytes := envelopeBytes
	fileSeen := map[string]bool{}
	count := 0
	for i := in.offset; i < total; i++ {
		cand := in.candidates[i]
		cost, err := candidateCostBytes(cand, fileSeen)
		if err != nil {
			return nil, err
		}
		if usedBytes+cost > budgetBytes {
			break
		}
		usedBytes += cost
		fileSeen[cand.sym.FilePath] = true
		count++
	}

	// Exact measurement pass.
	for {
		result, err := assembleContext(in, count)
		if err != nil {
			return nil, err
		}
		if result.EstimatedTokens <= in.maxTokens || count == 0 {
			if count == 0 && in.offset < total {
				return nil, fmt.Errorf("%w: max_tokens is %d, which fits the envelope but no context symbol",
					ErrContextBudgetTooSmall, in.maxTokens)
			}
			return result, nil
		}
		count--
	}
}

// candidateCostBytes is the serialized cost of adding one candidate: the symbol
// record, plus a file envelope when the candidate opens a file this page has not
// carried yet, plus one byte per JSON separator.
func candidateCostBytes(cand contextCandidate, fileSeen map[string]bool) (int, error) {
	_, symBytes, err := tokenest.OfJSON(taskSymbol(cand))
	if err != nil {
		return 0, err
	}
	cost := symBytes + 1
	if !fileSeen[cand.sym.FilePath] {
		_, fileBytes, err := tokenest.OfJSON(graph.TaskContextFile{
			Path:     cand.sym.FilePath,
			Language: cand.sym.Language,
			// The widest relevance_score this envelope can carry, so the estimate
			// cannot come in under what is emitted. Under-charging here is not
			// cosmetic: a page that the fill pass admits and the exact pass then
			// rejects down to zero items fails closed, and the cursor that produced
			// it can never be advanced.
			RelevanceScore: widestFileRelevanceScore,
			Symbols:        []graph.TaskContextSymbol{},
		})
		if err != nil {
			return 0, err
		}
		cost += fileBytes + 1
	}
	return cost, nil
}

// assembleContext builds the response for exactly count candidates starting at
// the input offset, including the continuation cursor and the exact estimate of
// the document it returns.
func assembleContext(in budgetInput, count int) (*graph.TaskContext, error) {
	total := len(in.candidates)
	end := in.offset + count
	selected := in.candidates[in.offset:end]

	files, testFiles := projectCandidates(selected, fileRanks(in.candidates))
	result := &graph.TaskContext{
		Task:             in.task,
		Files:            files,
		TestFiles:        testFiles,
		MaxTokens:        in.maxTokens,
		HasMore:          end < total,
		ReturnedSymbols:  count,
		RemainingSymbols: total - end,
	}
	if result.HasMore {
		cursor, err := encodeContextCursor(cursorAt(in.cursor, end))
		if err != nil {
			return nil, err
		}
		result.NextCursor = cursor
	}
	// The estimate is part of the document it describes, so writing it changes the
	// size -- and crossing a digit boundary (999 -> 1000) changes it again.
	// Iterate to the fixed point, which is reached in one or two rounds because
	// each round only changes the field's width, then take the larger of the fixed
	// point and one final measurement so the reported figure is never below the
	// truth.
	for range 4 {
		measured, _, err := tokenest.OfJSON(result)
		if err != nil {
			return nil, err
		}
		if measured == result.EstimatedTokens {
			return result, nil
		}
		result.EstimatedTokens = measured
	}
	measured, _, err := tokenest.OfJSON(result)
	if err != nil {
		return nil, err
	}
	if measured > result.EstimatedTokens {
		result.EstimatedTokens = measured
	}
	return result, nil
}

func cursorAt(base contextCursorState, offset int) contextCursorState {
	base.Offset = offset
	return base
}

// fileRanks assigns each file its position in the whole ranked stream, so a
// file's relevance_score means the same thing on page three as on page one. A
// page-relative position would report 1.0 for whatever file a page happened to
// start with.
func fileRanks(candidates []contextCandidate) map[string]int {
	ranks := make(map[string]int)
	for _, cand := range candidates {
		if _, seen := ranks[cand.sym.FilePath]; !seen {
			ranks[cand.sym.FilePath] = len(ranks)
		}
	}
	return ranks
}

// projectCandidates groups the selected candidates into the public file-grouped
// shape. File order follows each file's best candidate, which is its first
// appearance in the already-ordered stream, and symbol order inside a file
// follows the same stream -- so both are as deterministic as the ranking.
//
// Test-file candidates keep their own section, as they did before P14. A file
// can appear on more than one page when its symbols straddle a page boundary;
// repeating a file envelope is cheap, repeating a symbol is not allowed.
func projectCandidates(selected []contextCandidate, ranks map[string]int) (files, testFiles []graph.TaskContextFile) {
	prodIdx := map[string]int{}
	testIdx := map[string]int{}
	// A file's language is the first non-empty one among its candidates on this
	// page: a symbol the parser could not classify must not blank out the file.
	setLanguage := func(file *graph.TaskContextFile, language string) {
		if file.Language == "" {
			file.Language = language
		}
	}
	for _, cand := range selected {
		path := cand.sym.FilePath
		sym := taskSymbol(cand)
		if cand.isTest {
			if idx, ok := testIdx[path]; ok {
				testFiles[idx].Symbols = append(testFiles[idx].Symbols, sym)
				setLanguage(&testFiles[idx], cand.sym.Language)
				continue
			}
			testIdx[path] = len(testFiles)
			testFiles = append(testFiles, graph.TaskContextFile{
				Path:     path,
				Language: cand.sym.Language,
				Symbols:  []graph.TaskContextSymbol{sym},
			})
			continue
		}
		if idx, ok := prodIdx[path]; ok {
			files[idx].Symbols = append(files[idx].Symbols, sym)
			setLanguage(&files[idx], cand.sym.Language)
			continue
		}
		prodIdx[path] = len(files)
		files = append(files, graph.TaskContextFile{
			Path:     path,
			Language: cand.sym.Language,
			Symbols:  []graph.TaskContextSymbol{sym},
		})
	}
	// relevance_score keeps its pre-P14 meaning: a file's rank position, highest
	// first -- over the whole ranked stream, not over the page.
	for i := range files {
		files[i].RelevanceScore = fileRelevanceScore(ranks[files[i].Path])
	}
	return files, testFiles
}

// truncateCardField cuts a field to at most limit runes, marking the cut so a
// reader can tell a bounded value from a complete one. Cutting on rune
// boundaries keeps the JSON valid for non-ASCII identifiers and comments.
func truncateCardField(value string, limit int) string {
	if value == "" || len(value) <= limit {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	keep := limit - len(contextTruncationMarker)
	if keep < 1 {
		keep = 1
	}
	return string(runes[:keep]) + contextTruncationMarker
}

// widestFileRelevanceScore is the longest value fileRelevanceScore can
// serialize, used to price a file envelope before its position is known.
const widestFileRelevanceScore = 0.55

// fileRelevanceScore keeps the pre-P14 formula -- a file's rank position in the
// ranked stream, highest first -- and rounds it to two decimals. Without the
// rounding, 1.0-9*0.05 marshals as 0.5499999999999999: nineteen bytes of binary
// floating-point noise in a public field, and nineteen bytes the budget's
// per-item estimate would have to predict.
func fileRelevanceScore(position int) float64 {
	score := math.Round((1.0-float64(position)*0.05)*100) / 100
	if score < 0.1 {
		return 0.1
	}
	return score
}

// Card fields are bounded. A doc comment in a well-documented repository runs to
// several hundred bytes -- on codegraph itself the unbounded record averaged
// ~2.6kB, so a 4000-token page carried six symbols and a smaller budget could not
// fit one. A selector needs enough of the declaration to choose; the rest is one
// find_symbol call away at detail=skeleton or above.
const (
	maxContextSignatureChars = 200
	maxContextDocChars       = 160
	contextTruncationMarker  = "..."
)

// taskSymbol projects one candidate to the public compact record. It carries
// identity, location, and bounded declaration metadata only: source belongs to
// the P13 detail levels of find_symbol and friends, reached through the exact
// symbol_id. stable_key remains semantic metadata and may collide.
func taskSymbol(cand contextCandidate) graph.TaskContextSymbol {
	return graph.TaskContextSymbol{
		Name:          cand.sym.Name,
		Kind:          cand.sym.Kind,
		Signature:     truncateCardField(cand.sym.Signature, maxContextSignatureChars),
		DocSummary:    truncateCardField(cand.sym.DocSummary, maxContextDocChars),
		Relevance:     cand.relevance,
		QualifiedName: cand.sym.QualifiedName,
		SymbolID:      cand.sym.ID,
		StableKey:     cand.sym.StableKey,
	}
}
