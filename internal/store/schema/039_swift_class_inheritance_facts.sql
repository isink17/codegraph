CREATE TABLE IF NOT EXISTS swift_inheritance_relations (
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
CREATE INDEX IF NOT EXISTS idx_swift_inheritance_repo_file ON swift_inheritance_relations(repo_id, file_id);
CREATE INDEX IF NOT EXISTS idx_swift_inheritance_child ON swift_inheritance_relations(repo_id, child_qualified_name);

CREATE TABLE IF NOT EXISTS swift_declaration_facts (
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
CREATE INDEX IF NOT EXISTS idx_swift_declaration_facts_repo_file ON swift_declaration_facts(repo_id, file_id);
