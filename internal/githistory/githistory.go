// Package githistory reads bounded Git history for a repository root: per-file
// aggregates, and the window commit that last touched each symbol range. It is enrichment only: nothing here feeds symbol, edge or reference
// resolution, and every failure degrades to an absent-with-reason state
// instead of an error the scan would have to surface.
//
// The window is the latest WindowLimit first-parent commits reachable from the
// evaluated HEAD commit (the watermark). It is anchored to that commit, never
// to the wall clock, so the same commit graph always yields the same values.
package githistory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// WindowLimit is the number of first-parent commits the window holds.
	WindowLimit = 250
	// Algorithm identifies the aggregation rules. A stored state with another
	// value is recomputed, the way parser profiles invalidate parsed files.
	Algorithm = "file-v1+blame-v1"
)

// Status of a repository's history.
const (
	StatusOK        = "ok"
	StatusTruncated = "truncated" // shallow clone: the window may be cut short
	StatusAbsent    = "absent"
)

// Absent reasons. ReasonNotRepository and ReasonGitUnavailable mean there is
// no history to read; the others are failures inside a Git repository and are
// deliberately distinct from them.
const (
	ReasonDisabled        = "disabled"
	ReasonNotComputed     = "not_computed"
	ReasonGitUnavailable  = "git_unavailable"
	ReasonNotRepository   = "not_a_git_repository"
	ReasonNoCommits       = "no_commits"
	ReasonUnsafeDirectory = "unsafe_directory"
	ReasonTimeout         = "git_timeout"
	ReasonGitFailed       = "git_failed"
)

// State is the repository-level window metadata.
type State struct {
	Status        string `json:"status"`
	AbsentReason  string `json:"absent_reason,omitempty"`
	Watermark     string `json:"watermark,omitempty"`
	WatermarkTime int64  `json:"watermark_committer_time,omitempty"`
	WindowLimit   int    `json:"window_limit,omitempty"`
	WindowCommits int    `json:"window_commits"`
	Algorithm     string `json:"algorithm,omitempty"`
	// Mailmap fingerprints the repository's working-tree .mailmap, which Git
	// applies to %aE. Part of the reuse key only.
	Mailmap string `json:"-"`
}

// Absent builds the state for a repository with no readable history.
func Absent(reason string) State { return State{Status: StatusAbsent, AbsentReason: reason} }

// FileStats aggregates the window commits that touched one current path.
type FileStats struct {
	Path             string
	Commits          int
	FirstSHA         string
	FirstTime        int64
	LastSHA          string
	LastTime         int64
	Authors          int
	TopAuthor        string
	TopAuthorCommits int
	LinesAdded       int64
	LinesDeleted     int64
	Reverts          int
}

// Error is a Git failure carrying its absent reason.
type Error struct {
	Reason string
	Err    error
	// quietMiss marks exit status 1 with nothing on stderr: what
	// `rev-parse -q --verify` reports for a name that does not resolve.
	quietMiss bool
}

func (e *Error) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// ReasonOf maps any error to an absent reason.
func ReasonOf(err error) string {
	var gerr *Error
	if errors.As(err, &gerr) {
		return gerr.Reason
	}
	return ReasonGitFailed
}

// Timeout bounds every Git invocation. A variable so tests can shorten it.
var Timeout = 60 * time.Second

// gitBinary is the executable looked up on PATH. A variable so tests can make
// Git unavailable.
var gitBinary = "git"

// scrubbedEnv names variables that would point Git at a repository other than
// the codegraph root (a hook environment, for example). History always
// describes the repository that contains the root.
var scrubbedEnv = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_PREFIX": true,
	"GIT_COMMON_DIR": true, "GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_NAMESPACE": true,
	// Configuration injected through the environment, and object or history
	// rewrites, would change results without changing HEAD.
	"GIT_CONFIG_PARAMETERS": true, "GIT_CONFIG_COUNT": true,
	"GIT_REPLACE_REF_BASE": true, "GIT_NO_REPLACE_OBJECTS": true, "GIT_GRAFT_FILE": true,
	"GIT_SHALLOW_FILE": true,
}

func gitEnv() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if scrubbedEnv[name] || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, kv)
	}
	// Read-only: never take the optional index lock or rewrite the index, and
	// keep messages in a stable language for classification.
	return append(env, "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
}

func run(ctx context.Context, root string, args ...string) ([]byte, error) {
	return runInput(ctx, root, "", args...)
}

