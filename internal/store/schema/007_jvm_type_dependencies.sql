ALTER TABLE jvm_compilation_scope_evidence
    ADD COLUMN source_state TEXT NOT NULL DEFAULT 'unknown'
        CHECK (source_state IN ('complete', 'incomplete', 'unknown'));
ALTER TABLE jvm_compilation_scope_evidence
    ADD COLUMN generated_state TEXT NOT NULL DEFAULT 'unknown'
        CHECK (generated_state IN ('complete', 'incomplete', 'unknown'));
ALTER TABLE jvm_compilation_scope_evidence
    ADD COLUMN excluded_state TEXT NOT NULL DEFAULT 'unknown'
        CHECK (excluded_state IN ('complete', 'incomplete', 'unknown'));
ALTER TABLE jvm_compilation_scope_evidence
    ADD COLUMN dependency_state TEXT NOT NULL DEFAULT 'unknown'
        CHECK (dependency_state IN ('complete', 'incomplete', 'unknown'));
ALTER TABLE jvm_compilation_scope_evidence
    ADD COLUMN external_metadata_state TEXT NOT NULL DEFAULT 'unknown'
        CHECK (external_metadata_state IN ('complete', 'incomplete', 'unknown'));
ALTER TABLE jvm_compilation_scope_evidence
    ADD COLUMN compiler_identity_state TEXT NOT NULL DEFAULT 'unknown'
        CHECK (compiler_identity_state IN ('complete', 'incomplete', 'unknown'));

CREATE TABLE jvm_type_declarations (
    repo_id INTEGER NOT NULL,
    evidence_key TEXT NOT NULL,
    declaration_key TEXT NOT NULL,
    lookup_key TEXT NOT NULL,
    file_path TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    source_language TEXT NOT NULL,
    declaration_kind TEXT NOT NULL,
    PRIMARY KEY (repo_id, evidence_key),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);
CREATE INDEX idx_jvm_type_declarations_lookup
    ON jvm_type_declarations(repo_id, lookup_key);
CREATE INDEX idx_jvm_type_declarations_path
    ON jvm_type_declarations(repo_id, file_path);

CREATE TABLE jvm_type_dependencies (
    repo_id INTEGER NOT NULL,
    consumer_key TEXT NOT NULL,
    consumer_path TEXT NOT NULL,
    lookup_key TEXT NOT NULL,
    dependency_kind TEXT NOT NULL CHECK (dependency_kind IN ('positive', 'negative', 'unknown', 'ambiguous')),
    target_key TEXT NOT NULL DEFAULT '',
    target_fingerprint TEXT NOT NULL DEFAULT '',
    source_language TEXT NOT NULL,
    type_position TEXT NOT NULL,
    type_ordinal INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('current', 'dirty')),
    provenance TEXT NOT NULL,
    PRIMARY KEY (repo_id, consumer_key, type_position, type_ordinal, lookup_key),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);
CREATE INDEX idx_jvm_type_dependencies_lookup
    ON jvm_type_dependencies(repo_id, lookup_key, state);
CREATE INDEX idx_jvm_type_dependencies_consumer_path
    ON jvm_type_dependencies(repo_id, consumer_path);

CREATE TABLE jvm_type_dependency_index_state (
    repo_id INTEGER PRIMARY KEY,
    initialized INTEGER NOT NULL CHECK (initialized IN (0, 1)),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);
INSERT INTO jvm_type_dependency_index_state(repo_id, initialized)
SELECT id, 0 FROM repos;

CREATE TABLE jvm_type_dependency_pending_paths (
    repo_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    PRIMARY KEY (repo_id, path),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);
