package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const TaskSetSchema = "codegraph.agent_task_set/v1"

// TaskSet holds the expected facts for each task. Scoring is rubric matching
// of the final answer text, not a semantic judgment.
type TaskSet struct {
	Schema string `json:"schema"`
	Tasks  []Task `json:"tasks"`
}

type Task struct {
	ID        string `json:"id"`
	Prompt    string `json:"prompt"`
	Required  []Fact `json:"required"`
	Forbidden []Fact `json:"forbidden"`
}

// Fact is one expected (or forbidden) answer fact. Kind substring and regex
// match the pattern; kind number needs a regex with one capture group whose
// text, with commas removed, parses to Value.
type Fact struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Pattern    string   `json:"pattern"`
	Value      *float64 `json:"value,omitempty"`
	IgnoreCase bool     `json:"ignore_case,omitempty"`
}

func (f Fact) regex() (*regexp.Regexp, error) {
	p := f.Pattern
	if f.IgnoreCase {
		p = "(?i)" + p
	}
	return regexp.Compile(p)
}

func (f Fact) matches(answer string) bool {
	if f.Kind == "substring" {
		if f.IgnoreCase {
			return strings.Contains(strings.ToLower(answer), strings.ToLower(f.Pattern))
		}
		return strings.Contains(answer, f.Pattern)
	}
	re, err := f.regex()
	if err != nil {
		return false
	}
	if f.Kind == "regex" {
		return re.MatchString(answer)
	}
	for _, m := range re.FindAllStringSubmatch(answer, -1) {
		if v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64); err == nil && v == *f.Value {
			return true
		}
	}
	return false
}

func validateTaskSet(ts *TaskSet) error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if ts.Schema != TaskSetSchema {
		bad("task set schema %q, want %q", ts.Schema, TaskSetSchema)
	}
	if len(ts.Tasks) == 0 {
		bad("task set has no tasks")
	}
	ids := map[string]bool{}
	for _, t := range ts.Tasks {
		if t.ID == "" || ids[t.ID] {
			bad("task id %q empty or duplicated", t.ID)
		}
		ids[t.ID] = true
		if len(t.Required) == 0 {
			bad("task %s: at least one required fact", t.ID)
		}
		facts := map[string]bool{}
		for _, f := range append(append([]Fact{}, t.Required...), t.Forbidden...) {
			if f.ID == "" || facts[f.ID] {
				bad("task %s: fact id %q empty or duplicated", t.ID, f.ID)
			}
			facts[f.ID] = true
			if err := validateFact(f); err != nil {
				bad("task %s fact %s: %v", t.ID, f.ID, err)
			}
		}
	}
	return errors.Join(errs...)
}

func validateFact(f Fact) error {
	if f.Pattern == "" {
		return errors.New("empty pattern")
	}
	switch f.Kind {
	case "substring":
		if f.Value != nil {
			return errors.New("value only applies to kind number")
		}
		return nil
	case "regex", "number":
	default:
		return fmt.Errorf("kind %q, want substring, regex or number", f.Kind)
	}
	re, err := f.regex()
	if err != nil {
		return err
	}
	if f.Kind == "number" && (f.Value == nil || re.NumSubexp() != 1) {
		return errors.New("kind number needs a value and exactly one capture group")
	}
	if f.Kind == "regex" && f.Value != nil {
		return errors.New("value only applies to kind number")
	}
	return nil
}

// Evaluation is the rubric score of one session's final main-session answer.
type Evaluation struct {
	Status          string       `json:"status"`
	Reason          string       `json:"reason,omitempty"`
	AnswerSHA256    string       `json:"answer_sha256,omitempty"`
	AnswerChars     int          `json:"answer_chars"`
	RequiredTotal   int          `json:"required_total"`
	RequiredMatched int          `json:"required_matched"`
	ForbiddenHits   int          `json:"forbidden_hits"`
	Required        []FactResult `json:"required"`
	Forbidden       []FactResult `json:"forbidden"`
}

type FactResult struct {
	ID      string `json:"id"`
	Matched bool   `json:"matched"`
}