// runInput runs Git with input on stdin.
func runInput(ctx context.Context, root, input string, args ...string) ([]byte, error) {
	path, err := exec.LookPath(gitBinary)
	if err != nil {
		return nil, &Error{Reason: ReasonGitUnavailable, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, append([]string{"-C", root}, args...)...)
	cmd.Env = gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	err = cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, &Error{Reason: ReasonTimeout, Err: ctx.Err()}
	}
	msg := strings.TrimSpace(stderr.String())
	full := fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	var exitErr *exec.ExitError
	if msg == "" && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, &Error{Reason: ReasonGitFailed, Err: full, quietMiss: true}
	}
	switch {
	case strings.Contains(msg, "dubious ownership"):
		return nil, &Error{Reason: ReasonUnsafeDirectory, Err: full}
	case strings.Contains(msg, "not a git repository"):
		return nil, &Error{Reason: ReasonNotRepository, Err: full}
	}
	return nil, &Error{Reason: ReasonGitFailed, Err: full}
}

// Probe resolves the watermark: the HEAD commit and whether the clone is
// shallow. WindowCommits is left for Files to fill.
func Probe(ctx context.Context, root string) (State, error) {
	out, err := run(ctx, root, "rev-parse", "--is-shallow-repository", "--show-toplevel")
	if err != nil {
		return State{}, err
	}
	shallowLine, toplevel, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	shallow := shallowLine == "true"
	mailmap := ""
	if data, err := os.ReadFile(filepath.Join(toplevel, ".mailmap")); err == nil {
		mailmap = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	// An unborn HEAD fails this quietly; a HEAD naming a missing or corrupt
	// object passes it and fails `show` below as git_failed.
	out, err = run(ctx, root, "rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		if ReasonOf(err) == ReasonGitFailed {
			return State{}, &Error{Reason: ReasonNoCommits, Err: err}
		}
		return State{}, err
	}
	sha := strings.TrimSpace(string(out))
	out, err = run(ctx, root, "show", "-s", "--no-show-signature", "--format=%ct", sha)
	if err != nil {
		return State{}, err
	}
	ct, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return State{}, &Error{Reason: ReasonGitFailed, Err: err}
	}
	status := StatusOK
	if shallow {
		status = StatusTruncated
	}
	return State{Status: status, Watermark: sha, WatermarkTime: ct, WindowLimit: WindowLimit, Algorithm: Algorithm, Mailmap: mailmap}, nil
}

