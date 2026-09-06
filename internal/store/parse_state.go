package store

// Persisted `files.parse_state` values. The column has existed since migration
// 001; these constants make it an enforced lifecycle contract rather than
// decorative metadata, and they are the vocabulary the indexer's re-entry
// decision reads back (see ExistingFileMeta.ParseState).
//
// The distinction that matters is "does the persisted parser-owned graph
// describe the file's current bytes?". Only ParseStateIndexed and
// ParseStateSkipped answer yes.
const (
	// ParseStatePending is the column default, and the marker repair
	// migrations write (033/034/035) to force a reparse. It never claims the
	// graph is current.
	ParseStatePending = "pending"

	// ParseStateIndexed means this file's persisted graph was produced by a
	// successful parse of exactly these bytes.
	ParseStateIndexed = "indexed"

	// ParseStateSkipped means the scan did not reparse the file because its
	// content hash was unchanged — the ordinary result of a checkout, rebase or
	// rebuild moving mtime. The persisted graph still describes these bytes.
	ParseStateSkipped = "skipped"

	// ParseStateOversize means the file is larger than `max_file_size_bytes`
	// and was never handed to a parser. A file in this state owns NO
	// parser-owned graph: whatever it declared under smaller bytes has been
	// retired, because a successful scan must not present an old graph as the
	// current one.
	ParseStateOversize = "oversize"

	// ParseStateFailed means a parse was attempted under
	// `parse_error_policy = best_effort` and failed. Like ParseStateOversize,
	// a file in this state owns no parser-owned graph.
	ParseStateFailed = "failed"

	// ParseStateDeleted marks a tombstone row (is_deleted = 1).
	ParseStateDeleted = "deleted"
)

// ParseStateDescribesCurrentBytes reports whether a persisted parse_state
// means the file's stored graph already describes the bytes on disk. It is the
// predicate the indexer's size/mtime fast path is allowed to trust: a file
// whose previous state was oversize, failed or pending must have its current
// eligibility re-evaluated even when nothing about the file changed, because
// what changed may be the configuration or the parser.
func ParseStateDescribesCurrentBytes(state string) bool {
	return state == ParseStateIndexed || state == ParseStateSkipped
}
