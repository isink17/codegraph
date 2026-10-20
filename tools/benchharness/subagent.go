package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// SubagentRef declares one subagent transcript recorded for a session.
type SubagentRef struct {
	Transcript       string `json:"transcript"`
	TranscriptSHA256 string `json:"transcript_sha256"`
}

// SubagentResult is one subagent transcript file of a session. Parent is
// "main" or the spawning subagent's id; it is empty (depth 0) when no
// Task/Agent result in the session names this agent.
type SubagentResult struct {
	AgentID          string `json:"agent_id"`
	Transcript       string `json:"transcript"`
	TranscriptSHA256 string `json:"transcript_sha256"`
	Parent           string `json:"parent,omitempty"`
	ToolUseID        string `json:"tool_use_id,omitempty"`
	Depth            int    `json:"depth"`
	// DuplicateRequests counts requests whose message.id another file owns.
	DuplicateRequests int `json:"duplicate_requests"`
	SessionMetrics
}

// Workload sums processed tokens across the agents of one session. Context
// sizes are per agent and are never summed.
type Workload struct {
	MainProcessed      int `json:"main_processed_tokens"`
	SubagentProcessed  int `json:"subagent_processed_tokens"`
	SidechainProcessed int `json:"in_file_sidechain_processed_tokens"`
	TotalProcessed     int `json:"total_processed_tokens"`
	LinkedSubagents    int `json:"linked_subagents"`
	UnlinkedSubagents  int `json:"unlinked_subagents"`
	UnlinkedAgentCalls int `json:"unlinked_agent_calls"`
}

var agentFileRE = regexp.MustCompile(`^agent-([A-Za-z0-9_-]+)\.jsonl$`)

func subagentDir(transcript string) string {
	return strings.TrimSuffix(transcript, ".jsonl") + "/subagents"
}

// discoverSubagents lists the agent-*.jsonl files next to a session
// transcript (Claude Code's <session>/subagents/ layout), sorted by name.
func discoverSubagents(dir, transcript string) ([]string, error) {
	sub := subagentDir(transcript)
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(sub)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "agent-") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		if !agentFileRE.MatchString(n) {
			return nil, fmt.Errorf("%s/%s: agent id has characters outside [A-Za-z0-9_-]", sub, n)
		}
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("%s/%s is not a regular file", sub, e.Name())
		}
		out = append(out, sub+"/"+e.Name())
	}
	return out, nil
}

type agentFile struct {
	res  SubagentResult
	t    *Transcript
	done bool
}

// loadSubagents reads every discovered subagent file and, when the manifest
// declares the session's subagents, requires the declared list to match the
// discovered files and hashes exactly.
func loadSubagents(dir string, ref SessionRef, m *Manifest) ([]*agentFile, error) {
	paths, err := discoverSubagents(dir, ref.Transcript)
	if err != nil {
		return nil, err
	}
	var declared map[string]string
	if ref.Subagents != nil {
		declared = map[string]string{}
		for _, s := range *ref.Subagents {
			declared[s.Transcript] = s.TranscriptSHA256
		}
	}
	var errs []error
	var out []*agentFile
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		sha := sha256Hex(b)
		if declared != nil {
			want, ok := declared[p]
			delete(declared, p)
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("subagent transcript %s is not listed in the manifest", p))
				continue
			case want != sha:
				errs = append(errs, fmt.Errorf("subagent transcript %s sha256 %s, manifest says %s", p, sha, want))
				continue
			}
		}
		id := agentFileRE.FindStringSubmatch(p[strings.LastIndexByte(p, '/')+1:])[1]
		t, err := parseTranscript(bytes.NewReader(b), true)
		if err != nil {
			errs = append(errs, fmt.Errorf("subagent transcript %s: %w", p, err))
			continue
		}
		for a := range t.agentIDs {
			if a != id {
				errs = append(errs, fmt.Errorf("subagent transcript %s: events carry agentId %q", p, a))
			}
		}
		for v := range t.versions {
			if v != m.Client.Version {
				errs = append(errs, fmt.Errorf("subagent transcript %s: client version %q, manifest version %q", p, v, m.Client.Version))
			}
		}
		out = append(out, &agentFile{res: SubagentResult{AgentID: id, Transcript: p, TranscriptSHA256: sha}, t: t})
	}
	for _, p := range slices.Sorted(maps.Keys(declared)) {
		errs = append(errs, fmt.Errorf("subagent transcript %s is listed in the manifest but missing", p))
	}
	return out, errors.Join(errs...)
}

