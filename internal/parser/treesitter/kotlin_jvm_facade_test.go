//go:build cgo

package treesitter

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestKotlinJVMFileFacadeEvidence(t *testing.T) {
	none := graph.JVMFileFacade{}
	for _, tc := range []struct {
		name, path, src string
		want            graph.JVMFileFacade
	}{
		{"default facade", "lib/Actions.kt", "package lib\nfun run() {}", graph.JVMFileFacade{Class: "ActionsKt"}},
		{"default facade capitalizes", "foo.kt", "package lib\nfun run() {}", graph.JVMFileFacade{Class: "FooKt"}},
		{"default facade keeps underscore and digits", "my_file2.kt", "fun run() {}", graph.JVMFileFacade{Class: "My_file2Kt"}},
		{"top-level property owns a facade", "Values.kt", "package lib\nval x = 1", graph.JVMFileFacade{Class: "ValuesKt"}},
		{"explicit JvmName", "Actions.kt", "@file:JvmName(\"Actions\")\npackage lib\nfun run() {}", graph.JVMFileFacade{Class: "Actions", Explicit: true}},
		{"JvmName without package", "Actions.kt", "@file:JvmName(\"Api\")\nfun run() {}", graph.JVMFileFacade{Class: "Api", Explicit: true}},
		{"JvmName after shebang", "Actions.kt", "#!/usr/bin/env kotlin\n@file:JvmName(\"Api\")\npackage lib\nfun run() {}", graph.JVMFileFacade{Class: "Api", Explicit: true}},
		{"qualified JvmName", "Actions.kt", "@file:kotlin.jvm.JvmName(\"Api\")\npackage lib\nfun run() {}", graph.JVMFileFacade{Class: "Api", Explicit: true}},
		{"explicit import of JvmName", "Actions.kt", "@file:JvmName(\"Api\")\npackage lib\nimport kotlin.jvm.JvmName\nfun run() {}", graph.JVMFileFacade{Class: "Api", Explicit: true}},
		{"multifile", "A.kt", "@file:JvmName(\"Utils\")\n@file:JvmMultifileClass\npackage lib\nfun first() {}", graph.JVMFileFacade{Class: "Utils", Explicit: true, Multifile: true}},
		{"bracket multifile", "A.kt", "@file:[JvmName(\"Utils\") JvmMultifileClass]\npackage lib\nfun first() {}", graph.JVMFileFacade{Class: "Utils", Explicit: true, Multifile: true}},
		{"multifile without JvmName", "A.kt", "@file:JvmMultifileClass\npackage lib\nfun first() {}", graph.JVMFileFacade{Class: "AKt", Multifile: true}},
		{"unrelated file annotation", "Actions.kt", "@file:Suppress(\"x\")\npackage lib\nfun run() {}", graph.JVMFileFacade{Class: "ActionsKt"}},
		{"declaration JvmName is not file-targeted", "Actions.kt", "package lib\n@JvmName(\"other\")\nfun run() {}", graph.JVMFileFacade{Class: "ActionsKt"}},
		{"comment and string spellings are not annotations", "Actions.kt", "/* @file:JvmName(\"A\") */\npackage lib\nval s = \"@file:JvmName(\\\"B\\\")\"\nfun run() {}", graph.JVMFileFacade{Class: "ActionsKt"}},
		{"no top-level callable", "Model.kt", "package lib\nclass Model { fun run() {} }", none},
		{"script", "build.kts", "fun run() {}", none},
		{"filename outside identifier subset", "my-file.kt", "fun run() {}", none},
		{"filename starting with digit", "1st.kt", "fun run() {}", none},
		{"dotted filename", "a.b.kt", "fun run() {}", none},
		{"non-literal JvmName", "Actions.kt", "@file:JvmName(NAME)\npackage lib\nfun run() {}", none},
		{"concatenated JvmName", "Actions.kt", "@file:JvmName(\"A\" + \"B\")\npackage lib\nfun run() {}", none},
		{"interpolated JvmName", "Actions.kt", "@file:JvmName(\"A$x\")\npackage lib\nfun run() {}", none},
		{"named JvmName argument", "Actions.kt", "@file:JvmName(value = \"A\")\npackage lib\nfun run() {}", none},
		{"empty JvmName", "Actions.kt", "@file:JvmName(\"\")\npackage lib\nfun run() {}", none},
		{"JvmName outside identifier subset", "Actions.kt", "@file:JvmName(\"my-api\")\npackage lib\nfun run() {}", none},
		{"JvmName without argument", "Actions.kt", "@file:JvmName\npackage lib\nfun run() {}", none},
		{"duplicate JvmName", "Actions.kt", "@file:JvmName(\"A\")\n@file:JvmName(\"B\")\npackage lib\nfun run() {}", none},
		{"JvmMultifileClass with arguments", "Actions.kt", "@file:JvmMultifileClass()\npackage lib\nfun run() {}", none},
		{"misplaced file annotation", "Actions.kt", "package lib\n@file:JvmName(\"A\")\nfun run() {}", none},
		{"aliased JvmName import", "Actions.kt", "@file:N(\"A\")\npackage lib\nimport kotlin.jvm.JvmName as N\nfun run() {}", none},
		{"foreign JvmName import", "Actions.kt", "@file:JvmName(\"A\")\npackage lib\nimport other.JvmName\nfun run() {}", none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewKotlin().Parse(context.Background(), tc.path, []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if p.Scope.JVMFacade != tc.want {
				t.Fatalf("facade = %+v, want %+v", p.Scope.JVMFacade, tc.want)
			}
		})
	}
}
