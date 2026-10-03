package githistory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/githistory/gittest"
)

func TestIsRevert(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"Revert \"feat: x\"\n\nThis reverts commit " + sha + ".\n", true},
		{"Revert \"feat: x\" (#12)\n\nReverts owner/repo#11\n", true},
		{"undo parser tweak\n\nThis reverts commit " + sha[:12] + ".\n", true},
		{"Revert \"merge\"\n\nThis reverts commit " + sha + ", reversing\nchanges made to " + sha + ".\n", true},
		{"revert the parser tweak\n", false},
		{"revert: drop flag\n", false},
		{"Reverted the change from yesterday\n", false},
		{"Revert parser tweak\n", false},
		{"fix\n\nThis reverts the earlier change.\n", false},
		{"fix\n\nNote: This reverts commit " + sha + "\n", false},
		{"fix\n\nThis reverts commit xyz\n", false},
	} {
		if got := IsRevert(tc.msg); got != tc.want {
			t.Errorf("IsRevert(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// historyRepo exercises renames, a first-parent merge, mailmap, a revert, a
// "revert" prose negative control, a deletion and a binary file.
func historyRepo(t *testing.T) (*gittest.Repo, map[string]string) {
	r := gittest.Init(t)
	sha := map[string]string{}
	r.Write("a.go", "1\n")
	r.Write("b.go", "b1\n")
	r.Write("c.go", "c\n")
	r.Write(".mailmap", "Alice <alice@example.com> <alice@old.example>\n")
	sha["c1"] = r.Commit("", "c1")
	r.Write("a.go", "1\n2\n")
	sha["c2"] = r.Commit("alice@old.example", "c2")
	if err := os.MkdirAll(filepath.Join(r.Dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.Git("mv", "a.go", "pkg/a2.go")
	r.Write("pkg/a2.go", "1\n2\n3\n")
	sha["c3"] = r.Commit("", "c3 rename")
	r.Write("b.go", "b1\nb2\n")
	sha["c4"] = r.Commit("bob@example.com", "c4")
	r.Git("checkout", "-q", "-b", "feature")
	r.Write("e.go", "e1\n")
	r.Commit("carol@example.com", "f1")
	r.Write("e.go", "e1\ne2\n")
	r.Commit("carol@example.com", "f2")
	r.Git("checkout", "-q", "main")
	r.Tick()
	r.Git("merge", "-q", "--no-ff", "-m", "m1", "feature")
	sha["m1"] = r.Git("rev-parse", "HEAD")
	r.Git("rm", "-q", "c.go")
	sha["c5"] = r.Commit("", "c5 delete")
	r.Tick()
	r.Git("revert", "--no-edit", sha["c4"])
	sha["c6"] = r.Git("rev-parse", "HEAD")
	r.Write("b.go", "b1\nb3\n")
	sha["c7"] = r.Commit("", "revert the earlier tweak\n\nNo trailer here.")
	r.Write("bin.dat", "\x00\x01\x02")
	sha["c8"] = r.Commit("", "c8 binary")
	return r, sha
}

func byPath(files []FileStats) map[string]FileStats {
	out := map[string]FileStats{}
	for _, f := range files {
		out[f.Path] = f
	}
	return out
}

func TestFilesFollowsRenamesMergesMailmapAndReverts(t *testing.T) {
	r, sha := historyRepo(t)
	ctx := context.Background()
	state, err := Probe(ctx, r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusOK || state.Watermark != sha["c8"] || state.WindowLimit != WindowLimit || state.Algorithm != Algorithm {
		t.Fatalf("state = %+v", state)
	}
	files, n, err := Files(ctx, r.Dir, state.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	// c1 c2 c3 c4 m1 c5 c6 c7 c8: the feature commits are not first-parent.
	if n != 9 {
		t.Fatalf("window commits = %d, want 9", n)
	}
	got := byPath(files)
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if want := []string{".mailmap", "b.go", "bin.dat", "e.go", "pkg/a2.go"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v (renamed-away a.go and deleted c.go must not appear)", paths, want)
	}
	a2 := got["pkg/a2.go"]
	// Rename followed: c1 and c2 were made on a.go. Mailmap folds the old email.
	if a2.Commits != 3 || a2.Authors != 1 || a2.TopAuthor != "alice@example.com" || a2.TopAuthorCommits != 3 ||
		a2.FirstSHA != sha["c1"] || a2.LastSHA != sha["c3"] || a2.LinesAdded != 3 || a2.LinesDeleted != 0 || a2.Reverts != 0 {
		t.Fatalf("pkg/a2.go = %+v", a2)
	}
	b := got["b.go"]
	// c1, c4, c6 (revert), c7 (prose "revert", not a revert).
	if b.Commits != 4 || b.Authors != 2 || b.TopAuthor != "alice@example.com" || b.TopAuthorCommits != 3 ||
		b.Reverts != 1 || b.FirstSHA != sha["c1"] || b.LastSHA != sha["c7"] {
		t.Fatalf("b.go = %+v", b)
	}
	// The merge counts once, as its first-parent diff, under the merge author.
	e := got["e.go"]
	if e.Commits != 1 || e.FirstSHA != sha["m1"] || e.TopAuthor != "alice@example.com" || e.LinesAdded != 2 {
		t.Fatalf("e.go = %+v", e)
	}
	if bin := got["bin.dat"]; bin.Commits != 1 || bin.LinesAdded != 0 || bin.LinesDeleted != 0 {
		t.Fatalf("bin.dat = %+v", bin)
	}
}

func TestTopAuthorTieBreaksByEmail(t *testing.T) {
	r := gittest.Init(t)
	r.Write("x.go", "1\n")
	r.Commit("zed@example.com", "one")
	r.Write("x.go", "2\n")
	r.Commit("amy@example.com", "two")
	state, err := Probe(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := Files(context.Background(), r.Dir, state.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].TopAuthor != "amy@example.com" || files[0].TopAuthorCommits != 1 || files[0].Authors != 2 {
		t.Fatalf("files = %+v", files)
	}
}

// Git's -M only pairs a deleted path with an added one, so a real swap is
// reported as two modifications; the parser must still not chain two renames
// recorded in one commit. Synthetic output, newest commit first.
func TestRenamesInOneCommitResolveAgainstTheOlderMap(t *testing.T) {
	header := func(sha string, ct int) string {
		return fmt.Sprintf("\x1e%s\x1f%d\x1fa@example.com\x1fmsg\n", sha, ct)
	}
	out := header("c3", 3) + "\x00\n1\t0\t\x00x\x00y\x001\t0\t\x00y\x00x\x00" +
		header("c2", 2) + "\x00\n1\t0\tx\x00" +
		header("c1", 1) + "\x00\n1\t0\ty\x00"
	got, n, err := aggregate([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	// Before c3, x held what is now y and y what is now x.
	if n != 3 || got["y"].stats.Commits != 2 || got["y"].stats.FirstSHA != "c2" || got["x"].stats.Commits != 2 || got["x"].stats.FirstSHA != "c1" {
		t.Fatalf("n=%d y=%+v x=%+v", n, got["y"].stats, got["x"].stats)
	}
}

func TestSubdirectoryRootUsesRelativePaths(t *testing.T) {
	r, sha := historyRepo(t)
	root := filepath.Join(r.Dir, "pkg")
	state, err := Probe(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	files, n, err := Files(context.Background(), root, state.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	if n != 9 {
		t.Fatalf("window commits = %d, want 9 (the window is repository commits)", n)
	}
	// Only paths inside the root, relative to it. History from before the file
	// moved into the root is outside the root and is not attributed.
	if len(files) != 1 || files[0].Path != "a2.go" || files[0].FirstSHA != sha["c3"] {
		t.Fatalf("files = %+v", files)
	}
	r.Write("pkg/new.go", "n\n")
	r.Write("b.go", "outside\n")
	changes, err := WorktreeChanges(context.Background(), root, state.Watermark, []string{"a2.go", "new.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changes, []string{"new.go"}) {
		t.Fatalf("changes = %v", changes)
	}
}

func TestWorktreeChanges(t *testing.T) {
	r := gittest.Init(t)
	r.Write(".gitignore", "*.log\n")
	r.Write("mod.go", "1\n")
	r.Write("staged.go", "1\n")
	r.Write("gone.go", "1\n")
	r.Write("same.go", "1\n")
	sha := r.Commit("", "one")
	r.Write("mod.go", "2\n")
	r.Write("staged.go", "2\n")
	r.Git("add", "staged.go")
	if err := os.Remove(filepath.Join(r.Dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	r.Write("dir/untracked.go", "u\n")
	r.Write("debug.log", "ignored\n")
	// Same content, new mtime: not a change.
	r.Write("same.go", "1\n")
	r.Write("notindexed.go", "u\n")
	// The indexer indexes debug.log although Git ignores it: it is not in the
	// watermark, so it differs. notindexed.go is untracked but not indexed.
	indexed := []string{"debug.log", "dir/untracked.go", "mod.go", "same.go", "staged.go"}
	changes, err := WorktreeChanges(context.Background(), r.Dir, sha, indexed)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"debug.log", "dir/untracked.go", "gone.go", "mod.go", "staged.go"}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %v, want %v", changes, want)
	}
}

// fastImport writes n linear commits in one git process: hot.txt changes in
// every commit, cold.txt in the first five, edge.txt in commits 10 and 11.
func fastImport(t *testing.T, r *gittest.Repo, n int) {
	var b strings.Builder
	blob := func(path, content string) {
		fmt.Fprintf(&b, "M 644 inline %s\ndata %d\n%s\n", path, len(content), content)
	}
	for i := 1; i <= n; i++ {
		msg := fmt.Sprintf("commit %d", i)
		fmt.Fprintf(&b, "commit refs/heads/main\nmark :%d\ncommitter C <c@example.com> %d +0000\ndata %d\n%s\n", i, 1_700_000_000+i*60, len(msg), msg)
		if i > 1 {
			fmt.Fprintf(&b, "from :%d\n", i-1)
		}
		blob("hot.txt", fmt.Sprintf("v%d\n", i))
		if i <= 5 {
			blob("cold.txt", fmt.Sprintf("v%d\n", i))
		}
		if i == 10 || i == 11 {
			blob("edge.txt", fmt.Sprintf("v%d\n", i))
		}
	}
	cmd := exec.Command("git", "-C", r.Dir, "fast-import", "--quiet")
	cmd.Stdin = strings.NewReader(b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	r.Git("reset", "-q", "--hard")
}

func TestWindowBoundaryIsLatest250FirstParentCommits(t *testing.T) {
	r := gittest.Init(t)
	fastImport(t, r, 260)
	state, err := Probe(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	files, n, err := Files(context.Background(), r.Dir, state.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	if n != WindowLimit {
		t.Fatalf("window commits = %d, want %d", n, WindowLimit)
	}
	got := byPath(files)
	if _, ok := got["cold.txt"]; ok {
		t.Fatalf("cold.txt changed only before the window: %+v", got["cold.txt"])
	}
	oldest := r.Git("rev-parse", "HEAD~249")
	if hot := got["hot.txt"]; hot.Commits != 250 || hot.FirstSHA != oldest || hot.LastSHA != state.Watermark || hot.FirstTime != 1_700_000_000+11*60 {
		t.Fatalf("hot.txt = %+v", hot)
	}
	// Commit 11 is the oldest commit in the window; commit 10 is just outside.
	if edge := got["edge.txt"]; edge.Commits != 1 || edge.FirstSHA != oldest {
		t.Fatalf("edge.txt = %+v", edge)
	}
}

func TestShallowCloneIsTruncated(t *testing.T) {
	r, _ := historyRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	r.Git("clone", "-q", "--depth", "3", "file://"+filepath.ToSlash(r.Dir), clone)
	state, err := Probe(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusTruncated {
		t.Fatalf("status = %q, want truncated", state.Status)
	}
	_, n, err := Files(context.Background(), clone, state.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("window commits = %d, want 3", n)
	}
}

func TestAbsentReasonsAreDistinct(t *testing.T) {
	ctx := context.Background()
	gittest.Require(t)
	reason := func(root string) string {
		t.Helper()
		_, err := Probe(ctx, root)
		if err == nil {
			t.Fatalf("Probe(%s) succeeded", root)
		}
		return ReasonOf(err)
	}

	t.Run("not a repository", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
		if got := reason(dir); got != ReasonNotRepository {
			t.Fatalf("reason = %q", got)
		}
	})
	t.Run("git unavailable", func(t *testing.T) {
		old := gitBinary
		gitBinary = "codegraph-test-no-such-git"
		defer func() { gitBinary = old }()
		if got := reason(t.TempDir()); got != ReasonGitUnavailable {
			t.Fatalf("reason = %q", got)
		}
	})
	t.Run("no commits", func(t *testing.T) {
		if got := reason(gittest.Init(t).Dir); got != ReasonNoCommits {
			t.Fatalf("reason = %q", got)
		}
	})
	t.Run("corrupt object", func(t *testing.T) {
		r := gittest.Init(t)
		r.Write("x.go", "1\n")
		sha := r.Commit("", "one")
		if err := os.Remove(filepath.Join(r.Dir, ".git", "objects", sha[:2], sha[2:])); err != nil {
			t.Fatal(err)
		}
		if got := reason(r.Dir); got != ReasonGitFailed {
			t.Fatalf("reason = %q", got)
		}
	})
	t.Run("unsafe directory", func(t *testing.T) {
		r := gittest.Init(t)
		r.Write("x.go", "1\n")
		r.Commit("", "one")
		t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
		got := reason(r.Dir)
		if got == ReasonGitFailed {
			t.Skip("this git does not honour GIT_TEST_ASSUME_DIFFERENT_OWNER")
		}
		if got != ReasonUnsafeDirectory {
			t.Fatalf("reason = %q", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		r := gittest.Init(t)
		old := Timeout
		Timeout = time.Nanosecond
		defer func() { Timeout = old }()
		if got := reason(r.Dir); got != ReasonTimeout {
			t.Fatalf("reason = %q", got)
		}
	})
}

// A hook-style GIT_DIR pointing at another repository must not redirect
// history away from the root being indexed.
func TestRepositoryEnvironmentIsIgnored(t *testing.T) {
	r := gittest.Init(t)
	r.Write("x.go", "1\n")
	want := r.Commit("", "one")
	other := gittest.Init(t)
	other.Write("y.go", "1\n")
	other.Commit("", "other")
	t.Setenv("GIT_DIR", filepath.Join(other.Dir, ".git"))
	t.Setenv("GIT_WORK_TREE", other.Dir)
	state, err := Probe(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Watermark != want {
		t.Fatalf("watermark = %s, want %s", state.Watermark, want)
	}
}

// User configuration that changes log output must not change the aggregates.
func TestUserConfigDoesNotChangeAggregates(t *testing.T) {
	r, _ := historyRepo(t)
	ctx := context.Background()
	state, err := Probe(ctx, r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := Files(ctx, r.Dir, state.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"log.showRoot", "false"}, {"log.diffMerges", "remerge"},
		{"diff.algorithm", "patience"}, {"diff.renameLimit", "1"}, {"diff.renames", "false"},
		{"log.showSignature", "true"}, {"color.ui", "always"}, {"diff.relative", "true"},
	} {
		r.Git("config", kv[0], kv[1])
	}
	got, _, err := Files(ctx, r.Dir, state.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("with user config:\n got %+v\nwant %+v", got, want)
	}
}
