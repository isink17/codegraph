package store

import (
	"context"
	"fmt"
	"sort"
)

// SemanticGraphForTest renders the repo's resolved relationships as sorted,
// id-free text.
//
// Every field is a semantic identity (file path, qualified name, resolution
// strategy/confidence, reason/score) rather than a SQLite row id, so two index
// runs that produce the same graph produce the same snapshot even though their
// AUTOINCREMENT ids differ. That is exactly the property the P10 order tests
// assert, and comparing ids instead would make them pass or fail for the wrong
// reason.
//
// Test-only helper: the production API deliberately does not grow another
// exported diagnostic method for this.
func (s *Store) SemanticGraphForTest(ctx context.Context, repoID int64) ([]string, error) {
	lines := make([]string, 0, 64)

	edgeRows, err := s.db.QueryContext(ctx, `
		SELECT sf.path, ss.qualified_name, e.edge_kind, e.dst_name,
			COALESCE(df.path, ''), COALESCE(ds.qualified_name, ''), COALESCE(ds.kind, ''),
			e.resolution_strategy, e.resolution_confidence
		FROM edges e
		JOIN symbols ss ON ss.id = e.src_symbol_id
		JOIN files sf ON sf.id = e.file_id
		LEFT JOIN symbols ds ON ds.id = e.dst_symbol_id
		LEFT JOIN files df ON df.id = ds.file_id
		WHERE e.repo_id = ?
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer edgeRows.Close()
	for edgeRows.Next() {
		var srcFile, srcName, kind, dstName, dstFile, dstQName, dstKind, strategy, confidence string
		if err := edgeRows.Scan(&srcFile, &srcName, &kind, &dstName, &dstFile, &dstQName, &dstKind, &strategy, &confidence); err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("edge %s:%s -%s-> %q => %s:%s(%s) [%s/%s]",
			srcFile, srcName, kind, dstName, dstFile, dstQName, dstKind, strategy, confidence))
	}
	if err := edgeRows.Err(); err != nil {
		return nil, err
	}

	linkRows, err := s.db.QueryContext(ctx, `
		SELECT tf.path, COALESCE(ts.qualified_name, ''), t.target_stable_key,
			COALESCE(gf.path, ''), COALESCE(gs.qualified_name, ''), t.reason, t.score
		FROM test_links t
		JOIN files tf ON tf.id = t.test_file_id
		LEFT JOIN symbols ts ON ts.id = t.test_symbol_id
		LEFT JOIN symbols gs ON gs.id = t.target_symbol_id
		LEFT JOIN files gf ON gf.id = t.target_file_id
		WHERE t.repo_id = ?
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer linkRows.Close()
	for linkRows.Next() {
		var testFile, testName, targetKey, targetFile, targetName, reason string
		var score float64
		if err := linkRows.Scan(&testFile, &testName, &targetKey, &targetFile, &targetName, &reason, &score); err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("testlink %s:%s -> %q => %s:%s [%s/%.2f]",
			testFile, testName, targetKey, targetFile, targetName, reason, score))
	}
	if err := linkRows.Err(); err != nil {
		return nil, err
	}

	sort.Strings(lines)
	return lines, nil
}