// linkSubagents walks Task/Agent calls breadth-first from the main session,
// links each call to the subagent file its result names, and assigns every
// message.id to one file: the main thread, then subagents in spawn order
// (so a fork's copy of its parent's history counts once, under the parent),
// then unlinked files by id, then sidechain events inside the main file. It
// returns the Task/Agent tool_use ids linked to a file and the count of calls
// that are not. Linking reads every copy of a call, including copies on
// requests another file owns. threads lists the session's agents in that
// owner order for contamination checks, which run only when contamination is
// set (a without-CodeGraph arm).
func linkSubagents(main *Transcript, subs []*agentFile, contamination bool) (checked map[string]bool, unlinked int, threads []thread, err error) {
	byID := map[string]*agentFile{}
	for _, s := range subs {
		byID[s.res.AgentID] = s
	}
	if err := checkCopies(append([]*Transcript{main}, agentTranscripts(subs)...), contamination); err != nil {
		return nil, 0, nil, err
	}
	owned := map[string]*request{}
	claim := func(t *Transcript, sidechain bool) (dups int, err error) {
		for _, r := range t.requests {
			if r.sidechain != sidechain {
				continue
			}
			o, ok := owned[r.id]
			if !ok {
				owned[r.id] = r
				continue
			}
			if o.Input != r.Input || o.CacheRead != r.CacheRead || o.CacheCreation != r.CacheCreation {
				return 0, fmt.Errorf("message %s: conflicting usage across transcript files", r.id)
			}
			o.Output = max(o.Output, r.Output)
			r.dup = true
			dups++
		}
		return dups, nil
	}
	if _, err := claim(main, false); err != nil {
		return nil, 0, nil, err
	}
	threads = []thread{{"main", main, false}}
	checked = map[string]bool{}
	type node struct {
		name  string
		t     *Transcript
		depth int
	}
	queue := []node{{"main", main, 0}}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, u := range n.t.toolUses {
			if (u.Name != "Task" && u.Name != "Agent") || u.Sidechain {
				continue
			}
			child := byID[n.t.links[u.ID]]
			if child == nil {
				continue
			}
			if child.done {
				// A fork's copy of the call that already linked this child.
				if child.res.ToolUseID == u.ID {
					continue
				}
				return nil, 0, nil, fmt.Errorf("subagent %s is named by more than one Task/Agent result", child.res.AgentID)
			}
			child.done = true
			child.res.Parent, child.res.ToolUseID, child.res.Depth = n.name, u.ID, n.depth+1
			if child.res.DuplicateRequests, err = claim(child.t, false); err != nil {
				return nil, 0, nil, err
			}
			checked[u.ID] = true
			threads = append(threads, thread{child.res.AgentID, child.t, false})
			queue = append(queue, node{child.res.AgentID, child.t, n.depth + 1})
		}
	}
	for _, s := range subs {
		if !s.done {
			if s.res.DuplicateRequests, err = claim(s.t, false); err != nil {
				return nil, 0, nil, err
			}
			threads = append(threads, thread{s.res.AgentID, s.t, false})
		}
	}
	if _, err := claim(main, true); err != nil {
		return nil, 0, nil, err
	}
	threads = append(threads, thread{"sidechain", main, true})
	calls := map[string]bool{}
	for _, t := range append([]*Transcript{main}, agentTranscripts(subs)...) {
		for _, u := range t.toolUses {
			if (u.Name == "Task" || u.Name == "Agent") && !checked[u.ID] {
				calls[u.ID] = true
			}
		}
	}
	return checked, len(calls), threads, nil
}

// checkCopies fails when copies of one tool_use id name different spawned
// agents, so linking never picks one copy. With contamination set it also
// fails when copies carry different results for a call contamination would
// classify, so the outcome is never picked either; an arm with CodeGraph
// enabled never reads those outcomes.
func checkCopies(ts []*Transcript, contamination bool) error {
	attempts := map[string]bool{}
	for _, t := range ts {
		for _, u := range t.toolUses {
			if kind, _ := classifyUse(u); contamination && kind != "" && kind != "subagent" {
				attempts[u.ID] = true
			}
		}
	}
	links := map[string]string{}
	results := map[string]toolResult{}
	for _, t := range ts {
		for _, id := range slices.Sorted(maps.Keys(t.links)) {
			a := t.links[id]
			if prev, ok := links[id]; ok && prev != a {
				return fmt.Errorf("tool_use %s: copies name agents %q and %q", id, prev, a)
			}
			links[id] = a
		}
		for _, id := range slices.Sorted(maps.Keys(t.results)) {
			if !attempts[id] {
				continue
			}
			r := t.results[id]
			if t.conflicts[id] {
				return fmt.Errorf("tool_use %s: copies within one transcript file carry conflicting tool results", id)
			}
			if prev, ok := results[id]; ok && prev != r {
				return fmt.Errorf("tool_use %s: copies across transcript files carry conflicting tool results (%s and %s)", id, resultOutcome(prev), resultOutcome(r))
			}
			results[id] = r
		}
	}
	return nil
}

