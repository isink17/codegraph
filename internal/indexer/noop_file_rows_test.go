//go:build cgo

package indexer

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/query"
)

// SELECT * deliberately covers every persisted column, including scan markers,
// timestamps, parser profiles, and future metadata additions.
func snapshotFileRows(t *testing.T, db *sql.DB) [][]any {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM files ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for j := range values {
			ptrs[j] = &values[j]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUpdatePresenceRespectsFiltering(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, policy := range []string{"include", "exclude-file", "exclude-dir", "language", "extension"} {
			name := policy + "/whole-tree"
			if scoped {
				name = policy + "/path-scoped"
			}
			t.Run(name, func(t *testing.T) {
				r := newLifecycleRepo(t, tree{"src/main.py": "def target():\n    return 1\n"})
				db := r.raw(t)
				rel := "src/main.py"
				opts := Options{RepoRoot: r.root}
				switch policy {
				case "include":
					opts.Include = []string{"**/*.go"}
				case "exclude-file":
					opts.Exclude = []string{rel}
				case "exclude-dir":
					opts.Exclude = []string{"src/**"}
				case "language":
					opts.Languages = []string{"go"}
				case "extension":
					// Simulate a persisted entry from before an extension was
					// denied. Such entries must follow skip_only semantics.
					rel = "src/main.png"
					if err := os.Rename(filepath.Join(r.root, "src", "main.py"), filepath.Join(r.root, "src", "main.png")); err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec(`UPDATE files SET path=? WHERE path='src/main.py'`, rel); err != nil {
						t.Fatal(err)
					}
				}
				if scoped {
					opts.Paths = []string{rel}
				}
				before := snapshotFileRows(t, db)
				summary, err := r.idx.Update(r.ctx, opts)
				if err != nil {
					t.Fatal(err)
				}
				wantDeleted := 0
				if !scoped && policy != "language" {
					wantDeleted = 1
				}
				if summary.FilesDeleted != wantDeleted {
					t.Fatalf("FilesDeleted = %d, want %d", summary.FilesDeleted, wantDeleted)
				}
				if wantDeleted == 0 {
					if after := snapshotFileRows(t, db); !reflect.DeepEqual(before, after) {
						t.Fatalf("preserved rows changed:\nbefore %v\nafter %v", before, after)
					}
				}
				// A physically removed language-filtered file is still deleted;
				// language filtering only preserves entries actually encountered.
				if policy == "language" {
					r.remove(t, rel)
					summary, err = r.idx.Update(r.ctx, opts)
					if err != nil || summary.FilesDeleted != 1 {
						t.Fatalf("removed filtered file: %+v, %v", summary, err)
					}
				}
			})
		}
	}
}

func TestNoopUpdatePreservesEveryFileRow(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"main.py":     "from provider import target\ndef run():\n    target()\n",
		"provider.py": "def target():\n    return 1\n",
		"README":      "plain text\n",
	})
	db := r.raw(t)
	if target := r.refTarget(t, "main.py", "target"); target == "" {
		t.Fatal("fixture call reference must resolve before deletion")
	}
	before := snapshotFileRows(t, db)
	for n := 0; n < 2; n++ {
		summary := r.update(t)
		if summary.FilesChanged != 0 || summary.FilesIndexed != 0 || summary.FilesDeleted != 0 {
			t.Fatalf("no-op update = %+v", summary)
		}
		if after := snapshotFileRows(t, db); !reflect.DeepEqual(before, after) {
			t.Fatalf("no-op rewrote files rows:\nbefore %v\nafter  %v", before, after)
		}
		var scans int
		if err := db.QueryRow(`SELECT COUNT(*) FROM scans WHERE status='completed'`).Scan(&scans); err != nil {
			t.Fatal(err)
		}
		if scans != n+2 {
			t.Fatalf("completed scans = %d, want %d", scans, n+2)
		}
	}
	r.remove(t, "provider.py")
	if summary := r.update(t); summary.FilesDeleted != 1 {
		t.Fatalf("deletion update = %+v", summary)
	}
	var deleted int
	var state string
	if err := db.QueryRow(`SELECT is_deleted,parse_state FROM files WHERE path='provider.py'`).Scan(&deleted, &state); err != nil || deleted != 1 || state != "deleted" {
		t.Fatalf("tombstone = %d/%s, %v", deleted, state, err)
	}
	for _, table := range []string{"symbols", "references_tbl", "edges"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE file_id=(SELECT id FROM files WHERE path='provider.py')`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("purged %s rows = %d, %v", table, count, err)
		}
	}
	r.assertFreshParity(t, "deletion after no-op scans")
	if target := r.refTarget(t, "main.py", "target"); target != "" {
		t.Fatalf("deleted target still referenced: %s", target)
	}
	callees, err := query.New(r.store, nil).FindCalleesResult(r.ctx, r.repoID, "main.run", 0, 20, 0)
	if err != nil || len(callees.Callees) != 0 {
		t.Fatalf("callees after deletion = %+v, %v", callees, err)
	}
	before = snapshotFileRows(t, db)
	r.update(t)
	if after := snapshotFileRows(t, db); !reflect.DeepEqual(before, after) {
		t.Fatalf("no-op rewrote active or tombstoned rows:\nbefore %v\nafter %v", before, after)
	}
}
