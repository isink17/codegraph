// Package detail implements CodeGraph's progressive-disclosure model.
//
// The symbol/traversal tools that dominate an agent's token budget --
// find_symbol, search_symbols, find_callers, find_callees, get_impact_radius --
// render every symbol they return through one projection with four levels of
// increasing cost. The tools whose records are already card-sized
// (search_semantic, find_related_tests, find_dead_code, trace_dependencies,
// context_for_task) keep their own shapes; projecting them could only make a
// response larger.
//
// The four levels are:
//
//	card     -- identity and location only; enough to choose and to drill down
//	skeleton -- declaration shape: signature, container, visibility, members
//	excerpt  -- bounded source around the symbol
//	full     -- every field the pre-P13 response carried, plus complete source
//
// The levels are cumulative: each one is a superset of the one before it, and a
// field means the same thing at every level and in every tool. Callers pick a
// level; they never pick a field set.
package detail

import (
	"fmt"
	"strings"
)

// Level names one step of the progressive-disclosure ladder.
type Level string

const (
	Card     Level = "card"
	Skeleton Level = "skeleton"
	Excerpt  Level = "excerpt"
	Full     Level = "full"
)

// Levels lists every valid level from cheapest to most expensive. It is the
// single source of truth for MCP tool schemas and CLI flag help, so the wire
// vocabulary cannot drift from the code.
var Levels = []Level{Card, Skeleton, Excerpt, Full}

var levelRank = map[Level]int{Card: 0, Skeleton: 1, Excerpt: 2, Full: 3}

// Valid reports whether l is one of the four defined levels.
func (l Level) Valid() bool {
	_, ok := levelRank[l]
	return ok
}

// AtLeast reports whether l includes everything other includes. Projection code
// asks this rather than switching on individual levels, so adding a level later
// does not require revisiting every field.
func (l Level) AtLeast(other Level) bool {
	return levelRank[l] >= levelRank[other]
}

func (l Level) String() string { return string(l) }

// Names returns the level vocabulary as strings, for schemas and flag help.
func Names() []string {
	out := make([]string, 0, len(Levels))
	for _, l := range Levels {
		out = append(out, string(l))
	}
	return out
}

// Parse resolves a wire value to a level. An empty or whitespace-only value
// selects fallback, which is how an omitted `detail` argument becomes the
// tool's default. Anything else that is not a defined level is an error: a
// misspelled level must not silently downgrade or upgrade a response.
func Parse(raw string, fallback Level) (Level, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback, nil
	}
	candidate := Level(trimmed)
	if candidate.Valid() {
		return candidate, nil
	}
	return "", fmt.Errorf("invalid detail %q: want one of %s", raw, strings.Join(Names(), ", "))
}
