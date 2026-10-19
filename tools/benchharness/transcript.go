package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

type usage struct {
	Input         int `json:"input_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	Output        int `json:"output_tokens"`
}

func (u usage) context() int { return u.Input + u.CacheRead + u.CacheCreation }

type rawEvent struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Timestamp   string `json:"timestamp"`
	Version     string `json:"version"`
	Message     *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *usage          `json:"usage"`
	} `json:"message"`
}

type rawBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type toolUse struct {
	ID        string
	Name      string
	Input     json.RawMessage
	Sidechain bool
}

type toolResult struct {
	IsError bool
	Text    string
}

type request struct {
	usage
	sidechain bool
	// gap is the main-session content written between the previous main
	// request and this one; reset marks a compaction boundary inside it.
	gap     []segment
	reset   bool
	text    []string
	toolUse bool
	blocks  map[string]bool
}

// Transcript is the parsed content of one Claude Code JSONL session file.
type Transcript struct {
	requests    []*request
	mainModels  map[string]bool
	sideModels  map[string]bool
	versions    map[string]bool
	toolUses    []toolUse
	results     map[string]toolResult
	compactions int

	pending      []segment
	pendingReset bool
	toolCats     map[string]string
	lastMain     *request
	userAfter    bool
	first, last  time.Time
	timestamps   int
}

// ParseTranscript reads a Claude Code JSONL transcript. Claude Code writes one
// assistant event per content block, each repeating the request's message.id
// and usage, so requests are deduplicated by message.id. Input and cache
// fields must agree across those events; output_tokens is a streamed snapshot
// and the largest value seen for an id is kept.
func ParseTranscript(r io.Reader) (*Transcript, error) {
	t := &Transcript{mainModels: map[string]bool{}, sideModels: map[string]bool{}, versions: map[string]bool{}, results: map[string]toolResult{}, toolCats: map[string]string{}}
	byID := map[string]*request{}
	seenTool := map[string]bool{}
	br := bufio.NewReader(r)
	for line := 1; ; line++ {
		b, readErr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(b)) > 0 {
			if err := t.addEvent(b, byID, seenTool); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	if len(t.mainModels) == 0 {
		return nil, errors.New("transcript has no main-session model request")
	}
	return t, nil
}

func (t *Transcript) addEvent(b []byte, byID map[string]*request, seenTool map[string]bool) error {
	var ev rawEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		return err
	}
	if ev.Version != "" {
		t.versions[ev.Version] = true
	}
	if !ev.IsSidechain && ev.Timestamp != "" {
		ts, err := time.Parse(time.RFC3339Nano, ev.Timestamp)
		if err != nil {
			return err
		}
		if t.timestamps == 0 || ts.Before(t.first) {
			t.first = ts
		}
		if t.timestamps == 0 || ts.After(t.last) {
			t.last = ts
		}
		t.timestamps++
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "compact_boundary" && !ev.IsSidechain {
			t.compactions++
			t.pending, t.pendingReset = nil, true
		}
	case "assistant":
		m := ev.Message
		if m == nil {
			return errors.New("assistant event without message")
		}
		// Claude Code writes "<synthetic>" messages locally (API errors,
		// interrupts); no model request was made.
		if m.Model == "<synthetic>" {
			return nil
		}
		if m.ID == "" || m.Usage == nil || m.Model == "" {
			return errors.New("assistant message without id, model or usage")
		}
		req, ok := byID[m.ID]
		if ok {
			if req.Input != m.Usage.Input || req.CacheRead != m.Usage.CacheRead || req.CacheCreation != m.Usage.CacheCreation {
				return fmt.Errorf("message %s: conflicting usage across events", m.ID)
			}
			if req.sidechain != ev.IsSidechain {
				return fmt.Errorf("message %s: appears in both main session and sidechain", m.ID)
			}
			req.Output = max(req.Output, m.Usage.Output)
		} else {
			req = &request{usage: *m.Usage, sidechain: ev.IsSidechain, blocks: map[string]bool{}}
			byID[m.ID] = req
			t.requests = append(t.requests, req)
			if !req.sidechain {
				req.gap, req.reset = t.pending, t.pendingReset
				t.pending, t.pendingReset = nil, false
				t.lastMain, t.userAfter = req, false
			}
		}
		if ev.IsSidechain {
			t.sideModels[m.Model] = true
		} else {
			t.mainModels[m.Model] = true
		}
		for _, bl := range blocks(m.Content) {
			// Events of one message.id may repeat a text or thinking block.
			if bl.Type == "text" || bl.Type == "thinking" {
				key := bl.Type + "\x00" + bl.Text + "\x00" + bl.Thinking
				if req.blocks[key] {
					continue
				}
				req.blocks[key] = true
			}
			switch {
			case bl.Type == "tool_use" && !seenTool[bl.ID]:
				seenTool[bl.ID] = true
				u := toolUse{ID: bl.ID, Name: bl.Name, Input: bl.Input, Sidechain: ev.IsSidechain}
				t.toolUses = append(t.toolUses, u)
				// An empty category marks a sidechain tool whose result is not
				// main-session context.
				t.toolCats[u.ID] = ""
				if !ev.IsSidechain {
					req.toolUse = true
					t.toolCats[u.ID] = residualCategory(u)
					t.addSegment(catAssistant, runes(bl.Name)+runes(string(bl.Input)), false)
				}
			case ev.IsSidechain:
			case bl.Type == "text":
				req.text = append(req.text, bl.Text)
				t.addSegment(catAssistant, runes(bl.Text), false)
			case bl.Type == "thinking":
				t.addSegment(catAssistant, runes(bl.Thinking), false)
			}
		}
	case "user":
		if ev.Message == nil {
			return nil
		}
		var prompt string
		var texts []string
		if json.Unmarshal(ev.Message.Content, &prompt) == nil {
			texts = append(texts, prompt)
			if !ev.IsSidechain {
				t.addSegment(catUserText, runes(prompt), false)
			}
		}
		nonText := false
		for _, bl := range blocks(ev.Message.Content) {
			switch {
			case bl.Type == "tool_result":
				nonText = true
				res := toolResult{IsError: bl.IsError, Text: contentText(bl.Content)}
				t.results[bl.ToolUseID] = res
				cat, ok := t.toolCats[bl.ToolUseID]
				if !ok {
					cat = catOtherTool
				}
				if !ev.IsSidechain && cat != "" {
					t.addSegment(cat, runes(res.Text), true)
				}
			case bl.Type == "text":
				texts = append(texts, bl.Text)
				if !ev.IsSidechain {
					t.addSegment(catUserText, runes(bl.Text), false)
				}
			default:
				nonText = true
			}
		}
		if !ev.IsSidechain && t.lastMain != nil && !ev.IsMeta && (nonText || !localCommandOnly(texts)) {
			t.userAfter = true
		}
	}
	return nil
}

var localCommandPrefixes = []string{"<command-name>", "<command-message>", "<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>"}

// localCommandOnly reports user content made only of Claude Code's local
// slash-command wrappers (such as /exit), which the model never answers.
func localCommandOnly(texts []string) bool {
	found := false
	for _, s := range texts {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		local := false
		for _, p := range localCommandPrefixes {
			local = local || strings.HasPrefix(s, p)
		}
		if !local {
			return false
		}
		found = true
	}
	return found
}

// blocks decodes a content array; a plain string content (a typed prompt) has
// no blocks.
func blocks(raw json.RawMessage) []rawBlock {
	var out []rawBlock
	if len(raw) > 0 && raw[0] == '[' {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.Text
	}
	return strings.Join(texts, "\n")
}

// SessionMetrics are the per-session numbers derived from one transcript.
type SessionMetrics struct {
	Models          []string       `json:"models"`
	ClientVersions  []string       `json:"client_versions"`
	Requests        int            `json:"requests"`
	InputTokens     int            `json:"input_tokens"`
	CacheReadTokens int            `json:"cache_read_input_tokens"`
	CacheCreation   int            `json:"cache_creation_input_tokens"`
	OutputTokens    int            `json:"output_tokens"`
	ProcessedTokens int            `json:"processed_tokens"`
	FinalContext    int            `json:"final_context_tokens"`
	PeakContext     int            `json:"peak_context_tokens"`
	Compactions     int            `json:"compactions"`
	WallClockMS     *int           `json:"wall_clock_ms"`
	ToolCalls       map[string]int `json:"tool_calls"`
	ToolCallsTotal  int            `json:"tool_calls_total"`
	Sidechain       SidechainStats `json:"sidechain"`
}

// SidechainStats covers subagent events found inside the main transcript;
// they are never added to the main-session totals.
type SidechainStats struct {
	Models          []string `json:"models"`
	Requests        int      `json:"requests"`
	ProcessedTokens int      `json:"processed_tokens"`
	OutputTokens    int      `json:"output_tokens"`
	ToolCalls       int      `json:"tool_calls"`
}

func (t *Transcript) Metrics() SessionMetrics {
	s := SessionMetrics{
		Models:         sortedKeys(t.mainModels),
		ClientVersions: sortedKeys(t.versions),
		ToolCalls:      map[string]int{},
		Compactions:    t.compactions,
		Sidechain:      SidechainStats{Models: sortedKeys(t.sideModels)},
	}
	for _, r := range t.requests {
		if r.sidechain {
			s.Sidechain.Requests++
			s.Sidechain.ProcessedTokens += r.context()
			s.Sidechain.OutputTokens += r.Output
			continue
		}
		s.Requests++
		s.InputTokens += r.Input
		s.CacheReadTokens += r.CacheRead
		s.CacheCreation += r.CacheCreation
		s.OutputTokens += r.Output
		s.ProcessedTokens += r.context()
		s.FinalContext = r.context()
		s.PeakContext = max(s.PeakContext, r.context())
	}
	for _, u := range t.toolUses {
		if u.Sidechain {
			s.Sidechain.ToolCalls++
			continue
		}
		s.ToolCalls[u.Name]++
		s.ToolCallsTotal++
	}
	if t.timestamps >= 2 {
		ms := int(t.last.Sub(t.first).Milliseconds())
		s.WallClockMS = &ms
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// deniedRE matches the exact tool_result shapes of a PreToolUse hook denial
// or a permission denial.
var deniedRE = regexp.MustCompile(`^(?:Hook )?PreToolUse:\S+ (?:hook error|denied)` +
	`|^Permission to use ` +
	`|^The user doesn't want to proceed`)

// notFoundLineRE matches one shell line reporting that the codegraph binary
// itself was not found (bash, dash and zsh shapes).
var notFoundLineRE = regexp.MustCompile(`^(?:\S+: )?(?:(?:line )?\d+: )?(?:\S*[/\\])?codegraph(?:\.exe)?: (?:command )?not found$` +
	`|^(?:\S+: )?command not found: (?:\S*[/\\])?codegraph(?:\.exe)?$`)

var exitCodeLineRE = regexp.MustCompile(`^Exit code \d+$`)

// blockedResult reports a hook or permission denial, or a result made only of
// codegraph-not-found and exit-code lines. Any other error counts as
// contaminating because codegraph may have run and printed output.
func blockedResult(text string) bool {
	text = strings.TrimSpace(text)
	if deniedRE.MatchString(text) {
		return true
	}
	notFound := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || exitCodeLineRE.MatchString(line):
		case notFoundLineRE.MatchString(line):
			notFound = true
		default:
			return false
		}
	}
	return notFound
}

