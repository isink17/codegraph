//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

func TestKotlinMemberStaysInProvenOwnerPackage(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a/Service.kt": `package a
class Service { fun run() {} }`,
		"b/Service.kt": `package b
class Service { fun run() {} }`,
		"Caller.kt": `package c
import a.Service
fun call() { Service.run() }`,
	})
	if got := r.edgeState(t, "Caller.kt", "Service.run"); !strings.Contains(got, "a/Service.kt") || strings.Contains(got, "b/Service.kt") {
		t.Fatalf("Service.run = %s", got)
	}
}

func TestKotlinExplicitThisMemberCallResolvesThroughQueries(t *testing.T) {
	const caller = `class UserService {
    fun validate() {}

    fun save() {
        this.validate()
    }
}`
	r := newLifecycleRepo(t, tree{"UserService.kt": caller})

	if got := r.edgeState(t, "UserService.kt", "this.validate"); !strings.Contains(got, "UserService.kt:UserService.validate(function)") {
		t.Fatalf("this.validate = %s, want resolved target", got)
	}
	if got := r.edgeState(t, "UserService.kt", "this.validate"); !strings.Contains(got, "kotlin_package_scope/high") {
		t.Fatalf("this.validate = %s, want Kotlin scope provenance", got)
	}
	assertJVMReference(t, r, "UserService.kt", "this.validate", true)
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var edgeLine, refLine int
	var contextName string
	if err := db.QueryRowContext(r.ctx, `SELECT e.line,r.start_line,s.qualified_name
		FROM edges e JOIN references_tbl r ON r.file_id=e.file_id AND r.qualified_name=e.dst_name
		JOIN symbols s ON s.id=r.context_symbol_id
		WHERE e.repo_id=? AND e.dst_name='this.validate'`, r.repoID).Scan(&edgeLine, &refLine, &contextName); err != nil {
		t.Fatal(err)
	}
	if edgeLine != 5 || refLine != 5 || contextName != "UserService.save" {
		t.Fatalf("source/reference identity = edge line %d, reference line %d, context %q", edgeLine, refLine, contextName)
	}
	assertJVMQueryRelation(t, r, "UserService.save", "UserService.validate")
	impact, err := r.store.ImpactRadius(context.Background(), r.repoID, []string{"UserService.validate"}, nil, 1, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := impact["symbols"].([]graph.Symbol); len(got) != 2 {
		t.Fatalf("impact symbols = %d, want caller and target", len(got))
	}

	r.write(t, "UserService.kt", "// touched\n"+caller)
	r.update(t, "UserService.kt")
	assertJVMQueryRelation(t, r, "UserService.save", "UserService.validate")
	r.update(t)
	assertJVMQueryRelation(t, r, "UserService.save", "UserService.validate")

	for _, tc := range []struct {
		name, source string
		resolved     bool
	}{
		{"renamed target", `class UserService {
    fun check() {}
    fun save() { this.validate() }
}`, false},
		{"ambiguous overload", `class UserService {
    fun validate() {}
    fun validate(value: Int) {}
    fun save() { this.validate() }
}`, false},
		{"unique target restored", caller, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.write(t, "UserService.kt", tc.source)
			r.update(t, "UserService.kt")
			got := r.edgeState(t, "UserService.kt", "this.validate")
			if tc.resolved {
				if !strings.Contains(got, "UserService.kt:UserService.validate(function)") {
					t.Fatalf("this.validate = %s, want resolved", got)
				}
				assertJVMReference(t, r, "UserService.kt", "this.validate", true)
			} else {
				if !strings.Contains(got, ":: [/] ") && !strings.Contains(got, ":: [/]") {
					t.Fatalf("this.validate = %s, want unresolved", got)
				}
				assertJVMReference(t, r, "UserService.kt", "this.validate", false)
				assertJVMNoQueryRelation(t, r, "UserService.save", "UserService.validate")
			}
			r.assertFreshParity(t, tc.name)
		})
	}

	r.write(t, "UserService.kt", `class UserService {
    fun validate() {}
    fun save() { this.validate() }
    fun other() { service.validate() }
}`)
	r.update(t, "UserService.kt")
	if got := r.edgeState(t, "UserService.kt", "service.validate"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("service.validate = %s, want unresolved", got)
	}
}

func TestKotlinExplicitThisUsesInnermostDeclaredType(t *testing.T) {
	r := newLifecycleRepo(t, tree{"Nested.kt": `class Outer {
    fun validate() {}
    fun save() { this.validate() }

    class Inner {
        fun validate() {}
        fun save() { this.validate() }
        fun other() { service.validate() }
    }
}`})
	if got := r.edgeState(t, "Nested.kt", "this.validate"); !strings.Contains(got, "Nested.kt:Outer.Inner.validate(function)") {
		t.Fatalf("nested this.validate = %s, want inner target", got)
	}
	if got := r.edgeState(t, "Nested.kt", "service.validate"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("service.validate = %s, want unresolved", got)
	}
	assertJVMQueryRelation(t, r, "Outer.Inner.save", "Outer.Inner.validate")
	assertJVMNoQueryRelation(t, r, "Outer.Inner.save", "Outer.validate")
}
