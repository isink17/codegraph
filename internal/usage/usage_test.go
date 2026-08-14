package usage

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/tokenest"
)

func fixedClock() func() time.Time {
	base := time.Unix(1700000000, 0)
	n := 0
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Second)
	}
}

func TestZeroCallReportIsValid(t *testing.T) {
	report := New("full", fixedClock()).Snapshot(false, 0)
	if report.Schema != Schema {
		t.Fatalf("schema = %q, want %q", report.Schema, Schema)
	}
	if report.Session.MCPCalls != 0 || report.Session.EstimatedTokens != 0 {
		t.Fatalf("empty session = %+v", report.Session)
	}
	if len(report.Tools) != 0 {
		t.Fatalf("tools = %v, want empty", report.Tools)
	}
	// Both invocation rows exist even with no calls, so the report shape does not
	// depend on what a session happened to do.
	if len(report.Invocation) != 2 || report.Invocation[0].Via != ViaDirect || report.Invocation[1].Via != ViaGateway {
		t.Fatalf("invocation rows = %+v", report.Invocation)
	}
	if _, err := json.Marshal(report); err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
}

func TestSessionTotalsIncludeDiscovery(t *testing.T) {
	m := New("gateway", fixedClock())
	m.RecordToolsList(2, 3585)
	m.RecordToolCall(Key{Tool: "audit", Via: ViaDirect}, 10, 400, false)

	report := m.Snapshot(false, 0)
	if report.Session.MCPCalls != 2 || report.Session.ToolCalls != 1 || report.Session.ToolsListCalls != 1 {
		t.Fatalf("session counts = %+v", report.Session)
	}
	if report.Session.ResponseBytes != 3585+400 {
		t.Fatalf("session response bytes = %d, want %d", report.Session.ResponseBytes, 3585+400)
	}
	if report.Discovery.ResponseBytes != 3585 || report.Discovery.Calls != 1 {
		t.Fatalf("discovery = %+v", report.Discovery)
	}
	// The documented invariant: a bucket's estimated_tokens is exactly the sum of
	// its two stream estimates.
	if got, want := report.Session.EstimatedTokens,
		report.Session.RequestEstimatedTokens+report.Session.ResponseEstimatedTokens; got != want {
		t.Fatalf("session estimated_tokens = %d, want %d", got, want)
	}
	if got := tokenest.FromBytes(3585 + 400); got != report.Session.ResponseEstimatedTokens {
		t.Fatalf("response tokens = %d, want %d", report.Session.ResponseEstimatedTokens, got)
	}
}

func TestErrorsAreCountedNotSkipped(t *testing.T) {
	m := New("full", fixedClock())
	m.RecordToolCall(Key{Tool: "find_symbol", Via: ViaDirect}, 20, 60, true)
	report := m.Snapshot(false, 0)
	if report.Session.Errors != 1 || report.Tools[0].Errors != 1 || report.Tools[0].Calls != 1 {
		t.Fatalf("error accounting = %+v / %+v", report.Session, report.Tools)
	}
	if report.Tools[0].ResponseBytes != 60 {
		t.Fatalf("failed call response bytes = %d, want 60", report.Tools[0].ResponseBytes)
	}
}

func TestReportOrderIsDeterministic(t *testing.T) {
	build := func() Report {
		m := New("full", fixedClock())
		m.RecordToolCall(Key{Tool: "b_tool", Via: ViaDirect}, 1, 100, false)
		m.RecordToolCall(Key{Tool: "a_tool", Via: ViaGateway}, 1, 500, false)
		m.RecordToolCall(Key{Tool: "a_tool", Via: ViaDirect, Format: "compact"}, 1, 100, false)
		m.RecordToolCall(Key{Tool: "a_tool", Via: ViaDirect, Format: "json"}, 1, 100, false)
		return m.Snapshot(false, 0)
	}
	first, err := json.Marshal(build())
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	second, err := json.Marshal(build())
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("report is not deterministic:\n%s\n%s", first, second)
	}
	// Response bytes descending, then name, then via, then format.
	got := build().Tools
	want := []struct {
		name   string
		via    Via
		format string
	}{
		{"a_tool", ViaGateway, ""},
		{"a_tool", ViaDirect, "compact"},
		{"a_tool", ViaDirect, "json"},
		{"b_tool", ViaDirect, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Via != w.via || got[i].Format != w.format {
			t.Fatalf("row %d = %+v, want %v", i, got[i], w)
		}
	}
}

func TestSnapshotDoesNotMutateAndResetClears(t *testing.T) {
	m := New("full", fixedClock())
	m.RecordToolCall(Key{Tool: "audit", Via: ViaDirect}, 5, 50, false)
	m.RecordToolsList(0, 100)

	if first, second := m.Snapshot(false, 0), m.Snapshot(false, 0); first.Session.MCPCalls != second.Session.MCPCalls {
		t.Fatalf("snapshot mutated state: %d then %d", first.Session.MCPCalls, second.Session.MCPCalls)
	}
	pre := m.Snapshot(true, 0)
	if pre.Session.MCPCalls != 2 {
		t.Fatalf("pre-reset snapshot = %+v", pre.Session)
	}
	post := m.Snapshot(false, 0)
	if post.Session.MCPCalls != 0 || len(post.Tools) != 0 || post.Discovery.Calls != 0 {
		t.Fatalf("post-reset snapshot = %+v / %+v", post.Session, post.Discovery)
	}
	// The returned pre-reset document is a value, not a view: clearing the meter
	// does not retroactively empty it.
	if pre.Tools[0].Calls != 1 {
		t.Fatalf("pre-reset snapshot changed after reset: %+v", pre.Tools)
	}
}

