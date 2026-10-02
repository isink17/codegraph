-- Clean pre-v2 baseline. Development indexes are rebuilt, never upgraded.

CREATE TABLE dirty_files (
    repo_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    reason TEXT NOT NULL,
    queued_at TEXT NOT NULL,
    PRIMARY KEY (repo_id, path)
);

CREATE TABLE edges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    src_symbol_id INTEGER NOT NULL,
    dst_symbol_id INTEGER,
    dst_name TEXT NOT NULL DEFAULT '',
    edge_kind TEXT NOT NULL,
    evidence TEXT NOT NULL DEFAULT '',
    file_id INTEGER NOT NULL,
    line INTEGER NOT NULL DEFAULT 0, resolution_strategy TEXT NOT NULL DEFAULT '', resolution_confidence TEXT NOT NULL DEFAULT '', call_arity INTEGER,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE TABLE file_imports (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    import_path TEXT NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE TABLE file_scope_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    language TEXT NOT NULL,
    package_name TEXT NOT NULL DEFAULT '', module_path TEXT NOT NULL DEFAULT '', crate_root TEXT NOT NULL DEFAULT '', jvm_facade_class TEXT NOT NULL DEFAULT '', jvm_facade_explicit INTEGER NOT NULL DEFAULT 0, jvm_multifile INTEGER NOT NULL DEFAULT 0,
    UNIQUE(repo_id, file_id)
);

CREATE TABLE file_tokens (
    file_id INTEGER NOT NULL,
    token TEXT NOT NULL,
    weight REAL NOT NULL,
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE TABLE files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    language TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    mtime_unix_ns INTEGER NOT NULL DEFAULT 0,
    content_sha256 TEXT NOT NULL DEFAULT '',
    parse_state TEXT NOT NULL DEFAULT 'pending',
    last_scan_id INTEGER NOT NULL DEFAULT 0,
    indexed_at TEXT NOT NULL DEFAULT '',
    is_deleted INTEGER NOT NULL DEFAULT 0, parser_profile TEXT NOT NULL DEFAULT '', parser_call_edges INTEGER NOT NULL DEFAULT 0,
    UNIQUE (repo_id, path),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

CREATE TABLE go_local_binding_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    scope_start_line INTEGER NOT NULL,
    scope_end_line INTEGER NOT NULL,
    type_name TEXT NOT NULL DEFAULT '',
    type_package TEXT NOT NULL DEFAULT '',
    type_import_path TEXT NOT NULL DEFAULT '',
    is_pointer INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE TABLE kotlin_jvm_callable_evidence (
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER NOT NULL,
    is_known INTEGER NOT NULL CHECK (is_known IN (0, 1)),
    jvm_arity_min INTEGER,
    jvm_arity_max INTEGER,
    CHECK (
        (is_known = 1 AND jvm_arity_min IS NOT NULL AND jvm_arity_max IS NOT NULL
            AND 0 <= jvm_arity_min AND jvm_arity_min <= jvm_arity_max)
        OR (is_known = 0 AND jvm_arity_min IS NULL AND jvm_arity_max IS NULL)
    ),
    PRIMARY KEY (repo_id, symbol_id),
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id),
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);

CREATE TABLE kotlin_jvm_name_evidence (
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER NOT NULL,
    is_known INTEGER NOT NULL CHECK (is_known IN (0, 1)),
    jvm_name TEXT,
    CHECK (
        (is_known = 1 AND jvm_name IS NOT NULL AND jvm_name != '')
        OR (is_known = 0 AND jvm_name IS NULL)
    ),
    PRIMARY KEY (repo_id, symbol_id),
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id),
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);

