package store

import (
	"context"
	"testing"
)

// The C++ ownership predicate has two spellings -- one SQL, one Go -- and the
// repo-wide and incremental paths disagree about which edges a generic
// strategy may answer if they drift.
func TestCppScopeVetoSQLMatchesGoTwin(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	files := map[string]int64{}
	for _, language := range []string{"cpp", "go", "php"} {
		id, err := insertTestFileLang(ctx, s, repo.ID, "src."+language, language)
		if err != nil {
			t.Fatal(err)
		}
		files[language] = id
	}
	caller, err := insertTestSymbolLang(ctx, s, repo.ID, files["cpp"], "caller", "caller", "cpp")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id                      int64
		language, dst, evidence string
	}
	var rows []row
	names := []string{"foo", "ns::foo", "::foo", "obj.foo", "obj.ns::foo", "foo<int>", "obj.foo<int>", "operator+", "a.b.c"}
	for _, language := range []string{"cpp", "go", "php"} {
		for _, dst := range names {
			for _, evidence := range []string{"", "direct:" + dst, "macro_unexpanded:CALL"} {
				id, err := insertTestEdge(ctx, s, repo.ID, files[language], caller, dst)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.ExecContext(ctx, `UPDATE edges SET evidence = ? WHERE id = ?`, evidence, id); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, row{id: id, language: language, dst: dst, evidence: evidence})
			}
		}
	}

	// The SQL twin: the veto is a NOT, so its negation is the owned set.
	ownedBySQL := map[int64]struct{}{}
	sqlRows, err := s.db.QueryContext(ctx, `
		SELECT edges.id FROM edges JOIN files f ON f.id = edges.file_id
		WHERE edges.repo_id = ? AND NOT (`+cppScopeVetoSQL+`)`, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlRows.Close()
	for sqlRows.Next() {
		var id int64
		if err := sqlRows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ownedBySQL[id] = struct{}{}
	}
	if err := sqlRows.Err(); err != nil {
		t.Fatal(err)
	}
	owned := 0
	for _, r := range rows {
		_, sqlOwned := ownedBySQL[r.id]
		goOwned := cppScopeOwned(edgeTarget{srcLanguage: r.language, dstName: r.dst, evidence: r.evidence})
		if sqlOwned != goOwned {
			t.Errorf("%s %q [%s]: cppScopeVetoSQL owned=%v, cppScopeOwned=%v", r.language, r.dst, r.evidence, sqlOwned, goOwned)
		}
		if goOwned {
			owned++
			if r.language != "cpp" {
				t.Errorf("%s %q: owned outside C++", r.language, r.dst)
			}
		}
	}
	if owned == 0 || owned == len(rows) {
		t.Fatalf("owned %d of %d rows; the fixture does not separate the two sides", owned, len(rows))
	}
}