func TestCardinalityIsBounded(t *testing.T) {
	m := New("full", fixedClock())
	for i := 0; i < maxTrackedKeys*3; i++ {
		m.RecordToolCall(Key{Tool: string(rune('a'+i%26)) + string(rune('a'+i/26)) + string(rune(i)), Via: ViaDirect}, 1, 1, false)
	}
	report := m.Snapshot(false, 0)
	if limit := maxTrackedKeys + len(vias); len(report.Tools) > limit {
		t.Fatalf("tracked keys = %d, want at most %d", len(report.Tools), limit)
	}
	var overflow bool
	for _, row := range report.Tools {
		if row.Name == OverflowTool {
			overflow = true
		}
	}
	if !overflow {
		t.Fatal("overflow bucket was never used")
	}
	if report.Session.ToolCalls != maxTrackedKeys*3 {
		t.Fatalf("calls = %d, want %d", report.Session.ToolCalls, maxTrackedKeys*3)
	}
}

// TestToolRowsAreTrimmedNotFalsified: the report is bounded, and what it drops
// it says it dropped -- the totals still cover every call.
func TestToolRowsAreTrimmedNotFalsified(t *testing.T) {
	m := New("full", fixedClock())
	const rows = DefaultToolRows + 7
	for i := 0; i < rows; i++ {
		m.RecordToolCall(Key{Tool: string(rune('a'+i)), Via: ViaDirect}, 10, 100+i, false)
	}
	report := m.Snapshot(false, 0)
	if len(report.Tools) != DefaultToolRows {
		t.Fatalf("rows = %d, want the default cap %d", len(report.Tools), DefaultToolRows)
	}
	if report.ToolsTotal != rows {
		t.Fatalf("tools_total = %d, want %d", report.ToolsTotal, rows)
	}
	// The trimmed rows are the cheap ones: the table keeps what a reader came for.
	if report.Tools[0].ResponseBytes != int64(100+rows-1) {
		t.Fatalf("first row = %+v, want the most expensive", report.Tools[0])
	}
	// Totals are unaffected by trimming.
	if report.Session.ToolCalls != rows {
		t.Fatalf("session tool calls = %d, want %d", report.Session.ToolCalls, rows)
	}
	var wantBytes int64
	for i := 0; i < rows; i++ {
		wantBytes += int64(100 + i)
	}
	if report.Session.ResponseBytes != wantBytes {
		t.Fatalf("session response bytes = %d, want %d", report.Session.ResponseBytes, wantBytes)
	}
	// An explicit limit overrides the default in both directions.
	if got := len(m.Snapshot(false, 3).Tools); got != 3 {
		t.Fatalf("limit=3 rows = %d, want 3", got)
	}
	if got := len(m.Snapshot(false, rows*2).Tools); got != rows {
		t.Fatalf("limit above the row count = %d, want %d", got, rows)
	}
}

func TestConcurrentRecordingIsRaceFree(t *testing.T) {
	m := New("gateway", fixedClock())
	const workers, per = 8, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			via := ViaDirect
			if w%2 == 1 {
				via = ViaGateway
			}
			for i := 0; i < per; i++ {
				m.RecordToolCall(Key{Tool: "find_symbol", Via: via, Detail: "card"}, 4, 8, i%10 == 0)
				if i%50 == 0 {
					m.Snapshot(false, 0)
				}
			}
		}(w)
	}
	wg.Wait()
	report := m.Snapshot(false, 0)
	if report.Session.ToolCalls != workers*per {
		t.Fatalf("calls = %d, want %d", report.Session.ToolCalls, workers*per)
	}
	if report.Session.ResponseBytes != int64(workers*per*8) {
		t.Fatalf("response bytes = %d, want %d", report.Session.ResponseBytes, workers*per*8)
	}
	if report.Session.Errors != workers*per/10 {
		t.Fatalf("errors = %d, want %d", report.Session.Errors, workers*per/10)
	}
}

func TestNegativeSizesAreClamped(t *testing.T) {
	m := New("full", fixedClock())
	m.RecordToolCall(Key{Tool: "audit", Via: ViaDirect}, -5, -9, false)
	if got := m.Snapshot(false, 0).Session.ResponseBytes; got != 0 {
		t.Fatalf("response bytes = %d, want 0", got)
	}
}

func TestNilMeterIsInert(t *testing.T) {
	var m *Meter
	m.RecordToolCall(Key{Tool: "audit", Via: ViaDirect}, 1, 1, false)
	m.RecordToolsList(1, 1)
	m.SetToolMode("gateway")
	if report := m.Snapshot(true, 0); report.Schema != Schema || len(report.Tools) != 0 {
		t.Fatalf("nil meter snapshot = %+v", report)
	}
}
