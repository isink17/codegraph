package store

import (
	"fmt"
	"reflect"
	"testing"
)

// The pre-aggregated facts join must report exactly what the direct LEFT JOIN
// reported, including the doubled counts of a symbol matched by two candidate
// rows (an empty and an exact signature).
func TestSwiftDeclarationFactsBySymbolMatchesDirectJoin(t *testing.T) {
	f := newSwiftScopeFixture(t)
	other := f.file("Other.swift")
	none := f.symbol(f.mainFile, "none", "Owner", "function", "none()", false)
	final := f.symbol(f.mainFile, "final", "Owner", "function", "final()", false)
	mixed := f.symbol(other, "mixed", "Owner", "function", "mixed(_:)", true)
	twice := f.symbol(f.mainFile, "twice", "Owner", "function", "twice()", false)
	f.symbol(f.mainFile, "unmatched", "Owner", "function", "unmatched()", false)
	f.declarationFact(f.mainFile, final, true)
	f.dispatchFact(other, mixed, false, "static")
	f.dispatchFact(other, mixed, true, "class")
	f.dispatchFact(f.mainFile, twice, false, "instance")
	f.dispatchFact(f.mainFile, twice, true, "")

	conn, err := f.store.db.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(f.ctx, `CREATE TEMP TABLE tmp_facts_equality(owner TEXT NOT NULL,name TEXT NOT NULL,signature TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(f.ctx, `INSERT INTO tmp_facts_equality VALUES
		('Owner','none',''),('Owner','final','final()'),('Owner','mixed',''),('Owner','twice',''),('Owner','twice','twice()')`); err != nil {
		t.Fatal(err)
	}
	const from = `FROM symbols s JOIN tmp_facts_equality c ON c.owner=s.container_name AND c.name=s.name AND (c.signature='' OR c.signature=s.signature)
		JOIN files f ON f.id=s.file_id `
	const where = ` WHERE s.repo_id=? AND f.is_deleted=0 GROUP BY s.id ORDER BY s.id`
	direct := `SELECT s.id,COUNT(d.id),COALESCE(SUM(CASE WHEN d.is_final=1 THEN 1 ELSE 0 END),0),COALESCE(MIN(d.dispatch_kind),''),COALESCE(MAX(d.dispatch_kind),'') ` +
		from + `LEFT JOIN swift_declaration_facts d ON d.repo_id=s.repo_id AND d.symbol_id=s.id` + where
	aggregated := `SELECT s.id,COALESCE(SUM(d.facts),0),COALESCE(SUM(d.finals),0),COALESCE(MIN(d.dispatch_min),''),COALESCE(MAX(d.dispatch_max),'') ` +
		from + `LEFT JOIN ` + swiftDeclarationFactsBySymbol + ` d ON d.symbol_id=s.id` + where

	load := func(query string, args ...any) []string {
		t.Helper()
		rows, err := conn.QueryContext(f.ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, facts, finals int64
			var minKind, maxKind string
			if err := rows.Scan(&id, &facts, &finals, &minKind, &maxKind); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%d|%d|%d|%s|%s", id, facts, finals, minKind, maxKind))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := load(direct, f.repoID)
	got := load(aggregated, f.repoID, f.repoID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregated facts join = %v, direct join = %v", got, want)
	}
	expected := []string{
		fmt.Sprintf("%d|0|0||", none),
		fmt.Sprintf("%d|1|1|instance|instance", final),
		fmt.Sprintf("%d|2|1|class|static", mixed),
		fmt.Sprintf("%d|4|2||instance", twice),
	}
	if !reflect.DeepEqual(want, expected) {
		t.Fatalf("direct join = %v, want %v", want, expected)
	}
}
