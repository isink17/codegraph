package constraints

import (
	"errors"
	"path"
	"strings"

	"github.com/isink17/codegraph/internal/platform"
)

// pattern is an anchored segment glob.
//
// The grammar is deliberately not the indexer's include/exclude matcher, which
// trims input, falls back to the basename and so lets `foo.go` match at any
// depth. Here a pattern is matched against the whole repository-relative
// logical path, segment by segment:
//
//   - `**` as a whole segment matches zero or more segments;
//   - any other segment is a path.Match pattern (`*`, `?`, `[...]`) confined to
//     one segment;
//   - matching is byte-exact on every OS: no case folding, no trimming, no
//     separator translation.
type pattern struct {
	raw  string
	segs []string
}

// compilePattern validates a pattern. Its literal shape must be a valid logical
// repository path, and a backslash is always rejected so that a Windows
// spelling such as `internal\store\**` fails loudly instead of matching nothing.
func compilePattern(raw string) (pattern, error) {
	if strings.ContainsRune(raw, '\\') {
		return pattern{}, errors.New("backslash is not allowed; use / as the separator")
	}
	if _, err := platform.LogicalRepositoryPath(raw); err != nil {
		return pattern{}, errors.New("must be a repository-relative path: no leading /, no empty, . or .. segment, no drive letter")
	}
	segs := strings.Split(raw, "/")
	for _, seg := range segs {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return pattern{}, err
		}
	}
	return pattern{raw: raw, segs: segs}, nil
}

func (p pattern) match(pathSegs []string) bool { return matchSegs(p.segs, pathSegs) }

// ponytail: recursive `**` backtracking is exponential in the number of `**`
// segments; patterns are short and hand-written, memoize if that ever changes.
func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
