package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/store"
)

// pathCanary is one legal-but-unusual repository filename and the unique
// symbol its source declares, so path ownership is provable per file.
type pathCanary struct {
	// nativeRel is the host-native relative spelling used to create the file.
	nativeRel string
	pkg       string
	symbol    string
	// posixOnly marks a filename whose bytes are legal only where '\' is data.
	posixOnly bool
}

var legalPathCanaries = []pathCanary{
	{nativeRel: filepath.Join("src", "plain.go"), pkg: "src", symbol: "PlainCanary"},
	{nativeRel: filepath.Join("dir with space", "file with space.go"), pkg: "spacedir", symbol: "SpaceCanary"},
	{nativeRel: filepath.Join("unicode-ž", "λ_file.go"), pkg: "unicodedir", symbol: "UnicodeCanary"},
	{nativeRel: filepath.Join("nested", "x", "y.go"), pkg: "x", symbol: "SlashNestedCanary"},
	{nativeRel: filepath.Join("nested", `x\y.go`), pkg: "nested", symbol: "BackslashCanary", posixOnly: true},
}

func canarySource(pkg, symbol string) string {
	return "package " + pkg + "\n\nfunc " + symbol + "() {}\n"
}

// walkLogicalPaths enumerates the real filesystem and applies only the
// separator conversion the platform contract performs at the native->logical
// boundary. On POSIX that conversion is the identity, so a literal backslash
// survives; on Windows the native separator becomes '/'.
func walkLogicalPaths(t *testing.T, repoRoot string) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// canaryDB reads the exact persisted state that the proof protects.
type canaryDB struct {
	t      *testing.T
	raw    *sql.DB
	repoID int64
}

func openCanaryDB(t *testing.T, dbPath, canonical string) *canaryDB {
	t.Helper()
	raw, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var repoID int64
	if err := raw.QueryRow(`SELECT id FROM repos WHERE canonical_path = ?`, canonical).Scan(&repoID); err != nil {
		_ = raw.Close()
		t.Fatalf("repo row: %v", err)
	}
	return &canaryDB{t: t, raw: raw, repoID: repoID}
}

// close releases the raw handle promptly: `index --rebuild` removes the
// database file, and an open handle would block that on Windows.
func (c *canaryDB) close() { _ = c.raw.Close() }

func (c *canaryDB) marker() string {
	c.t.Helper()
	var value string
	if err := c.raw.QueryRow(`SELECT value FROM settings WHERE key = ?`, "format.canonical_repository_paths.v1."+strconv.FormatInt(c.repoID, 10)).Scan(&value); err != nil {
		c.t.Fatalf("repository path-format marker for repo %d: %v", c.repoID, err)
	}
	return value
}

