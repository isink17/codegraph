-- Speeds up common large-repo queries and scan bookkeeping that filter out deleted files.
-- Partial index keeps bloat low while supporting:
--   - WHERE repo_id = ? AND is_deleted = 0
--   - GROUP BY language for active files
CREATE INDEX IF NOT EXISTS idx_files_repo_active_lang
ON files(repo_id, language)
WHERE is_deleted = 0;

