-- Supports RelatedTests(file=...) lookups without scanning test_links.
CREATE INDEX IF NOT EXISTS idx_test_links_repo_target_file
ON test_links(repo_id, target_file_id)
WHERE target_file_id IS NOT NULL;

