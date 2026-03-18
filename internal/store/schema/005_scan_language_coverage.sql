CREATE TABLE IF NOT EXISTS scan_language_coverage (
    scan_id INTEGER NOT NULL,
    language TEXT NOT NULL,
    seen INTEGER NOT NULL DEFAULT 0,
    indexed INTEGER NOT NULL DEFAULT 0,
    skipped INTEGER NOT NULL DEFAULT 0,
    parse_failed INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scan_id, language),
    FOREIGN KEY (scan_id) REFERENCES scans(id)
);

CREATE INDEX IF NOT EXISTS idx_scan_lang_cov_scan
ON scan_language_coverage(scan_id);
