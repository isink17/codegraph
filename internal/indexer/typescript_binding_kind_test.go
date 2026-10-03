//go:build cgo

package indexer

import (
	"strings"
	"testing"
)

// A TypeScript or JavaScript call binds only a declaration its spelling can
// name. A bare identifier resolves through module and function scope, where a
// class method is never a binding. A member call `Foo.bar()` names the member
// `bar`, so the imported `Foo` is not its callee. Every entrypoint must give
// the same answer.
func TestTypeScriptCallBindsOnlyNameableDeclarations(t *testing.T) {
	cases := []struct {
		name   string
		caller string // path of the calling file
		src    string // its source
		others tree   // every other file
		dst    string // the call's dst_name
		want   string // "" for unresolved, else the target file:qname(kind)
	}{
		{
			name:   "named import member call",
			caller: "main.ts", src: "import { Foo } from \"./foo\";\nexport function caller() { Foo.bar(); }\n",
			others: tree{"foo.ts": "export class Foo {\n  static bar(): void {}\n}\n"},
			dst:    "Foo.bar",
		},
		{
			name:   "default import member call from JavaScript",
			caller: "main.js", src: "import Foo from \"./foo.js\";\nexport function caller() { Foo.bar(); }\n",
			others: tree{"foo.ts": "export default class Foo {\n  static bar() {}\n}\n"},
			dst:    "Foo.bar",
		},
		{
			name:   "named import deeper member call",
			caller: "main.ts", src: "import { Foo } from \"./foo\";\nexport function caller() { Foo.a.b(); }\n",
			others: tree{"foo.ts": "export class Foo {\n  static a = { b() {} };\n}\n"},
			dst:    "Foo.a.b",
		},
		{
			name:   "bare call does not name a same-file method",
			caller: "main.ts", src: "class Local {\n  run(): void {}\n}\nexport function caller() { run(); }\n",
			others: tree{"other.ts": "export function unrelated() {}\n"},
			dst:    "run",
		},
		{
			name:   "bare call does not name a same-file method in JavaScript",
			caller: "main.js", src: "class Local {\n  run() {}\n}\nexport function caller() { run(); }\n",
			others: tree{"other.js": "export function unrelated() {}\n"},
			dst:    "run",
		},
		{
			name:   "bare call beside a same-named method binds the function",
			caller: "main.ts", src: "function run(): void {}\nclass Local {\n  run(): void {}\n}\nexport function caller() { run(); }\n",
			others: tree{"other.ts": "export function unrelated() {}\n"},
			dst:    "run", want: "main.ts:main.run(function)",
		},
		{
			name:   "namespace import member call",
			caller: "main.ts", src: "import * as ns from \"./foo\";\nexport function caller() { ns.bar(); }\n",
			others: tree{"foo.ts": "export function bar(): void {}\n"},
			dst:    "ns.bar", want: "foo.ts:foo.bar(function)",
		},
		{
			name:   "same-file free function",
			caller: "main.js", src: "function helper() {}\nexport function caller() { helper(); }\n",
			others: tree{"other.js": "export function unrelated() {}\n"},
			dst:    "helper", want: "main.js:main.helper(function)",
		},
		{
			name:   "imported function",
			caller: "main.ts", src: "import { foo } from \"./foo\";\nexport function caller() { foo(); }\n",
			others: tree{"foo.ts": "export function foo(): void {}\n"},
			dst:    "foo", want: "foo.ts:foo.foo(function)",
		},
		{
			// A type alias or an interface is not a value; nothing calls it.
			name:   "bare call does not name a type alias",
			caller: "main.ts", src: "type Foo = number;\nexport function caller() { Foo(); }\n",
			others: tree{"other.ts": "export function unrelated() {}\n"},
			dst:    "Foo",
		},
		{
			name:   "bare call does not name an interface",
			caller: "main.ts", src: "interface Foo {}\nexport function caller() { Foo(); }\n",
			others: tree{"other.ts": "export function unrelated() {}\n"},
			dst:    "Foo",
		},
		{
			// ns.bar is itself a namespace object, not a function.
			name:   "namespace member that is a namespace",
			caller: "main.ts", src: "import * as ns from \"./foo\";\nexport function caller() { ns.bar(); }\n",
			others: tree{"foo.ts": "export * as bar from \"./baz\";\n", "baz.ts": "export function bar(): void {}\n"},
			dst:    "ns.bar",
		},
		{
			// `export * as ns from` creates no binding in its own module.
			name:   "same-file namespace re-export is not a local binding",
			caller: "main.ts", src: "export * as ns from \"./y\";\nexport function caller() { ns.bar(); }\n",
			others: tree{"y.ts": "export function bar(): void {}\n"},
			dst:    "ns.bar",
		},
		{
			// Neither does `export { foo } from`.
			name:   "same-file named re-export is not a local binding",
			caller: "main.ts", src: "export { foo } from \"./y\";\nexport function caller() { foo(); }\n",
			others: tree{"y.ts": "export function foo(): void {}\n"},
			dst:    "foo",
		},
		{
			name:   "named import of a re-exported namespace",
			caller: "main.ts", src: "import { ns } from \"./x\";\nexport function caller() { ns.bar(); }\n",
			others: tree{"x.ts": "export * as ns from \"./y\";\n", "y.ts": "export function bar(): void {}\n"},
			dst:    "ns.bar", want: "y.ts:y.bar(function)",
		},
		{
			name:   "imported type alias is not callable",
			caller: "main.ts", src: "import { Foo } from \"./x\";\nexport function caller() { Foo(); }\n",
			others: tree{"x.ts": "export type Foo = number;\n"},
			dst:    "Foo",
		},
		{
			name:   "namespace member interface is not callable",
			caller: "main.ts", src: "import * as ns from \"./x\";\nexport function caller() { ns.Foo(); }\n",
			others: tree{"x.ts": "export interface Foo {}\n"},
			dst:    "ns.Foo",
		},
		{
			// `export { foo }` names what the module offers; the declaration
			// is still the module's own binding.
			name:   "same-file function exported by a later export list",
			caller: "main.ts", src: "function foo(): void {}\nexport { foo };\nexport function caller() { foo(); }\n",
			others: tree{"other.ts": "export function unrelated() {}\n"},
			dst:    "foo", want: "main.ts:main.foo(function)",
		},
		{
			name:   "namespace import re-exported by an export list",
			caller: "main.ts", src: "import * as ns from \"./y\";\nexport { ns };\nexport function caller() { ns.bar(); }\n",
			others: tree{"y.ts": "export function bar(): void {}\n"},
			dst:    "ns.bar", want: "y.ts:y.bar(function)",
		},
		{
			// A default-exported declaration is still a binding of its own
			// module under its declared name.
			name:   "same-file default-exported function",
			caller: "main.ts", src: "export default function run(): void {}\nexport function caller() { run(); }\n",
			others: tree{"other.ts": "export function unrelated() {}\n"},
			dst:    "run", want: "main.ts:main.run(function)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			all := tree{c.caller: c.src}
			var otherPaths []string
			for p, src := range c.others {
				all[p] = src
				otherPaths = append(otherPaths, p)
			}
			entrypoints := map[string]func() *lifecycleRepo{
				"fresh": func() *lifecycleRepo { return newLifecycleRepo(t, all) },
				// The other files arrive after the caller.
				"names": func() *lifecycleRepo {
					r := newLifecycleRepo(t, tree{c.caller: c.src})
					for p, src := range c.others {
						r.write(t, p, src)
					}
					r.update(t, otherPaths...)
					return r
				},
				// Only the caller changes.
				"paths": func() *lifecycleRepo {
					r := newLifecycleRepo(t, all)
					r.write(t, c.caller, "// touched\n"+c.src)
					r.update(t, c.caller)
					return r
				},
				"combined": func() *lifecycleRepo {
					r := newLifecycleRepo(t, tree{c.caller: c.src})
					for p, src := range c.others {
						r.write(t, p, src)
					}
					r.write(t, c.caller, "// touched\n"+c.src)
					r.update(t, append([]string{c.caller}, otherPaths...)...)
					return r
				},
			}
			for _, entry := range []string{"fresh", "names", "paths", "combined"} {
				r := entrypoints[entry]()
				got := r.edgeState(t, c.caller, c.dst)
				if c.want == "" {
					if !strings.Contains(got, ":: [/]") {
						t.Errorf("%s: %s = %s, want unresolved", entry, c.dst, got)
					}
				} else if !strings.Contains(got, c.want) || !strings.Contains(got, "typescript_module_scope/high") {
					t.Errorf("%s: %s = %s, want %s", entry, c.dst, got, c.want)
				}
				r.assertFreshParity(t, entry)
			}
		})
	}
}
