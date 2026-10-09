-- Scan coverage evidence. Rows written before this migration keep the
-- defaults, which read as "not recorded": scope '', heads '', overlap NULL.
ALTER TABLE scans ADD COLUMN scope TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN head_at_start TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN head_at_finish TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN overlapping_scans INTEGER;
