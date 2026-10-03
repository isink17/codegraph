package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory"
	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func historyIndexer(s *store.Store) *Indexer {
	return New(s, parser.NewRegistry(goparser.New()), nil)
}

// dumpTables renders every row of the selected tables, sorted, without the
// excluded columns. It is a byte-level oracle: no row id or value is skipped
// unless named.
func dumpTables(t *testing.T, db *sql.DB, keep func(table string) bool, dropColumn func(col string) bool) string {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if keep(name) {
			tables = append(tables, name)
		}
	}
	rows.Close()
	var out []string
	for _, table := range tables {
		colRows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		var cols []string
		for colRows.Next() {
			var c string
			if err := colRows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			if !dropColumn(c) {
				cols = append(cols, `quote(`+c+`)`)
			}
		}
		colRows.Close()
		if len(cols) == 0 {
			continue
		}
		dataRows, err := db.QueryContext(ctx, `SELECT `+strings.Join(cols, ` || '|' || `)+` FROM `+table)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		var lines []string
		for dataRows.Next() {
			var line string
			if err := dataRows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, table+": "+line)
		}
		dataRows.Close()
		sort.Strings(lines)
		out = append(out, fmt.Sprintf("%s rows=%d", table, len(lines)))
		out = append(out, lines...)
	}
	return strings.Join(out, "\n")
}

func isHistoryTable(name string) bool { return strings.HasPrefix(name, "git_") }

// semanticDump is everything except history, scan bookkeeping and clocks.
// dropIDs compares two separately built databases: row ids, and the full-text
// index internals keyed by them, follow parallel insertion order there.
func semanticDump(t *testing.T, s *profileStore, dropIDs bool) string {
	return dumpTables(t, s.raw(t), func(table string) bool {
		switch table {
		case "scans", "scan_language_coverage", "schema_migrations", "sqlite_sequence", "session_events":
			return false
		}
		if dropIDs && strings.Contains(table, "_fts") {
			return false
		}
		return !isHistoryTable(table)
	}, func(col string) bool {
		switch col {
		case "indexed_at", "created_at", "updated_at", "applied_at", "queued_at", "last_scan_id", "scan_id", "mtime_unix_ns":
			return true
		}
		return dropIDs && (col == "id" || strings.HasSuffix(col, "_id"))
	})
}

func historyDump(t *testing.T, s *profileStore) string {
	return dumpTables(t, s.raw(t), isHistoryTable, func(string) bool { return false })
}

func goHistoryRepo(t *testing.T) *gittest.Repo {
	r := gittest.Init(t)
	r.Write("go.mod", "module example.com/h\n\ngo 1.22\n")
	r.Write("a.go", "package h\n\nfunc A() { B() }\n")
	r.Write("b.go", "package h\n\nfunc B() {}\n")
	r.Commit("", "one")
	r.Write("b.go", "package h\n\nfunc B() { C() }\n\nfunc C() {}\n")
	r.Commit("bob@example.com", "two")
	return r
}