CREATE TABLE php_composer_psr4_mapping (
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

CREATE TABLE references_tbl (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER,
    ref_kind TEXT NOT NULL,
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL,
    start_col INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    end_col INTEGER NOT NULL,
    context_symbol_id INTEGER,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE TABLE repos (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    root_path TEXT NOT NULL,
    canonical_path TEXT NOT NULL UNIQUE,
    config_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE rust_module_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    owner_module TEXT NOT NULL,
    module_name TEXT NOT NULL,
    external_path TEXT NOT NULL DEFAULT '',
    is_inline INTEGER NOT NULL DEFAULT 0,
    visibility TEXT NOT NULL DEFAULT ''
);

CREATE TABLE scan_language_coverage (
    scan_id INTEGER NOT NULL,
    language TEXT NOT NULL,
    seen INTEGER NOT NULL DEFAULT 0,
    indexed INTEGER NOT NULL DEFAULT 0,
    skipped INTEGER NOT NULL DEFAULT 0,
    parse_failed INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scan_id, language),
    FOREIGN KEY (scan_id) REFERENCES scans(id)
);

CREATE TABLE scans (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    scan_kind TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    status TEXT NOT NULL,
    files_seen INTEGER NOT NULL DEFAULT 0,
    files_changed INTEGER NOT NULL DEFAULT 0,
    files_deleted INTEGER NOT NULL DEFAULT 0,
    error_text TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE scope_import_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    language TEXT NOT NULL,
    source_specifier TEXT NOT NULL,
    imported_name TEXT NOT NULL DEFAULT '',
    local_name TEXT NOT NULL DEFAULT '',
    import_kind TEXT NOT NULL,
    wildcard INTEGER NOT NULL DEFAULT 0,
    is_static INTEGER NOT NULL DEFAULT 0,
    is_reexport INTEGER NOT NULL DEFAULT 0,
    is_namespace_export INTEGER NOT NULL DEFAULT 0,
    is_type_only INTEGER NOT NULL DEFAULT 0
, owner_module TEXT NOT NULL DEFAULT '');

CREATE TABLE scope_module_candidate_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    source_file_id INTEGER NOT NULL,
    source_specifier TEXT NOT NULL,
    candidate_path TEXT NOT NULL,
    UNIQUE(repo_id, source_file_id, source_specifier, candidate_path)
);

CREATE TABLE session_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id    INTEGER NOT NULL,
    session_id TEXT    NOT NULL DEFAULT '',
    event_type TEXT    NOT NULL,  -- 'read', 'edit', 'decision', 'task', 'fact'
    key        TEXT    NOT NULL DEFAULT '',
    value      TEXT    NOT NULL DEFAULT '',
    metadata   TEXT    NOT NULL DEFAULT '{}',
    created_at TEXT    NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE swift_declaration_facts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER NOT NULL,
    is_final INTEGER NOT NULL DEFAULT 0,
    is_override INTEGER NOT NULL DEFAULT 0,
    dispatch_kind TEXT NOT NULL CHECK (dispatch_kind IN ('', 'instance', 'static', 'class')),
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id),
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);

CREATE TABLE swift_extension_memberships (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER NOT NULL,
    target_qualified_name TEXT NOT NULL,
    extension_start_line INTEGER NOT NULL,
    extension_start_col INTEGER NOT NULL,
    extension_end_line INTEGER NOT NULL,
    extension_end_col INTEGER NOT NULL,
    is_generic INTEGER NOT NULL DEFAULT 0,
    is_constrained INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id),
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);

CREATE TABLE swift_inheritance_relations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    child_qualified_name TEXT NOT NULL,
    target_qualified_name TEXT NOT NULL,
    relation_kind TEXT NOT NULL CHECK (relation_kind IN ('superclass', 'conformance', 'unproven')),
    start_line INTEGER NOT NULL,
    start_col INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    end_col INTEGER NOT NULL,
    is_generic INTEGER NOT NULL DEFAULT 0,
    is_constrained INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE TABLE swift_lexical_binding_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    binding_kind TEXT NOT NULL,
    owner_module TEXT NOT NULL DEFAULT '',
    scope_start_line INTEGER NOT NULL,
    scope_end_line INTEGER NOT NULL
);

CREATE TABLE symbol_embeddings (
    symbol_id   INTEGER PRIMARY KEY,
    file_id     INTEGER NOT NULL,
    repo_id     INTEGER NOT NULL,
    embedding   BLOB    NOT NULL,
    dimensions  INTEGER NOT NULL,
    model_name  TEXT    NOT NULL DEFAULT '',
    updated_at  TEXT    NOT NULL,
    FOREIGN KEY (symbol_id) REFERENCES symbols(id),
    FOREIGN KEY (file_id)   REFERENCES files(id)
);

CREATE VIRTUAL TABLE symbol_fts USING fts5(
    repo_id UNINDEXED,
    symbol_id UNINDEXED,
    name,
    qualified_name,
    signature,
    doc_summary
);

CREATE TABLE symbol_tokens (
    symbol_id INTEGER NOT NULL,
    token TEXT NOT NULL,
    weight REAL NOT NULL,
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);

