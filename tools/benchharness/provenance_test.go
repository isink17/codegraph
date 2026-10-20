package main

import (
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
	subs := []*agentFile{{res: SubagentResult{AgentID: "x1"}, t: x1}, {res: SubagentResult{AgentID: "x2"}, t: x2}}
	if _, _, _, err := linkSubagents(main, subs); err == nil || !strings.Contains(err.Error(), "tu1") {
		t.Fatalf("tool_use linked to two agents: err=%v x2 parent=%q", err, x2Parent(subs))
	}

	// The conflicting copy names an agent that has no file.
	x1 = ownFile(t, asstLine("m0", true, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`), spawnResult("tu1", "gone"))
	if _, _, _, err := linkSubagents(main, []*agentFile{{res: SubagentResult{AgentID: "x1"}, t: x1}}); err == nil {
		t.Fatal("tool_use linked to a file and to a missing agent")
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
	if _, _, _, err := linkSubagents(main, []*agentFile{{res: SubagentResult{AgentID: "x1"}, t: x1}}); err == nil || !strings.Contains(err.Error(), "cg1") {
		t.Fatalf("conflicting results across files: %v", err)
	}

	// An unlinked file's copy is checked too.
	x1 = ownFile(t, asstLine("m0", true, 1, 0, 10, 1, cgCall), toolResultLine("cg1", "files: 3"))
	if _, _, _, err := linkSubagents(main, []*agentFile{{res: SubagentResult{AgentID: "y9"}, t: x1}}); err == nil {
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
	if _, _, _, err := linkSubagents(inFile, nil); err == nil {
		t.Fatal("in-file conflicting results accepted")
	}

	// Differing results of a call contamination ignores are not checked.
	reads, err := parse(t, asstLine("m0", false, 1, 0, 10, 1, `{"type":"tool_use","id":"r","name":"Read","input":{}}`), toolResultLine("r", "a"), toolResultLine("r", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := linkSubagents(reads, nil); err != nil {
		t.Fatal(err)
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
