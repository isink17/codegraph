package githistory

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
)

func filesAtHead(t *testing.T, r *gittest.Repo) map[string]FileStats {
	t.Helper()
	ctx := context.Background()
	state, err := Probe(ctx, r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := Files(ctx, r.Dir, state.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	return byPath(files)
}

func writeUserConfig(t *testing.T, body string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
}

// Myers and patience diff this pair differently (6/2 against 8/4), so a user's
// diff.algorithm would change --numstat unless the algorithm is pinned.
func TestUserDiffAlgorithmDoesNotChangeLineCounts(t *testing.T) {
	r := gittest.Init(t)
	r.Write("f.txt", "x\n{\na\n{\nb\nb\nb\n\n")
	r.Commit("", "one")
	r.Write("f.txt", "}\n{\nb\n{\nb\nb\nx\n{\n\nb\na\n\n")
	r.Commit("", "two")
	writeUserConfig(t, "[diff]\n\talgorithm = patience\n")
	got := filesAtHead(t, r)["f.txt"]
	if got.LinesAdded != 14 || got.LinesDeleted != 2 {
		t.Fatalf("added/deleted = %d/%d, want 14/2 (Myers)", got.LinesAdded, got.LinesDeleted)
	}
}

// Two similar-but-not-identical renames in one commit pair up under the pinned
// limit; a user's diff.renameLimit=1 would skip inexact detection and split
// each into a delete and an add, losing the older commit.
func TestUserRenameLimitDoesNotChangeRenameFollowing(t *testing.T) {
	r := gittest.Init(t)
	body := func(tag string) string {
		s := ""
		for i := 0; i < 10; i++ {
			s += tag + string(rune('a'+i)) + "\n"
		}
		return s
	}
	r.Write("one.txt", body("1"))
	r.Write("two.txt", body("2"))
	r.Commit("", "add")
	r.Git("mv", "one.txt", "uno.txt")
	r.Git("mv", "two.txt", "dos.txt")
	r.Write("uno.txt", body("1")+"extra\n")
	r.Write("dos.txt", body("2")+"extra\n")
	r.Commit("", "rename")
	writeUserConfig(t, "[diff]\n\trenameLimit = 1\n")
	got := filesAtHead(t, r)
	for _, p := range []string{"uno.txt", "dos.txt"} {
		if got[p].Commits != 2 {
			t.Fatalf("%s commits = %d, want 2 (rename followed): %+v", p, got[p].Commits, got)
		}
	}
}

// A user-level mailmap must not change author identity, while the repository's
// own .mailmap still applies.
func TestUserMailmapDoesNotChangeAuthors(t *testing.T) {
	r, _ := historyRepo(t)
	want := filesAtHead(t, r)
	userMap := filepath.Join(t.TempDir(), "mailmap")
	if err := os.WriteFile(userMap, []byte("Zed <zed@example.com> <alice@example.com>\nYan <yan@example.com> <bob@example.com>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Forward slashes: Git config reads backslashes in an unquoted value as
	// escapes, and Git for Windows accepts slash paths.
	writeUserConfig(t, "[mailmap]\n\tfile = "+filepath.ToSlash(userMap)+"\n")
	got := filesAtHead(t, r)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("user mailmap file changed aggregates:\n got %+v\nwant %+v", got, want)
	}
	// The same map stored as a blob and named by mailmap.blob, which Git also
	// reads in a repository with a working tree.
	blob := strings.TrimSpace(r.Git("hash-object", "-w", userMap))
	writeUserConfig(t, "[mailmap]\n\tblob = "+blob+"\n")
	if got := filesAtHead(t, r); !reflect.DeepEqual(got, want) {
		t.Fatalf("user mailmap blob changed aggregates:\n got %+v\nwant %+v", got, want)
	}
	// The repository .mailmap (alice@old.example -> alice@example.com) still applies.
	if a := got["pkg/a2.go"]; a.Authors != 1 || a.TopAuthor != "alice@example.com" {
		t.Fatalf("repo .mailmap not applied: %+v", a)
	}
}

// Configuration injected through the environment must not reach Git. With
// core.autocrlf=true a CRLF worktree copy of an LF blob stops differing, so a
// leaked setting would hide the change.
func TestEnvInjectedConfigIsIgnored(t *testing.T) {
	for name, env := range map[string][][2]string{
		"parameters": {{"GIT_CONFIG_PARAMETERS", "'core.autocrlf'='true'"}},
		"count":      {{"GIT_CONFIG_COUNT", "1"}, {"GIT_CONFIG_KEY_0", "core.autocrlf"}, {"GIT_CONFIG_VALUE_0", "true"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := gittest.Init(t)
			r.Write("x.go", "1\n")
			sha := r.Commit("", "one")
			r.Write("x.go", "1\r\n")
			for _, kv := range env {
				t.Setenv(kv[0], kv[1])
			}
			changes, err := WorktreeChanges(context.Background(), r.Dir, sha, []string{"x.go"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(changes, []string{"x.go"}) {
				t.Fatalf("changes = %v, want [x.go]", changes)
			}
		})
	}
}

// A configured fsmonitor hook runs on index reads; the history reads must not
// start one.
func TestFsmonitorIsNotInvoked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook")
	}
	r := gittest.Init(t)
	r.Write("x.go", "1\n")
	sha := r.Commit("", "one")
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\nprintf '\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeUserConfig(t, "[core]\n\tfsmonitor = "+hook+"\n")
	if _, err := WorktreeChanges(context.Background(), r.Dir, sha, []string{"x.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("fsmonitor hook was invoked")
	}
}