// ContaminationCall is one CodeGraph use attempt in an arm that must not use it.
type ContaminationCall struct {
	ToolUseID string `json:"tool_use_id"`
	Tool      string `json:"tool"`
	Kind      string `json:"kind"`
	Detail    string `json:"detail,omitempty"`
	Sidechain bool   `json:"sidechain"`
	Outcome   string `json:"outcome"`
}

// Contamination reports CodeGraph use in a without-CodeGraph arm. A session is
// contaminated when any attempt was not blocked (succeeded, failed inside
// codegraph, has no result to prove it was blocked, or ran in a subagent
// whose transcript was not checked).
type Contamination struct {
	Checked         bool                `json:"checked"`
	Contaminated    bool                `json:"contaminated"`
	BlockedAttempts int                 `json:"blocked_attempts"`
	Calls           []ContaminationCall `json:"calls"`
}

func classifyUse(u toolUse) (kind, detail string) {
	switch {
	case strings.HasPrefix(u.Name, "mcp__"):
		server, _, _ := strings.Cut(strings.TrimPrefix(u.Name, "mcp__"), "__")
		if strings.Contains(strings.ToLower(server), "codegraph") {
			return "mcp", ""
		}
	case u.Name == "Skill":
		if strings.Contains(strings.ToLower(string(u.Input)), "codegraph") {
			return "skill", string(u.Input)
		}
	case u.Name == "Bash":
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(u.Input, &in)
		if runsCodegraph(in.Command) {
			return "cli", in.Command
		}
	case u.Name == "Task" || u.Name == "Agent":
		// Subagent tool calls live in separate transcript files that are not
		// ingested, so their CodeGraph use cannot be ruled out.
		return "subagent", ""
	}
	return "", ""
}

func (t *Transcript) Contamination() Contamination {
	c := Contamination{Checked: true, Calls: []ContaminationCall{}}
	for _, u := range t.toolUses {
		kind, detail := classifyUse(u)
		if kind == "" {
			continue
		}
		call := ContaminationCall{ToolUseID: u.ID, Tool: u.Name, Kind: kind, Detail: detail, Sidechain: u.Sidechain, Outcome: callOutcome(kind, t.results, u.ID)}
		c.Calls = append(c.Calls, call)
	}
	c.Contaminated, c.BlockedAttempts = contaminationTotals(c.Calls)
	return c
}

func callOutcome(kind string, results map[string]toolResult, id string) string {
	if kind == "subagent" {
		return "unverified"
	}
	res, ok := results[id]
	switch {
	case !ok:
		return "no_result"
	case !res.IsError:
		return "succeeded"
	case blockedResult(res.Text):
		return "blocked"
	}
	return "error"
}

func contaminationTotals(calls []ContaminationCall) (contaminated bool, blocked int) {
	for _, c := range calls {
		if c.Outcome == "blocked" {
			blocked++
		} else {
			contaminated = true
		}
	}
	return contaminated, blocked
}