// Head returns the HEAD commit of root with a single git call.
func Head(ctx context.Context, root string) (string, error) {
	out, err := run(ctx, root, "rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// WorktreeChanges lists root-relative paths whose working-tree content
// differs from the watermark commit: tracked files modified, staged or
// deleted, plus every indexed path Git does not track. An indexed path is
// compared against the tracked set rather than Git's untracked listing,
// because codegraph indexes files that .gitignore excludes (generated code,
// for example) and those differ from the watermark too. Sorted, unique.
func WorktreeChanges(ctx context.Context, root, watermark string, indexed []string) ([]string, error) {
	tracked, err := run(ctx, root, "-c", "core.fsmonitor=false", "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", "--no-color", "--relative", watermark, "--")
	if err != nil {
		return nil, err
	}
	// -v tags each entry: lowercase for assume-unchanged, S for skip-worktree.
	// Git diff trusts both flags and skips the file, so an edit to it is
	// invisible above; its content is hashed below instead.
	listed, err := run(ctx, root, "-c", "core.fsmonitor=false", "ls-files", "-z", "-v", "--cached")
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	var hidden []string
	for _, entry := range strings.Split(string(listed), "\x00") {
		if len(entry) < 3 {
			continue
		}
		tag, p := entry[0], entry[2:]
		known[p] = true
		if tag == 'S' || (tag >= 'a' && tag <= 'z') {
			hidden = append(hidden, p)
		}
	}
	seen := map[string]bool{}
	for _, p := range strings.Split(string(tracked), "\x00") {
		if p != "" {
			seen[p] = true
		}
	}
	isIndexed := map[string]bool{}
	for _, p := range indexed {
		isIndexed[p] = true
		if !known[p] {
			seen[p] = true
		}
	}
	hidden = slices.DeleteFunc(hidden, func(p string) bool { return seen[p] || !isIndexed[p] })
	differ, err := contentDiffers(ctx, root, watermark, hidden)
	if err != nil {
		return nil, err
	}
	for _, p := range differ {
		seen[p] = true
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

// contentDiffers returns the paths whose working-tree file, cleaned by the
// same filters `git add` applies, hashes to another blob than the watermark's.
func contentDiffers(ctx context.Context, root, watermark string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	hashes, err := runInput(ctx, root, strings.Join(paths, "\n")+"\n", "hash-object", "--stdin-paths")
	if err != nil {
		return nil, err
	}
	got := strings.Fields(string(hashes))
	if len(got) != len(paths) {
		return nil, &Error{Reason: ReasonGitFailed, Err: fmt.Errorf("hash-object returned %d hashes for %d paths", len(got), len(paths))}
	}
	tree, err := run(ctx, root, append([]string{"ls-tree", "-z", watermark, "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	blob := map[string]string{}
	for _, entry := range strings.Split(string(tree), "\x00") {
		meta, p, ok := strings.Cut(entry, "\t")
		if fields := strings.Fields(meta); ok && len(fields) == 3 {
			blob[p] = fields[2]
		}
	}
	var out []string
	for k, p := range paths {
		if blob[p] != got[k] {
			out = append(out, p)
		}
	}
	return out, nil
}

// Filtered returns the paths with a `filter` attribute (Git LFS, a custom
// smudge/clean pair). Their working-tree lines need not be the blob's lines
// blame reads, so symbol ranges cannot be matched to them.
func Filtered(ctx context.Context, root string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out, err := runInput(ctx, root, strings.Join(paths, "\x00")+"\x00", "check-attr", "-z", "--stdin", "filter")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(string(out), "\x00")
	var filtered []string
	for k := 0; k+2 < len(fields); k += 3 {
		if v := fields[k+2]; v != "unspecified" && v != "unset" {
			filtered = append(filtered, fields[k])
		}
	}
	return filtered, nil
}

// revertSubject and revertTrailer are the two markers Git itself writes for
// `git revert` (and GitHub's revert button writes the subject). A commit is a
// revert when its subject starts with `Revert "` or any message line starts
// with `This reverts commit <hex>`. Nothing else -- "revert" in prose, a
// `revert:` conventional prefix, an issue reference -- counts.
var revertTrailer = regexp.MustCompile(`(?m)^This reverts commit [0-9a-f]{7,64}\b`)

// IsRevert classifies a full commit message.
func IsRevert(message string) bool {
	subject, _, _ := strings.Cut(message, "\n")
	return strings.HasPrefix(subject, `Revert "`) || revertTrailer.MatchString(message)
}

type commit struct {
	sha    string
	time   int64
	author string
	revert bool
}

type acc struct {
	stats   FileStats
	authors map[string]int
	lastIdx int
}

// Files aggregates the window for every path present in the watermark tree.
// It returns the per-file stats sorted by path and the number of commits the
// window actually holds.
func Files(ctx context.Context, root, watermark string) ([]FileStats, int, error) {
	// Explicit flags override user configuration that would change the
	// result: --root keeps the root commit's diff under log.showRoot=false, and
	// log.diffMerges pins -m to the first-parent diff on Git versions where -m
	// follows that setting (older versions ignore the key and already do).
	// The diff algorithm and rename limit change --numstat counts and rename
	// pairing, and user-level mailmap settings change author identity, none of
	// which moves HEAD; pin them so a reused result equals a fresh one. The
	// repository's own .mailmap is still read (and fingerprinted by Probe).
	out, err := run(ctx, root, "-c", "log.diffMerges=first-parent", "-c", "diff.renameLimit=1000",
		"-c", "mailmap.file=", "-c", "mailmap.blob=",
		"log", "-z", "--first-parent", "-m", "-M", "--diff-algorithm=myers", "--root", "--numstat",
		"--no-color", "--no-ext-diff", "--no-show-signature", "--relative",
		"--format=%x1e%H%x1f%ct%x1f%aE%x1f%B", "-n", strconv.Itoa(WindowLimit), watermark, "--")
	if err != nil {
		return nil, 0, err
	}
	tree, err := run(ctx, root, "ls-tree", "-r", "-z", "--name-only", watermark)
	if err != nil {
		return nil, 0, err
	}
	present := map[string]bool{}
	for _, p := range strings.Split(string(tree), "\x00") {
		if p != "" {
			present[p] = true
		}
	}
	byPath, n, err := aggregate(out)
	if err != nil {
		return nil, 0, &Error{Reason: ReasonGitFailed, Err: err}
	}
	files := make([]FileStats, 0, len(byPath))
	for path, a := range byPath {
		if !present[path] {
			continue // deleted at the watermark
		}
		s := a.stats
		s.Authors = len(a.authors)
		for email, c := range a.authors {
			if c > s.TopAuthorCommits || (c == s.TopAuthorCommits && email < s.TopAuthor) {
				s.TopAuthor, s.TopAuthorCommits = email, c
			}
		}
		files = append(files, s)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, n, nil
}

// aggregate parses `git log -z --numstat` output, newest commit first.
//
// Renames (-M) are followed inside the window: walking from newest to oldest,
// a rename old -> new makes every older commit on `old` count toward the path
// `new` currently resolves to. Otherwise attribution is by path, as `git log
// -- <path>` attributes it.
func aggregate(out []byte) (map[string]*acc, int, error) {
	tokens := strings.Split(string(out), "\x00")
	alias := map[string]string{}
	resolve := func(p string) string {
		if cur, ok := alias[p]; ok {
			return cur
		}
		return p
	}
	byPath := map[string]*acc{}
	var cur *commit
	n := 0
	// Renames apply from the next (older) commit on, all at once, so a swap
	// a->b, b->a inside one commit resolves both names against the same map.
	var pending [][2]string
	touch := func(path string, added, deleted int64) {
		a := byPath[path]
		if a == nil {
			a = &acc{stats: FileStats{Path: path}, authors: map[string]int{}, lastIdx: -1}
			byPath[path] = a
		}
		a.stats.LinesAdded += added
		a.stats.LinesDeleted += deleted
		if a.lastIdx == n {
			return // a second historical name in the same commit
		}
		a.lastIdx = n
		s := &a.stats
		s.Commits++
		a.authors[cur.author]++
		if cur.revert {
			s.Reverts++
		}
		if s.LastSHA == "" || later(cur.time, cur.sha, s.LastTime, s.LastSHA) {
			s.LastSHA, s.LastTime = cur.sha, cur.time
		}
		if s.FirstSHA == "" || later(s.FirstTime, s.FirstSHA, cur.time, cur.sha) {
			s.FirstSHA, s.FirstTime = cur.sha, cur.time
		}
	}
	for i := 0; i < len(tokens); i++ {
		tok := strings.TrimPrefix(tokens[i], "\n")
		if tok == "" {
			continue
		}
		if strings.HasPrefix(tok, "\x1e") {
			fields := strings.SplitN(tok[1:], "\x1f", 4)
			if len(fields) != 4 {
				return nil, 0, fmt.Errorf("malformed commit header %q", tok)
			}
			ct, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return nil, 0, fmt.Errorf("commit %s time: %w", fields[0], err)
			}
			for _, r := range pending {
				alias[r[0]] = r[1]
			}
			pending = pending[:0]
			n++
			cur = &commit{sha: fields[0], time: ct, author: fields[2], revert: IsRevert(fields[3])}
			continue
		}
		if cur == nil {
			return nil, 0, fmt.Errorf("numstat before commit header: %q", tok)
		}
		parts := strings.SplitN(tok, "\t", 3)
		if len(parts) != 3 {
			return nil, 0, fmt.Errorf("malformed numstat %q", tok)
		}
		added, deleted := count(parts[0]), count(parts[1])
		if parts[2] != "" {
			touch(resolve(parts[2]), added, deleted)
			continue
		}
		if i+2 >= len(tokens) {
			return nil, 0, fmt.Errorf("truncated rename record")
		}
		oldPath, newPath := tokens[i+1], tokens[i+2]
		i += 2
		target := resolve(newPath)
		touch(target, added, deleted)
		pending = append(pending, [2]string{oldPath, target})
	}
	return byPath, n, nil
}

// later reports whether (t1, s1) follows (t2, s2) in the total order:
// committer time, then SHA.
func later(t1 int64, s1 string, t2 int64, s2 string) bool {
	if t1 != t2 {
		return t1 > t2
	}
	return s1 > s2
}

// count reads a numstat column; binary files report "-" and count as zero.
func count(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// Range is a 1-based inclusive line range of the watermark version of a file.
type Range struct {
	Start int
	End   int
}

// SymbolStats is the newest window commit among the lines of one range, as
// git blame attributes them at the watermark. LastSHA is empty when every line
// was last touched before the window. Nothing here counts changes to a symbol:
// blame only knows which commit last touched each surviving line.
type SymbolStats struct {
	Path       string
	Range      Range
	LastSHA    string
	LastTime   int64
	LastAuthor string
}

// trustedRangePrefixes are the parser profiles whose symbols carry body
// ranges. Heuristic adapters emit single-line ranges, so blaming them would
// publish a number for the declaration line only.
var trustedRangePrefixes = []string{"treesitter:", "go-ast:", "python-regex:"}

// TrustedRanges reports whether symbols parsed under profile have ranges
// trustworthy enough for symbol-level history.
func TrustedRanges(profile string) bool {
	for _, p := range trustedRangePrefixes {
		if strings.HasPrefix(profile, p) {
			return true
		}
	}
	return false
}

// WindowBoundary returns the first-parent commit just outside a window of
// windowCommits commits, or "" when the window reaches the root (or a shallow
// graft) of the history: a window shorter than WindowLimit, or a full one whose
// oldest commit has no first parent. Any other failure is returned, so a blame
// is never run without its bound.
func WindowBoundary(ctx context.Context, root, watermark string, windowCommits int) (string, error) {
	if windowCommits < WindowLimit {
		return "", nil
	}
	out, err := run(ctx, root, "rev-parse", "-q", "--verify", watermark+"~"+strconv.Itoa(WindowLimit)+"^{commit}")
	var gerr *Error
	if errors.As(err, &gerr) && gerr.quietMiss {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Blame attributes each range of path to the newest window commit among its
// lines, using `git blame --first-parent` at the watermark and stopping at the
// window boundary. Ranges beyond the end of the file are clipped to it.
//
// In a shallow clone the oldest available commit has no parent, so it would
// absorb every line older than the cut; it is a boundary there instead, and
// those lines read as before the window.
func Blame(ctx context.Context, root, watermark, boundary, path string, shallow bool, ranges []Range) ([]SymbolStats, error) {
	args := []string{"-c", "mailmap.file=", "-c", "mailmap.blob=", "-c", "diff.algorithm=myers",
		"-c", "blame.ignoreRevsFile=", "-c", "diff.renameLimit=1000",
		"blame", "--porcelain", "--first-parent", "--no-textconv"}
	// Blame follows whole-file renames on its own and reads .mailmap for
	// author-mail. The overrides match Files: no user mailmap, no textconv,
	// no ignore-revs list, Myers diff, and --root so the root commit is a
	// window commit rather than a boundary.
	if !shallow {
		args = append(args, "--root")
	}
	rev := watermark
	if boundary != "" {
		rev = boundary + ".." + watermark
	}
	out, err := run(ctx, root, append(args, rev, "--", path)...)
	if err != nil {
		return nil, err
	}
	lines, commits, err := parseBlame(out)
	if err != nil {
		return nil, &Error{Reason: ReasonGitFailed, Err: fmt.Errorf("blame %s: %w", path, err)}
	}
	stats := make([]SymbolStats, 0, len(ranges))
	for _, r := range ranges {
		s := SymbolStats{Path: path, Range: r}
		for line := max(r.Start, 1); line <= r.End && line <= len(lines); line++ {
			c := commits[lines[line-1]]
			if c.boundary {
				continue
			}
			if s.LastSHA == "" || later(c.time, lines[line-1], s.LastTime, s.LastSHA) {
				s.LastSHA, s.LastTime, s.LastAuthor = lines[line-1], c.time, c.author
			}
		}
		stats = append(stats, s)
	}
	return stats, nil
}

type blameCommit struct {
	time     int64
	author   string
	boundary bool
}

// parseBlame reads `git blame --porcelain`: the commit of every final line in
// order, and the details of each commit, which porcelain prints once.
func parseBlame(out []byte) ([]string, map[string]*blameCommit, error) {
	var lines []string
	commits := map[string]*blameCommit{}
	var cur *blameCommit
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case line == "":
		case line[0] == '\t':
			cur = nil
		case cur == nil:
			// Header: <sha> <orig line> <final line> [<group size>].
			fields := strings.Fields(line)
			if len(fields) < 3 {
				return nil, nil, fmt.Errorf("malformed blame header %q", line)
			}
			final, err := strconv.Atoi(fields[2])
			if err != nil || final != len(lines)+1 {
				return nil, nil, fmt.Errorf("blame line out of order: %q", line)
			}
			lines = append(lines, fields[0])
			cur = commits[fields[0]]
			if cur == nil {
				cur = &blameCommit{}
				commits[fields[0]] = cur
			}
		default:
			key, value, _ := strings.Cut(line, " ")
			switch key {
			case "committer-time":
				t, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return nil, nil, fmt.Errorf("committer-time %q: %w", value, err)
				}
				cur.time = t
			case "author-mail":
				cur.author = strings.TrimSuffix(strings.TrimPrefix(value, "<"), ">")
			case "boundary":
				cur.boundary = true
			}
		}
	}
	return lines, commits, nil
}