CREATE TABLE symbols (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    language TEXT NOT NULL,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL,
    container_name TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL DEFAULT '',
    visibility TEXT NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL,
    start_col INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    end_col INTEGER NOT NULL,
    doc_summary TEXT NOT NULL DEFAULT '',
    stable_key TEXT NOT NULL, qualified_suffix TEXT NOT NULL DEFAULT '', dot_tail2 TEXT NOT NULL DEFAULT '', dot_tail3 TEXT NOT NULL DEFAULT '', is_static INTEGER, arity_min INTEGER, arity_max INTEGER,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE TABLE test_links (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    test_file_id INTEGER NOT NULL,
    test_symbol_id INTEGER,
    target_file_id INTEGER,
    target_symbol_id INTEGER,
    reason TEXT NOT NULL,
    score REAL NOT NULL DEFAULT 0, target_stable_key TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (test_file_id) REFERENCES files(id)
);

CREATE INDEX idx_dirty_files_repo_queued
ON dirty_files(repo_id, queued_at);

CREATE INDEX idx_edges_file_id
ON edges(file_id);

CREATE INDEX idx_edges_repo_bound_dotname
    ON edges(repo_id, dst_name)
    WHERE dst_symbol_id IS NOT NULL AND instr(dst_name, '.') > 0;

CREATE INDEX idx_edges_repo_dst ON edges(repo_id, dst_symbol_id);

CREATE INDEX idx_edges_repo_dst_file
ON edges(repo_id, dst_symbol_id, file_id);

CREATE INDEX idx_edges_repo_file_unresolved
ON edges(repo_id, file_id)
WHERE dst_symbol_id IS NULL;

CREATE INDEX idx_edges_repo_name ON edges(repo_id, dst_name);

CREATE INDEX idx_edges_repo_src ON edges(repo_id, src_symbol_id);

CREATE INDEX idx_edges_repo_unresolved_dotname
ON edges(repo_id, dst_name)
WHERE dst_symbol_id IS NULL AND instr(dst_name, '.') > 0;

CREATE INDEX idx_edges_repo_unresolved_name_src
ON edges(repo_id, dst_name, src_symbol_id)
WHERE dst_symbol_id IS NULL;

CREATE INDEX idx_file_imports_file_id
ON file_imports(file_id);

CREATE INDEX idx_file_scope_evidence_repo_crate_file
    ON file_scope_evidence(repo_id, crate_root, file_id);

CREATE INDEX idx_file_scope_evidence_repo_file
    ON file_scope_evidence(repo_id, file_id);

CREATE INDEX idx_file_scope_evidence_repo_jvm_facade
    ON file_scope_evidence(repo_id, jvm_facade_class) WHERE jvm_facade_class != '';

CREATE INDEX idx_file_tokens_file_id ON file_tokens(file_id);

CREATE INDEX idx_file_tokens_token ON file_tokens(token);

CREATE INDEX idx_files_repo_active_lang
ON files(repo_id, language)
WHERE is_deleted = 0;

CREATE INDEX idx_files_repo_path ON files(repo_id, path);

CREATE INDEX idx_go_local_binding_repo_file
    ON go_local_binding_evidence(repo_id, file_id);

CREATE INDEX idx_go_local_binding_repo_file_name
    ON go_local_binding_evidence(repo_id, file_id, name);

CREATE INDEX idx_kotlin_jvm_callable_repo_file
    ON kotlin_jvm_callable_evidence(repo_id, file_id);

CREATE INDEX idx_kotlin_jvm_name_repo_file
    ON kotlin_jvm_name_evidence(repo_id, file_id);

CREATE INDEX idx_php_composer_psr4_mapping_repo_role_prefix
    ON php_composer_psr4_mapping(repo_id, mapping_role, namespace_prefix);

CREATE INDEX idx_php_composer_psr4_mapping_repo_root
    ON php_composer_psr4_mapping(repo_id, root_path);

CREATE INDEX idx_refs_file_id
ON references_tbl(file_id);

CREATE INDEX idx_refs_repo_context_symbol_id
ON references_tbl(repo_id, context_symbol_id)
WHERE context_symbol_id IS NOT NULL;

CREATE INDEX idx_refs_repo_name ON references_tbl(repo_id, name);

CREATE INDEX idx_refs_repo_symbol_id
ON references_tbl(repo_id, symbol_id)
WHERE symbol_id IS NOT NULL;

CREATE INDEX idx_rust_module_evidence_repo_name
    ON rust_module_evidence(repo_id, module_name);

CREATE INDEX idx_rust_module_evidence_repo_owner
    ON rust_module_evidence(repo_id, file_id, owner_module);

CREATE INDEX idx_scan_lang_cov_scan
ON scan_language_coverage(scan_id);

CREATE INDEX idx_scope_import_evidence_file
    ON scope_import_evidence(repo_id, file_id);

CREATE INDEX idx_scope_import_evidence_repo_local
    ON scope_import_evidence(repo_id, local_name);

CREATE INDEX idx_scope_import_evidence_repo_source
    ON scope_import_evidence(repo_id, source_specifier);

CREATE INDEX idx_scope_module_candidates_repo_path
    ON scope_module_candidate_evidence(repo_id, candidate_path);

CREATE INDEX idx_scope_module_candidates_repo_source
    ON scope_module_candidate_evidence(repo_id, source_file_id);

CREATE INDEX idx_session_events_key ON session_events(repo_id, key);

CREATE INDEX idx_session_events_repo ON session_events(repo_id);

CREATE INDEX idx_session_events_session ON session_events(repo_id, session_id);

CREATE INDEX idx_session_events_type ON session_events(repo_id, event_type);

CREATE INDEX idx_swift_declaration_facts_repo_file ON swift_declaration_facts(repo_id, file_id);

CREATE INDEX idx_swift_extension_memberships_repo_file
    ON swift_extension_memberships(repo_id, file_id);

CREATE INDEX idx_swift_extension_memberships_symbol
    ON swift_extension_memberships(repo_id, symbol_id);

CREATE INDEX idx_swift_inheritance_child ON swift_inheritance_relations(repo_id, child_qualified_name);

CREATE INDEX idx_swift_inheritance_repo_file ON swift_inheritance_relations(repo_id, file_id);

CREATE INDEX idx_swift_lexical_binding_repo_file
    ON swift_lexical_binding_evidence(repo_id, file_id);

CREATE INDEX idx_swift_lexical_binding_repo_file_name
    ON swift_lexical_binding_evidence(repo_id, file_id, name);

CREATE INDEX idx_symbol_embeddings_file ON symbol_embeddings(file_id);

CREATE INDEX idx_symbol_embeddings_repo ON symbol_embeddings(repo_id);

CREATE INDEX idx_symbol_tokens_symbol_id ON symbol_tokens(symbol_id);

CREATE INDEX idx_symbol_tokens_token_symbol
ON symbol_tokens(token, symbol_id, weight);

CREATE INDEX idx_symbols_file_start
ON symbols(file_id, start_line);

CREATE INDEX idx_symbols_repo_dot_tail2
    ON symbols(repo_id, dot_tail2)
    WHERE dot_tail2 != '';

CREATE INDEX idx_symbols_repo_dot_tail2_nocase
    ON symbols(repo_id, dot_tail2 COLLATE NOCASE)
    WHERE dot_tail2 != '';

CREATE INDEX idx_symbols_repo_dot_tail3
    ON symbols(repo_id, dot_tail3)
    WHERE dot_tail3 != '';

CREATE INDEX idx_symbols_repo_name ON symbols(repo_id, name);

CREATE INDEX idx_symbols_repo_name_has_container
ON symbols(repo_id, name)
WHERE container_name != '';

CREATE INDEX idx_symbols_repo_name_kind
ON symbols(repo_id, name, kind);

CREATE INDEX idx_symbols_repo_name_resolve_kind
ON symbols(repo_id, name)
WHERE kind IN ('function', 'method', 'class', 'type', 'struct', 'interface');

CREATE INDEX idx_symbols_repo_qname ON symbols(repo_id, qualified_name);

CREATE INDEX idx_symbols_repo_qsuffix
    ON symbols(repo_id, qualified_suffix)
    WHERE qualified_suffix != '';

CREATE INDEX idx_symbols_repo_stable ON symbols(repo_id, stable_key);

CREATE INDEX idx_test_links_repo_target
ON test_links(repo_id, target_symbol_id);

CREATE INDEX idx_test_links_repo_target_file
ON test_links(repo_id, target_file_id)
WHERE target_file_id IS NOT NULL;

CREATE INDEX idx_test_links_repo_target_key
ON test_links(repo_id, target_stable_key);

CREATE INDEX idx_test_links_repo_test_file
ON test_links(repo_id, test_file_id);

CREATE INDEX idx_test_links_repo_test_symbol
ON test_links(repo_id, test_symbol_id);

CREATE INDEX idx_test_links_test_file_id
ON test_links(test_file_id);

INSERT INTO settings(key, value) VALUES('format.schema_baseline', 'v2-20261002');
