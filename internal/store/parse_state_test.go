package store

import (
	"database/sql"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestRetireFileGraphsBatchDropsTokenOnlyGraph(t *testing.T) {
	f := newGateFixture(t)
	if _, err := f.store.ReplaceFileGraphsBatch(f.ctx, f.repoID, 1, []ReplaceFileGraphInput{{
		Path: "tokens.go", Language: "go", SizeBytes: 10, MtimeUnixNS: 7, ContentHash: "h",
		ParserProfile: "treesitter:go:v1", ParserCallEdges: true,
		Parsed: graph.ParsedFile{Language: "go", FileTokens: map[string]float64{"alpha": 1, "beta": 1}},
	}}); err != nil {
		t.Fatalf("ReplaceFileGraphsBatch() error = %v", err)
	}
	var fileID int64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT id FROM files WHERE repo_id = ? AND path = 'tokens.go'`, f.repoID).Scan(&fileID); err != nil {
		t.Fatalf("file lookup error = %v", err)
	}
	var tokens int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM file_tokens WHERE file_id = ?`, fileID).Scan(&tokens); err != nil {
		t.Fatalf("token count error = %v", err)
	}
	if tokens == 0 {
		t.Fatal("token-only graph did not persist file_tokens")
	}
	var ownsProfile bool
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT `+FileParserOwnedEvidencePredicate+` FROM files f WHERE f.id = ?`, fileID).Scan(&ownsProfile); err != nil {
		t.Fatalf("provenance predicate error = %v", err)
	}
	if ownsProfile {
		t.Fatal("file_tokens-only graph classified as parser-profile evidence")
	}
	if groups, err := f.store.FileParserProfileGroups(f.ctx, f.repoID); err != nil {
		t.Fatalf("FileParserProfileGroups() error = %v", err)
	} else if len(groups) != 0 {
		t.Fatalf("profile groups = %#v, want none for token-only graph", groups)
	}

	firstStats := &WriteStats{}
	retired, err := f.store.RetireFileGraphsBatch(f.ctx, f.repoID, 2, []FileMetadataUpdate{{
		Path: "tokens.go", Language: "go", SizeBytes: 20, MtimeUnixNS: 8, ContentHash: "oversize",
	}}, ParseStateOversize, firstStats)
	if err != nil {
		t.Fatalf("RetireFileGraphsBatch() error = %v", err)
	}
	if retired != 0 {
		t.Fatalf("retired = %d, want 0: token-only rows are not provenance evidence", retired)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM file_tokens WHERE file_id = ?`, fileID).Scan(&tokens); err != nil {
		t.Fatalf("post-retirement token count error = %v", err)
	}
	if tokens != 0 {
		t.Fatalf("post-retirement file_tokens = %d, want 0", tokens)
	}
	if firstStats.FileGraphDeleteStatements == 0 {
		t.Fatal("first retirement did not execute graph deletion")
	}
	secondStats := &WriteStats{}
	retired, err = f.store.RetireFileGraphsBatch(f.ctx, f.repoID, 3, []FileMetadataUpdate{{
		Path: "tokens.go", Language: "go", SizeBytes: 20, MtimeUnixNS: 8, ContentHash: "oversize",
	}}, ParseStateOversize, secondStats)
	if err != nil {
		t.Fatalf("second RetireFileGraphsBatch() error = %v", err)
	}
	if retired != 0 || secondStats.FileGraphDeleteStatements != 0 {
		t.Fatalf("repeat retirement = retired %d, delete statements %d; want 0, 0", retired, secondStats.FileGraphDeleteStatements)
	}
}

func TestRetireFileGraphsBatchMixedOwnership(t *testing.T) {
	f := newGateFixture(t)
	empty := f.file(t, "empty.go", "go")
	if _, err := f.store.ReplaceFileGraphsBatch(f.ctx, f.repoID, 1, []ReplaceFileGraphInput{
		{Path: "symbol.go", Language: "go", SizeBytes: 1, Parsed: graph.ParsedFile{Language: "go", Symbols: []graph.Symbol{{Language: "go", Kind: "function", Name: "A", QualifiedName: "A", StableKey: "go:A"}}}},
		{Path: "tokens.go", Language: "go", SizeBytes: 1, Parsed: graph.ParsedFile{Language: "go", FileTokens: map[string]float64{"alpha": 1}}},
	}); err != nil {
		t.Fatalf("ReplaceFileGraphsBatch() error = %v", err)
	}
	retired, err := f.store.RetireFileGraphsBatch(f.ctx, f.repoID, 2, []FileMetadataUpdate{
		{Path: "symbol.go", Language: "go"},
		{Path: "tokens.go", Language: "go"},
		{Path: "empty.go", Language: "go"},
	}, ParseStateFailed, nil)
	if err != nil {
		t.Fatalf("RetireFileGraphsBatch() error = %v", err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1 resolver-evidence file", retired)
	}
	for _, path := range []string{"symbol.go", "tokens.go", "empty.go"} {
		var n int
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM symbols s JOIN files f ON f.id = s.file_id WHERE f.path = ?`, path).Scan(&n); err != nil {
			t.Fatalf("symbol count for %s: %v", path, err)
		}
		if n != 0 {
			t.Errorf("symbols for %s = %d, want 0", path, n)
		}
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM file_tokens t JOIN files f ON f.id = t.file_id WHERE f.path = ?`, path).Scan(&n); err != nil {
			t.Fatalf("token count for %s: %v", path, err)
		}
		if n != 0 {
			t.Errorf("file_tokens for %s = %d, want 0", path, n)
		}
	}
	_ = empty
}

