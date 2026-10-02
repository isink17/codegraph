package store

import (
	"testing"
)

// setBinding injects a competing binding for current resolver invalidation tests.
func (f *parityFixture) setBinding(t *testing.T, edgeID, dstID int64, strategy, confidence string) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `
		UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ?
		WHERE id = ?`, dstID, strategy, confidence, edgeID); err != nil {
		t.Fatalf("force binding: %v", err)
	}
}
