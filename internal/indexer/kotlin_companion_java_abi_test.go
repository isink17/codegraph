//go:build cgo

package indexer

import (
	"strings"
	"testing"
)

func TestKotlinCompanionCallableABILifecycle(t *testing.T) {
	java := `package app;
import lib.Service;
class Caller {
    void companion() { Service.Companion.run(); }
    void bridge() { Service.run(); }
    void instanceField() { Service.Companion.INSTANCE.run(); }
}`
	ordinary := `package lib
class Service {
    companion object { fun run() {} }
}`
	static := `package lib
class Service {
    companion object { @JvmStatic fun run() {} }
}`
	r := newLifecycleRepo(t, tree{"Caller.java": java, "Service.kt": ordinary})
	assertJVMResolved(t, r, "Caller.java", "Service.Companion.run", "Service.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Service.run")
	assertJVMUnresolved(t, r, "Caller.java", "Service.Companion.INSTANCE.run")
	assertJVMReference(t, r, "Caller.java", "Service.Companion.run", true)
	assertJVMReference(t, r, "Caller.java", "Service.run", false)
	assertJVMQueryRelation(t, r, "app.Caller.companion", "lib.Service.Companion.run")
	assertJVMNoQueryRelation(t, r, "app.Caller.bridge", "lib.Service.Companion.run")
	r.assertFreshParity(t, "unnamed ordinary companion")

	r.write(t, "Service.kt", static)
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.java", "Service.Companion.run", "Service.kt", "java_import_scope")
	assertJVMResolved(t, r, "Caller.java", "Service.run", "Service.kt", "java_import_scope")
	assertJVMReference(t, r, "Caller.java", "Service.Companion.run", true)
	assertJVMReference(t, r, "Caller.java", "Service.run", true)
	assertJVMQueryRelation(t, r, "app.Caller.companion", "lib.Service.Companion.run")
	assertJVMQueryRelation(t, r, "app.Caller.bridge", "lib.Service.Companion.run")
	r.assertFreshParity(t, "JvmStatic added")

	r.write(t, "Service.kt", ordinary)
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.java", "Service.Companion.run", "Service.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Service.run")
	assertJVMReference(t, r, "Caller.java", "Service.run", false)
	r.assertFreshParity(t, "JvmStatic removed")

	named := `package lib
class Service {
    companion object Factory { fun run() {} }
}`
	r.write(t, "Service.kt", named)
	namedCaller := strings.TrimSuffix(java, "}") + "    void named() { Service.Factory.run(); }\n}"
	r.write(t, "Caller.java", namedCaller)
	r.update(t, "Service.kt", "Caller.java")
	assertJVMUnresolved(t, r, "Caller.java", "Service.Companion.run")
	assertJVMResolved(t, r, "Caller.java", "Service.Factory.run", "Service.kt", "java_import_scope")
	assertJVMReference(t, r, "Caller.java", "Service.Companion.run", false)
	assertJVMReference(t, r, "Caller.java", "Service.Factory.run", true)
	r.assertFreshParity(t, "rename companion field")

	namedStatic := strings.Replace(named, "fun run()", "@JvmStatic fun run()", 1)
	r.write(t, "Service.kt", namedStatic)
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.java", "Service.Factory.run", "Service.kt", "java_import_scope")
	assertJVMResolved(t, r, "Caller.java", "Service.run", "Service.kt", "java_import_scope")
	r.assertFreshParity(t, "named JvmStatic added")
	r.write(t, "Service.kt", named)
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.java", "Service.Factory.run", "Service.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Service.run")
	r.assertFreshParity(t, "named JvmStatic removed")

	renamed := strings.Replace(named, "Factory", "Registry", 1)
	r.write(t, "Service.kt", renamed)
	registryCaller := strings.TrimSuffix(namedCaller, "}") + "    void renamed() { Service.Registry.run(); }\n}"
	r.write(t, "Caller.java", registryCaller)
	r.update(t, "Service.kt", "Caller.java")
	assertJVMUnresolved(t, r, "Caller.java", "Service.Factory.run")
	assertJVMResolved(t, r, "Caller.java", "Service.Registry.run", "Service.kt", "java_import_scope")
	r.assertFreshParity(t, "rename named companion")

	private := strings.Replace(renamed, "companion object", "private companion object", 1)
	r.write(t, "Service.kt", private)
	r.update(t, "Service.kt")
	assertJVMUnresolved(t, r, "Caller.java", "Service.Registry.run")
	r.assertFreshParity(t, "private companion")

	r.write(t, "Service.kt", renamed)
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.java", "Service.Registry.run", "Service.kt", "java_import_scope")
	r.assertFreshParity(t, "public companion restored")

	memberRenamed := strings.Replace(renamed, "fun run()", "fun execute()", 1)
	memberCaller := strings.TrimSuffix(registryCaller, "}") + "    void execute() { Service.Registry.execute(); }\n}"
	r.write(t, "Service.kt", memberRenamed)
	r.write(t, "Caller.java", memberCaller)
	r.update(t, "Service.kt", "Caller.java")
	assertJVMUnresolved(t, r, "Caller.java", "Service.Registry.run")
	assertJVMResolved(t, r, "Caller.java", "Service.Registry.execute", "Service.kt", "java_import_scope")
	assertJVMReference(t, r, "Caller.java", "Service.Registry.run", false)
	assertJVMReference(t, r, "Caller.java", "Service.Registry.execute", true)
	r.assertFreshParity(t, "member rename")

	r.write(t, "Service.kt", "package lib\nclass Service {}")
	r.update(t, "Service.kt")
	assertJVMUnresolved(t, r, "Caller.java", "Service.Registry.execute")
	r.assertFreshParity(t, "companion removal")
	r.write(t, "Service.kt", memberRenamed)
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.java", "Service.Registry.execute", "Service.kt", "java_import_scope")
	r.assertFreshParity(t, "companion restore")

	r.remove(t, "Service.kt")
	r.update(t)
	assertJVMUnresolved(t, r, "Caller.java", "Service.Registry.run")
	r.assertFreshParity(t, "companion file deletion")
	r.write(t, "Service.kt", renamed)
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.java", "Service.Registry.run", "Service.kt", "java_import_scope")
	r.assertFreshParity(t, "companion file restored")
}

