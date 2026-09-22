package store

import (
	"context"
	"database/sql"
	"strconv"
)

const swiftPMManifestFingerprintSettingKey = "scope.swiftpm_manifest_fingerprint.v2"

func swiftPMManifestFingerprintKey(repoID int64) string {
	return swiftPMManifestFingerprintSettingKey + "." + strconv.FormatInt(repoID, 10)
}

func (s *Store) SwiftPMManifestFingerprint(ctx context.Context, repoID int64) (string, bool, error) {
	var value sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, swiftPMManifestFingerprintKey(repoID)).Scan(&value); err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}
	return value.String, value.Valid, nil
}

func (s *Store) SetSwiftPMManifestFingerprint(ctx context.Context, repoID int64, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, swiftPMManifestFingerprintKey(repoID), value)
	return err
}
