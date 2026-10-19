package main

import (
	"regexp"
	"strings"
)

// runsCodegraph reports whether a Bash command line runs the codegraph binary
// as a command. It tokenizes the line like a shell (quotes, escapes,
// separators, command substitution), then looks at each simple command's
// command word after assignments, keywords and common wrappers. Arguments
// such as `cd /src/codegraph` or `grep codegraph` are not commands.
// ponytail: approximate lexer, no aliases/functions/eval/variables-as-commands.
func runsCodegraph(line string) bool { return runsCodegraphDepth(line, 0) }

func runsCodegraphDepth(line string, depth int) bool {
	if depth > 4 {
		return false
	}
	cmds, subs := lexShell(line)
	for _, words := range cmds {
		if commandRunsCodegraph(words, depth) {
			return true
		}
	}
	for _, s := range subs {
		if runsCodegraphDepth(s, depth+1) {
			return true
		}
	}
	return false
}

// lexShell splits a command line into simple commands (word lists). Command
// substitutions inside double quotes are returned separately.
func lexShell(s string) (cmds [][]string, subs []string) {
	var words []string
	var cur strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	endCmd := func() {
		endWord()
		if len(words) > 0 {
			cmds = append(cmds, words)
			words = nil
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			endWord()
		case c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')' || c == '`':
			endCmd()
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			endCmd()
			i++
		case (c == '{' || c == '}' || c == '!') && !inWord:
			endCmd()
		case c == '\\':
			if i+1 < len(s) {
				i++
				if s[i] != '\n' {
					cur.WriteByte(s[i])
				}
			}
			inWord = true
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			i = lexDoubleQuoted(s, i+1, &cur, &subs)
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	endCmd()
	return cmds, subs
}

// lexDoubleQuoted appends the literal text of a double-quoted string starting
// at i and returns the index of its closing quote.
func lexDoubleQuoted(s string, i int, cur *strings.Builder, subs *[]string) int {
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return i
		case c == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0:
			i++
			cur.WriteByte(s[i])
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			depth, j := 1, i+2
			for ; j < len(s) && depth > 0; j++ {
				switch s[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			*subs = append(*subs, s[i+2:max(i+2, j-1)])
			i = j - 1
		case c == '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j < 0 {
				j = len(s) - i - 1
			}
			*subs = append(*subs, s[i+1:i+1+j])
			i += j + 1
		default:
			cur.WriteByte(c)
		}
	}
	return i
}

var assignmentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

var shellKeywords = map[string]bool{"if": true, "then": true, "do": true, "while": true, "until": true, "else": true, "elif": true}

// wrapperArgOpts lists, per wrapper, the options that take a separate argument.
var wrapperArgOpts = map[string]map[string]bool{
	"env":     {"-u": true, "-C": true, "-S": true, "--unset": true, "--chdir": true},
	"sudo":    {"-u": true, "-g": true, "-C": true, "-D": true, "-h": true, "-p": true, "-r": true, "-t": true, "-U": true, "-T": true},
	"doas":    {"-u": true, "-C": true},
	"timeout": {"-s": true, "-k": true, "--signal": true, "--kill-after": true},
	"nice":    {"-n": true, "--adjustment": true},
	"stdbuf":  {"-i": true, "-o": true, "-e": true},
	"time":    {"-f": true, "-o": true},
	"xargs":   {"-n": true, "-I": true, "-P": true, "-L": true, "-d": true, "-E": true, "-s": true, "-a": true},
	"npx":     {"-p": true, "--package": true, "-c": true},
	"bunx":    {"-p": true, "--package": true},
	"pnpx":    {"-p": true, "--package": true},
	"nohup":   {},
	"exec":    {"-a": true},
	"command": {},
	"builtin": {},
}

var goRunTargetRE = regexp.MustCompile(`(?:^|/)cmd/codegraph(?:@\S+)?$`)

func commandRunsCodegraph(words []string, depth int) bool {
	for len(words) > 0 {
		w := words[0]
		if assignmentRE.MatchString(w) || shellKeywords[w] {
			words = words[1:]
			continue
		}
		name := commandName(w)
		if name == "codegraph" {
			return true
		}
		args := words[1:]
		switch name {
		case "npx", "bunx", "pnpx":
			args = skipOptions(name, args)
			return len(args) > 0 && isCodegraphPackage(args[0])
		case "command":
			// command -v/-V (alone or in a cluster such as -pv) only looks the name up.
			for _, a := range args {
				if !strings.HasPrefix(a, "-") || a == "--" {
					break
				}
				if strings.ContainsAny(a[1:], "vV") {
					return false
				}
			}
		case "bash", "sh", "zsh", "dash", "ksh":
			for i, a := range args {
				if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
					return i+1 < len(args) && runsCodegraphDepth(args[i+1], depth+1)
				}
			}
			return false
		case "go":
			if len(args) > 0 && args[0] == "run" {
				for _, a := range args[1:] {
					if goRunTargetRE.MatchString(a) {
						return true
					}
				}
			}
			return false
		}
		if _, ok := wrapperArgOpts[name]; !ok {
			return false
		}
		words = skipOptions(name, args)
		if name == "timeout" && len(words) > 0 {
			words = words[1:] // duration
		}
	}
	return false
}

func skipOptions(wrapper string, args []string) []string {
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			return args[1:]
		}
		if len(a) < 2 || a[0] != '-' {
			return args
		}
		if wrapperArgOpts[wrapper][a] && len(args) > 1 {
			args = args[2:]
		} else {
			args = args[1:]
		}
	}
	return args
}

func commandName(w string) string {
	if i := strings.LastIndexAny(w, `/\`); i >= 0 {
		w = w[i+1:]
	}
	if strings.HasSuffix(strings.ToLower(w), ".exe") {
		w = w[:len(w)-4]
	}
	return w
}

// isCodegraphPackage matches an npm package spec such as codegraph,
// codegraph@1.2.0 or @scope/codegraph@latest.
func isCodegraphPackage(spec string) bool {
	if i := strings.LastIndexByte(spec, '@'); i > 0 {
		spec = spec[:i]
	}
	return commandName(spec) == "codegraph"
}
