package detail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// Source-window bounds. All of them are line counts, because the indexer stores
// line ranges and nothing else: there are no byte offsets in the symbols table,
// so every source rule here is line-based by necessity as well as by choice.
const (
	// ContextLines is how many lines of surrounding context a rendered window
	// carries on each side of the symbol's own range. It is the same at excerpt
	// and at full, which is what makes full a superset of excerpt rather than a
	// differently-shaped answer.
	ContextLines = 3
	// ExcerptMaxLines is the ceiling on the lines an excerpt returns, context
	// included.
	ExcerptMaxLines = 60
	// FullMaxLines is the ceiling on the lines one symbol returns at full
	// detail. Full means "the whole symbol", and this is high enough that
	// ordinary declarations are never touched by it -- but a response is not
	// allowed to be unbounded, and a symbol longer than this is cut with the
	// same explicit truncation markers an excerpt uses.
	FullMaxLines = 1200
)

// ExcerptContextLines is retained as the excerpt-specific name for ContextLines.
//
// Deprecated: use ContextLines, which is what both excerpt and full apply.
const ExcerptContextLines = ContextLines

// Source is the rendered source attached to a symbol at excerpt or full detail.
//
// StartLine/EndLine describe what Text actually contains. AvailableStartLine and
// AvailableEndLine describe the symbol's own indexed range, so a client that
// sees Truncated knows exactly what it did not get.
type Source struct {
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`

	Truncated          bool `json:"truncated,omitempty"`
	AvailableStartLine int  `json:"available_start_line,omitempty"`
	AvailableEndLine   int  `json:"available_end_line,omitempty"`
}

// FileState is what the index recorded about a file's bytes, used to notice that
// the file changed after it was indexed.
type FileState struct {
	SizeBytes     int64
	MtimeUnixNs   int64
	ContentSHA256 string
}

// FileStater reports the indexed state of several files at once. Freshness is
// checked in one batched call per response, never once per symbol.
type FileStater interface {
	FileStates(ctx context.Context, paths []string) (map[string]FileState, error)
}

// sourceReader reads indexed files under one repository root, caching each
// file's lines for the lifetime of a single projection so that projecting N
// symbols from one file costs one read rather than N.
//
// It is not safe for concurrent use; a projection is a single-goroutine
// operation and each response builds its own reader.
type sourceReader struct {
	root     string
	rootReal string
	cache    map[string][]string
	fails    map[string]string
	stale    map[string]bool
	indexed  map[string]FileState
}

func newSourceReader(repoRoot string) *sourceReader {
	return &sourceReader{
		root:    strings.TrimSpace(repoRoot),
		cache:   map[string][]string{},
		fails:   map[string]string{},
		stale:   map[string]bool{},
		indexed: map[string]FileState{},
	}
}

// enabled reports whether this reader can render source at all.
func (r *sourceReader) enabled() bool { return r.root != "" }

// resolvedRoot returns the repository root with symlinks resolved, doing the
// work once and only when a file is actually about to be read. Resolving a path
// costs one syscall per component, and card -- the default level -- never reads
// a file, so it must not pay for this.
func (r *sourceReader) resolvedRoot() string {
	if r.rootReal != "" {
		return r.rootReal
	}
	r.rootReal = r.root
	if resolved, err := filepath.EvalSymlinks(r.root); err == nil {
		r.rootReal = resolved
	}
	return r.rootReal
}

// noteIndexedState merges what the index knows about a set of files into this
// reader. It merges rather than replaces so that a projector used for more than
// one batch does not lose the first batch's state.
func (r *sourceReader) noteIndexedState(states map[string]FileState) {
	for path, state := range states {
		r.indexed[path] = state
	}
}

// resolve turns an indexed repository-relative path into an absolute path that
// is provably inside the repository root, with symlinks resolved on both sides.
//
// Indexed paths are the only input here -- no tool argument reaches this
// function -- but the containment check is kept anyway, because a stale row, a
// path containing `..`, or a symlink pointing out of the tree must not be able
// to read outside the repository, and detail=full must not become a way around
// that.
func (r *sourceReader) resolve(rel string) (string, string) {
	if rel == "" {
		return "", "no indexed file path for this symbol"
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "path outside repository root"
	}
	root := r.resolvedRoot()
	abs := filepath.Join(root, clean)
	// A file that does not exist cannot be resolved; report that directly rather
	// than as a containment failure.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "file not found on disk"
		}
		return "", "file unreadable"
	}
	rootWithSep := strings.TrimSuffix(root, string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(resolved, rootWithSep) {
		return "", "path outside repository root"
	}
	return resolved, ""
}

// lines returns the file's lines with line endings stripped, or a reason string
// explaining why the source is unavailable.
//
// CRLF is handled here and only here: a trailing "\r" is removed from every
// line, so a Windows-checked-out file and a Unix one yield identical text and
// identical line numbering. Rendered source is therefore always LF-joined.
func (r *sourceReader) lines(rel string) ([]string, string) {
	if cached, ok := r.cache[rel]; ok {
		return cached, ""
	}
	if reason, ok := r.fails[rel]; ok {
		return nil, reason
	}
	abs, reason := r.resolve(rel)
	if reason != "" {
		r.fails[rel] = reason
		return nil, reason
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			r.fails[rel] = "file not found on disk"
		} else {
			r.fails[rel] = "file unreadable"
		}
		return nil, r.fails[rel]
	}
	r.stale[rel] = r.driftedFromIndex(rel, abs, data)

	split := strings.Split(string(data), "\n")
	for i, line := range split {
		split[i] = strings.TrimSuffix(line, "\r")
	}
	// A trailing newline produces one empty final element that is not a line of
	// the file; dropping it keeps EndLine equal to the real last line number.
	if n := len(split); n > 0 && split[n-1] == "" {
		split = split[:n-1]
	}
	r.cache[rel] = split
	return split, ""
}

// driftedFromIndex reports whether the bytes just read differ from the ones the
// indexer saw.
//
// Size is checked first because it settles most cases for free. A differing
// modification time alone is not drift -- a checkout or a `touch` rewrites the
// same bytes -- so when the size still matches, the content hash decides. The
// bytes are already in hand, so hashing them costs no extra read, and it is only
// reached for files whose mtime actually moved.
func (r *sourceReader) driftedFromIndex(rel, abs string, data []byte) bool {
	indexed, ok := r.indexed[rel]
	if !ok {
		return false
	}
	if indexed.SizeBytes != int64(len(data)) {
		return true
	}
	info, err := os.Stat(abs)
	if err != nil || indexed.MtimeUnixNs == 0 || info.ModTime().UnixNano() == indexed.MtimeUnixNs {
		return false
	}
	if indexed.ContentSHA256 == "" {
		// Nothing to compare against; a moved mtime with an unchanged size is
		// more often a checkout than an edit, so do not cry drift on a guess.
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) != indexed.ContentSHA256
}

// slice renders the source window for one symbol range.
//
// startLine/endLine are 1-based and inclusive, as stored by the indexer.
// contextLines is added on each side, and maxLines caps the result. Excerpt and
// full pass the same contextLines and different ceilings, which is what makes
// full a superset of excerpt for every symbol rather than only for long ones.
//
// Line metadata can be stale -- the file on disk may have shrunk since the last
// index -- so every bound is clamped to the file that actually exists. Clamping
// is reported the same way the ceiling is, because a response that quietly
// returns less source than the symbol has is exactly the silent truncation this
// model exists to avoid.
func (r *sourceReader) slice(rel string, startLine, endLine, contextLines, maxLines int) (*Source, string) {
	if !r.enabled() {
		return nil, "source rendering is disabled: no repository root"
	}
	lines, reason := r.lines(rel)
	if reason != "" {
		return nil, reason
	}
	total := len(lines)
	if total == 0 {
		return nil, "file is empty"
	}
	if startLine <= 0 {
		startLine = 1
	}
	if endLine < startLine {
		endLine = startLine
	}
	if startLine > total {
		return nil, "indexed line range is past the end of the file"
	}

	symEnd := endLine
	truncated := false
	if symEnd > total {
		symEnd = total
		truncated = true
	}

	from := max(1, startLine-contextLines)
	to := min(total, symEnd+contextLines)
	if maxLines > 0 && to-from+1 > maxLines {
		to = from + maxLines - 1
		truncated = true
	}

	src := &Source{
		StartLine: from,
		EndLine:   to,
		Text:      strings.Join(lines[from-1:to], "\n"),
	}
	if truncated {
		src.Truncated = true
		src.AvailableStartLine = startLine
		src.AvailableEndLine = endLine
	}
	return src, ""
}

// staleNote returns a note when the file backing rel changed after it was
// indexed, and the empty string otherwise.
func (r *sourceReader) staleNote(rel string) string {
	if r.stale[rel] {
		return "file changed since it was indexed; line ranges may not match the source shown"
	}
	return ""
}
