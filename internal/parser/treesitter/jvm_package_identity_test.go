//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// jvmPackageFixtures pair a source with the package its syntax declares. Every
// "fake" or "docs" spelling sits in a comment or a string, so it must never
// become the file's package, a qualified name, or a stable key.
var jvmPackageFixtures = []struct {
	name, path, src, want string
}{
	{"java normal", "A.java", "package com.real;\nclass A {}", "com.real"},
	{"java block comment before package", "A.java", "/* ... package docs; */ package com.real;\nclass A {}", "com.real"},
	{"java line comment", "A.java", "// package com.fake;\npackage com.real;\nclass A {}", "com.real"},
	{"java javadoc", "A.java", "/**\n * Moved from package com.fake;\n */\npackage com.real;\nclass A {}", "com.real"},
	{"java string literal", "A.java", "class A { String s = \"package com.fake;\"; }", ""},
	{"java commented-out package only", "A.java", "// package com.fake;\n/* package com.fake; */\nclass A {}", ""},
	{"java comment inside name", "A.java", "package com. /* fake */ real;\nclass A {}", "com.real"},
	{"java package-info", "package-info.java", "/** Docs for package com.fake; */\n@Deprecated\n@Generated(\"package com.fake;\")\npackage com.real;\n", "com.real"},
	{"java duplicate package fails closed", "A.java", "package com.real;\npackage com.fake;\nclass A {}", ""},
	{"kotlin normal", "A.kt", "package com.real\nfun a() {}", "com.real"},
	{"kotlin block comment before package", "A.kt", "/*\npackage com.fake\n*/\npackage com.real\nfun a() {}", "com.real"},
	{"kotlin line comment", "A.kt", "// package com.fake\npackage com.real // package com.fake\nfun a() {}", "com.real"},
	{"kotlin kdoc", "A.kt", "/**\n package com.fake\n */\npackage com.real\nfun a() {}", "com.real"},
	{"kotlin string literal", "A.kt", "fun a() = \"\"\"\npackage com.fake\n\"\"\"", ""},
	{"kotlin string literal before package", "A.kt", "@file:JvmName(\"\"\"\npackage com.fake\n\"\"\")\npackage com.real\nfun a() {}", "com.real"},
	{"kotlin nested block comment", "A.kt", "/* /* nested */\npackage com.fake */\npackage com.real\nfun a() {}", "com.real"},
	{"kotlin duplicate package fails closed", "A.kt", "package com.real\npackage com.fake\nfun a() {}", ""},
	{"kotlin duplicate package with comment fails closed", "A.kt", "package com.real\npackage /* comment */ com.fake\nfun a() {}", ""},
	{"kotlin duplicate package same line fails closed", "A.kt", "package com.real; package com.fake\nfun a() {}", ""},
	{"kotlin comment after package", "A.kt", "package com.real\n/*\npackage com.fake\n*/\nfun a() {}", "com.real"},
	{"kotlin backticked first segment fails closed", "A.kt", "package `com`.real\nfun a() {}", ""},
	{"kotlin backticked segment fails closed", "A.kt", "package com.`fake`\nfun a() {}", ""},
	{"kotlin commented-out package only", "A.kt", "/*\npackage com.fake\n*/\n// package com.fake\nfun a() {}", ""},
	{"kotlin comment inside name", "A.kt", "package com . /* fake */ real\nfun a() {}", "com.real"},
	{"kotlin file annotation header", "A.kt", "/* package com.fake */\n@file:JvmName(\"Fake\")\npackage com.real\nfun a() {}", "com.real"},
	{"kotlin script", "a.kts", "/*\npackage com.fake\n*/\npackage com.real\nfun a() {}", "com.real"},
}

func TestJVMPackageIdentityComesFromSyntax(t *testing.T) {
	for _, tc := range jvmPackageFixtures {
		t.Run(tc.name, func(t *testing.T) {
			var pf graph.ParsedFile
			var err error
			if strings.HasSuffix(tc.path, ".java") {
				pf, err = NewJava().Parse(context.Background(), tc.path, []byte(tc.src))
			} else {
				pf, err = NewKotlin().Parse(context.Background(), tc.path, []byte(tc.src))
			}
			if err != nil {
				t.Fatal(err)
			}
			if pf.Scope.Package != tc.want {
				t.Fatalf("package = %q, want %q", pf.Scope.Package, tc.want)
			}
			for _, s := range pf.Symbols {
				if strings.Contains(s.QualifiedName, "fake") || strings.Contains(s.StableKey, "fake") || strings.Contains(s.QualifiedName, "docs") {
					t.Fatalf("symbol %s / %s carries a commented package", s.QualifiedName, s.StableKey)
				}
				if tc.want != "" && !strings.HasPrefix(s.QualifiedName, tc.want+".") {
					t.Fatalf("symbol %s is outside package %s", s.QualifiedName, tc.want)
				}
			}
		})
	}
}
