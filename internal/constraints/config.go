// Package constraints evaluates architectural constraints -- declared path
// groups and the dependency rules between them -- against an already-indexed
// graph.
//
// The evaluator is observational: it issues SELECT statements only, never
// indexes, never re-resolves, and never turns an unresolved edge, an import
// string or another repository's row into a violation. Every list it returns has
// a total semantic order that uses no database row id, so a fresh index and an
// incremental history of the same tree produce the same bytes.
package constraints

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Rule kinds.
const (
	KindForbiddenDependency = "forbidden_dependency"
	KindAllowedDependencies = "allowed_dependencies"
	KindAllowedDependents   = "allowed_dependents"
	KindForbiddenCycles     = "forbidden_cycles"
)

// Config error codes. They are a public contract: added, never renamed.
const (
	CodeJSON              = "json"
	CodeUnknownField      = "unknown_field"
	CodeSchemaVersion     = "schema_version"
	CodeGroupName         = "group_name"
	CodeGroupIncludeEmpty = "group_include_empty"
	CodePattern           = "pattern"
	CodeRuleID            = "rule_id"
	CodeRuleKind          = "rule_kind"
	CodeRuleField         = "rule_field"
	CodeRuleGroup         = "rule_group"
	CodeRuleContradiction = "rule_contradiction"
	CodeGroupOverlap      = "group_overlap"
)

