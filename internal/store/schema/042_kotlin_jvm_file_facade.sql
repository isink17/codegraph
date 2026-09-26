-- P24.5-B6A: Kotlin file-level JVM facade facts. The facade owner is
-- package_name + jvm_facade_class; '' means no provable facade. These are
-- source facts only: no facade symbol is ever persisted.
ALTER TABLE file_scope_evidence ADD COLUMN jvm_facade_class TEXT NOT NULL DEFAULT '';
ALTER TABLE file_scope_evidence ADD COLUMN jvm_facade_explicit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE file_scope_evidence ADD COLUMN jvm_multifile INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_file_scope_evidence_repo_jvm_facade
    ON file_scope_evidence(repo_id, jvm_facade_class) WHERE jvm_facade_class != '';
