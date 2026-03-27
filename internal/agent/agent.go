package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ToolFunc executes a tool by name with JSON arguments and returns JSON result.
type ToolFunc func(ctx context.Context, name string, args json.RawMessage) (map[string]any, error)

// LLMFunc sends a prompt to an LLM and returns the response text.
type LLMFunc func(ctx context.Context, prompt string) (string, error)

// Agent runs a ReAct reasoning loop over codegraph tools.
type Agent struct {
	llm      LLMFunc
	callTool ToolFunc
	maxSteps int
}

// New creates an Agent with the given LLM, tool executor, and max reasoning steps.
func New(llm LLMFunc, callTool ToolFunc, maxSteps int) *Agent {
	if maxSteps <= 0 {
		maxSteps = 5
	}
	if maxSteps > 10 {
		maxSteps = 10
	}
	return &Agent{llm: llm, callTool: callTool, maxSteps: maxSteps}
}

// Result is the final output of an agentic query.
type Result struct {
	Answer    string `json:"answer"`
	Steps     []Step `json:"steps"`
	StepCount int    `json:"step_count"`
}

// Step records a single reasoning step in the ReAct loop.
type Step struct {
	Thought string         `json:"thought"`
	Action  string         `json:"action,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	Result  string         `json:"result,omitempty"`
}

var availableTools = []struct {
	Name string
	Desc string
}{
	{"find_symbol", "Find symbols by exact or fuzzy query. Args: {\"query\": \"...\"}"},
	{"find_callers", "Find callers of a symbol. Args: {\"symbol\": \"...\"}"},
	{"find_callees", "Find callees of a symbol. Args: {\"symbol\": \"...\"}"},
	{"search_semantic", "Hybrid semantic search. Args: {\"query\": \"...\"}"},
	{"context_for_task", "Return relevant files, symbols for a task. Args: {\"task\": \"...\"}"},
	{"trace_dependencies", "Trace transitive dependency chains. Args: {\"symbol\": \"...\", \"direction\": \"upstream|downstream\", \"depth\": 3}"},
	{"architecture_overview", "High-level repository architecture. Args: {}"},
	{"graph_stats", "Return repository graph statistics. Args: {}"},
	{"find_dead_code", "Find symbols with no callers. Args: {}"},
	{"detect_frameworks", "Detect frameworks and libraries. Args: {}"},
}

func buildSystemPrompt(query string) string {
	var sb strings.Builder
	sb.WriteString("You are a code analysis agent. You answer questions about a codebase by calling tools and reasoning about the results.\n\n")
	sb.WriteString("Available tools:\n")
	for _, t := range availableTools {
		fmt.Fprintf(&sb, "- %s: %s\n", t.Name, t.Desc)
	}
	sb.WriteString("\nRespond in exactly this format when you need to call a tool:\n")
	sb.WriteString("Thought: <your reasoning>\n")
	sb.WriteString("Action: <tool_name>\n")
	sb.WriteString("Args: {\"key\": \"value\"}\n\n")
	sb.WriteString("When you have enough information to answer, respond in this format:\n")
	sb.WriteString("Thought: <your final reasoning>\n")
	sb.WriteString("Answer: <your final answer>\n\n")
	sb.WriteString("Important rules:\n")
	sb.WriteString("- Always start with a Thought line.\n")
	sb.WriteString("- Only call one tool per step.\n")
	sb.WriteString("- Args must be valid JSON on a single line.\n")
	sb.WriteString("- Do not invent tool names. Only use tools from the list above.\n\n")
	fmt.Fprintf(&sb, "User question: %s\n", query)
	return sb.String()
}

// Run executes the ReAct reasoning loop for the given query.
func (a *Agent) Run(ctx context.Context, query string) (*Result, error) {
	var steps []Step
	var conversation strings.Builder

	conversation.WriteString(buildSystemPrompt(query))

	for i := 0; i < a.maxSteps; i++ {
		resp, err := a.llm(ctx, conversation.String())
		if err != nil {
			return nil, fmt.Errorf("LLM call failed at step %d: %w", i+1, err)
		}

		thought, action, args, answer := parseResponse(resp)

		if answer != "" {
			steps = append(steps, Step{Thought: thought})
			return &Result{
				Answer:    answer,
				Steps:     steps,
				StepCount: len(steps),
			}, nil
		}

		if action == "" {
			// No action and no answer — treat accumulated text as final answer.
			finalAnswer := thought
			if finalAnswer == "" {
				finalAnswer = strings.TrimSpace(resp)
			}
			steps = append(steps, Step{Thought: finalAnswer})
			return &Result{
				Answer:    finalAnswer,
				Steps:     steps,
				StepCount: len(steps),
			}, nil
		}

		step := Step{
			Thought: thought,
			Action:  action,
			Args:    args,
		}

		argsJSON, err := json.Marshal(args)
		if err != nil {
			argsJSON = []byte("{}")
		}

		toolResult, toolErr := a.callTool(ctx, action, json.RawMessage(argsJSON))
		var observation string
		if toolErr != nil {
			observation = fmt.Sprintf("Error: %s", toolErr.Error())
		} else {
			resultJSON, _ := json.Marshal(toolResult)
			observation = string(resultJSON)
			// Truncate very large observations to avoid blowing up the prompt.
			if len(observation) > 4000 {
				observation = observation[:4000] + "... (truncated)"
			}
		}
		step.Result = observation
		steps = append(steps, step)

		fmt.Fprintf(&conversation, "\n%s\nObservation: %s\n", resp, observation)
	}

	// Max steps exceeded — synthesize partial answer from what we have.
	return &Result{
		Answer:    "Reached maximum reasoning steps. Partial results are available in the steps.",
		Steps:     steps,
		StepCount: len(steps),
	}, nil
}

// parseResponse extracts Thought, Action, Args, and Answer from an LLM response.
// It handles multi-line values by accumulating lines until the next field prefix.
func parseResponse(resp string) (thought, action string, args map[string]any, answer string) {
	var currentField string
	var buf strings.Builder

	flush := func() {
		val := strings.TrimSpace(buf.String())
		switch currentField {
		case "thought":
			thought = val
		case "action":
			action = val
		case "args":
			parsed := map[string]any{}
			if json.Unmarshal([]byte(val), &parsed) == nil {
				args = parsed
			}
		case "answer":
			answer = val
		}
		buf.Reset()
	}

	for line := range strings.SplitSeq(resp, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "Thought:"); ok {
			flush()
			currentField = "thought"
			buf.WriteString(strings.TrimSpace(after))
		} else if after, ok := strings.CutPrefix(trimmed, "Action:"); ok {
			flush()
			currentField = "action"
			buf.WriteString(strings.TrimSpace(after))
		} else if after, ok := strings.CutPrefix(trimmed, "Args:"); ok {
			flush()
			currentField = "args"
			buf.WriteString(strings.TrimSpace(after))
		} else if after, ok := strings.CutPrefix(trimmed, "Answer:"); ok {
			flush()
			currentField = "answer"
			buf.WriteString(strings.TrimSpace(after))
		} else if currentField != "" && trimmed != "" {
			// Continuation line for the current field.
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(trimmed)
		}
	}
	flush()
	return
}