func TestJavaCompanionOuterScopeForms(t *testing.T) {
	kotlin := `package lib
class Service {
    companion object Factory { @JvmStatic fun run() {} }
}`
	for _, tc := range []struct {
		name, java, path, strategy string
	}{
		{"explicit import", `package app; import lib.Service; class Caller { void call() { Service.Factory.run(); Service.run(); } }`, "Caller.java", "java_import_scope"},
		{"wildcard import", `package app; import lib.*; class Caller { void call() { Service.Factory.run(); Service.run(); } }`, "Caller.java", "java_package_scope"},
		{"same package", `package lib; class Caller { void call() { Service.Factory.run(); Service.run(); } }`, "Caller.java", "java_package_scope"},
		{"fully qualified", `package app; class Caller { void call() { lib.Service.Factory.run(); lib.Service.run(); } }`, "Caller.java", "java_package_scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tree{tc.path: tc.java, "Service.kt": kotlin})
			first, second := "Service.Factory.run", "Service.run"
			if tc.name == "fully qualified" {
				first, second = "lib.Service.Factory.run", "lib.Service.run"
			}
			for _, call := range []string{first, second} {
				assertJVMResolved(t, r, "Caller.java", call, "Service.kt", tc.strategy)
			}
			r.assertFreshParity(t, tc.name)
		})
	}
}

func TestKotlinCompanionOuterTypeCollisionAndRenameLifecycle(t *testing.T) {
	service := `package lib
class Service {
    companion object { @JvmStatic fun run() {} }
}`
	java := `package app;
import lib.Service;
import lib.Renamed;
class Caller {
    void oldOwner() { Service.run(); }
    void newOwner() { Renamed.run(); }
}`
	r := newLifecycleRepo(t, tree{"Caller.java": java, "Service.kt": service})
	assertJVMResolved(t, r, "Caller.java", "Service.run", "Service.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Renamed.run")
	r.assertFreshParity(t, "initial owner")

	r.write(t, "JavaService.java", "package lib; public class Service {}")
	r.update(t, "JavaService.java")
	assertJVMUnresolved(t, r, "Caller.java", "Service.run")
	r.assertFreshParity(t, "competing Java owner added")

	r.remove(t, "JavaService.java")
	r.update(t, "JavaService.java")
	assertJVMResolved(t, r, "Caller.java", "Service.run", "Service.kt", "java_import_scope")
	r.assertFreshParity(t, "competing Java owner removed")

	r.write(t, "Service.kt", strings.Replace(service, "class Service", "class Renamed", 1))
	r.update(t, "Service.kt")
	assertJVMUnresolved(t, r, "Caller.java", "Service.run")
	assertJVMResolved(t, r, "Caller.java", "Renamed.run", "Service.kt", "java_import_scope")
	r.assertFreshParity(t, "outer class rename")
}