func agentTranscripts(subs []*agentFile) []*Transcript {
	out := make([]*Transcript, len(subs))
	for i, s := range subs {
		out[i] = s.t
	}
	return out
}

// sessionAgents parses, links and measures a session's subagent files.
func sessionAgents(dir string, ref SessionRef, m *Manifest, main *Transcript, contamination bool) ([]*agentFile, map[string]bool, int, []thread, error) {
	subs, err := loadSubagents(dir, ref, m)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	checked, unlinked, threads, err := linkSubagents(main, subs, contamination)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].res.AgentID < subs[j].res.AgentID })
	for _, s := range subs {
		s.res.SessionMetrics = s.t.Metrics()
	}
	return subs, checked, unlinked, threads, nil
}

func workload(s *SessionResult, unlinkedCalls int) *Workload {
	w := &Workload{MainProcessed: s.ProcessedTokens, SidechainProcessed: s.Sidechain.ProcessedTokens, UnlinkedAgentCalls: unlinkedCalls}
	for _, a := range s.Subagents {
		w.SubagentProcessed += a.ProcessedTokens
		if a.Parent == "" {
			w.UnlinkedSubagents++
		} else {
			w.LinkedSubagents++
		}
	}
	w.TotalProcessed = w.MainProcessed + w.SubagentProcessed + w.SidechainProcessed
	return w
}

// validateSubagents checks a v3 session's subagent list against its manifest
// entry and its own arithmetic.
func validateSubagents(s *SessionResult, ref *SessionRef) error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	byID := map[string]*SubagentResult{}
	for i := range s.Subagents {
		a := &s.Subagents[i]
		if byID[a.AgentID] != nil || a.Transcript != subagentDir(ref.Transcript)+"/agent-"+a.AgentID+".jsonl" || !agentFileRE.MatchString("agent-"+a.AgentID+".jsonl") || !sha64RE.MatchString(a.TranscriptSHA256) {
			bad("subagent %q: duplicate id, transcript path not under the session's subagents directory, or bad sha256", a.AgentID)
		}
		byID[a.AgentID] = a
		if a.InputTokens+a.CacheReadTokens+a.CacheCreation != a.ProcessedTokens || a.ProcessedTokens < a.FinalContext || a.PeakContext < a.FinalContext {
			bad("subagent %s: inconsistent token counts", a.AgentID)
		}
	}
	for _, a := range s.Subagents {
		switch {
		case a.Parent == "":
			if a.Depth != 0 || a.ToolUseID != "" {
				bad("subagent %s: unlinked with depth or tool_use_id", a.AgentID)
			}
		case a.Parent == "main":
			if a.Depth != 1 || a.ToolUseID == "" {
				bad("subagent %s: linked to main needs depth 1 and a tool_use_id", a.AgentID)
			}
		default:
			if p := byID[a.Parent]; p == nil || p.Parent == "" || a.Depth != p.Depth+1 || a.ToolUseID == "" {
				bad("subagent %s: parent %q missing, unlinked, or depth inconsistent", a.AgentID, a.Parent)
			}
		}
	}
	if ref.Subagents != nil {
		want := map[string]string{}
		for _, d := range *ref.Subagents {
			want[d.Transcript] = d.TranscriptSHA256
		}
		got := map[string]string{}
		for _, a := range s.Subagents {
			got[a.Transcript] = a.TranscriptSHA256
		}
		if !maps.Equal(want, got) {
			bad("subagent transcripts do not match the manifest's declared list")
		}
	}
	w := s.Workload
	if w == nil {
		bad("workload missing")
		return errors.Join(errs...)
	}
	unlinkedCalls := w.UnlinkedAgentCalls
	if *w != *workload(s, unlinkedCalls) || unlinkedCalls < 0 {
		bad("workload does not match the session and its subagents")
	}
	unverified := 0
	for _, c := range s.Contamination.Calls {
		if c.Agent != "main" && c.Agent != "sidechain" && byID[c.Agent] == nil {
			bad("contamination call %s: unknown agent %q", c.ToolUseID, c.Agent)
		}
		if c.Kind == "subagent" {
			unverified++
			if c.Outcome != "unverified" {
				bad("contamination call %s: Task/Agent calls without a checked subagent file are unverified", c.ToolUseID)
			}
		}
	}
	if s.Contamination.Checked && unverified != unlinkedCalls {
		bad("%d unverified Task/Agent calls, workload counts %d unlinked", unverified, unlinkedCalls)
	}
	return errors.Join(errs...)
}
