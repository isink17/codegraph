package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Gateway mode trades tool-definition context for one level of indirection.
//
// A gateway client is advertised a small core -- the tools an ordinary
// navigation session uses on nearly every task -- plus two meta tools:
// tool_search, which finds a specialized tool by name or keyword, and tool_call,
// which invokes it. No capability is removed; the cost of describing 25 tools an
// agent may never call is simply not charged to every session up front.
//
// The two meta tools are the only new behavior in this file. Everything they
// reach is the same canonical registry entry, the same validator, the same
// handler, and the same serializer a full-mode client reaches directly.

const (
	// defaultToolSearchLimit keeps a discovery result card-sized. An agent looking
	// for one tool does not need twenty candidates, and tool_search's own output is
	// charged to the budget gateway mode exists to protect.
	defaultToolSearchLimit = 5
	// maxToolSearchLimit is a hard ceiling. This is a new API, so it is bounded at
	// the source rather than inheriting an unbounded limit convention.
	maxToolSearchLimit = 20
)

// callToolText produces the model-visible text of one `tools/call`.
//
// It is the mode-aware layer above callToolContent: the gateway meta tools are
// answered here, and every other name falls through to the one canonical path.
// Because gatewayToolCall re-enters at callToolContent -- below this function --
// a meta tool cannot reach a meta tool. Recursion is not rejected at runtime; it
// is unreachable.
func (s *Server) callToolText(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if desc, ok := toolByName[name]; ok && desc.gatewayMeta {
		if s.ToolMode() != ToolModeGateway {
			// In full mode these tools do not exist. Saying so with the same message
			// any other unknown name gets keeps the full surface exactly as it was.
			return "", fmt.Errorf("unknown tool %q", name)
		}
		if err := validateToolArguments(name, raw); err != nil {
			return "", err
		}
		switch name {
		case "tool_search":
			return s.gatewayToolSearch(raw)
		case "tool_call":
			return s.gatewayToolCall(ctx, raw)
		}
	}
	return s.callToolContent(ctx, name, raw)
}

