package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// cppReceiverReparseSettingKey records that a repository's C/C++ files have
// been marked for the one-time reparse P22.11 needs.
//
// Why a reparse and not a binding repair. Every other resolver upgrade so far
// (see repairTypeScopeBindingsOnce) could be expressed as "clear the bindings
// the current rules refuse and let the resolver decide again", because the
// facts those rules read were already persisted correctly. P22.11 is different:
// the defect is in the persisted fact itself. An older release wrote
// `dst_name = 'size'` for `v.size()`, so clearing the binding would only let
// the resolver rebind the same bare name to the same wrong target. The receiver
// exists nowhere but in `edges.evidence`, and reconstructing a destination
// identity by parsing that string in SQL would be a second, drifting
// implementation of C++ call syntax -- the duplication resolver_testfile.go
// forbids. Re-running the real parser over the affected files is the only
// repair that cannot disagree with the parser.
//
// How the reparse is triggered: the indexer skips an unchanged file on
// (size, mtime) before it ever hashes it, and on the content hash after. So
// both have to be invalidated for the file to reach the adapter again. Nothing
// else about the row is touched -- the file keeps its id, so no symbol,
// reference, or edge is orphaned before the reparse replaces them.
//
// Keyed per repository, and written only after the update succeeds, so a
// failure re-runs rather than marking a repository upgraded that is not.
const cppReceiverReparseSettingKey = "parser.cpp_receiver_reparsed.v1"

func cppReceiverRepairKey(repoID int64) string {
	return cppReceiverReparseSettingKey + "." + strconv.FormatInt(repoID, 10)
}

// CppReceiverUpgradePending reports whether this repository still owes the
// one-time C/C++ reparse. It is separate from the mark itself so a caller can
// answer the cheap question first and only then pay for the parser-capability
// probe that decides whether marking is safe at all.
func (s *Store) CppReceiverUpgradePending(ctx context.Context, repoID int64) (bool, error) {
	var done string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, cppReceiverRepairKey(repoID)).Scan(&done)
	switch {
	case err == nil:
		return done != "1", nil
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	default:
		return false, err
	}
}

// MarkCppFilesForReparseOnce invalidates the change-detection metadata of this
// repository's C/C++ files the first time it is called, so the run that calls
// it reparses them and replaces their pre-P22.11 receiver-discarded call edges.
// Subsequent calls cost one indexed SELECT and do nothing.
//
// The caller is responsible for only invoking this on a run that actually walks
// the whole repository, admits the cpp language, and has a C/C++ adapter that
// produces call edges; see the call site in the indexer for why each matters.
func (s *Store) MarkCppFilesForReparseOnce(ctx context.Context, repoID int64) error {
	pending, err := s.CppReceiverUpgradePending(ctx, repoID)
	if err != nil || !pending {
		return err
	}
	// mtime_unix_ns = -1 defeats the (size, mtime) skip, which runs before the
	// file is read at all; content_sha256 = '' defeats the hash comparison that
	// runs after. A real file can match neither. Scoped to this repository, so a
	// database holding several upgrades them independently.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE files SET mtime_unix_ns = -1, content_sha256 = ''
		WHERE repo_id = ? AND is_deleted = 0 AND language = 'cpp'`, repoID); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES(?, '1')
		 ON CONFLICT(key) DO UPDATE SET value = '1'`, cppReceiverRepairKey(repoID))
	return err
}
