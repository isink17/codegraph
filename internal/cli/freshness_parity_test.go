package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/constraints"
	"github.com/isink17/codegraph/internal/editsafety"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

// freshnessSurfaces reads one graph through every freshness consumer: the
// stats command, MCP graph_stats over the production serve path, strict
// check_constraints and safe_to_edit.
type freshnessSurfaces struct {
	stats, mcp     json.RawMessage
	freshness      graph.Freshness
	strict         *constraints.StrictFreshness
	strictExit     int
	safe           editsafety.Result
	safeExit       int
	safeFreshState string
}

func readFreshnessSurfaces(t *testing.T, root string) freshnessSurfaces {
	t.Helper()
	var out freshnessSurfaces
	statsOut, _, err := runCLI(t, "stats", root)
	if err != nil {
		t.Fatal(err)
	}
	var stats struct {
		Freshness json.RawMessage `json:"freshness"`
	}
	if err := json.Unmarshal([]byte(statsOut), &stats); err != nil {
		t.Fatal(err)
	}
	out.stats = compactJSON(t, stats.Freshness)
	if err := json.Unmarshal(stats.Freshness, &out.freshness); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	lines := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}` + "\n"
	if err := runServeWith(context.Background(), loadTestConfig(t), strings.NewReader(lines), &stdout, &stderr, []string{"--repo-root", root}); err != nil {
		t.Fatalf("serve: %v (%s)", err, stderr.String())
	}
	responses := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var resp struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(responses[len(responses)-1]), &resp); err != nil || len(resp.Result.Content) == 0 {
		t.Fatalf("graph_stats response %s (%v)", responses[len(responses)-1], err)
	}
	var envelope struct {
		Data struct {
			Freshness json.RawMessage `json:"freshness"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &envelope); err != nil {
		t.Fatal(err)
	}
	out.mcp = compactJSON(t, envelope.Data.Freshness)

	_, res, code := checkCLI(t, "check_constraints", root, "--strict-freshness")
	out.strict, out.strictExit = res.Freshness, code
	_, out.safe, out.safeExit, _ = safeToEdit(t, root, "--symbol", "infra.B")
	out.safeFreshState = out.safe.Freshness.State
	return out
}

func compactJSON(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A stale reason added to freshness reaches every consumer through the one
// verdict path: stats and graph_stats print the same object, strict
// check_constraints exits 3 and safe_to_edit withholds its verdict.
func TestFreshnessParityAcrossSurfaces(t *testing.T) {
	root, _ := safeToEditRepo(t, map[string]string{constraints.ConfigFileName: cliConstraintsConfig})
	opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
	if err != nil {
		t.Fatal(err)
	}
	dbPath, repoID := opened.DBPath, opened.Repo.ID
	opened.Close()

	expect := func(step string, wantReason string, live string) freshnessSurfaces {
		t.Helper()
		got := readFreshnessSurfaces(t, root)
		if !bytes.Equal(got.stats, got.mcp) {
			t.Fatalf("%s: stats and graph_stats differ\nstats: %s\nmcp:   %s", step, got.stats, got.mcp)
		}
		if got.freshness.LiveWatcher != live {
			t.Fatalf("%s: live_watcher %s, want %s", step, got.freshness.LiveWatcher, live)
		}
		stale := wantReason != ""
		if slices.Contains(got.freshness.Reasons, wantReason) != stale || (got.freshness.Verdict == graph.FreshnessVerdictStaleKnown) != stale {
			t.Fatalf("%s: verdict %s reasons %v, want %q", step, got.freshness.Verdict, got.freshness.Reasons, wantReason)
		}
		if stale {
			if got.strictExit != 3 || got.strict == nil || got.strict.State != graph.FreshnessKnownStale || !slices.Contains(got.strict.StateReasons, wantReason) {
				t.Fatalf("%s: strict check_constraints exit %d freshness %+v", step, got.strictExit, got.strict)
			}
			if got.safeExit != editsafety.ExitKnownStale || got.safe.Reason != editsafety.ReasonKnownStale || got.safe.Verdict != nil || got.safeFreshState != graph.FreshnessKnownStale {
				t.Fatalf("%s: safe_to_edit exit %d reason %s state %s", step, got.safeExit, got.safe.Reason, got.safeFreshState)
			}
		} else if got.strictExit == 3 || got.safeExit == editsafety.ExitKnownStale {
			t.Fatalf("%s: strict exit %d, safe_to_edit exit %d on a graph with no stale reason", step, got.strictExit, got.safeExit)
		}
		return got
	}
	base := expect("indexed", "", store.LiveWatcherNone)
	if base.freshness.ParserProfileCompatibility.State != "current" {
		t.Fatalf("indexed by this binary: %+v", base.freshness.ParserProfileCompatibility)
	}

	// A parser upgrade: every Go file carries an older version of this
	// binary's Go profile.
	current, ok := newDefaultRegistry().ProfileForLanguage("go")
	if !ok {
		t.Fatal("no go profile")
	}
	i := strings.LastIndex(current.ID, ":v")
	var version int
	if _, err := fmt.Sscanf(current.ID[i+2:], "%d", &version); err != nil {
		t.Fatal(err)
	}
	older := fmt.Sprintf("%s:v%d", current.ID[:i], version-1)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	setProfile := func(profile string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE files SET parser_profile = ? WHERE language = 'go' AND parser_profile <> ''`, profile); err != nil {
			t.Fatal(err)
		}
	}
	setProfile(older)
	upgraded := expect("parser upgrade", "parser_profile_obsolete", store.LiveWatcherNone)
	if c := upgraded.freshness.ParserProfileCompatibility; c.State != "obsolete" || len(c.Languages) != 1 || c.Languages[0].Language != "go" || c.Languages[0].Current != current.ID {
		t.Fatalf("parser upgrade: %+v", c)
	}
	setProfile(current.ID)

	// A watcher of this repository, in any process on this host, that has not
	// reconciled.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lock, err := st.OpenWatcherLock(repoID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Close)
	if err := lock.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := lock.HoldLive(); err != nil {
		t.Fatal(err)
	}
	expect("reconciling watcher", "reconcile_pending", store.LiveWatcherUnverified)
	lock.Release()
	expect("watching", "", store.LiveWatcherWatching)
	lock.Close()
	expect("watcher exited", "", store.LiveWatcherNone)
}