func index(t *testing.T, s *profileStore, root string, update, noHistory bool) store.ScanSummary {
	t.Helper()
	idx := historyIndexer(s.Store)
	opts := Options{RepoRoot: root, NoHistory: noHistory}
	var summary store.ScanSummary
	var err error
	if update {
		summary, err = idx.Update(context.Background(), opts)
	} else {
		summary, err = idx.Index(context.Background(), opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

// History is enrichment: the semantic graph is byte-identical with it on and
// off, in the same database and across fresh databases.
func TestHistoryNeverChangesSemanticGraph(t *testing.T) {
	r := goHistoryRepo(t)
	r.Write("a.go", "package h\n\nfunc A() { B(); C() }\n") // dirty
	off := newProfileStore(t)
	if sum := index(t, off, r.Dir, false, true); sum.History == nil || sum.History.AbsentReason != githistory.ReasonDisabled {
		t.Fatalf("history off: %+v", sum.History)
	}
	before := semanticDump(t, off, false)
	if !strings.Contains(before, "edges rows=") || strings.Contains(before, "edges rows=0") {
		t.Fatalf("fixture produced no edges:\n%s", before)
	}
	if sum := index(t, off, r.Dir, true, false); sum.History == nil || sum.History.Status != githistory.StatusOK {
		t.Fatalf("history on: %+v", sum.History)
	}
	if after := semanticDump(t, off, false); after != before {
		t.Fatalf("enabling history changed the semantic graph:\n--- off\n%s\n--- on\n%s", before, after)
	}
	if strings.Contains(historyDump(t, off), "git_file_history rows=0") {
		t.Fatal("history was not stored")
	}
	index(t, off, r.Dir, true, true)
	if again := semanticDump(t, off, false); again != before {
		t.Fatal("disabling history changed the semantic graph")
	}
	if dump := historyDump(t, off); !strings.Contains(dump, "git_file_history rows=0") || !strings.Contains(dump, "'disabled'") {
		t.Fatalf("disabled history kept rows:\n%s", dump)
	}

	on := newProfileStore(t)
	index(t, on, r.Dir, false, false)
	if a, b := semanticDump(t, off, true), semanticDump(t, on, true); a != b {
		t.Fatalf("fresh index with history differs from one without:\n--- off\n%s\n--- on\n%s", a, b)
	}
}

// Fresh index == incremental update == update after a rewrite, for every
// persisted history value.
func TestHistoryFreshIncrementalAndRewriteParity(t *testing.T) {
	r := goHistoryRepo(t)
	inc := newProfileStore(t)
	index(t, inc, r.Dir, false, false)

	assertParity := func(step string) {
		t.Helper()
		fresh := newProfileStore(t)
		index(t, fresh, r.Dir, false, false)
		if a, b := historyDump(t, inc), historyDump(t, fresh); a != b {
			t.Fatalf("%s: incremental history differs from fresh:\n--- incremental\n%s\n--- fresh\n%s", step, a, b)
		}
	}
	state := func() githistory.State {
		t.Helper()
		st, _, err := inc.GitHistoryState(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Descendant commits.
	r.Write("c.go", "package h\n\nfunc D() {}\n")
	r.Commit("carol@example.com", "three")
	r.Write("a.go", "package h\n\nfunc A() { D() }\n")
	head := r.Commit("", "four")
	index(t, inc, r.Dir, true, false)
	if st := state(); st.Watermark != head || st.WindowCommits != 4 {
		t.Fatalf("after advance: %+v", st)
	}
	assertParity("advance")

	// Worktree-only change: watermark unchanged, change set refreshed.
	r.Write("b.go", "package h\n\nfunc B() {}\n")
	r.Write("new.go", "package h\n")
	r.Write(".codegraph/graph.sqlite-wal", "x") // codegraph's own artifacts
	index(t, inc, r.Dir, true, false)
	assertParity("dirty")
	if dump := historyDump(t, inc); !strings.Contains(dump, "git_worktree_changes: 1|'new.go'") || strings.Contains(dump, ".codegraph") {
		t.Fatalf("worktree changes wrong:\n%s", dump)
	}

	// A working-tree .mailmap edit changes attribution without moving HEAD.
	r.Write(".mailmap", "Alice <alice@example.com> <bob@example.com>\n")
	index(t, inc, r.Dir, true, false)
	assertParity("mailmap")
	if strings.Contains(historyDump(t, inc), "bob@example.com") {
		t.Fatal("mailmap edit not applied")
	}

	// Rewrite: reset two commits back and commit something else. The old
	// watermark is no longer reachable from HEAD.
	r.Git("reset", "-q", "--hard", "HEAD~2")
	r.Write("e.go", "package h\n\nfunc E() {}\n")
	rewritten := r.Commit("dave@example.com", "rewrite")
	index(t, inc, r.Dir, true, false)
	if st := state(); st.Watermark != rewritten || st.WindowCommits != 3 {
		t.Fatalf("after rewrite: %+v", st)
	}
	assertParity("rewrite")
	if strings.Contains(historyDump(t, inc), "carol@example.com") {
		t.Fatal("rewritten-away commit still attributed")
	}
}

func TestHistoryAbsentWithoutGitScanSucceeds(t *testing.T) {
	gittest.Require(t)
	root := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	writeProfileFile(t, filepath.Join(root, "a.go"), "package h\n\nfunc A() {}\n")
	s := newProfileStore(t)
	sum := index(t, s, root, false, false)
	if sum.FilesIndexed != 1 || sum.History == nil || sum.History.Status != githistory.StatusAbsent ||
		sum.History.AbsentReason != githistory.ReasonNotRepository {
		t.Fatalf("summary = %+v history = %+v", sum, sum.History)
	}
	got, err := s.GitHistory(context.Background(), 1, store.GitHistoryQuery{Paths: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.History.AbsentReason != githistory.ReasonNotRepository || len(got.Files) != 0 {
		t.Fatalf("result = %+v", got)
	}
}

// Git's slash paths join files.path directly, nested and from a subdirectory
// root, and an OS-native query path finds the same row (the Windows case).
func TestHistoryJoinsFilesAndFlagsWorktree(t *testing.T) {
	r := gittest.Init(t)
	r.Write("pkg/sub/a.go", "package sub\n\nfunc A() {}\n")
	r.Write("pkg/sub/b.go", "package sub\n\nfunc B() {}\n")
	r.Write(".gitignore", "gen.go\n")
	r.Commit("", "one")
	r.Write("pkg/sub/a.go", "package sub\n\nfunc A() { B() }\n")
	r.Write("pkg/sub/new.go", "package sub\n")
	// Git ignores gen.go but codegraph indexes it; it is not in the watermark.
	r.Write("pkg/sub/gen.go", "package sub\n\nfunc G() {}\n")

	for _, tc := range []struct{ root, prefix string }{{r.Dir, "pkg/sub/"}, {filepath.Join(r.Dir, "pkg"), "sub/"}} {
		s := newProfileStore(t)
		index(t, s, tc.root, false, false)
		got, err := s.GitHistory(context.Background(), 1, store.GitHistoryQuery{Paths: []string{
			filepath.FromSlash(tc.prefix + "a.go"), tc.prefix + "b.go", tc.prefix + "gen.go", tc.prefix + "new.go",
		}})
		if err != nil {
			t.Fatal(err)
		}
		var summary []string
		for _, f := range got.Files {
			summary = append(summary, fmt.Sprintf("%s indexed=%v differs=%v commits=%d", f.Path, f.Indexed, f.WorktreeDiffers, f.CommitCount))
		}
		want := []string{
			tc.prefix + "a.go indexed=true differs=true commits=1",
			tc.prefix + "b.go indexed=true differs=false commits=1",
			tc.prefix + "gen.go indexed=true differs=true commits=0",
			tc.prefix + "new.go indexed=true differs=true commits=0",
		}
		if !reflect.DeepEqual(summary, want) {
			t.Fatalf("root %s:\n got %v\nwant %v", tc.root, summary, want)
		}
		list, err := s.GitHistory(context.Background(), 1, store.GitHistoryQuery{PathPrefix: tc.prefix + "n", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if list.Total != 1 || len(list.Files) != 1 || list.Files[0].Path != tc.prefix+"new.go" {
			t.Fatalf("prefix listing = %+v", list)
		}
	}
}

// Deepening a shallow clone grows the window without moving HEAD.
func TestHistoryShallowDeepenParity(t *testing.T) {
	src := goHistoryRepo(t)
	src.Write("c.go", "package h\n\nfunc D() {}\n")
	src.Commit("", "three")
	clone := &gittest.Repo{T: t, Dir: filepath.Join(t.TempDir(), "clone")}
	src.Git("clone", "-q", "--depth", "1", "file://"+filepath.ToSlash(src.Dir), clone.Dir)
	inc := newProfileStore(t)
	if sum := index(t, inc, clone.Dir, false, false); sum.History.Status != githistory.StatusTruncated || sum.History.WindowCommits != 1 {
		t.Fatalf("shallow: %+v", sum.History)
	}
	clone.Git("fetch", "-q", "--deepen", "1")
	if sum := index(t, inc, clone.Dir, true, false); sum.History.WindowCommits != 2 {
		t.Fatalf("after deepen: %+v", sum.History)
	}
	fresh := newProfileStore(t)
	index(t, fresh, clone.Dir, false, false)
	if a, b := historyDump(t, inc), historyDump(t, fresh); a != b {
		t.Fatalf("deepened incremental differs from fresh:\n%s\n---\n%s", a, b)
	}
}
