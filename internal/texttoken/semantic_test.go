package texttoken

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func tokensOf(t *testing.T, s string) []string {
	t.Helper()
	m := map[string]float64{}
	SemanticWeightsStringsInto(m, s)
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s=%g", k, v))
	}
	sort.Strings(out)
	return out
}

func TestSemanticDecomposition(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"RealConnectionPool", []string{"connection=1", "pool=1", "real=1", "realconnectionpool=1"}},
		{"DiskLruCache", []string{"cache=1", "disk=1", "disklrucache=1", "lru=1"}},
		{"HTTPServer", []string{"http=1", "httpserver=1", "server=1"}},
		{"getURL", []string{"get=1", "geturl=1", "url=1"}},
		{"wrap_text", []string{"text=1", "wrap=1", "wrap_text=1"}},
		{"HTTP2Writer", []string{"http2=1", "http2writer=1", "writer=1"}},
		{"utf8Decode", []string{"decode=1", "utf8=1", "utf8decode=1"}},
		// A dotted reference: the whole token stays so an exact spelling still
		// matches, and the two halves appear so a partial one can.
		{"RealCall.execute", []string{"call=1", "execute=1", "real=1", "realcall.execute=1", "realcall=1"}},
		// Ordinary prose is never cut into substrings.
		{"connection", []string{"connection=1"}},
		{"the quick brown", []string{"brown=1", "quick=1", "the=1"}},
		// Every component is under the two-character floor, so only the whole
		// token survives; consecutive and trailing separators add nothing.
		{"a_b__c", []string{"a_b__c=1"}},
		{"__leading", []string{"__leading=1", "leading=1"}},
		{"trailing__", []string{"trailing=1", "trailing__=1"}},
		// A repeated component counts once per occurrence, which is what k1
		// saturation is there to answer.
		{"parser.parser", []string{"parser.parser=1", "parser=2"}},
		// Non-ASCII is a delimiter here exactly as in Weights.
		{"caf\u00e9Bar", []string{"bar=1", "caf=1"}},
		{"digits2only", []string{"digits2only=1"}},
	}
	for _, tc := range cases {
		got := tokensOf(t, tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q:\n got %v\nwant %v", tc.in, got, tc.want)
		}
	}
}

func TestSemanticDecomposesQualifiedPath(t *testing.T) {
	got := strings.Join(tokensOf(t, "okhttp3/internal/connection/RealConnectionPool.kt"), " ")
	want := "connection=2 internal=1 kt=1 " +
		"okhttp3/internal/connection/realconnectionpool.kt=1 okhttp3=1 pool=1 real=1 realconnectionpool=1"
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

// The whole token a query spells out must still be present, or an exact
// reference that used to score stops scoring.
func TestSemanticPreservesWholeToken(t *testing.T) {
	for _, s := range []string{
		"RealConnectionPool", "Http2Writer.headers", "parse_short_arg",
		"okhttp3.internal.http2.Http2Writer.headers",
	} {
		m := map[string]float64{}
		SemanticWeightsStringsInto(m, s)
		whole := map[string]float64{}
		WeightsStringInto(whole, s)
		for token := range whole {
			if m[token] < 1 {
				t.Errorf("%q: whole token %q lost", s, token)
			}
		}
	}
}

// An explicit reference and the stored qualified name have to share tokens,
// which under the whole-token rules they did not: `http2writer.headers` and
// `okhttp3.internal.http2.http2writer.headers` are two different strings.
func TestSemanticExplicitReferenceIntersectsQualifiedName(t *testing.T) {
	query := map[string]float64{}
	SemanticWeightsStringsInto(query, "Http2Writer.headers")
	indexed := map[string]float64{}
	SemanticWeightsStringsInto(indexed, "okhttp3.internal.http2.Http2Writer.headers")
	shared := 0
	for token := range query {
		if indexed[token] > 0 {
			shared++
		}
	}
	if shared < 2 {
		t.Fatalf("query and qualified name share %d tokens, want at least 2", shared)
	}
	if old := WeightsString("Http2Writer.headers"); len(old) != 1 {
		t.Fatalf("whole-token rules changed: %v", old)
	}
}

func TestSemanticWeightsStringIsSortedAndDeduplicated(t *testing.T) {
	got := SemanticWeightsString("RealCall.execute realCall")
	want := []string{"call", "execute", "real", "realcall", "realcall.execute"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// Decomposition must not reach the whole-token tokenizer, which file_tokens
// and the parser-profile contract depend on being unchanged.
func TestWeightsUnchangedByDecomposition(t *testing.T) {
	for _, s := range []string{"RealConnectionPool", "getURL", "wrap_text", "HTTP2Writer"} {
		got := WeightsString(s)
		if len(got) != 1 {
			t.Errorf("Weights(%q) = %v, want exactly the whole token", s, got)
		}
	}
}

// A symbol's prose is not decomposed; its identity is.
func TestSemanticSymbolWeightsDecomposesIdentityOnly(t *testing.T) {
	got := map[string]float64{}
	SemanticSymbolWeightsInto(got, "parseShortArg", "crate::parser::parseShortArg",
		"fn parseShortArg(self) -> ClapResult", "Reads a shortFlag cluster.")
	if got["parse"] == 0 || got["short"] == 0 || got["arg"] == 0 {
		t.Errorf("identity not decomposed: %v", got)
	}
	if got["flag"] != 0 {
		t.Errorf("doc summary was decomposed (`shortFlag` -> `flag`): %v", got)
	}
	if got["clapresult"] == 0 {
		t.Errorf("signature lost its whole token: %v", got)
	}
	if got["clap"] != 0 {
		t.Errorf("signature was decomposed (`ClapResult` -> `clap`): %v", got)
	}
}

func TestSemanticWeightsIsDeterministic(t *testing.T) {
	const s = "okhttp3.internal.connection.RealConnectionPool.callAcquirePooledConnection"
	first := map[string]float64{}
	SemanticWeightsStringsInto(first, s)
	for i := 0; i < 50; i++ {
		again := map[string]float64{}
		SemanticWeightsStringsInto(again, s)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs", i)
		}
	}
}

// A token longer than the stack buffer must not corrupt the strings already
// emitted from it.
func TestSemanticVeryLongIdentifier(t *testing.T) {
	long := strings.Repeat("VeryLongSegment", 40)
	m := map[string]float64{}
	SemanticWeightsStringsInto(m, long)
	if m["very"] != 40 || m["long"] != 40 || m["segment"] != 40 {
		t.Fatalf("components miscounted: very=%v long=%v segment=%v", m["very"], m["long"], m["segment"])
	}
	if m[strings.ToLower(long)] != 1 {
		t.Fatalf("whole token lost for a %d-byte identifier", len(long))
	}
}

func TestSemanticEmptyInput(t *testing.T) {
	m := map[string]float64{}
	SemanticWeightsStringsInto(m, "", "   ", "_", "-", ".")
	for _, k := range []string{"_", "-", "."} {
		if m[k] != 0 {
			t.Errorf("single separator %q became a token", k)
		}
	}
	if len(SemanticWeightsString("")) != 0 {
		t.Error("empty query produced tokens")
	}
}
