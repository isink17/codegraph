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

func TestKotlinReceiverSoundnessLifecycle(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"With.kt": `package withcase
class OtherService { fun validate() {} }
class UserService {
    fun validate() {}
    fun withRun(other: OtherService) { with(other) { this.validate() } }
    fun nestedRun(other: OtherService) { with(other) { run { this.validate() } } }
}`,
		"Apply.kt": `package applycase
class OtherService { fun validate() {} }
class UserService {
    fun validate() {}
    fun applyRun(other: OtherService) { other.apply { this.validate() } }
}`,
		"Extension.kt": `package extcase
class OtherService { fun validate() {} }
class UserService {
    fun validate() {}
    fun OtherService.execute() { this.validate() }
}`,
		"Lambda.kt": `package lambdacase
class UserService {
    fun validate() {}
    fun run() { val callback = { this.validate() } }
}`,
		"Typed.kt": `package typedcase
class OtherService { fun validate() {} }
class UserService {
    fun validate() {}
    fun typed() { val callback: OtherService.() -> Unit = { this.validate() } }
}`,
		"Ordinary.kt": `package ordinarycase
class UserService {
    fun validate() {}
    fun ordinary(items: List<Int>, value: Int) {
        items.forEach { this.validate() }
        value.let { this.validate() }
    }
}`,
	})
	for _, path := range []string{"With.kt", "Apply.kt", "Extension.kt", "Typed.kt"} {
		if got := r.edgeState(t, path, "this.validate"); !strings.Contains(got, ":: [/]") {
			t.Fatalf("%s this.validate = %s, want unresolved receiver identity", path, got)
		}
		assertJVMReference(t, r, path, "this.validate", false)
	}
	if got := r.edgeState(t, "Lambda.kt", "this.validate"); !strings.Contains(got, "Lambda.kt:lambdacase.UserService.validate(function)") {
		t.Fatalf("ordinary lambda this.validate = %s, want enclosing target", got)
	}
	assertJVMReference(t, r, "Lambda.kt", "this.validate", true)
	assertJVMQueryRelation(t, r, "lambdacase.UserService.run", "lambdacase.UserService.validate")
	if got := r.edgeState(t, "Ordinary.kt", "this.validate"); !strings.Contains(got, "Ordinary.kt:ordinarycase.UserService.validate(function)") {
		t.Fatalf("call-passed ordinary lambdas this.validate = %s, want enclosing target", got)
	}
	assertJVMReference(t, r, "Ordinary.kt", "this.validate", true)
	r.assertFreshParity(t, "receiver-changing contexts")

	r.write(t, "With.kt", `package withcase
class OtherService { fun validate() {} }
class UserService {
    fun validate() {}
    fun withRun(other: OtherService) { this.validate() }
}`)
	r.update(t, "With.kt")
	if got := r.edgeState(t, "With.kt", "this.validate"); !strings.Contains(got, "With.kt:withcase.UserService.validate(function)") {
		t.Fatalf("ordinary member after lambda removal = %s, want resolved", got)
	}
	assertJVMReference(t, r, "With.kt", "this.validate", true)

	r.write(t, "Extension.kt", `package extcase
class OtherService { fun validate() {} }
class UserService {
    fun validate() {}
    fun execute() { this.validate() }
}`)
	r.update(t, "Extension.kt")
	if got := r.edgeState(t, "Extension.kt", "this.validate"); !strings.Contains(got, "Extension.kt:extcase.UserService.validate(function)") {
		t.Fatalf("ordinary member after extension removal = %s, want resolved", got)
	}
	assertJVMReference(t, r, "Extension.kt", "this.validate", true)
	r.assertFreshParity(t, "receiver context removal")
}

func TestKotlinCrossFileExtensionShadowsOrdinaryLambdaHelper(t *testing.T) {
	const caller = `package p
class UserService {
    fun validate() {}
    fun run(api: ReceiverApi) { api.forEach { this.validate() } }
}`
	const api = `package p
class OtherService { fun validate() {} }
class ReceiverApi
fun ReceiverApi.forEach(action: OtherService.() -> Unit) {}
`
	r := newLifecycleRepo(t, tree{"UserService.kt": caller, "ReceiverApi.kt": api})
	if got := r.edgeState(t, "UserService.kt", "this.validate"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("cross-file custom forEach bound enclosing receiver: %s", got)
	}
	assertJVMReference(t, r, "UserService.kt", "this.validate", false)
	if got := r.edgeState(t, "UserService.kt", "this.validate"); strings.Contains(got, "UserService.validate") || strings.Contains(got, "OtherService.validate") {
		t.Fatalf("cross-file custom forEach fabricated receiver identity: %s", got)
	}
	r.assertFreshParity(t, "cross-file extension")

	const ordinary = `package p
class UserService {
    fun validate() {}
    fun run(items: List<Int>) { items.forEach { this.validate() } }
}`
	standard := newLifecycleRepo(t, tree{"UserService.kt": ordinary, "ReceiverApi.kt": "package p\nclass ReceiverApi\n"})
	if got := standard.edgeState(t, "UserService.kt", "this.validate"); !strings.Contains(got, "UserService.validate") {
		t.Fatalf("standard-library forEach lost support: %s", got)
	}
	assertJVMReference(t, standard, "UserService.kt", "this.validate", true)
	db, err := sql.Open(store.SQLiteDriverName(), standard.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE files SET parser_profile='treesitter:kotlin:v13' WHERE path='UserService.kt'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	standard.write(t, "ReceiverApi.kt", api)
	if summary := standard.update(t, "ReceiverApi.kt", "UserService.kt"); summary.FilesIndexed == 0 || !strings.Contains(strings.Join(summary.ParserProfileLanguages, ","), "kotlin") {
		t.Fatalf("v13-to-v14 receiver evidence transition = %+v", summary)
	}
	if got := standard.edgeState(t, "UserService.kt", "this.validate"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("same-package custom extension did not withdraw standard helper binding: %s", got)
	}
	assertJVMReference(t, standard, "UserService.kt", "this.validate", false)
	standard.assertFreshParity(t, "custom extension added")

	standard.write(t, "ReceiverApi.kt", "package p\nclass ReceiverApi\nprivate fun ReceiverApi.forEach(action: OtherService.() -> Unit) {}\n")
	standard.update(t, "ReceiverApi.kt", "UserService.kt")
	if got := standard.edgeState(t, "UserService.kt", "this.validate"); !strings.Contains(got, "UserService.validate") {
		t.Fatalf("invisible private extension incorrectly withdrew standard helper binding: %s", got)
	}
	assertJVMReference(t, standard, "UserService.kt", "this.validate", true)
	standard.assertFreshParity(t, "invisible private extension")
}

func TestKotlinCrossFileMemberShadowsOrdinaryLambdaHelper(t *testing.T) {
	const caller = `package p
class UserService {
    fun validate() {}
    fun run(api: ReceiverApi) { api.forEach { this.validate() } }
}`
	const api = `package p
class OtherService { fun validate() {} }
class ReceiverApi {
    fun forEach(action: OtherService.() -> Unit) {}
}
`
	r := newLifecycleRepo(t, tree{"UserService.kt": caller, "ReceiverApi.kt": api})
	if got := r.edgeState(t, "UserService.kt", "this.validate"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("cross-file member forEach bound enclosing receiver: %s", got)
	}
	assertJVMReference(t, r, "UserService.kt", "this.validate", false)
	r.assertFreshParity(t, "cross-file member receiver lambda")
}
