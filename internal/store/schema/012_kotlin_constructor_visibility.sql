ALTER TABLE jvm_type_evidence
    ADD COLUMN constructor_visibility TEXT NOT NULL DEFAULT 'unknown'
    CHECK (constructor_visibility IN ('public', 'private', 'protected', 'internal', 'unknown'));