// gatewayToolCall invokes a canonical tool on behalf of a gateway client.
//
// The target's arguments are forwarded as the raw bytes they arrived as, and the
// target's model-visible text is returned unchanged. There is one validation, one
// dispatch, and one serialization -- the same three the direct call performs -- so
// a compact document is not re-encoded, a budgeted context is not re-budgeted, and
// no result is wrapped in a JSON string inside JSON.
func (s *Server) gatewayToolCall(ctx context.Context, raw json.RawMessage) (string, error) {
	// Marked before the target is resolved: a gateway call that reaches no
	// capability is still a gateway call, and stays charged to tool_call because
	// tool_call is the only thing that ran.
	callMeterFrom(ctx).markGateway()
	var req struct {
		Name string `json:"name"`
		// Raw so the target's own decoder sees the caller's bytes. Decoding to a map
		// and re-encoding here would add a parse, an allocation, and a chance to
		// change the payload.
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("invalid arguments payload: %w", err)
	}
	name := strings.TrimSpace(req.Name)
	if _, ok := dispatchableTool(name); !ok {
		// Covers an unknown name and the meta tools themselves, which have no
		// handler. The message is the one an unknown direct call produces.
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return s.callToolContent(ctx, name, req.Arguments)
}

// toolSearchResult is one discovery match. It is card-sized on purpose: an agent
// choosing between candidates needs identity and intent, not a schema per row.
type toolSearchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category,omitempty"`
	// Listed reports that this tool is already in the gateway `tools/list`.
	//
	// It says where the tool is advertised, not whether it can be called: gateway
	// mode reduces the advertisement, not the surface, so a tool with listed=false
	// is still callable by name -- tool_call is the discoverable route to it, not
	// the only one.
	Listed bool `json:"listed"`
	// Mutates reports a tool that writes to the graph or to session state, so an
	// agent is not surprised by a write it did not intend.
	Mutates     bool           `json:"mutates,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// gatewayToolSearch answers tool_search: a deterministic, bounded, local name and
// description search over the canonical registry. No index, no embeddings, no
// network -- the corpus is every non-meta registry entry, name and one-line
// description.
func (s *Server) gatewayToolSearch(raw json.RawMessage) (string, error) {
	var req struct {
		Query         string `json:"query"`
		Limit         int    `json:"limit"`
		IncludeSchema bool   `json:"include_schema"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("invalid arguments payload: %w", err)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultToolSearchLimit
	}
	if limit > maxToolSearchLimit {
		limit = maxToolSearchLimit
	}

	query, directive := splitQueryDirective(strings.ToLower(strings.TrimSpace(req.Query)))
	notes := []string{}
	if directive != "" {
		// What happened to the rest of the query depends on which branch answers it,
		// so the note says only what is true of every branch: the prefix was not a
		// directive, and here is what a query may be instead.
		notes = append(notes, "tool_search has no "+directive+": directive; it takes a tool name, a comma-separated list of names, or plain keywords.")
	}

	// A query that is nothing but tool names is answered as a lookup rather than a
	// ranking. An agent that already knows what it wants -- including by the
	// client-qualified mcp__<server>__<tool> spelling -- gets exactly those tools
	// in the order asked for, and never a neighbour that merely shares a word.
	if named, ok := exactToolNames(query); ok {
		total := len(named)
		if len(named) > limit {
			named = named[:limit]
		}
		results := make([]toolSearchResult, 0, len(named))
		for _, desc := range named {
			results = append(results, searchResult(desc, req.IncludeSchema && total == 1))
		}
		if req.IncludeSchema && total != 1 {
			notes = append(notes, "include_schema needs a single exact tool name; call tool_search again with one of the names above")
		}
		return marshalToolSearch(results, total, limit, strings.Join(notes, " "))
	}

	// An exact name is the one query that can be answered with a schema: it names
	// a single tool, so there is no ambiguity about whose arguments to return.
	if req.IncludeSchema {
		if desc, ok := toolByName[query]; ok && !desc.gatewayMeta {
			return marshalToolSearch([]toolSearchResult{searchResult(desc, true)}, 1, limit, strings.Join(notes, " "))
		}
	}

	terms := searchTerms(query)
	type scored struct {
		desc *toolDescriptor
		// demoted marks a hidden diagnostic on a query that is not its exact name.
		// It orders below every real match instead of scoring below one: a penalty
		// applied to the score would push weak matches under the score > 0 filter
		// and make the tool undiscoverable rather than merely last.
		demoted bool
		score   int
		order   int
	}
	matches := make([]scored, 0, len(toolRegistry))
	identified := false
	for i := range toolRegistry {
		desc := &toolRegistry[i]
		if desc.gatewayMeta {
			// The meta tools are already in tools/list; returning them as discoveries
			// would spend context restating what the client can already see.
			continue
		}
		score, named := toolSearchScore(desc, query, terms)
		if score > 0 {
			identified = identified || named
			matches = append(matches, scored{
				desc:    desc,
				demoted: desc.diagnostic && query != desc.name,
				score:   score,
				order:   i,
			})
		}
	}
	// Real tools first, then score, then registry order: the same query always
	// returns the same list in the same order, and a diagnostic never displaces a
	// capability an agent could actually navigate with.
	sort.SliceStable(matches, func(a, b int) bool {
		if matches[a].demoted != matches[b].demoted {
			return !matches[a].demoted
		}
		if matches[a].score != matches[b].score {
			return matches[a].score > matches[b].score
		}
		return matches[a].order < matches[b].order
	})

	total := len(matches)
	if len(matches) > limit {
		matches = matches[:limit]
	}
	results := make([]toolSearchResult, 0, len(matches))
	for _, m := range matches {
		results = append(results, searchResult(m.desc, false))
	}
	if req.IncludeSchema {
		notes = append(notes, "include_schema needs an exact tool name; call tool_search again with one of the names above")
	}
	if total > 0 && !identified {
		// No term reached any tool's name or category, so the whole ranking rests on
		// words that happen to appear in prose. The list is still returned, because a
		// weak match is better evidence than silence, but it is labelled so it is not
		// read as a recommendation. The condition is the claim the note makes, rather
		// than a threshold on a score that sums evidence of different kinds.
		notes = append(notes, "no tool matched this query by name or category; these are weak description-word matches and may all be irrelevant.")
	}
	return marshalToolSearch(results, total, limit, strings.Join(notes, " "))
}

func searchResult(desc *toolDescriptor, withSchema bool) toolSearchResult {
	out := toolSearchResult{
		Name:        desc.name,
		Description: desc.description,
		Category:    desc.category,
		Listed:      desc.gatewayCore,
		Mutates:     desc.mutates,
	}
	if withSchema {
		// The canonical schema, not a gateway paraphrase of it: an agent that reads
		// this and then calls tool_call is reading the contract the validator
		// enforces.
		def := desc.definition()
		if schema, ok := def["inputSchema"].(map[string]any); ok {
			out.InputSchema = schema
		}
	}
	return out
}

