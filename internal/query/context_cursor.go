package query

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Cursor errors. Callers (the MCP handler) surface them verbatim: a stale cursor
// is a client-correctable condition, not an internal failure.
var (
	// ErrContextCursorInvalid marks a cursor that cannot be decoded or whose
	// contents are structurally impossible.
	ErrContextCursorInvalid = errors.New("invalid context cursor")
	// ErrContextCursorStale marks a decodable cursor that does not belong to the
	// request it was replayed against: another repository, another task, changed
	// ranking options, a re-indexed graph, or a re-ranked candidate stream.
	ErrContextCursorStale = errors.New("stale context cursor")
)

const (
	// contextCursorVersion is bumped whenever the encoded state or the meaning of
	// an offset changes, so old cursors fail closed instead of misreading.
	contextCursorVersion = 1
	// maxContextCursorInput bounds what the decoder will look at. A client
	// cannot make the server allocate by sending a megabyte of base64.
	maxContextCursorInput = 4096
)

// contextCursorState is the whole cursor. It is stateless: everything needed to
// validate a continuation travels in it, so any process serving the same graph
// can honour a cursor and no server-side session is kept.
//
// The task itself is never stored, only a hash of it: a cursor is passed around
// in agent logs and tool transcripts, and it does not need to carry the user's
// prompt text with it.
type contextCursorState struct {
	V      int    `json:"v"`
	Repo   int64  `json:"r"`
	Scan   int64  `json:"s"`
	Task   string `json:"t"`
	Opts   string `json:"o"`
	Rank   string `json:"f"`
	Offset int    `json:"n"`
}

func encodeContextCursor(st contextCursorState) (string, error) {
	payload, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeContextCursor(raw string) (contextCursorState, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return contextCursorState{}, fmt.Errorf("%w: empty", ErrContextCursorInvalid)
	}
	if len(trimmed) > maxContextCursorInput {
		return contextCursorState{}, fmt.Errorf("%w: longer than %d bytes", ErrContextCursorInvalid, maxContextCursorInput)
	}
	payload, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		// Tolerate padded base64 from clients that add it; reject anything else.
		payload, err = base64.URLEncoding.DecodeString(trimmed)
		if err != nil {
			return contextCursorState{}, fmt.Errorf("%w: not base64url", ErrContextCursorInvalid)
		}
	}
	var st contextCursorState
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return contextCursorState{}, fmt.Errorf("%w: %v", ErrContextCursorInvalid, err)
	}
	if st.V != contextCursorVersion {
		return contextCursorState{}, fmt.Errorf("%w: version %d, want %d", ErrContextCursorInvalid, st.V, contextCursorVersion)
	}
	if st.Offset < 0 {
		return contextCursorState{}, fmt.Errorf("%w: negative offset", ErrContextCursorInvalid)
	}
	if st.Repo <= 0 || st.Task == "" || st.Opts == "" || st.Rank == "" {
		return contextCursorState{}, fmt.Errorf("%w: incomplete state", ErrContextCursorInvalid)
	}
	return st, nil
}

// validateContextCursor checks a decoded cursor against the request it was
// replayed against. Every mismatch fails closed: continuing across a
// re-indexed graph or a re-ranked stream would silently skip and repeat
// context, which is worse than an error an agent can retry from page one.
func validateContextCursor(got, want contextCursorState, candidateCount int) error {
	switch {
	case got.Repo != want.Repo:
		return fmt.Errorf("%w: cursor belongs to repo %d, request is repo %d", ErrContextCursorStale, got.Repo, want.Repo)
	case got.Task != want.Task:
		return fmt.Errorf("%w: task differs from the task the cursor was issued for", ErrContextCursorStale)
	case got.Opts != want.Opts:
		return fmt.Errorf("%w: ranking options differ from the options the cursor was issued for", ErrContextCursorStale)
	case got.Scan != want.Scan:
		return fmt.Errorf("%w: graph was re-indexed (scan %d -> %d); restart without a cursor", ErrContextCursorStale, got.Scan, want.Scan)
	case got.Rank != want.Rank:
		return fmt.Errorf("%w: candidate ranking changed; restart without a cursor", ErrContextCursorStale)
	case got.Offset > candidateCount:
		return fmt.Errorf("%w: offset %d past %d candidates", ErrContextCursorStale, got.Offset, candidateCount)
	}
	return nil
}

// taskFingerprint hashes the task text the ranking was computed for. Whitespace
// is trimmed so a trailing newline does not invalidate a page, but nothing else
// is normalized: a different task is a different ranking.
func taskFingerprint(task string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(task)))
	return hex.EncodeToString(sum[:])[:16]
}

// optionsFingerprint hashes the options that affect which candidates exist and
// in what order. max_tokens is deliberately absent: an agent may ask for a
// smaller first page and a larger second one, and that must not invalidate the
// ranking the cursor points into.
func optionsFingerprint(opts ContextForTaskOptions) string {
	h := sha256.New()
	_, _ = h.Write([]byte("codegraph/context-opts/v1\n"))
	_, _ = h.Write([]byte(strconv.Itoa(opts.MaxFiles) + "\n"))
	_, _ = h.Write([]byte(strconv.Itoa(opts.MaxSymbols) + "\n"))
	_, _ = h.Write([]byte(strconv.FormatBool(opts.IncludeTests) + "\n"))
	_, _ = h.Write([]byte(strconv.FormatBool(opts.IncludeCallers) + "\n"))
	return hex.EncodeToString(h.Sum(nil))[:16]
}
