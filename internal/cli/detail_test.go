package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func detailCLIRepo(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(`package main

// HelloWorld greets.
func HelloWorld() string {
	return "hello"
}

func main() {
	_ = HelloWorld()
}
`), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}

	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), []string{"index", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("Run(index) error = %v", err)
	}
	return repoRoot
}

func runFindSymbol(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), append([]string{"find-symbol"}, args...), &out, &errOut); err != nil {
		t.Fatalf("Run(find-symbol %v) error = %v\n%s", args, err, errOut.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, output:\n%s", err, out.String())
	}
	return decoded
}

func firstMatch(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	matches, ok := payload["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Fatalf("no matches in %v", payload)
	}
	return matches[0].(map[string]any)
}

// TestCLIDefaultOutputIsUnchanged pins the deliberate difference between the two
// surfaces: MCP defaults to card because agents pay for every token, while the
// CLI has always printed every indexed field and scripts depend on that. Adding
// --detail must not quietly shrink what `codegraph find-symbol` prints.
func TestCLIDefaultOutputIsUnchanged(t *testing.T) {
	repoRoot := detailCLIRepo(t)
	match := firstMatch(t, runFindSymbol(t, repoRoot, "HelloWorld"))

	for _, field := range []string{
		"symbol_id", "file_id", "language", "kind", "name", "qualified_name",
		"signature", "range", "stable_key", "file",
	} {
		if _, ok := match[field]; !ok {
			t.Fatalf("CLI default output dropped %q: %v", field, match)
		}
	}
	// The projection is opt-in, so its own fields must be absent by default.
	if _, ok := match["line"]; ok {
		t.Fatalf("CLI default output switched to the projected shape: %v", match)
	}
}

func TestCLIDetailFlagSharesMCPSemantics(t *testing.T) {
	repoRoot := detailCLIRepo(t)

	card := firstMatch(t, runFindSymbol(t, "--detail", "card", repoRoot, "HelloWorld"))
	for _, present := range []string{"name", "qualified_name", "kind", "file", "line", "stable_key"} {
		if _, ok := card[present]; !ok {
			t.Fatalf("CLI card is missing %q: %v", present, card)
		}
	}
	for _, absent := range []string{"signature", "range", "source", "file_id"} {
		if _, ok := card[absent]; ok {
			t.Fatalf("CLI card carries %q: %v", absent, card)
		}
	}

	excerpt := firstMatch(t, runFindSymbol(t, "--detail", "excerpt", repoRoot, "HelloWorld"))
	src, ok := excerpt["source"].(map[string]any)
	if !ok {
		t.Fatalf("CLI excerpt has no source: %v", excerpt)
	}
	if !strings.Contains(src["text"].(string), `return "hello"`) {
		t.Fatalf("CLI excerpt did not include the body: %v", src["text"])
	}

	full := firstMatch(t, runFindSymbol(t, "--detail", "full", repoRoot, "HelloWorld"))
	if full["range"] == nil || full["source"] == nil {
		t.Fatalf("CLI full is missing legacy or source fields: %v", full)
	}
}

func TestCLIQueryPresenceDistinguishesEmptyAndMissing(t *testing.T) {
	repoRoot := detailCLIRepo(t)

	run := func(args ...string) map[string]any {
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), args, &out, &errOut); err != nil {
			t.Fatalf("Run(%v) error = %v", args, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
			t.Fatalf("json.Unmarshal(%v) error = %v\n%s", args, err, out.String())
		}
		return payload
	}

	searchEmpty := run("search", "--limit", "1", "--offset", "100", repoRoot, "HelloWorld")
	searchMissing := run("search", repoRoot, "NoSuchSymbol")
	if searchEmpty["matched"] != true || searchMissing["matched"] != false {
		t.Fatalf("search presence empty=%v missing=%v", searchEmpty, searchMissing)
	}

	callersEmpty := run("callers", repoRoot, "main")
	callersMissing := run("callers", repoRoot, "NoSuchSymbol")
	if callersEmpty["target_found"] != true || callersMissing["target_found"] != false {
		t.Fatalf("caller presence empty=%v missing=%v", callersEmpty, callersMissing)
	}

	impactMissing := run("impact", repoRoot, "NoSuchSymbol")
	seedPresence, ok := impactMissing["seed_presence"].(map[string]any)
	if !ok || seedPresence["found"] != float64(0) || seedPresence["requested"] != float64(1) {
		t.Fatalf("impact presence = %v", impactMissing)
	}
}

