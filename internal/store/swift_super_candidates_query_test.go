package store

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

// The super-call candidate query must report what the direct facts LEFT JOIN
// reported: zero facts as count 0 with NULL override/final, several facts as
// their count and maxima, same-named candidates separately, and facts of
// another repository ignored.
func TestSwiftSuperCandidatesQueryMatchesDirectJoin(t *testing.T) {
	f := newSwiftScopeFixture(t)
	other := f.file("Other.swift")
	none := f.symbol(f.mainFile, "none", "Base", "function", "none()", false)
	one := f.symbol(f.mainFile, "one", "Base", "function", "one()", false)
	many := f.symbol(f.mainFile, "many", "Base", "function", "many()", true)
	dupA := f.symbol(f.mainFile, "dup", "Base", "function", "dup()", false)
	dupB := f.symbol(other, "dup", "Base", "function", "dup(_:)", true)
	private := f.symbol(f.mainFile, "hidden", "Base", "function", "hidden()", false)
	f.symbol(f.mainFile, "unmatched", "Base", "function", "unmatched()", false)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, private); err != nil {
		t.Fatal(err)
	}
	facts := []struct {
		file, symbol    int64
		final, override int
		dispatch        string
	}{
		{f.mainFile, one, 0, 1, "instance"},
		{f.mainFile, many, 0, 0, "static"},
		{f.mainFile, many, 1, 1, "class"},
		{f.mainFile, many, 0, 0, ""},
		{f.mainFile, dupA, 1, 0, "instance"},
		{other, dupB, 0, 1, "class"},
		{other, dupB, 0, 0, "static"},
		{f.mainFile, private, 0, 0, "instance"},
	}
	for _, fact := range facts {
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO swift_declaration_facts(repo_id,file_id,symbol_id,is_final,is_override,dispatch_kind) VALUES(?,?,?,?,?,?)`,
			f.repoID, fact.file, fact.symbol, fact.final, fact.override, fact.dispatch); err != nil {
			t.Fatal(err)
		}
	}
	otherRepo, err := f.store.UpsertRepo(f.ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherRepoFile, err := insertTestFileLang(f.ctx, f.store, otherRepo.ID, "Foreign.swift", "swift")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO swift_declaration_facts(repo_id,file_id,symbol_id,is_final,is_override,dispatch_kind) VALUES(?,?,?,1,1,'static'),(?,?,?,1,1,'static')`,
		otherRepo.ID, otherRepoFile, none, otherRepo.ID, otherRepoFile, one); err != nil {
		t.Fatal(err)
	}

	conn, err := f.store.db.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(f.ctx, `CREATE TEMP TABLE tmp_swift_super_candidates(owner TEXT NOT NULL,name TEXT NOT NULL,PRIMARY KEY(owner,name)) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(f.ctx, `INSERT INTO tmp_swift_super_candidates VALUES ('Base','none'),('Base','one'),('Base','many'),('Base','dup'),('Base','hidden')`); err != nil {
		t.Fatal(err)
	}
	const direct = `SELECT s.id,s.file_id,s.container_name,s.name,s.signature,s.visibility,s.is_static,s.arity_min,s.arity_max,s.start_line,s.start_col,s.end_line,s.end_col,COUNT(d.id),COALESCE(MAX(d.dispatch_kind),''),MAX(d.is_override),MAX(d.is_final)
FROM symbols s JOIN files f ON f.id=s.file_id JOIN tmp_swift_super_candidates k ON k.owner=s.container_name AND k.name=s.name
LEFT JOIN swift_declaration_facts d ON d.repo_id=s.repo_id AND d.symbol_id=s.id
WHERE s.repo_id=? AND s.language='swift' AND s.kind='function' AND f.is_deleted=0 GROUP BY s.id`

	load := func(query string, args ...any) []string {
		t.Helper()
		rows, err := conn.QueryContext(f.ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var c swiftSuperCandidate
			if err := rows.Scan(&c.id, &c.file, &c.owner, &c.name, &c.signature, &c.visibility, &c.static, &c.min, &c.max, &c.startLine, &c.startCol, &c.endLine, &c.endCol, &c.factCount, &c.dispatch, &c.isOverride, &c.isFinal); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%d|%s|%s|%d|%s|%s|%s", c.id, c.name, c.visibility, c.factCount, c.dispatch, nullInt(c.isOverride), nullInt(c.isFinal)))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := load(direct, f.repoID)
	got := load(swiftSuperCandidatesQuery, f.repoID, f.repoID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregated candidates differ from direct join\n got=%v\nwant=%v", got, want)
	}
	expected := []string{
		fmt.Sprintf("%d|none||0||null|null", none),
		fmt.Sprintf("%d|one||1|instance|1|0", one),
		fmt.Sprintf("%d|many||3|static|1|1", many),
		fmt.Sprintf("%d|dup||1|instance|0|1", dupA),
		fmt.Sprintf("%d|dup||2|static|1|0", dupB),
		fmt.Sprintf("%d|hidden|private|1|instance|0|0", private),
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("candidates\n got=%v\nwant=%v", got, expected)
	}
}

func nullInt(v sql.NullInt64) string {
	if !v.Valid {
		return "null"
	}
	return fmt.Sprint(v.Int64)
}
