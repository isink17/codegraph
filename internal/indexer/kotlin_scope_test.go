//go:build cgo

package indexer

import (
	"strings"
	"testing"
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