// TestCLIDetailWorksAfterPositionalArguments covers the spelling the help
// examples imply. Go's flag package stops at the first non-flag argument, so a
// trailing --detail used to be dropped without a word, which is worse than an
// error: the command printed a different payload than the one asked for.
func TestCLIDetailWorksAfterPositionalArguments(t *testing.T) {
	repoRoot := detailCLIRepo(t)

	trailing := firstMatch(t, runFindSymbol(t, repoRoot, "HelloWorld", "--detail", "card"))
	leading := firstMatch(t, runFindSymbol(t, "--detail", "card", repoRoot, "HelloWorld"))

	if fmt.Sprint(trailing) != fmt.Sprint(leading) {
		t.Fatalf("flag position changed the result:\n%v\n%v", trailing, leading)
	}
	if _, ok := trailing["signature"]; ok {
		t.Fatalf("trailing --detail was ignored: %v", trailing)
	}
}

// TestCLIBlankDetailMeansTheDefaultLevel keeps the two surfaces reading the
// same wire value the same way: over MCP a blank detail selects the default
// level, so `--detail ""` must not mean "no projection at all".
func TestCLIBlankDetailMeansTheDefaultLevel(t *testing.T) {
	repoRoot := detailCLIRepo(t)

	blank := firstMatch(t, runFindSymbol(t, "--detail", "", repoRoot, "HelloWorld"))
	card := firstMatch(t, runFindSymbol(t, "--detail", "card", repoRoot, "HelloWorld"))

	if fmt.Sprint(blank) != fmt.Sprint(card) {
		t.Fatalf("blank --detail did not select the default level:\n%v\n%v", blank, card)
	}
}

func TestCLIRejectsUnknownDetail(t *testing.T) {
	repoRoot := detailCLIRepo(t)
	var out, errOut bytes.Buffer
	err := Run(context.Background(), []string{"find-symbol", "--detail", "verbose", repoRoot, "HelloWorld"}, &out, &errOut)
	if err == nil {
		t.Fatal("unknown --detail was accepted")
	}
	if !strings.Contains(err.Error(), "invalid detail") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// TestCLIQueryMayStartWithADash guards the compatibility cost of making flags
// work in any position: a token that looks like a flag but is not one this
// command defines has always been a query, and must stay one.
func TestCLIQueryMayStartWithADash(t *testing.T) {
	repoRoot := detailCLIRepo(t)

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), []string{"search_symbols", repoRoot, "-weird"}, &out, &errOut); err != nil {
		t.Fatalf("a query beginning with - was rejected: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, output:\n%s", err, out.String())
	}
	if _, ok := decoded["matches"]; !ok {
		t.Fatalf("no matches key in %v", decoded)
	}
}

// TestCLIFlagValueIsNotMistakenForAPositional pins the other half of the split:
// a non-boolean flag's value must travel with the flag, not become the query.
func TestCLIFlagValueIsNotMistakenForAPositional(t *testing.T) {
	repoRoot := detailCLIRepo(t)

	payload := runFindSymbol(t, repoRoot, "HelloWorld", "--limit", "1", "--detail", "card")
	matches, ok := payload["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("--limit 1 after the positionals did not apply: %v", payload)
	}
	if _, ok := matches[0].(map[string]any)["signature"]; ok {
		t.Fatalf("--detail after the positionals did not apply: %v", matches[0])
	}
}
