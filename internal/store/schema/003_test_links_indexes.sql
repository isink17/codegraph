CREATE INDEX IF NOT EXISTS idx_test_links_repo_target
ON test_links(repo_id, target_symbol_id);

CREATE INDEX IF NOT EXISTS idx_test_links_repo_test_file
ON test_links(repo_id, test_file_id);