func TestParseStateDescribesCurrentBytes(t *testing.T) {
	for state, want := range map[string]bool{
		ParseStateIndexed:  true,
		ParseStateSkipped:  true,
		ParseStateOversize: false,
		ParseStateFailed:   false,
		ParseStatePending:  false,
		ParseStateDeleted:  false,
		"":                 false,
		"something-new":    false,
	} {
		if got := ParseStateDescribesCurrentBytes(state); got != want {
			t.Errorf("ParseStateDescribesCurrentBytes(%q) = %v, want %v", state, got, want)
		}
	}
}

// The indexer's re-entry decision is only as good as what the change-detection
// load hands it, so the projection has to carry parse_state.
func TestExistingFilesCarryParseState(t *testing.T) {
	f := newGateFixture(t)
	if _, err := f.store.ReplaceFileGraphsBatch(f.ctx, f.repoID, 1, []ReplaceFileGraphInput{{
		Path: "a.go", Language: "go", SizeBytes: 3, MtimeUnixNS: 7, ContentHash: "h",
		Parsed: graph.ParsedFile{Language: "go", Symbols: []graph.Symbol{{
			Language: "go", Kind: "function", Name: "A", QualifiedName: "A", StableKey: "go:A",
		}}},
	}}); err != nil {
		t.Fatalf("ReplaceFileGraphsBatch() error = %v", err)
	}

	assertState := func(where string, want string) {
		t.Helper()
		for _, load := range []struct {
			name string
			fn   func() (map[string]ExistingFileMeta, error)
		}{
			{"ExistingFiles", func() (map[string]ExistingFileMeta, error) {
				return f.store.ExistingFiles(f.ctx, f.repoID)
			}},
			{"ExistingFilesForPaths", func() (map[string]ExistingFileMeta, error) {
				return f.store.ExistingFilesForPaths(f.ctx, f.repoID, []string{"a.go"})
			}},
		} {
			existing, err := load.fn()
			if err != nil {
				t.Fatalf("%s() error = %v", load.name, err)
			}
			if got := existing["a.go"].ParseState; got != want {
				t.Errorf("%s parse_state %s = %q, want %q", load.name, where, got, want)
			}
		}
	}
	assertState("after replace", ParseStateIndexed)

	retired, err := f.store.RetireFileGraphsBatch(f.ctx, f.repoID, 2, []FileMetadataUpdate{{
		Path: "a.go", Language: "go", SizeBytes: 9999, MtimeUnixNS: 11,
	}}, ParseStateOversize, nil)
	if err != nil {
		t.Fatalf("RetireFileGraphsBatch() error = %v", err)
	}
	if retired != 1 {
		t.Errorf("retired = %d, want 1", retired)
	}
	assertState("after retirement", ParseStateOversize)
}

