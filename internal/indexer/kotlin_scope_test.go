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

func TestKotlinConstructorCallableShadowingLifecycle(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Shadow.kt": `class Target
fun local() { val Target = {}; Target() }
fun parameter(Target: () -> Unit) { Target() }
fun positive() { Target() }`,
	})
	lines := r.projection(t)
	for _, want := range []string{
		`edge Shadow.kt:local -calls-> "Target" => :: [/]`,
		`edge Shadow.kt:parameter -calls-> "Target" => :: [/]`,
		`edge Shadow.kt:positive -constructs-> "Target" => Shadow.kt:Target(class)`,
	} {
		found := false
		for _, line := range lines {
			if strings.Contains(line, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("projection missing %q: %v", want, lines)
		}
	}
	assertKotlinTargetReferences(t, r, 4)

	r.write(t, "Shadow.kt", `class Target
fun local() { Target() }
fun parameter(Target: () -> Unit) { Target() }
fun positive() { Target() }`)
	r.update(t, "Shadow.kt")
	assertKotlinTargetReferences(t, r, 2, 4)
	r.assertFreshParity(t, "callable shadow removal")

	r.write(t, "Shadow.kt", `class Target
fun local() { val Target = {}; Target() }
fun parameter(Target: () -> Unit) { Target() }
fun positive() { Target() }`)
	r.update(t, "Shadow.kt")
	assertKotlinTargetReferences(t, r, 4)
	r.assertFreshParity(t, "callable shadow rebound")
}

func TestKotlinAmbiguousCallableCandidateDoesNotConstruct(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Types.kt": `class Target`,
		"Callables.kt": `fun Target() {}
fun Target(value: kotlin.Int) {}`,
		"Caller.kt": `fun create() { Target() }`,
	})
	assertJVMUnresolved(t, r, "Caller.kt", "Target")
	r.assertFreshParity(t, "ambiguous callable candidates")
}

func TestKotlinTypedCallableValueShadowing(t *testing.T) {
	r := newLifecycleRepo(t, tree{"Shadow.kt": `class Target
fun create(callback: () -> Unit) {
    val Target: () -> Unit = callback
    Target()
}
fun positive() { Target() }`})
	if got := r.edgeState(t, "Shadow.kt", "Target"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("typed callable shadow = %s", got)
	}
	lines := r.projection(t)
	for _, line := range lines {
		if strings.Contains(line, `Shadow.kt:positive -constructs-> "Target"`) {
			return
		}
	}
	t.Fatalf("typed callable shadow removed positive construction: %v", lines)
}

func assertKotlinTargetReferences(t *testing.T, r *lifecycleRepo, boundLines ...int) {
	t.Helper()
	want := map[int]bool{}
	for _, line := range boundLines {
		want[line] = true
	}
	rows, err := r.raw(t).Query(`SELECT r.start_line, r.symbol_id IS NOT NULL FROM references_tbl r JOIN files f ON f.id=r.file_id WHERE r.repo_id=? AND f.path='Shadow.kt' AND r.ref_kind='call' AND r.qualified_name='Target' ORDER BY r.start_line`, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var line int
		var bound bool
		if err := rows.Scan(&line, &bound); err != nil {
			t.Fatal(err)
		}
		if bound != want[line] {
			t.Fatalf("reference Shadow.kt:%d bound=%v, want=%v", line, bound, want[line])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
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
	r := newLifecycleRepo(t, tree{"UserService.kt": caller, "ReceiverApi.kt": "package p\nclass ReceiverApi\n"})
	if got := r.edgeState(t, "UserService.kt", "this.validate"); !strings.Contains(got, "UserService.validate") {
		t.Fatalf("standard-library forEach did not resolve before member addition: %s", got)
	}
	r.write(t, "ReceiverApi.kt", api)
	r.update(t, "ReceiverApi.kt")
	if got := r.edgeState(t, "UserService.kt", "this.validate"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("cross-file member forEach bound enclosing receiver: %s", got)
	}
	assertJVMReference(t, r, "UserService.kt", "this.validate", false)
	r.assertFreshParity(t, "cross-file member receiver lambda")
}

func TestKotlinConservativeClassConstructionLifecycle(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"lib/Types.kt": `package lib
class UserRepository
class Empty()
class Required(val value: kotlin.Int)
class PrivateConstructor private constructor()
class InternalConstructor internal constructor()
abstract class AbstractType
interface Backend
object Singleton
enum class Color { RED }
class Invokable { operator fun invoke(block: () -> Unit) {} }
class Outer { inner class Inner }
class Ambiguous
`,
		"lib/Other.kt": `package lib
class Ambiguous
`,
		"Caller.kt": `package app
import lib.UserRepository
import lib.Empty
import lib.Required
import lib.PrivateConstructor
import lib.InternalConstructor
import lib.AbstractType
import lib.Backend
import lib.Singleton
import lib.Color
import lib.Invokable
import lib.Outer.Inner
import lib.Ambiguous
class UserService {
    fun create() { UserRepository() }
    fun empty() { Empty() }
    fun required() { Required(1) }
    fun privateConstructor() { PrivateConstructor() }
    fun internalConstructor() { InternalConstructor() }
    fun abstractType() { AbstractType() }
    fun backend() { Backend() }
    fun singleton() { Singleton() }
    fun enumType() { Color() }
    fun trailingLambda() { Invokable() {} }
    fun innerType() { Inner() }
    fun ambiguous() { Ambiguous() }
}`,
	})
	for _, tc := range []struct {
		name, target string
		bound        bool
	}{
		{"UserRepository", "lib.UserRepository", true},
		{"Empty", "lib.Empty", true},
		{"Required", "lib.Required", false},
		{"PrivateConstructor", "lib.PrivateConstructor", false},
		{"InternalConstructor", "lib.InternalConstructor", false},
		{"AbstractType", "lib.AbstractType", false},
		{"Backend", "lib.Backend", false},
		{"Singleton", "lib.Singleton", false},
		{"Color", "lib.Color", false},
		{"Invokable", "lib.Invokable", false},
		{"Inner", "lib.Outer.Inner", false},
		{"Ambiguous", "lib.Ambiguous", false},
	} {
		state := r.edgeState(t, "Caller.kt", tc.name)
		if tc.bound && !strings.Contains(state, tc.target) {
			t.Errorf("%s = %s", tc.name, state)
		}
		if !tc.bound && !strings.Contains(state, ":: [/]") {
			t.Errorf("%s = %s", tc.name, state)
		}
	}
	callers, err := r.store.FindCallers(context.Background(), r.repoID, "lib.UserRepository", 0, 10, 0)
	if err != nil || len(callers) != 1 || callers[0].QualifiedName != "app.UserService.create" {
		t.Fatalf("constructor callers = %#v, %v", callers, err)
	}
	callees, err := r.store.FindCallees(context.Background(), r.repoID, "app.UserService.create", 0, 10, 0)
	if err != nil || len(callees) != 1 || callees[0].QualifiedName != "lib.UserRepository" {
		t.Fatalf("constructor callees = %#v, %v", callees, err)
	}
	var referenceTarget string
	if err := r.raw(t).QueryRow(`SELECT s.qualified_name FROM references_tbl r JOIN symbols s ON s.id=r.symbol_id JOIN files f ON f.id=r.file_id WHERE r.repo_id=? AND f.path='Caller.kt' AND r.name='UserRepository'`, r.repoID).Scan(&referenceTarget); err != nil || referenceTarget != "lib.UserRepository" {
		t.Fatalf("constructor reference target = %q, %v; want lib.UserRepository", referenceTarget, err)
	}
	impact, err := r.store.ImpactRadius(context.Background(), r.repoID, []string{"lib.UserRepository"}, nil, 1, 10, 0)
	if err != nil || impact == nil {
		t.Fatalf("constructor impact = %#v, %v", impact, err)
	}
	r.assertFreshParity(t, "initial Kotlin construction")
	r.write(t, "lib/Other.kt", "package lib\nclass Other\n")
	r.update(t, "lib/Other.kt")
	if got := r.edgeState(t, "Caller.kt", "Ambiguous"); !strings.Contains(got, "lib/Types.kt") {
		t.Fatalf("ambiguity removal did not rebind: %s", got)
	}
	r.write(t, "lib/Types.kt", "package lib\nclass UserRepository(val value: kotlin.Int)\nclass Empty()\nclass Required(val value: kotlin.Int)\nabstract class AbstractType\ninterface Backend\nobject Singleton\nclass Ambiguous\n")
	r.update(t, "lib/Types.kt")
	if got := r.edgeState(t, "Caller.kt", "UserRepository"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("constructor arity removal unexpectedly remained bound: %s", got)
	}
	var cleared sql.NullInt64
	if err := r.raw(t).QueryRow(`SELECT symbol_id FROM references_tbl r JOIN files f ON f.id=r.file_id WHERE r.repo_id=? AND f.path='Caller.kt' AND r.name='UserRepository'`, r.repoID).Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Valid {
		t.Fatalf("constructor reference retained stale target %d", cleared.Int64)
	}
	r.assertFreshParity(t, "constructor proof removal")
}

func TestKotlinConstructionHonorsFilePrivateClassVisibility(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"lib/Types.kt": `package lib
class PublicTarget
private class FilePrivateTarget`,
		"lib/Caller.kt": `package lib
private class SameFilePrivateTarget
fun create() { PublicTarget(); FilePrivateTarget(); SameFilePrivateTarget() }`,
	})
	for _, tc := range []struct {
		name, want string
		bound      bool
	}{
		{"public class", "PublicTarget", true},
		{"private class in another file", "FilePrivateTarget", false},
		{"private class in same file", "SameFilePrivateTarget", true},
	} {
		got := r.edgeState(t, "lib/Caller.kt", tc.want)
		if tc.bound && !strings.Contains(got, "lib/") {
			t.Errorf("%s = %s, want visible class target", tc.name, got)
		}
		if !tc.bound && !strings.Contains(got, ":: [/]") {
			t.Errorf("%s = %s, want unresolved private class target", tc.name, got)
		}
	}
	r.assertFreshParity(t, "Kotlin class visibility")
}