// Error is one config or setup problem.
type Error struct {
	Location string `json:"location"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// Group is one declared path group.
type Group struct {
	Name    string
	Include []pattern
	Exclude []pattern
}

// Rule is one validated rule. Group lists are deduplicated and sorted.
type Rule struct {
	ID     string
	Kind   string
	From   []string
	To     []string
	Of     []string
	Groups []string
}

// Config is a validated constraints document. Groups are sorted by name.
type Config struct {
	Groups []Group
	Rules  []Rule
}

var groupNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ruleFields lists, per kind, each allowed list field and whether it must be
// non-empty.
var ruleFields = map[string]map[string]bool{
	KindForbiddenDependency: {"from": true, "to": true},
	KindAllowedDependencies: {"from": true, "to": false},
	KindAllowedDependents:   {"of": true, "from": false},
	KindForbiddenCycles:     {"groups": true},
}

var knownRuleFields = map[string]bool{"id": true, "kind": true, "from": true, "to": true, "of": true, "groups": true}

type member struct {
	key string
	val json.RawMessage
}

// decodeObject decodes a JSON object preserving member order and duplicates,
// which a map would silently collapse.
func decodeObject(raw []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("want a JSON object")
	}
	var out []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		out = append(out, member{key: key, val: val})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the JSON object")
	}
	return out, nil
}

func decodeStrings(raw json.RawMessage) ([]string, error) {
	var out []string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("want an array of strings, got null")
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("want an array of strings")
	}
	return out, nil
}

func sortErrors(errs []Error) {
	sort.Slice(errs, func(i, j int) bool {
		a, b := errs[i], errs[j]
		if a.Location != b.Location {
			return a.Location < b.Location
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
}

// ParseConfig parses and statically validates a constraints document. It
// returns either a config or a non-empty, sorted error list -- never an empty
// rule set standing in for an invalid document.
func ParseConfig(raw []byte) (*Config, []Error) {
	var errs []Error
	add := func(loc, code, format string, args ...any) {
		errs = append(errs, Error{Location: loc, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	if !json.Valid(raw) {
		var v any
		err := json.Unmarshal(raw, &v)
		msg := "malformed JSON"
		if err != nil {
			msg = err.Error()
		}
		return nil, []Error{{Location: "", Code: CodeJSON, Message: msg}}
	}
	top, err := decodeObject(raw)
	if err != nil {
		return nil, []Error{{Location: "", Code: CodeJSON, Message: err.Error()}}
	}

	cfg := &Config{}
	seenTop := map[string]bool{}
	var groupsRaw, rulesRaw json.RawMessage
	versionSeen := false
	for _, m := range top {
		if seenTop[m.key] {
			add(m.key, CodeJSON, "duplicate field %q", m.key)
			continue
		}
		seenTop[m.key] = true
		switch m.key {
		case "schema_version":
			versionSeen = true
			if strings.TrimSpace(string(m.val)) != "1" {
				add("schema_version", CodeSchemaVersion, "schema_version must be 1, got %s", strings.TrimSpace(string(m.val)))
			}
		case "groups":
			groupsRaw = m.val
		case "rules":
			rulesRaw = m.val
		default:
			add(m.key, CodeUnknownField, "unknown field %q", m.key)
		}
	}
	if !versionSeen {
		add("schema_version", CodeSchemaVersion, "schema_version is missing; want 1")
	}

	declared := map[string]bool{}
	if groupsRaw != nil {
		members, err := decodeObject(groupsRaw)
		if err != nil {
			add("groups", CodeJSON, "groups: %v", err)
		}
		for _, m := range members {
			loc := "groups." + m.key
			if !groupNameRE.MatchString(m.key) {
				add(loc, CodeGroupName, "group name %q must match [a-z][a-z0-9_-]*", m.key)
			} else if declared[m.key] {
				add(loc, CodeGroupName, "group %q is declared more than once", m.key)
				continue
			}
			declared[m.key] = true
			g, gerrs := parseGroup(m.key, m.val)
			errs = append(errs, gerrs...)
			cfg.Groups = append(cfg.Groups, g)
		}
	}
	sort.Slice(cfg.Groups, func(i, j int) bool { return cfg.Groups[i].Name < cfg.Groups[j].Name })

	if rulesRaw != nil {
		var items []json.RawMessage
		if err := json.Unmarshal(rulesRaw, &items); err != nil || bytes.Equal(bytes.TrimSpace(rulesRaw), []byte("null")) {
			add("rules", CodeJSON, "rules must be an array of objects")
		}
		ids := map[string]bool{}
		for i, item := range items {
			r, rerrs := parseRule(fmt.Sprintf("rules[%d]", i), item, declared, ids)
			errs = append(errs, rerrs...)
			cfg.Rules = append(cfg.Rules, r)
		}
	}
	sort.Slice(cfg.Rules, func(i, j int) bool { return cfg.Rules[i].ID < cfg.Rules[j].ID })

	if len(errs) > 0 {
		sortErrors(errs)
		return nil, errs
	}
	return cfg, nil
}

func parseGroup(name string, raw json.RawMessage) (Group, []Error) {
	var errs []Error
	loc := "groups." + name
	g := Group{Name: name}
	members, err := decodeObject(raw)
	if err != nil {
		return g, []Error{{Location: loc, Code: CodeJSON, Message: fmt.Sprintf("group %q: %v", name, err)}}
	}
	seen := map[string]bool{}
	for _, m := range members {
		floc := loc + "." + m.key
		if m.key != "include" && m.key != "exclude" {
			errs = append(errs, Error{floc, CodeUnknownField, fmt.Sprintf("unknown field %q", m.key)})
			continue
		}
		if seen[m.key] {
			errs = append(errs, Error{floc, CodeJSON, fmt.Sprintf("duplicate field %q", m.key)})
			continue
		}
		seen[m.key] = true
		list, err := decodeStrings(m.val)
		if err != nil {
			errs = append(errs, Error{floc, CodeJSON, err.Error()})
			continue
		}
		for i, raw := range list {
			p, err := compilePattern(raw)
			if err != nil {
				errs = append(errs, Error{fmt.Sprintf("%s[%d]", floc, i), CodePattern, fmt.Sprintf("pattern %q: %v", raw, err)})
				continue
			}
			if m.key == "include" {
				g.Include = append(g.Include, p)
			} else {
				g.Exclude = append(g.Exclude, p)
			}
		}
		if m.key == "include" && len(list) == 0 {
			errs = append(errs, Error{floc, CodeGroupIncludeEmpty, fmt.Sprintf("group %q has an empty include", name)})
		}
	}
	if !seen["include"] {
		errs = append(errs, Error{loc + ".include", CodeGroupIncludeEmpty, fmt.Sprintf("group %q has no include", name)})
	}
	return g, errs
}

func parseRule(loc string, raw json.RawMessage, declared, ids map[string]bool) (Rule, []Error) {
	var errs []Error
	add := func(l, code, format string, args ...any) {
		errs = append(errs, Error{Location: l, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var r Rule
	members, err := decodeObject(raw)
	if err != nil {
		add(loc, CodeJSON, "rule: %v", err)
		return r, errs
	}
	fields := map[string]json.RawMessage{}
	for _, m := range members {
		if !knownRuleFields[m.key] {
			add(loc+"."+m.key, CodeUnknownField, "unknown field %q", m.key)
			continue
		}
		if _, dup := fields[m.key]; dup {
			add(loc+"."+m.key, CodeJSON, "duplicate field %q", m.key)
			continue
		}
		fields[m.key] = m.val
	}

	if v, ok := fields["id"]; !ok {
		add(loc+".id", CodeRuleID, "rule id is missing")
	} else if err := json.Unmarshal(v, &r.ID); err != nil {
		add(loc+".id", CodeJSON, "rule id must be a string")
	} else if r.ID == "" {
		add(loc+".id", CodeRuleID, "rule id is empty")
	} else if ids[r.ID] {
		add(loc+".id", CodeRuleID, "rule id %q is used more than once", r.ID)
	} else {
		ids[r.ID] = true
	}

	v, ok := fields["kind"]
	if !ok {
		add(loc+".kind", CodeRuleKind, "rule kind is missing")
		return r, errs
	}
	if err := json.Unmarshal(v, &r.Kind); err != nil {
		add(loc+".kind", CodeJSON, "rule kind must be a string")
		return r, errs
	}
	allowed, ok := ruleFields[r.Kind]
	if !ok {
		add(loc+".kind", CodeRuleKind, "unknown rule kind %q", r.Kind)
		return r, errs
	}

	lists := map[string][]string{}
	for name, val := range fields {
		if name == "id" || name == "kind" {
			continue
		}
		if _, ok := allowed[name]; !ok {
			add(loc+"."+name, CodeRuleField, "field %q is not allowed for kind %s", name, r.Kind)
			continue
		}
		list, err := decodeStrings(val)
		if err != nil {
			add(loc+"."+name, CodeJSON, "%s: %v", name, err)
			continue
		}
		for i, g := range list {
			if !declared[g] {
				add(fmt.Sprintf("%s.%s[%d]", loc, name, i), CodeRuleGroup, "group %q is not declared", g)
			}
		}
		lists[name] = dedupeSorted(list)
	}
	for name, nonEmpty := range allowed {
		list, present := lists[name]
		if _, raw := fields[name]; !raw {
			add(loc+"."+name, CodeRuleField, "required field %q is missing", name)
		} else if present && nonEmpty && len(list) == 0 {
			add(loc+"."+name, CodeRuleField, "field %q must name at least one group", name)
		}
	}
	r.From, r.To, r.Of, r.Groups = lists["from"], lists["to"], lists["of"], lists["groups"]

	switch r.Kind {
	case KindForbiddenDependency:
		for _, g := range r.From {
			if contains(r.To, g) {
				add(loc, CodeRuleContradiction, "group %q is in both from and to", g)
			}
		}
	case KindForbiddenCycles:
		if len(r.Groups) == 1 {
			add(loc, CodeRuleContradiction, "forbidden_cycles needs at least 2 distinct groups")
		}
	}
	return r, errs
}

func dedupeSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	n := 0
	for i, s := range out {
		if i == 0 || s != out[n-1] {
			out[n] = s
			n++
		}
	}
	return out[:n]
}

func contains(sorted []string, s string) bool {
	i := sort.SearchStrings(sorted, s)
	return i < len(sorted) && sorted[i] == s
}
