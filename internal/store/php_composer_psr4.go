package store

import (
	"context"
	"database/sql"
	"strconv"
)

const phpComposerPSR4FingerprintSettingKey = "scope.php_composer_psr4.v2"

type PHPComposerPSR4Mapping struct {
	ManifestPath    string
	MappingRole     string
	NamespacePrefix string
	RootPath        string
	RootOrdinal     int
}

func phpComposerPSR4FingerprintKey(repoID int64) string {
	return phpComposerPSR4FingerprintSettingKey + "." + strconv.FormatInt(repoID, 10)
}

func (s *Store) PHPComposerPSR4Fingerprint(ctx context.Context, repoID int64) (string, bool, error) {
	var value sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, phpComposerPSR4FingerprintKey(repoID)).Scan(&value); err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}
	return value.String, value.Valid, nil
}

func (s *Store) SetPHPComposerPSR4Fingerprint(ctx context.Context, repoID int64, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, phpComposerPSR4FingerprintKey(repoID), value)
	return err
}

// ReplacePHPComposerPSR4Mappings atomically replaces all Composer PSR-4
// evidence for repoID. An empty set records no usable Composer evidence.
func (s *Store) ReplacePHPComposerPSR4Mappings(ctx context.Context, repoID int64, mappings []PHPComposerPSR4Mapping) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replacePHPComposerPSR4Mappings(ctx, tx, repoID, mappings); err != nil {
		return err
	}
	return tx.Commit()
}

func replacePHPComposerPSR4Mappings(ctx context.Context, tx *sql.Tx, repoID int64, mappings []PHPComposerPSR4Mapping) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM php_composer_psr4_mapping WHERE repo_id=?`, repoID); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO php_composer_psr4_mapping(
			repo_id, manifest_path, mapping_role, namespace_prefix, root_path, root_ordinal
		) VALUES(?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, mapping := range mappings {
		if _, err := stmt.ExecContext(ctx, repoID, mapping.ManifestPath, mapping.MappingRole, mapping.NamespacePrefix, mapping.RootPath, mapping.RootOrdinal); err != nil {
			return err
		}
	}
	return nil
}

// ReconcilePHPComposerPSR4 atomically converges Composer evidence, only the
// bindings it owns, and reference identities derived from those bindings.
func (s *Store) ReconcilePHPComposerPSR4(ctx context.Context, repoID int64, mappings []PHPComposerPSR4Mapping) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := replacePHPComposerPSR4Mappings(ctx, tx, repoID, mappings); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+`
		WHERE repo_id=? AND resolution_strategy=?`, repoID, ResolutionStrategyPHPComposerPSR4); err != nil {
		return err
	}
	if _, err := resolvePHPScope(ctx, tx, repoID, nil); err != nil {
		return err
	}
	if err := reconcileReferenceIdentities(ctx, tx, repoID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PHPComposerPSR4Mappings(ctx context.Context, repoID int64) ([]PHPComposerPSR4Mapping, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT manifest_path, mapping_role, namespace_prefix, root_path, root_ordinal
		FROM php_composer_psr4_mapping
		WHERE repo_id=?
		ORDER BY mapping_role, namespace_prefix, root_ordinal, root_path`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mappings []PHPComposerPSR4Mapping
	for rows.Next() {
		var mapping PHPComposerPSR4Mapping
		if err := rows.Scan(&mapping.ManifestPath, &mapping.MappingRole, &mapping.NamespacePrefix, &mapping.RootPath, &mapping.RootOrdinal); err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	return mappings, rows.Err()
}