func marshalToolSearch(results []toolSearchResult, total, limit int, note string) (string, error) {
	data := map[string]any{
		"tools":         results,
		"total_matches": total,
		"limit":         limit,
	}
	if note != "" {
		data["note"] = note
	}
	payload, err := json.Marshal(map[string]any{"ok": true, "data": data})
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

// splitQueryDirective strips a leading `word:` directive from a query.
//
// tool_search has no query language, and an agent carrying a convention from
// another tool ("select:a,b") used to have its directive silently tokenized into
// the search. The prefix is removed so the names behind it can still be resolved,
// and the caller says so in a note rather than answering as though the syntax
// were understood. A colon inside the query proper is left alone: only a single
// leading word followed immediately by a colon and a non-space counts.
//
// The space is what separates a borrowed directive from ordinary prose. A
// directive is written closed up ("select:a,b"); a sentence that opens with a
// word and a colon ("terraform: the blocks that reference it") is a query whose
// first word is a search term -- and two registry descriptions contain exactly
// that word, so stripping it would lose a real match and claim a syntax error
// the caller did not make.
func splitQueryDirective(query string) (rest, directive string) {
	idx := strings.Index(query, ":")
	if idx <= 0 || idx+1 >= len(query) || query[idx+1] == ' ' || query[idx+1] == '\t' {
		return query, ""
	}
	word := query[:idx]
	for _, r := range word {
		if !(r >= 'a' && r <= 'z') {
			return query, ""
		}
	}
	return query[idx+1:], word
}

// exactToolNames resolves a query that is nothing but tool names.
//
// Every token must name a real non-meta tool, so one unknown word makes the
// whole query ordinary keywords again and it is ranked as before. Names may be
// client-qualified (mcp__codegraph__find_symbol), because that is the spelling an
// agent reads in its own tool list. Order is the caller's, duplicates collapse.
func exactToolNames(query string) ([]*toolDescriptor, bool) {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || r == ','
	})
	if len(fields) == 0 {
		return nil, false
	}
	out := make([]*toolDescriptor, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		desc, ok := toolByName[unqualifyToolName(field)]
		if !ok || desc.gatewayMeta {
			return nil, false
		}
		if seen[desc.name] {
			continue
		}
		seen[desc.name] = true
		out = append(out, desc)
	}
	return out, true
}

// unqualifyToolName drops an MCP client's server prefix. No canonical tool name
// contains a double underscore, so the last one is the end of the prefix.
func unqualifyToolName(name string) string {
	if idx := strings.LastIndex(name, "__"); idx >= 0 {
		return name[idx+2:]
	}
	return name
}

// searchTerms splits a query into distinct lowercase terms. Underscores split
// too, so "find related tests" and "find_related_tests" are the same query.
//
// Terms are deduplicated because the score is a sum: a query naming several
// tools of one family repeats their shared word, and an unrelated tool holding
// that word once would collect the repeat as many times as it appears. Counting
// a word once makes the score measure which words the query used, not how often.
func searchTerms(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return r == ' ' || r == '_' || r == '-' || r == '\t' || r == '.' || r == ','
	})
	terms := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		terms = append(terms, field)
	}
	return terms
}

// toolSearchScore ranks one tool against a query.
//
// The weights encode a simple order of evidence: the whole name is the strongest
// signal, then a name segment, then a category, then the description. The
// description carries real weight because these descriptions are the only prose
// in the registry -- "dependency trace" finds trace_dependencies through its
// description even though no name segment of it is the word "dependency".
//
// identified reports the kind of evidence rather than its amount: true when some
// term reached this tool's name or category, false when the whole score came
// from words appearing in its description. A caller cannot recover that from the
// number, because the number adds the two together.
func toolSearchScore(desc *toolDescriptor, query string, terms []string) (score int, identified bool) {
	if query != "" && query == desc.name {
		score += 100
		identified = true
	}
	segments := strings.Split(desc.name, "_")
	lowerDesc := strings.ToLower(desc.description)
	for _, term := range terms {
		for _, segment := range segments {
			switch {
			case segment == term:
				score += 40
				identified = true
			case sharesPrefix(segment, term):
				score += 30
				identified = true
			}
		}
		if term == desc.category {
			score += 20
			identified = true
		}
		if strings.Contains(lowerDesc, term) {
			score += 10
		}
	}
	return score, identified
}

// sharesPrefix reports whether one word is a prefix of the other, which is the
// cheapest stand-in for stemming that does not need a word list: "repo" matches
// "repository", "test" matches "tests". Four characters is the floor, so a
// two-letter fragment does not match half the registry.
func sharesPrefix(a, b string) bool {
	if a == b || len(a) < 4 || len(b) < 4 {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
