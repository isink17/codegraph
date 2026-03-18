CREATE INDEX IF NOT EXISTS idx_dirty_files_repo_queued
ON dirty_files(repo_id, queued_at);
