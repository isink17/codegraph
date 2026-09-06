package indexer

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrPathOutsideRepo is the sentinel behind every path-containment refusal.
// errors.Is(err, ErrPathOutsideRepo) is the supported test.
var ErrPathOutsideRepo = errors.New("path outside repository root")

// PathScopeError reports one Options.Paths candidate that does not name a
// location inside RepoRoot. It carries the caller's own input and the root the
// run is already scoped to, and nothing else: a rejection must not describe the
// filesystem the caller failed to reach.
type PathScopeError struct {
	Path     string
	RepoRoot string
}

func (e *PathScopeError) Error() string {
	return fmt.Sprintf("path %q is outside repository root %q", e.Path, e.RepoRoot)
}

func (e *PathScopeError) Is(target error) bool { return target == ErrPathOutsideRepo }

// absClean is filepath.Abs with a lexical fallback, so a root or candidate that
// Abs cannot resolve still gets normalized rather than silently comparing a
// relative path against an absolute one.
func absClean(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// pathWithin reports whether an absolute target lies at or under an absolute
// root. It goes through filepath.Rel rather than a string prefix test, because
// "/repo" is a prefix of "/repo2" but "/repo2" is a different repository.
func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// containRelPath maps one caller-supplied path candidate onto its canonical
// repo-relative form, or refuses it. It is purely lexical: containment must hold
// for a path that does not exist, because a deletion candidate is exactly that.
//
// Accepted, all collapsing to the same canonical form:
//
//	src/a.go
//	./src/a.go
//	src/x/../a.go
//	<repoRoot>/src/a.go
//
// Refused: "..", "../x.go", "src/../../x.go", any absolute path outside
// repoRoot, and any path whose volume differs from the root's.
//
// root must already be absolute (see absClean). Comparing a relative root
// against an absolute candidate would make filepath.Rel fail and reject a file
// that is plainly inside the repository -- `serve --repo-root .` is enough to
// produce a relative root.
func containRelPath(root, path string) (string, error) {
	reject := func() (string, error) {
		return "", &PathScopeError{Path: path, RepoRoot: root}
	}
	if strings.TrimSpace(path) == "" {
		return reject()
	}
	rel := path
	if filepath.IsAbs(path) {
		// Rel fails across Windows volumes; that failure is a rejection, not a
		// reason to fall back to the absolute path as a "relative" one.
		v, err := filepath.Rel(root, path)
		if err != nil {
			return reject()
		}
		rel = v
	}
	rel = filepath.Clean(rel)
	// A volume-relative candidate ("C:a.go") is neither absolute nor
	// repo-relative, so Join would produce a path the caller never named.
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return reject()
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return reject()
	}
	return rel, nil
}

// containment answers "is this candidate inside the active repository" for one
// run, memoizing the filesystem work so a watcher flush of many files in a few
// directories pays for the directories, not the files.
type containment struct {
	root string
	// realRoot is root with symlinks resolved, computed once on first need.
	realRoot   string
	realRootOK bool
	resolved   bool
	// dirOK caches, per repo-relative directory, whether it stays inside the
	// repository once its symlinks are resolved.
	dirOK map[string]bool
}

func (c *containment) resolvedRoot() (string, bool) {
	if !c.resolved {
		c.resolved = true
		if real, err := filepath.EvalSymlinks(c.root); err == nil {
			c.realRoot, c.realRootOK = real, true
		}
	}
	return c.realRoot, c.realRootOK
}

// contain returns the canonical repo-relative form of one candidate.
//
// Two checks, in cost order. The lexical one refuses "../" and outside absolute
// paths without touching the filesystem. The physical one refuses a candidate
// whose *directory* leaves the repository once symlinks resolve.
//
// The physical check exists because filepath.Join is lexical while os.Stat and
// os.ReadFile are not: with a directory symlink "link -> /outside" inside the
// repository, "link/secret.go" is lexically contained yet reads an outside file.
// A full scan cannot reach that content -- filepath.WalkDir does not descend
// directory symlinks -- so admitting it here would let a path-scoped run index
// what a full run provably cannot.
//
// A symlinked *file* inside a real repository directory stays admitted, which is
// the existing contract in both scan modes (see entryFileInfo): its directory
// resolves inside the repository, only its own target leaves.
func (c *containment) contain(path string) (string, error) {
	rel, err := containRelPath(c.root, path)
	if err != nil {
		// A lexical miss on an absolute path may just be a different alias of
		// the same directory -- on macOS "/tmp" is a symlink to "/private/tmp",
		// so a client's absolute path and the resolved root can disagree while
		// naming one file. Only on this cold path do we pay for resolution.
		aliased, ok := c.retryAliased(path)
		if !ok {
			return "", err
		}
		rel = aliased
	}
	if !c.dirWithin(filepath.Dir(rel)) {
		return "", &PathScopeError{Path: path, RepoRoot: c.root}
	}
	return rel, nil
}

func (c *containment) retryAliased(path string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	realRoot, ok := c.resolvedRoot()
	if !ok {
		return "", false
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	rel, err := containRelPath(realRoot, realPath)
	if err != nil {
		return "", false
	}
	return rel, true
}

// dirWithin reports whether a repo-relative directory still resolves inside the
// repository. A directory that does not exist is contained by construction:
// nothing can be read through it, and a missing path must stay usable for
// deletion retirement.
func (c *containment) dirWithin(dir string) bool {
	if ok, cached := c.dirOK[dir]; cached {
		return ok
	}
	ok := c.evalDirWithin(dir)
	if c.dirOK == nil {
		c.dirOK = make(map[string]bool)
	}
	c.dirOK[dir] = ok
	return ok
}

func (c *containment) evalDirWithin(dir string) bool {
	real, err := filepath.EvalSymlinks(filepath.Join(c.root, dir))
	if err != nil {
		return true // missing directory: a deletion candidate, nothing to read
	}
	realRoot, ok := c.resolvedRoot()
	if !ok {
		realRoot = c.root
	}
	return pathWithin(realRoot, real)
}

// containCandidates normalizes an Options.Paths list, refusing the whole run on
// the first escaping candidate. Duplicates that name the same repository path
// collapse to one candidate, in first-seen order so a path-scoped run stays
// deterministic.
func containCandidates(repoRoot string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	c := &containment{root: absClean(repoRoot)}
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		rel, err := c.contain(path)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		out = append(out, rel)
	}
	return out, nil
}
