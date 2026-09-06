package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrRepositoryScopeViolation is the sentinel behind every refusal to retarget
// this server. errors.Is(err, ErrRepositoryScopeViolation) is the supported test.
var ErrRepositoryScopeViolation = errors.New("repository scope violation")

// RepositoryScopeError reports a tool call that named a repository other than
// the one this server was opened for.
//
// One MCP server is one active repository, fixed at construction. repo_root and
// repo_path stay accepted so a client may assert which repository it believes it
// is talking to, but an assertion that does not match is an error rather than a
// retarget: the store, the repo id, and the query service on this server all
// belong to Active, and nothing on the tool surface can change that.
type RepositoryScopeError struct {
	Active    string
	Requested string
}

func (e *RepositoryScopeError) Error() string {
	return fmt.Sprintf("repository scope violation: MCP server is scoped to %q; requested repository %q is outside this server's active repository",
		e.Active, e.Requested)
}

func (e *RepositoryScopeError) Is(target error) bool {
	return target == ErrRepositoryScopeViolation
}

// assertActiveRepo accepts a client-supplied repository root only when it
// identifies this server's active repository, and refuses everything else --
// another repository, the parent directory, a subdirectory of the active root,
// or a path that does not exist.
//
// Identity, not string equality: a cleaned alias ("/repo/." or "/repo/sub/..")
// matches lexically, and a symlink alias to the same directory matches through
// os.SameFile. A path that cannot be stat'ed is refused rather than accepted on
// the strength of string normalization alone.
func (s *Server) assertActiveRepo(requested string) error {
	reject := func() error {
		return &RepositoryScopeError{Active: s.repoRoot, Requested: requested}
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return reject()
	}
	activeAbs, err := filepath.Abs(s.repoRoot)
	if err != nil {
		return reject()
	}
	// Lexical hit is authoritative and costs no syscall, which keeps the common
	// "client echoed back the root it was given" call free.
	if abs == activeAbs {
		return nil
	}
	requestedInfo, err := os.Stat(abs)
	if err != nil {
		return reject()
	}
	activeInfo, err := os.Stat(activeAbs)
	if err != nil {
		return reject()
	}
	if !os.SameFile(requestedInfo, activeInfo) {
		return reject()
	}
	return nil
}

// assertActiveRepoArgs validates every repository field a tool call supplied.
// Both repo_root and repo_path are checked, so a conflicting second field can
// never be silently ignored the way first-non-empty-wins would ignore it.
func (s *Server) assertActiveRepoArgs(repoRoot, repoPath string) error {
	for _, requested := range []string{repoRoot, repoPath} {
		if v := strings.TrimSpace(requested); v != "" {
			if err := s.assertActiveRepo(v); err != nil {
				return err
			}
		}
	}
	return nil
}
