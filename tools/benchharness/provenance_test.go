package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const cgCall = `{"type":"tool_use","id":"cg1","name":"Bash","input":{"command":"codegraph stats"}}`

func blockedResultLine(id string) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","is_error":true,"content":"bash: codegraph: command not found"}]}}`
}

func TestForkCopyCannotLinkSecondAgent(t *testing.T) {
	main, err := parse(t,
		asstLine("m0", false, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
		spawnResult("tu1", "x1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	// x1 is a fork whose copy of main's tu1 names a different agent.
	x1 := ownFile(t, asstLine("m0", true, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`), spawnResult("tu1", "x2"))
	x2 := ownFile(t, asstLine("z", true, 1, 0, 5, 1, `{"type":"text","text":"z"}`))
	gone := ownFile(t, asstLine("m0", true, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`), spawnResult("tu1", "gone"))
	// Agent identity is provenance, so both arms reject it.
	for _, contamination := range []bool{true, false} {
		subs := []*agentFile{{res: SubagentResult{AgentID: "x1"}, t: x1}, {res: SubagentResult{AgentID: "x2"}, t: x2}}
		if _, _, _, err := linkSubagents(main, subs, contamination); err == nil || !strings.Contains(err.Error(), "tu1") {
			t.Fatalf("contamination=%v: tool_use linked to two agents: err=%v x2 parent=%q", contamination, err, x2Parent(subs))
		}
		// The conflicting copy names an agent that has no file.
		if _, _, _, err := linkSubagents(main, []*agentFile{{res: SubagentResult{AgentID: "x1"}, t: gone}}, contamination); err == nil {
			t.Fatalf("contamination=%v: tool_use linked to a file and to a missing agent", contamination)
		}
	}

	// One file naming two agents for one tool_use.
	if _, err := parse(t,
		asstLine("m0", false, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
		spawnResult("tu1", "x1"), spawnResult("tu1", "x2"),
	); err == nil {
		t.Fatal("in-file tool_use linked to two agents")
	}
}

func x2Parent(subs []*agentFile) string { return subs[len(subs)-1].res.Parent }

func TestSameAgentSpawnCopiesMayDiffer(t *testing.T) {
	// A fork's copy of the spawn result names the same agent with other text.
	fork := `{"type":"user","toolUseResult":{"agentId":"x1"},"message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"partial"}]}}`
	for _, contamination := range []bool{true, false} {
		main, err := parse(t,
			asstLine("m0", false, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
			spawnResult("tu1", "x1"),
		)
		if err != nil {
			t.Fatal(err)
		}
		x1 := ownFile(t, asstLine("m0", true, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`), fork)
		subs := []*agentFile{{res: SubagentResult{AgentID: "x1"}, t: x1}}
		checked, unlinked, _, err := linkSubagents(main, subs, contamination)
		if err != nil || !checked["tu1"] || unlinked != 0 || subs[0].res.Parent != "main" {
			t.Fatalf("contamination=%v: err=%v checked=%v unlinked=%d", contamination, err, checked, unlinked)
		}
	}
}

func TestResultlessCopyDoesNotOverrideBlocked(t *testing.T) {
	main, err := parse(t,
		asstLine("m0", false, 1, 0, 10, 1, cgCall), blockedResultLine("cg1"),
		asstLine("m1", false, 1, 10, 5, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
		spawnResult("tu1", "x1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	// The fork copied m0 but not its result.
	x1 := ownFile(t, asstLine("m0", true, 1, 0, 10, 1, cgCall), asstLine("s1", true, 1, 0, 30, 1, `{"type":"text","text":"done"}`))
	c, _ := sessionCalls(t, main, &agentFile{res: SubagentResult{AgentID: "x1"}, t: x1})
	if c.Contaminated || len(c.Calls) != 1 || c.Calls[0].Outcome != "blocked" || c.BlockedAttempts != 1 {
		t.Fatalf("result-less copy overrode a blocked result: %+v", c)
	}

	// The owning copy has no result; a fork copy carries it.
	main, err = parse(t,
		asstLine("m0", false, 1, 0, 10, 1, cgCall),
		asstLine("m1", false, 1, 10, 5, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
		spawnResult("tu1", "x1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	x1 = ownFile(t, asstLine("m0", true, 1, 0, 10, 1, cgCall), toolResultLine("cg1", "files: 3"))
	c, _ = sessionCalls(t, main, &agentFile{res: SubagentResult{AgentID: "x1"}, t: x1})
	if !c.Contaminated || c.Calls[0].Outcome != "succeeded" {
		t.Fatalf("result on a copy ignored: %+v", c)
	}
}

func TestConflictingResultCopiesFail(t *testing.T) {
	main, err := parse(t,
		asstLine("m0", false, 1, 0, 10, 1, cgCall), blockedResultLine("cg1"),
		asstLine("m1", false, 1, 10, 5, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
		spawnResult("tu1", "x1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	x1 := ownFile(t, asstLine("m0", true, 1, 0, 10, 1, cgCall), toolResultLine("cg1", "files: 3"))
	if _, _, _, err := linkSubagents(main, []*agentFile{{res: SubagentResult{AgentID: "x1"}, t: x1}}, true); err == nil ||
		!strings.Contains(err.Error(), "cg1") || !strings.Contains(err.Error(), "across transcript files") || !strings.Contains(err.Error(), "(blocked and succeeded)") {
		t.Fatalf("conflicting results across files: %v", err)
	}

	// An unlinked file's copy is checked too.
	x1 = ownFile(t, asstLine("m0", true, 1, 0, 10, 1, cgCall), toolResultLine("cg1", "files: 3"))
	if _, _, _, err := linkSubagents(main, []*agentFile{{res: SubagentResult{AgentID: "y9"}, t: x1}}, true); err == nil {
		t.Fatal("conflicting result on an unlinked copy accepted")
	}

	// Identical copies are not a conflict.
	x1 = ownFile(t, asstLine("m0", true, 1, 0, 10, 1, cgCall), blockedResultLine("cg1"))
	c, _ := sessionCalls(t, main, &agentFile{res: SubagentResult{AgentID: "x1"}, t: x1})
	if c.Contaminated || c.BlockedAttempts != 1 {
		t.Fatalf("identical blocked copies: %+v", c)
	}

	inFile, err := parse(t, asstLine("m0", false, 1, 0, 10, 1, cgCall), blockedResultLine("cg1"), toolResultLine("cg1", "files: 3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := linkSubagents(inFile, nil, true); err == nil || !strings.Contains(err.Error(), "within one transcript file") {
		t.Fatalf("in-file conflicting results accepted: %v", err)
	}

	// Differing results of a call contamination ignores are not checked.
	reads, err := parse(t, asstLine("m0", false, 1, 0, 10, 1, `{"type":"tool_use","id":"r","name":"Read","input":{}}`), toolResultLine("r", "a"), toolResultLine("r", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := linkSubagents(reads, nil, true); err != nil {
		t.Fatal(err)
	}
}

// linkCounts runs linkSubagents on freshly parsed transcripts and summarizes
// the linking and token accounting it produces.
func linkCounts(t *testing.T, contamination bool, build func(string) ([]string, []func() *agentFile), res string) (string, error) {
	t.Helper()
	mainLines, subs := build(res)
	main, err := parse(t, mainLines...)
	if err != nil {
		t.Fatal(err)
	}
	var files []*agentFile
	for _, f := range subs {
		files = append(files, f())
	}
	checked, unlinked, threads, err := linkSubagents(main, files, contamination)
	if err != nil {
		return "", err
	}
	out := fmt.Sprint(len(checked), unlinked, len(threads), main.Metrics().ProcessedTokens)
	for _, f := range files {
		m := f.t.Metrics()
		out += fmt.Sprintf(" %s<%s@%d:%d/%d/%d", f.res.AgentID, f.res.Parent, f.res.Depth, f.res.DuplicateRequests, m.ProcessedTokens, m.Requests)
	}
	return out, nil
}

// An arm with CodeGraph enabled never computes contamination, so differing
// result copies of a CodeGraph call must not fail its ingest or change its
// token accounting.
func TestResultConflictIgnoredWhenCodeGraphEnabled(t *testing.T) {
	spawn := asstLine("m1", false, 1, 10, 5, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`)
	file := func(id string, lines ...string) func() *agentFile {
		return func() *agentFile { return &agentFile{res: SubagentResult{AgentID: id}, t: ownFile(t, lines...)} }
	}
	// Each case builds a session whose second copy of cg1 carries res.
	cases := map[string]func(res string) ([]string, []func() *agentFile){
		"cross-file linked": func(res string) ([]string, []func() *agentFile) {
			return []string{asstLine("m0", false, 1, 0, 10, 1, cgCall), blockedResultLine("cg1"), spawn, spawnResult("tu1", "x1")},
				[]func() *agentFile{file("x1", asstLine("m0", true, 1, 0, 10, 1, cgCall), res)}
		},
		"cross-file unlinked": func(res string) ([]string, []func() *agentFile) {
			return []string{asstLine("m0", false, 1, 0, 10, 1, cgCall), blockedResultLine("cg1")},
				[]func() *agentFile{file("y9", asstLine("m0", true, 1, 0, 10, 1, cgCall), res)}
		},
		"in-file": func(res string) ([]string, []func() *agentFile) {
			return []string{asstLine("m0", false, 1, 0, 10, 1, cgCall), blockedResultLine("cg1"), res}, nil
		},
		"nested in-file": func(res string) ([]string, []func() *agentFile) {
			return []string{spawn, spawnResult("tu1", "a1")}, []func() *agentFile{
				file("a1", asstLine("a", true, 1, 0, 10, 1, `{"type":"tool_use","id":"tu2","name":"Task","input":{}}`), spawnResult("tu2", "a2")),
				file("a2", asstLine("n", true, 1, 0, 7, 1, cgCall), blockedResultLine("cg1"), res),
			}
		},
	}
	conflict, agreed := toolResultLine("cg1", "files: 3"), blockedResultLine("cg1")
	for name, build := range cases {
		if _, err := linkCounts(t, true, build, conflict); err == nil || !strings.Contains(err.Error(), "conflicting tool results") {
			t.Errorf("%s: baseline accepted conflicting results: %v", name, err)
		}
		got, err := linkCounts(t, false, build, conflict)
		if err != nil {
			t.Errorf("%s: enabled arm rejected: %v", name, err)
			continue
		}
		want, err := linkCounts(t, true, build, agreed)
		if err != nil || got != want {
			t.Errorf("%s: accounting %q, want %q (%v)", name, got, want, err)
		}
	}
}

func TestNestedAndUnlinkedContamination(t *testing.T) {
	main, err := parse(t,
		asstLine("m0", false, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
		spawnResult("tu1", "a1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	a1 := ownFile(t, asstLine("a", true, 1, 0, 10, 1, `{"type":"tool_use","id":"tu2","name":"Task","input":{}}`), spawnResult("tu2", "b2"))
	// b2 is a fork of a1: it repeats a1's spawn and adds a codegraph call.
	b2 := ownFile(t,
		asstLine("a", true, 1, 0, 10, 1, `{"type":"tool_use","id":"tu2","name":"Task","input":{}}`), spawnResult("tu2", "b2"),
		asstLine("b", true, 1, 0, 20, 1, cgCall), toolResultLine("cg1", "files: 3"))
	u9 := ownFile(t, asstLine("u", true, 1, 0, 5, 1, `{"type":"tool_use","id":"cg9","name":"mcp__codegraph__stats","input":{}}`))
	subs := []*agentFile{{res: SubagentResult{AgentID: "a1"}, t: a1}, {res: SubagentResult{AgentID: "b2"}, t: b2}, {res: SubagentResult{AgentID: "u9"}, t: u9}}
	c, unlinked := sessionCalls(t, main, subs...)
	var got []string
	for _, call := range c.Calls {
		got = append(got, call.Agent+":"+call.ToolUseID+":"+call.Outcome)
	}
	if strings.Join(got, " ") != "b2:cg1:succeeded u9:cg9:no_result" || unlinked != 0 || !c.Contaminated ||
		subs[1].res.Parent != "a1" || subs[1].res.Depth != 2 || subs[2].res.Parent != "" || subs[2].res.Depth != 0 {
		t.Fatalf("calls %v unlinked %d b2=%+v u9=%+v", got, unlinked, subs[1].res, subs[2].res)
	}
}

// conflictingResult adds a second, differing result for an existing tool_use
// to a subagent file. User events carry no usage, so token accounting is
// unaffected.
func conflictingResult(t *testing.T, path, id string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"agentId": "` + strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl") + `", "isSidechain": true, "message": {"content": [{"content": "different", "tool_use_id": "` + id + `", "type": "tool_result"}], "role": "user"}, "type": "user", "version": "9.9.9-test"}` + "\n"
	if err := os.WriteFile(path, append(b, line...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIngestResultConflictOnlyFailsBaseline(t *testing.T) {
	sub := func(dir, session, name string) string { return filepath.Join(dir, session, "subagents", name) }
	_, err := Ingest(copySubagentFixture(t, func(d string) {
		conflictingResult(t, sub(d, "baseline-subagent-cg", "agent-eee.jsonl"), "te1")
	}, false), "2026-01-02T00:00:00Z", "")
	if err == nil || !strings.Contains(err.Error(), "te1") || !strings.Contains(err.Error(), "conflicting tool results") {
		t.Fatalf("baseline conflict: %v", err)
	}

	want := ingestSubagents(t, subagentManifest, "").Sessions[3]
	rec := ingestSubagents(t, copySubagentFixture(t, func(d string) {
		conflictingResult(t, sub(d, "treatment-agents", "agent-ggg.jsonl"), "tg1")
	}, false), "")
	got := rec.Sessions[3]
	if got.Contamination.Checked || len(got.Subagents) != 1 || len(want.Subagents) != 1 || *got.Workload != *want.Workload ||
		!reflect.DeepEqual(got.SessionMetrics, want.SessionMetrics) || !reflect.DeepEqual(got.Subagents[0].SessionMetrics, want.Subagents[0].SessionMetrics) ||
		got.Subagents[0].Parent != "main" || got.Subagents[0].ToolUseID != "tu_g" || got.Subagents[0].DuplicateRequests != want.Subagents[0].DuplicateRequests {
		t.Fatalf("enabled arm accounting changed:\ngot  %+v %+v\nwant %+v %+v", got.Subagents, *got.Workload, want.Subagents, *want.Workload)
	}
}
