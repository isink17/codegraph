package store

import (
	"context"
	"fmt"

	"github.com/isink17/codegraph/internal/graph"
)

// Generator half of the ~100k-symbol query-latency fixture. See
// query_latency_fixture_test.go for what the shape is trying to model and why.

// writeBigGraph persists the whole fixture through the production batch write
// path. Hub and hot files are written first only for readability; correctness
// does not depend on file order, because edge and test-link binding are Pass-2
// operations over the completed symbol table.
func writeBigGraph(ctx context.Context, s *Store, repoID int64, f *bigGraphFixture) error {
	const chunk = 96
	inputs := make([]ReplaceFileGraphInput, 0, chunk)
	stats := &WriteStats{}
	flush := func() error {
		if len(inputs) == 0 {
			return nil
		}
		if _, err := s.ReplaceFileGraphsBatchWithStats(ctx, repoID, 1, inputs, stats); err != nil {
			return err
		}
		inputs = inputs[:0]
		return nil
	}

	inputs = append(inputs, bigHubFile(), bigHotFile())
	if err := flush(); err != nil {
		return err
	}

	for p := 0; p < bigPkgCount; p++ {
		for j := 0; j < bigFilesPerPkg; j++ {
			inputs = append(inputs, bigSourceFile(p, j))
			if len(inputs) == chunk {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}

	for t := 0; t < bigTestFileCount; t++ {
		inputs = append(inputs, bigTestFile(t))
		if len(inputs) == chunk {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// bigVocab is a small domain vocabulary the fixture draws documentation and
// signature words from.
//
// Giving every symbol the *same* doc summary would be the easy thing to do and
// would also be a lie: it makes each vocabulary token appear on all ~100k
// symbols, so an FTS or token-overlap query degenerates into "match the entire
// table" and the resulting latency measures a property of the fixture rather
// than of the query. A real repository's vocabulary is spread out. Drawing a
// handful of words per symbol from this pool reproduces that spread while
// staying fully deterministic.
var bigVocab = []string{
	"request", "response", "handler", "router", "session", "token", "cache", "queue",
	"worker", "scheduler", "retry", "backoff", "timeout", "deadline", "context", "tracing",
	"metric", "counter", "histogram", "logger", "config", "option", "registry", "factory",
	"decoder", "encoder", "codec", "payload", "header", "cookie", "cursor", "paging",
	"migration", "schema", "column", "index", "transaction", "rollback", "commit", "replica",
	"shard", "partition", "bucket", "blob", "stream", "buffer", "channel", "mutex",
	"semaphore", "pool", "lease", "quota", "throttle", "circuit", "breaker", "fallback",
	"validator", "sanitizer", "parser", "lexer", "token", "grammar", "visitor", "walker",
}

// bigWords deterministically selects n vocabulary words for global symbol g.
func bigWords(g, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, bigVocab[(g*7+i*23+i*i)%len(bigVocab)])
	}
	return out
}

func bigFnSymbol(lang, name, qname, container, stable string, m, g int) graph.Symbol {
	words := bigWords(g, 6)
	return graph.Symbol{
		Language:      lang,
		Kind:          "function",
		Name:          name,
		QualifiedName: qname,
		ContainerName: container,
		Signature: fmt.Sprintf("func %s(ctx context.Context, %s *%s) (*%s, error)",
			name, words[0], words[1], words[2]),
		DocSummary: fmt.Sprintf("%s reconciles the %s %s against the %s %s",
			name, words[3], words[4], words[5], words[0]),
		Range: graph.Position{
			StartLine: bigSymStartLine(m), StartCol: 1,
			EndLine: bigSymEndLine(m), EndCol: 1,
		},
		StableKey: stable,
	}
}

// bigHubFile holds the deliberately extreme shapes: the high fan-in target, the
// medium and low fan-in controls, the high fan-out source, and the deep chain.
// Keeping them in one file makes the fixture's intent readable and keeps their
// names stable for the benchmark table.
func bigHubFile() ReplaceFileGraphInput {
	pf := graph.ParsedFile{Language: "go"}
	add := func(m int, name string) {
		qname := "hub." + name
		pf.Symbols = append(pf.Symbols, bigFnSymbol("go", name, qname, "", fmt.Sprintf("go:%s:%d", qname, m), m, bigHubG+m))
	}
	add(0, "HubTarget")
	add(1, "MediumTarget")
	add(2, "LowTarget")
	add(3, "HubSource")
	add(4, "HubUnresolvedSource")
	for i := 0; i < bigChainLen; i++ {
		add(5+i, fmt.Sprintf("Chain%03d", i))
	}

	// High fan-out: one source symbol calling thousands of distinct targets.
	srcLine := bigSymStartLine(3) + 2
	for k := 0; k < bigHubOutDegree; k++ {
		p := bigGoPkg(k)
		j := (k * 3) % bigFilesPerPkg
		m := (k * 11) % bigSymsPerFile
		qname, _ := bigSymbolQName(p, j, m)
		pf.Edges = append(pf.Edges, graph.Edge{DstName: qname, Kind: "calls", Evidence: "fixture", Line: srcLine})
	}
	// High fan-out of *unresolvable* names: the shape that exercises the
	// callee fallback path, where each distinct dst_name has to be looked up
	// against the symbol table because no resolver bound it.
	unresolvedLine := bigSymStartLine(4) + 2
	for k := 0; k < bigUnresolvedOutDegree; k++ {
		if k%2 == 0 {
			pf.Edges = append(pf.Edges, graph.Edge{
				DstName:  fmt.Sprintf("extlib.Vendor%04d.Call%04d", k, k),
				Kind:     "calls",
				Evidence: "fixture",
				Line:     unresolvedLine,
			})
			continue
		}
		pf.Edges = append(pf.Edges, graph.Edge{
			// Half of these are snake_case on purpose: '_' is a LIKE
			// metacharacter, and a lookup path that special-cases it would
			// otherwise never be measured.
			DstName:  fmt.Sprintf("extlib.vendor_%04d.call_%04d", k, k),
			Kind:     "calls",
			Evidence: "fixture",
			Line:     unresolvedLine,
		})
	}
	// Deep-but-bounded chain: Chain000 -> Chain001 -> ... -> Chain299.
	for i := 0; i < bigChainLen-1; i++ {
		pf.Edges = append(pf.Edges, graph.Edge{
			DstName:  fmt.Sprintf("hub.Chain%03d", i+1),
			Kind:     "calls",
			Evidence: "fixture",
			Line:     bigSymStartLine(5+i) + 2,
		})
	}
	pf.FileTokens = map[string]float64{"hub": 1, "router": 0.5}
	return ReplaceFileGraphInput{
		Path:        "internal/hub/hub.go",
		Language:    "go",
		SizeBytes:   4096,
		ContentHash: "hub",
		Parsed:      pf,
	}
}

// bigHotFile is the target of the concentrated test-link fan-out.
func bigHotFile() ReplaceFileGraphInput {
	pf := graph.ParsedFile{Language: "go"}
	for i := 0; i < bigHotSymCount; i++ {
		name := fmt.Sprintf("HotSym%02d", i)
		qname := "hot." + name
		pf.Symbols = append(pf.Symbols, bigFnSymbol("go", name, qname, "", fmt.Sprintf("go:%s:%d", qname, i), i, bigHotG+i))
	}
	pf.FileTokens = map[string]float64{"hot": 1, "cache": 0.5}
	return ReplaceFileGraphInput{
		Path:        "internal/hot/hot.go",
		Language:    "go",
		SizeBytes:   1024,
		ContentHash: "hot",
		Parsed:      pf,
	}
}

func bigHotStableKey(i int) string {
	return fmt.Sprintf("go:hot.HotSym%02d:%d", i, i)
}

// bigEdgeTarget picks a same-language destination. Stepping the package index
// by 10 preserves p%10, which is what bigLangForPkg keys off, so a Go file
// never emits an edge that the language gate would (correctly) refuse to bind.
func bigEdgeTarget(p, g, k int) (int, int, int) {
	p2 := (p + 10*((g*7+k*13)%16)) % bigPkgCount
	j2 := (g*3 + k*5) % bigFilesPerPkg
	m2 := (g*11 + k*17) % bigSymsPerFile
	return p2, j2, m2
}

func bigSourceFile(p, j int) ReplaceFileGraphInput {
	lang, _ := bigLangForPkg(p)
	pf := graph.ParsedFile{Language: lang}

	fnIdx := 0
	for m := 0; m < bigSymsPerFile; m++ {
		name := bigSymbolName(p, j, m)
		qname, container := bigSymbolQName(p, j, m)
		kind := bigSymbolKind(m)
		g := (p*bigFilesPerPkg+j)*bigSymsPerFile + m

		sym := bigFnSymbol(lang, name, qname, container, bigStableKey(p, j, m), m, g)
		sym.Kind = kind
		pf.Symbols = append(pf.Symbols, sym)
		pf.References = append(pf.References, graph.Reference{
			Kind:          "call",
			Name:          name,
			QualifiedName: qname,
			Range: graph.Position{
				StartLine: bigSymStartLine(m) + 3, StartCol: 1,
				EndLine: bigSymStartLine(m) + 3, EndCol: 4,
			},
		})

		if kind != "function" {
			continue
		}
		// Non-uniform out-degree: most functions are low-degree, a few are
		// medium, one is near-leaf.
		out := 2
		switch {
		case fnIdx >= 15:
			out = 1
		case fnIdx >= 12:
			out = 8
		}
		line := bigSymStartLine(m) + 2
		for k := 0; k < out; k++ {
			var dst string
			if (g+k)%10 == 7 {
				// Realistic external/unresolvable target.
				dst = fmt.Sprintf("extlib.Absent%06d", (g*3+k)%50000)
			} else {
				tp, tj, tm := bigEdgeTarget(p, g, k)
				dst, _ = bigSymbolQName(tp, tj, tm)
			}
			pf.Edges = append(pf.Edges, graph.Edge{DstName: dst, Kind: "calls", Evidence: "fixture", Line: line})
		}
		fnIdx++
	}

	// Hub / medium / low fan-in contributions. Only Go files contribute, so the
	// language gate binds every one of them.
	if p%10 < 6 {
		// Each hub edge is attributed to a *different* function in the file, so
		// the hub's in-degree is a count of distinct callers rather than a
		// count of repeated edges from one caller. That distinction is the
		// whole point of the scenario: FindCallers' cost tracks distinct
		// callers, and a fixture that piled every edge onto one source symbol
		// would report a hub that is cheap for the wrong reason.
		fnLines := bigFunctionLines()
		for k := 0; k < bigHubInPerFile && k < len(fnLines); k++ {
			pf.Edges = append(pf.Edges, graph.Edge{DstName: "hub.HubTarget", Kind: "calls", Evidence: "fixture", Line: fnLines[k]})
		}
		if (p*bigFilesPerPkg+j)%bigMediumEveryNth == 0 {
			pf.Edges = append(pf.Edges, graph.Edge{DstName: "hub.MediumTarget", Kind: "calls", Evidence: "fixture", Line: fnLines[0]})
		}
		if p == 0 && j < 3 {
			pf.Edges = append(pf.Edges, graph.Edge{DstName: "hub.LowTarget", Kind: "calls", Evidence: "fixture", Line: fnLines[0]})
		}
	}

	pf.Imports = []string{fmt.Sprintf("internal/pkg%03d", (p+1)%bigPkgCount), "context"}
	words := bigWords(p*bigFilesPerPkg+j, 3)
	pf.FileTokens = map[string]float64{
		words[0]:                   1,
		words[1]:                   0.5,
		fmt.Sprintf("pkg%03d", p):  1,
		fmt.Sprintf("file%02d", j): 0.25,
	}
	return ReplaceFileGraphInput{
		Path:        bigSrcPath(p, j),
		Language:    lang,
		SizeBytes:   int64(2048 + m0(p, j)),
		ContentHash: fmt.Sprintf("src-%03d-%02d", p, j),
		Parsed:      pf,
	}
}

func m0(p, j int) int { return (p*7 + j*13) % 4096 }

// bigFunctionLines returns a line inside each function symbol of a source file,
// in symbol order.
func bigFunctionLines() []int {
	out := make([]int, 0, bigSymsPerFile)
	for m := 0; m < bigSymsPerFile; m++ {
		if bigSymbolKind(m) == "function" {
			out = append(out, bigSymStartLine(m)+2)
		}
	}
	return out
}

// bigTestFile emits a Go test file whose links target either the hot file (for
// the first bigHotTestFiles files, producing the concentrated fan-out) or an
// ordinary Go source file.
func bigTestFile(t int) ReplaceFileGraphInput {
	p := bigGoPkg(t % 96)
	pf := graph.ParsedFile{Language: "go"}
	for m := 0; m < bigSymsPerTest; m++ {
		name := fmt.Sprintf("TestFn%06d", t*bigSymsPerTest+m)
		qname := fmt.Sprintf("pkg%03d.%s", p, name)
		stable := fmt.Sprintf("go:%s:t%d", qname, t*bigSymsPerTest+m)
		pf.Symbols = append(pf.Symbols, bigFnSymbol("go", name, qname, "", stable, m, bigTestG+t*bigSymsPerTest+m))

		var targetKey, targetName string
		if t < bigHotTestFiles {
			targetKey = bigHotStableKey(m % bigHotSymCount)
			targetName = fmt.Sprintf("hot.HotSym%02d", m%bigHotSymCount)
		} else {
			tp := bigGoPkg((t*3 + m) % 96)
			tj := (t*5 + m*7) % bigFilesPerPkg
			tm := (t*11 + m*3) % bigSymsPerFile
			targetKey = bigStableKey(tp, tj, tm)
			targetName, _ = bigSymbolQName(tp, tj, tm)
		}
		pf.TestLinks = append(pf.TestLinks, graph.TestLink{
			TestName:        name,
			TargetName:      targetName,
			Reason:          "name_match",
			Score:           0.9 - float64(m)*0.01,
			TestSymbolKey:   stable,
			TargetStableKey: targetKey,
		})
	}
	pf.FileTokens = map[string]float64{"test": 1, "harness": 0.5}
	return ReplaceFileGraphInput{
		Path:        fmt.Sprintf("internal/pkg%03d/gen_%03d_test.go", p, t),
		Language:    "go",
		SizeBytes:   1024,
		ContentHash: fmt.Sprintf("test-%03d", t),
		Parsed:      pf,
	}
}