// livePaths returns the exact bytes of files.path for the repo, and asserts
// there is no duplicate row under any spelling.
func (c *canaryDB) livePaths() map[string]struct{} {
	c.t.Helper()
	var total, distinct int
	if err := c.raw.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT path) FROM files WHERE repo_id = ? AND is_deleted = 0`, c.repoID).Scan(&total, &distinct); err != nil {
		c.t.Fatal(err)
	}
	if total != distinct {
		c.t.Fatalf("files.path not unique for repo %d: COUNT(*)=%d COUNT(DISTINCT path)=%d", c.repoID, total, distinct)
	}
	rows, err := c.raw.Query(`SELECT path FROM files WHERE repo_id = ? AND is_deleted = 0 ORDER BY path`, c.repoID)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			c.t.Fatal(err)
		}
		out[p] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		c.t.Fatal(err)
	}
	return out
}

func (c *canaryDB) symbolPaths(name string) []string {
	c.t.Helper()
	rows, err := c.raw.Query(`SELECT f.path FROM symbols s JOIN files f ON f.id = s.file_id WHERE s.repo_id = ? AND s.name = ? AND f.is_deleted = 0 ORDER BY f.path`, c.repoID, name)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		c.t.Fatal(err)
	}
	return out
}

// rowsForPath returns is_deleted for every files row with this exact path,
// including soft-deleted rows, so a re-add that inserted a duplicate row
// instead of reviving the identity is visible.
func (c *canaryDB) rowsForPath(path string) []int {
	c.t.Helper()
	rows, err := c.raw.Query(`SELECT is_deleted FROM files WHERE repo_id = ? AND path = ? ORDER BY id`, c.repoID, path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var d int
		if err := rows.Scan(&d); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		c.t.Fatal(err)
	}
	return out
}

func assertPathSet(t *testing.T, label string, got map[string]struct{}, want map[string]struct{}) {
	t.Helper()
	g, w := sortedKeys(got), sortedKeys(want)
	if len(g) != len(w) {
		t.Fatalf("%s: path set mismatch\n got  %q\n want %q", label, g, w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("%s: path set mismatch\n got  %q\n want %q", label, g, w)
		}
	}
}

// TestLegalRepositoryPathCanaryLifecycle proves, on a real temporary
// repository, that legal but unusual filenames (spaces, Unicode, nested
// directories and, on POSIX, a literal backslash filename component) keep a
// byte-exact logical identity through full index, symbol lookup, the public
// path-input boundary, incremental update, exact deletion, re-add and
// `index --rebuild`. Only the literal-backslash fixture is host-conditional;
// every other case runs on Windows too.
func TestLegalRepositoryPathCanaryLifecycle(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })

	// PHASE 2: real filesystem fixture.
	var active []pathCanary
	for _, c := range legalPathCanaries {
		if c.posixOnly && runtime.GOOS == "windows" {
			t.Logf("skip fixture %q: '\\' is a path separator on windows, not filename data", c.nativeRel)
			continue
		}
		full := filepath.Join(repoRoot, c.nativeRel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("host cannot create directory for %q: %v", c.nativeRel, err)
		}
		if err := os.WriteFile(full, []byte(canarySource(c.pkg, c.symbol)), 0o644); err != nil {
			t.Fatalf("host cannot create %q: %v", c.nativeRel, err)
		}
		active = append(active, c)
	}
	if len(active) < 4 {
		t.Fatalf("fixture too small: %d canaries", len(active))
	}

	// Expected identities come from the filesystem entries that were actually
	// created, converted only at the native->logical separator boundary, and
	// must equal the bytes the fixture wrote: a host that rewrote the Unicode
	// spelling would fail here rather than silently redefine the expectation.
	expected := walkLogicalPaths(t, repoRoot)
	if len(expected) != len(active) {
		t.Fatalf("filesystem enumerates %d files, fixture wrote %d: %q", len(expected), len(active), sortedKeys(expected))
	}
	symbolPath := map[string]string{}
	for _, c := range active {
		logical := filepath.ToSlash(c.nativeRel)
		if _, ok := expected[logical]; !ok {
			t.Fatalf("fixture %q not enumerated by filesystem under the same bytes; entries: %q", logical, sortedKeys(expected))
		}
		symbolPath[c.symbol] = logical
	}
	spacePath := symbolPath["SpaceCanary"]
	unicodePath := symbolPath["UnicodeCanary"]
	slashPath := symbolPath["SlashNestedCanary"]
	backslashPath, hasBackslash := symbolPath["BackslashCanary"]
	if spacePath != "dir with space/file with space.go" {
		t.Fatalf("space canary identity = %q", spacePath)
	}
	if slashPath != "nested/x/y.go" {
		t.Fatalf("nested canary identity = %q", slashPath)
	}
	if hasBackslash && backslashPath != `nested/x\y.go` {
		t.Fatalf("backslash canary identity = %q", backslashPath)
	}
	if unicodePath != "unicode-\u017e/\u03bb_file.go" {
		t.Fatalf("unicode canary identity = %q", unicodePath)
	}

	var out, errOut bytes.Buffer
	run := func(args ...string) {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if err := Run(ctx, args, &out, &errOut); err != nil {
			t.Fatalf("Run(%q) error = %v\nstderr: %s", args, err, errOut.String())
		}
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.CanonicalRepoPath(repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Every assertion below re-derives the observable identity from the
	// database and the query surfaces; numeric file IDs are never pinned.
	verify := func(label string, wantPaths map[string]struct{}, wantSymbols map[string]string) {
		t.Helper()
		dbPath, err := dbPathForRepo(cfg, repoRoot, canonical)
		if err != nil {
			t.Fatal(err)
		}
		db := openCanaryDB(t, dbPath, canonical)
		defer db.close()
		if m := db.marker(); m != "logical-slash-v1" {
			t.Fatalf("%s: marker = %q", label, m)
		}
		live := db.livePaths()
		assertPathSet(t, label+": files.path", live, wantPaths)
		for p := range live {
			if filepath.IsAbs(p) || strings.Contains(p, filepath.ToSlash(repoRoot)) {
				t.Fatalf("%s: absolute or checkout-prefixed files.path %q", label, p)
			}
		}
		for sym, want := range wantSymbols {
			if got := db.symbolPaths(sym); len(got) != 1 || got[0] != want {
				t.Fatalf("%s: symbol %s owned by %q, want exactly [%q]", label, sym, got, want)
			}
		}
		// PHASE 5/6: Service read surfaces agree with the persisted identity.
		app, _, repoID, err := openApp(ctx, cfg, repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close()
		for sym, want := range wantSymbols {
			res, err := app.Query.FindSymbolExactResult(ctx, repoID, sym, 10, 0)
			if err != nil {
				t.Fatalf("%s: FindSymbolExactResult(%s): %v", label, sym, err)
			}
			if !res.Matched || len(res.Matches) != 1 || res.Matches[0].FilePath != want {
				t.Fatalf("%s: FindSymbolExact(%s) = matched=%v %+v, want FilePath %q", label, sym, res.Matched, res.Matches, want)
			}
		}
		// Search is FTS token based, so each symbol is searched by its own name;
		for sym, want := range wantSymbols {
			search, err := app.Query.SearchSymbolsResult(ctx, repoID, sym, 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			owners := map[string]struct{}{}
			for _, m := range search.Matches {
				if m.Name == sym {
					owners[m.FilePath] = struct{}{}
				}
			}
			if _, ok := owners[want]; !ok || len(owners) != 1 {
				t.Fatalf("%s: search owners of %s = %q, want exactly [%q]", label, sym, sortedKeys(owners), want)
			}
		}
		listed := map[string]struct{}{}
		files, err := app.Query.ListFiles(ctx, repoID, "", 1000, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			p, ok := f["path"].(string)
			if !ok {
				t.Fatalf("%s: ListFiles path is %T, want string", label, f["path"])
			}
			listed[p] = struct{}{}
		}
		assertPathSet(t, label+": ListFiles", listed, wantPaths)
		all, err := app.Store.AllFilePaths(ctx, repoID)
		if err != nil {
			t.Fatal(err)
		}
		allSet := map[string]struct{}{}
		for _, p := range all {
			allSet[p] = struct{}{}
		}
		assertPathSet(t, label+": AllFilePaths", allSet, wantPaths)
		page, err := app.Query.ExportSymbolsPage(ctx, repoID, 1000, 0)
		if err != nil {
			t.Fatal(err)
		}
		exportOwners := map[string]string{}
		for _, s := range page {
			exportOwners[s.Name] = s.FilePath
		}
		for sym, want := range wantSymbols {
			if exportOwners[sym] != want {
				t.Fatalf("%s: ExportSymbolsPage owner of %s = %q, want %q", label, sym, exportOwners[sym], want)
			}
		}
		// PHASE 7: public path input. Presence binds byte-exact, so a spaced,
		// Unicode or literal-backslash spelling must address only its own row.
		requested := sortedKeys(wantPaths)
		presence, err := app.Query.RelatedTestsForFilesResult(ctx, repoID, requested, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if presence.Found != len(requested) || len(presence.Missing) != 0 {
			t.Fatalf("%s: public path presence found=%d missing=%q for %q", label, presence.Found, presence.Missing, requested)
		}
	}

	// PHASE 3: initial full index through the real CLI.
	run("index", repoRoot)
	verify("full index", expected, symbolPath)

	// PHASE 7 (POSIX): the two siblings are distinct public identities.
	if hasBackslash {
		app, _, repoID, err := openApp(ctx, cfg, repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		// `nested\x\y.go` names no row; a lookup that folded '\' to '/'
		// would resolve it (and the backslash sibling) onto nested/x/y.go.
		presence, err := app.Query.RelatedTestsForFilesResult(ctx, repoID, []string{backslashPath, slashPath, `nested\x\y.go`}, 10, 0)
		app.Close()
		if err != nil {
			t.Fatal(err)
		}
		if presence.Found != 2 || len(presence.Missing) != 1 || presence.Missing[0] != `nested\x\y.go` {
			t.Fatalf("sibling presence = %+v", presence)
		}
	}

	// PHASE 8: incremental content update on the unusual files.
	updated := map[string]string{}
	for sym, path := range symbolPath {
		updated[sym] = path
	}
	rename := func(sym string) {
		t.Helper()
		c := canaryBySymbol(active, sym)
		full := filepath.Join(repoRoot, c.nativeRel)
		if err := os.WriteFile(full, []byte(canarySource(c.pkg, sym+"Updated")), 0o644); err != nil {
			t.Fatal(err)
		}
		delete(updated, sym)
		updated[sym+"Updated"] = symbolPath[sym]
	}
	rename("SpaceCanary")
	rename("UnicodeCanary")
	if hasBackslash {
		rename("BackslashCanary")
	}
	// Path-scoped update takes the logical public spelling verbatim: this is
	// the indexer's Options.Paths boundary, exercised without shell quoting.
	{
		app, repo, _, err := openApp(ctx, cfg, repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		paths := []string{spacePath, unicodePath}
		if hasBackslash {
			paths = append(paths, backslashPath)
		}
		summary, err := app.Indexer.Update(ctx, indexer.Options{RepoRoot: repo.RootPath, Paths: paths, ScanKind: "update"})
		app.Close()
		if err != nil {
			t.Fatalf("path-scoped Update(%q): %v", paths, err)
		}
		if summary.FilesDeleted != 0 {
			t.Fatalf("path-scoped Update deleted %d files", summary.FilesDeleted)
		}
	}
	verify("path-scoped update", expected, updated)
	func() {
		dbPath, _ := dbPathForRepo(cfg, repoRoot, canonical)
		db := openCanaryDB(t, dbPath, canonical)
		defer db.close()
		stale := []string{"SpaceCanary", "UnicodeCanary"}
		if hasBackslash {
			stale = append(stale, "BackslashCanary")
		}
		for _, old := range stale {
			if got := db.symbolPaths(old); len(got) != 0 {
				t.Fatalf("stale symbol %s still owned by %q after update", old, got)
			}
		}
	}()
	// The CLI update path (full walk) must reach the same state.
	run("update_graph", repoRoot)
	verify("update_graph", expected, updated)

	// PHASE 9: exact delete of one unusual file.
	deleted := canaryBySymbol(active, "SpaceCanary")
	deletedPath := spacePath
	deletedSymbol := "SpaceCanaryUpdated"
	if hasBackslash {
		deleted = canaryBySymbol(active, "BackslashCanary")
		deletedPath = backslashPath
		deletedSymbol = "BackslashCanaryUpdated"
	}
	if err := os.Remove(filepath.Join(repoRoot, deleted.nativeRel)); err != nil {
		t.Fatal(err)
	}
	afterDelete := map[string]struct{}{}
	for p := range expected {
		if p != deletedPath {
			afterDelete[p] = struct{}{}
		}
	}
	afterDeleteSymbols := map[string]string{}
	for sym, p := range updated {
		if sym != deletedSymbol {
			afterDeleteSymbols[sym] = p
		}
	}
	run("update_graph", repoRoot)
	verify("delete", afterDelete, afterDeleteSymbols)
	func() {
		app, _, repoID, err := openApp(ctx, cfg, repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		res, err := app.Query.FindSymbolExactResult(ctx, repoID, deletedSymbol, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		presence, perr := app.Query.RelatedTestsForFilesResult(ctx, repoID, []string{slashPath}, 10, 0)
		app.Close()
		if perr != nil {
			t.Fatal(perr)
		}
		if res.Matched || len(res.Matches) != 0 {
			t.Fatalf("deleted symbol %s still resolvable: %+v", deletedSymbol, res.Matches)
		}
		if presence.Found != 1 || len(presence.Missing) != 0 {
			t.Fatalf("post-delete sibling presence = %+v, want %q found", presence, slashPath)
		}
		// The soft-deleted row is exactly the deleted identity, and only it.
		dbPath, _ := dbPathForRepo(cfg, repoRoot, canonical)
		db := openCanaryDB(t, dbPath, canonical)
		defer db.close()
		if rows := db.rowsForPath(deletedPath); len(rows) != 1 || rows[0] != 1 {
			t.Fatalf("deleted %q rows is_deleted = %v, want exactly [1]", deletedPath, rows)
		}
		if rows := db.rowsForPath(slashPath); len(rows) != 1 || rows[0] != 0 {
			t.Fatalf("sibling %q rows is_deleted = %v, want exactly [0]", slashPath, rows)
		}
	}()

	// PHASE 10: re-add at the exact same filesystem path.
	if err := os.WriteFile(filepath.Join(repoRoot, deleted.nativeRel), []byte(canarySource(deleted.pkg, deletedSymbol)), 0o644); err != nil {
		t.Fatal(err)
	}
	run("update_graph", repoRoot)
	verify("re-add", expected, updated)
	func() {
		dbPath, _ := dbPathForRepo(cfg, repoRoot, canonical)
		db := openCanaryDB(t, dbPath, canonical)
		defer db.close()
		if rows := db.rowsForPath(deletedPath); len(rows) != 1 || rows[0] != 0 {
			t.Fatalf("re-added %q rows is_deleted = %v, want exactly [0] (no duplicate row, no stale deletion)", deletedPath, rows)
		}
	}()

	// PHASE 11: full rebuild through the documented recovery path.
	run("index", repoRoot, "--rebuild")
	var rebuilt struct {
		Summary struct {
			Rebuild        bool     `json:"rebuild"`
			RemovedDBFiles []string `json:"removed_db_files"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(out.Bytes(), &rebuilt); err != nil {
		t.Fatalf("decode index --rebuild output: %v\n%s", err, out.String())
	}
	if !rebuilt.Summary.Rebuild || len(rebuilt.Summary.RemovedDBFiles) == 0 {
		t.Fatalf("index --rebuild did not remove and rebuild the database: %+v", rebuilt.Summary)
	}
	verify("rebuild", expected, updated)

	// PHASE 16: user-facing CLI returns the exact spelling for the spaced
	// file; argv entries are passed directly, never shell-joined.
	run("find_symbol", repoRoot, "SpaceCanaryUpdated", "--exact")
	var payload struct {
		Matches []struct {
			Name string `json:"name"`
			File string `json:"file"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode find_symbol output: %v\n%s", err, out.String())
	}
	if len(payload.Matches) != 1 || payload.Matches[0].File != spacePath {
		t.Fatalf("CLI find_symbol file = %+v, want %q", payload.Matches, spacePath)
	}
	if hasBackslash {
		run("find_symbol", repoRoot, "BackslashCanaryUpdated", "--exact")
		if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Matches) != 1 || payload.Matches[0].File != backslashPath {
			t.Fatalf("CLI find_symbol file = %+v, want %q", payload.Matches, backslashPath)
		}
	}
}

func canaryBySymbol(cs []pathCanary, symbol string) pathCanary {
	for _, c := range cs {
		if c.symbol == symbol {
			return c
		}
	}
	panic("unknown canary " + symbol)
}
