CREATE TABLE IF NOT EXISTS php_composer_psr4_mapping (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    manifest_path TEXT NOT NULL,
    mapping_role TEXT NOT NULL,
    namespace_prefix TEXT NOT NULL,
    root_path TEXT NOT NULL,
    root_ordinal INTEGER NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    UNIQUE(repo_id, manifest_path, mapping_role, namespace_prefix, root_ordinal)
);
CREATE INDEX IF NOT EXISTS idx_php_composer_psr4_mapping_repo_role_prefix
    ON php_composer_psr4_mapping(repo_id, mapping_role, namespace_prefix);
CREATE INDEX IF NOT EXISTS idx_php_composer_psr4_mapping_repo_root
    ON php_composer_psr4_mapping(repo_id, root_path);
