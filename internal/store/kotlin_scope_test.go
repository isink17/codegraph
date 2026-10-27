package store

import "testing"

func TestKotlinMemberRefusesNonCallableDeclaration(t *testing.T) {
	e := struct {
		id, file                             int64
		name, kind, evidence, pkg, container string
	}{file: 1, name: "this.Inner", pkg: "", container: "Outer"}
	owner := kotlinScopeSymbol{id: 1, name: "Outer", qname: "Outer", kind: "class", pkg: "", container: "Outer", language: "kotlin"}
	nested := kotlinScopeSymbol{id: 2, name: "Inner", qname: "Outer.Inner", kind: "class", pkg: "", container: "Outer", language: "kotlin"}

	if got, _ := kotlinMember(owner, "Inner", e, []kotlinScopeSymbol{nested}); got.id != 0 {
		t.Fatalf("non-callable Kotlin declaration resolved as a member: %+v", got)
	}
}