var unscoredReasons = map[string]bool{
	"ended_with_user_message": true,
	"ended_with_tool_use":     true,
	"no_answer_text":          true,
}

// FinalAnswer returns the text of the last main-session request when the
// session ended on it, or the reason it cannot be scored.
func (t *Transcript) FinalAnswer() (answer, reason string) {
	r := t.lastMain
	switch {
	case r.toolUse:
		return "", "ended_with_tool_use"
	case t.userAfter:
		return "", "ended_with_user_message"
	}
	answer = strings.Join(r.text, "\n")
	if strings.TrimSpace(answer) == "" {
		return "", "no_answer_text"
	}
	return answer, ""
}

func evaluate(task Task, answer, reason string) Evaluation {
	e := Evaluation{RequiredTotal: len(task.Required), Required: []FactResult{}, Forbidden: []FactResult{}}
	if reason != "" {
		e.Status, e.Reason = "unscored", reason
		return e
	}
	e.Status, e.AnswerSHA256, e.AnswerChars = "scored", sha256Hex([]byte(answer)), runes(answer)
	for _, f := range task.Required {
		m := f.matches(answer)
		e.Required = append(e.Required, FactResult{f.ID, m})
		if m {
			e.RequiredMatched++
		}
	}
	for _, f := range task.Forbidden {
		m := f.matches(answer)
		e.Forbidden = append(e.Forbidden, FactResult{f.ID, m})
		if m {
			e.ForbiddenHits++
		}
	}
	return e
}

func validateEvaluation(e *Evaluation, task Task) error {
	if e.RequiredTotal != len(task.Required) {
		return fmt.Errorf("required_total %d, task has %d", e.RequiredTotal, len(task.Required))
	}
	switch e.Status {
	case "unscored":
		if !unscoredReasons[e.Reason] || e.AnswerSHA256 != "" || e.AnswerChars != 0 || len(e.Required)+len(e.Forbidden) != 0 || e.RequiredMatched+e.ForbiddenHits != 0 {
			return fmt.Errorf("unscored evaluation must have a known reason and no results")
		}
		return nil
	case "scored":
	default:
		return fmt.Errorf("evaluation status %q", e.Status)
	}
	if e.Reason != "" || !sha64RE.MatchString(e.AnswerSHA256) || e.AnswerChars < 1 {
		return errors.New("scored evaluation needs answer_sha256 and no reason")
	}
	check := func(what string, got []FactResult, want []Fact, count int) error {
		n := 0
		if len(got) != len(want) {
			return fmt.Errorf("%s results do not match the task's facts", what)
		}
		for i := range got {
			if got[i].ID != want[i].ID {
				return fmt.Errorf("%s results do not match the task's facts", what)
			}
			if got[i].Matched {
				n++
			}
		}
		if n != count {
			return fmt.Errorf("%s count %d, results say %d", what, count, n)
		}
		return nil
	}
	return errors.Join(check("required", e.Required, task.Required, e.RequiredMatched), check("forbidden", e.Forbidden, task.Forbidden, e.ForbiddenHits))
}

// ArmCorrectness sums rubric scores over an arm's included sessions.
type ArmCorrectness struct {
	Scored          int `json:"scored"`
	Unscored        int `json:"unscored"`
	RequiredMatched int `json:"required_matched"`
	RequiredTotal   int `json:"required_total"`
	ForbiddenHits   int `json:"forbidden_hits"`
	AllRequiredMet  int `json:"all_required_no_forbidden"`
}

func armCorrectness(sessions []SessionResult) *ArmCorrectness {
	var c *ArmCorrectness
	for _, s := range sessions {
		e := s.Evaluation
		if e == nil {
			continue
		}
		if c == nil {
			c = &ArmCorrectness{}
		}
		if e.Status != "scored" {
			c.Unscored++
			continue
		}
		c.Scored++
		c.RequiredMatched += e.RequiredMatched
		c.RequiredTotal += e.RequiredTotal
		c.ForbiddenHits += e.ForbiddenHits
		if e.RequiredMatched == e.RequiredTotal && e.ForbiddenHits == 0 {
			c.AllRequiredMet++
		}
	}
	return c
}
