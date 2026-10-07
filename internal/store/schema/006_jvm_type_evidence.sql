CREATE TABLE jvm_type_evidence (
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER,
    evidence_key TEXT NOT NULL,
    owner_name TEXT NOT NULL DEFAULT '',
    source_language TEXT NOT NULL CHECK (source_language IN ('java', 'kotlin')),
    declaration_kind TEXT NOT NULL,
    modifiers TEXT NOT NULL DEFAULT '',
    has_type_parameters INTEGER NOT NULL CHECK (has_type_parameters IN (0, 1)),
    underlying_type TEXT NOT NULL DEFAULT '',
    underlying_state TEXT NOT NULL CHECK (underlying_state IN ('known', 'unknown', 'incomplete')),
    alias_target TEXT NOT NULL DEFAULT '',
    syntax_state TEXT NOT NULL CHECK (syntax_state IN ('known', 'unknown', 'incomplete')),
    provenance TEXT NOT NULL,
    PRIMARY KEY (repo_id, evidence_key),
    UNIQUE (repo_id, symbol_id),
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id),
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);

CREATE TABLE jvm_callable_type_evidence (
    repo_id INTEGER NOT NULL,
    symbol_id INTEGER NOT NULL,
    position TEXT NOT NULL CHECK (position IN ('parameter', 'result')),
    ordinal INTEGER NOT NULL,
    syntax TEXT NOT NULL,
    syntax_state TEXT NOT NULL CHECK (syntax_state IN ('known', 'unknown', 'incomplete')),
    PRIMARY KEY (repo_id, symbol_id, position, ordinal),
    FOREIGN KEY (repo_id, symbol_id) REFERENCES jvm_type_evidence(repo_id, symbol_id) ON DELETE CASCADE
);

CREATE INDEX idx_jvm_type_evidence_repo_file ON jvm_type_evidence(repo_id, file_id);

CREATE TABLE jvm_compilation_scope_evidence (
    repo_id INTEGER PRIMARY KEY,
    evidence_domain TEXT NOT NULL CHECK (evidence_domain = 'jvm-type-identity'),
    evidence_version INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('complete', 'incomplete', 'unknown')),
    provenance TEXT NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

INSERT INTO jvm_compilation_scope_evidence(repo_id, evidence_domain, evidence_version, state, provenance)
SELECT id, 'jvm-type-identity', 1, 'unknown', 'migration:006:no-compilation-scope-evidence' FROM repos;