func TestRetireFileGraphsBatchDropsGraphAndUnbindsInbound(t *testing.T) {
	f := newGateFixture(t)
	if _, err := f.store.ReplaceFileGraphsBatch(f.ctx, f.repoID, 1, []ReplaceFileGraphInput{{
		Path: "target.go", Language: "go", SizeBytes: 3, MtimeUnixNS: 7, ContentHash: "h",
		ParserProfile: "treesitter:go:v1", ParserCallEdges: true,
		Parsed: graph.ParsedFile{
			Language: "go",
			Symbols: []graph.Symbol{{
				Language: "go", Kind: "function", Name: "Foo", QualifiedName: "Foo", StableKey: "go:Foo",
				Range: graph.Position{StartLine: 1, EndLine: 20},
			}},
			References: []graph.Reference{{Kind: "identifier", Name: "Bar"}},
			Imports:    []string{"fmt"},
		},
	}}); err != nil {
		t.Fatalf("ReplaceFileGraphsBatch() error = %v", err)
	}
	var symbolID int64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT id FROM symbols WHERE name = 'Foo'`).Scan(&symbolID); err != nil {
		t.Fatalf("symbol lookup error = %v", err)
	}
	callerFile := f.file(t, "caller.go", "go")
	caller := f.symbol(t, callerFile, "Caller", "Caller", "go")
	edgeID := f.edge(t, callerFile, caller, "Foo")
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE edges SET dst_symbol_id = ?, resolution_strategy = 'exact', resolution_confidence = 'high' WHERE id = ?`,
		symbolID, edgeID); err != nil {
		t.Fatalf("bind edge error = %v", err)
	}

	retired, err := f.store.RetireFileGraphsBatch(f.ctx, f.repoID, 2, []FileMetadataUpdate{{
		Path: "target.go", Language: "go", SizeBytes: 4096, MtimeUnixNS: 11,
	}}, ParseStateFailed, nil)
	if err != nil {
		t.Fatalf("RetireFileGraphsBatch() error = %v", err)
	}
	if retired != 1 {
		t.Errorf("retired = %d, want 1 (the file held parser-owned evidence)", retired)
	}

	// The row survives, truthfully, and claims no parser.
	var (
		state     string
		size      int64
		deleted   int
		profile   string
		callEdges int
	)
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT parse_state, size_bytes, is_deleted, parser_profile, parser_call_edges FROM files WHERE repo_id = ? AND path = 'target.go'`,
		f.repoID).Scan(&state, &size, &deleted, &profile, &callEdges); err != nil {
		t.Fatalf("file row error = %v", err)
	}
	if state != ParseStateFailed || size != 4096 || deleted != 0 {
		t.Errorf("file row = (%q, %d, deleted=%d), want (failed, 4096, deleted=0)", state, size, deleted)
	}
	if profile != "" || callEdges != 0 {
		t.Errorf("parser provenance = (%q, %d), want cleared", profile, callEdges)
	}

	// Every parser-owned fact is gone.
	for _, q := range []string{
		`SELECT COUNT(*) FROM symbols WHERE file_id IN (SELECT id FROM files WHERE path = 'target.go')`,
		`SELECT COUNT(*) FROM references_tbl WHERE file_id IN (SELECT id FROM files WHERE path = 'target.go')`,
		`SELECT COUNT(*) FROM file_imports WHERE file_id IN (SELECT id FROM files WHERE path = 'target.go')`,
	} {
		var n int
		if err := f.store.db.QueryRowContext(f.ctx, q).Scan(&n); err != nil {
			t.Fatalf("count error = %v (%s)", err, q)
		}
		if n != 0 {
			t.Errorf("rows remaining for %s: %d, want 0", q, n)
		}
	}

	// And nothing still points at the symbol that disappeared.
	var dst sql.NullInt64
	var strategy string
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT dst_symbol_id, resolution_strategy FROM edges WHERE id = ?`, edgeID).Scan(&dst, &strategy); err != nil {
		t.Fatalf("edge lookup error = %v", err)
	}
	if dst.Valid {
		t.Errorf("inbound edge still bound to %d", dst.Int64)
	}
	if strategy != "" {
		t.Errorf("resolution_strategy = %q, want cleared", strategy)
	}

	// Retiring again is a no-op: nothing left to remove, and the caller is told
	// so, because dispatching a repo-wide invalidation for it would make every
	// update over a permanently broken file pay for one.
	retired, err = f.store.RetireFileGraphsBatch(f.ctx, f.repoID, 3, []FileMetadataUpdate{{
		Path: "target.go", Language: "go", SizeBytes: 4096, MtimeUnixNS: 11,
	}}, ParseStateFailed, nil)
	if err != nil {
		t.Fatalf("second RetireFileGraphsBatch() error = %v", err)
	}
	if retired != 0 {
		t.Errorf("second retirement reported %d retired, want 0", retired)
	}
}

// A retirement is a lifecycle transition, not a free-text state write: only the
// two states that mean "present but holding no parser-owned graph" are legal.
func TestRetireFileGraphsBatchRefusesOtherStates(t *testing.T) {
	f := newGateFixture(t)
	f.file(t, "a.go", "go")
	for _, state := range []string{ParseStateIndexed, ParseStateSkipped, ParseStateDeleted, ParseStatePending, ""} {
		_, err := f.store.RetireFileGraphsBatch(f.ctx, f.repoID, 1, []FileMetadataUpdate{{Path: "a.go", Language: "go"}}, state, nil)
		if err == nil {
			t.Errorf("RetireFileGraphsBatch(%q) error = nil, want refusal", state)
		}
	}
}
