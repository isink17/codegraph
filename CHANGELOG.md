# Changelog

## Unreleased

Dart: CGO builds (profile `treesitter:dart:v3`) record local functions and bind a bare call to a local function, or to a top-level function of a library with no `part` files, when that name is spelled nowhere else in the caller's top-level declaration or the library's directives and declarations (strategy `dart_lexical_function`); a file with a parse error binds nothing, a call to `_` (a wildcard since Dart 3.7) is never bound, and no repo-wide strategy answers a Dart call. Dart local functions are not cross-language link targets and are not listed by dead-code queries. The CGO profile now counts as a call graph: `codegraph doctor` no longer lists Dart as degraded in CGO builds, and a `CGO_ENABLED=0` `update` of a repository whose `.dart` files were indexed by a CGO build is refused as a downgrade.

Scala: a bare call is now resolved (`scala_local_function`, high) when Scala's scoping provably binds it to a local def of the same file. CGO builds record local defs (a `def` statement in a block, indented block or case clause, outside a local type, given or anonymous class) as `func:scala:local:<owner>.<name>:<line>:<col>`, and emit an edge only when the innermost scope declaring the name declares one def of it, spells the name nowhere else except as a bare callee or a selected member `x.f`, holds no `import` or `export`, and no type, template, given, extension, quote or splice lies between the call and that scope; overloads, any shadowing parameter, val, var, pattern, generator, given, nested def or local type, and any value use of the name refuse every call in the scope. A local def outranks members, inherited members and every outer import, so recursive helpers (`@tailrec def loop`) and local utilities now have callers. A case clause's defs are in scope only in its body, never in its pattern or guard. Local defs carry their line and column in their stable key, so edits above one change its key in diffs and symbol history. Lua local functions and Scala local defs are no longer cross-language link targets (a TypeScript import of a `.lua` or `.scala` file could bind a `cross_language_ref` to one) and are no longer listed by dead-code queries. Member calls, calls on objects, `apply`, implicit, given and extension calls, and all cross-file calls stay unresolved with no edge, and no repository-wide strategy answers a Scala call. Files with a parse error, layout damage, a hard keyword parsed as an identifier or misaligned indented statements get no local defs or edges, including some valid files the grammar misparses (about 2% of files in the audited repositories). Profile `treesitter:scala:v4`, so existing indexes reparse Scala files, and the CGO profile now counts as a call graph: `codegraph doctor` no longer lists Scala as degraded in CGO builds, and a `CGO_ENABLED=0` `update` of a repository whose `.scala` files were indexed by a CGO build is refused as a downgrade.

`check_constraints` gains opt-in strict freshness (`--strict-freshness`, MCP `strict_freshness: true`). Without it the output and exit codes are unchanged. With it, a result whose index was opened gains a `freshness` object (`verdict`, `reasons`, the graph_stats `state` and `state_reasons`, `head_now`, `last_full_scan`, `exit_code`), and `ok`/`violations` exit 0/1 only when the newest completed full scan started and finished at the current HEAD, overlapped no other scan and no scan failed since (`full_coverage_at_head`); otherwise the CLI exits 3 (`known_stale`: status `stale`, a moved HEAD, queued work or a failed latest scan; a newer running or abandoned scan with no other stale fact, including a first index still running, gives 4, though graph_stats calls it `known_stale`), 4 (`unknown`: evidence not recorded or not readable, an overlapping writer, any scan row still `running` (including one a crashed process abandoned, which keeps exit 4 until `codegraph index --rebuild` replaces the database), or scan activity during the check) or 5 (`insufficient_coverage`: no recorded full scan, a later failed scan, or HEAD moved during or since it; a path-scoped update never counts). `config_error`, `not_configured` and `not_indexed` still exit 2. No verdict means "fresh": uncommitted edits and unqueued watcher events are never checked. The full MCP `tools/list` grows by 38 bytes. See `docs/constraints.md`.

Scans now record their coverage (migration 9, additive columns on `scans`): `scope` (`full` for a walk of the whole repository under its own configuration, including `update` without paths; `paths` for a path-scoped update or watch flush; `filtered` for a walk under language, include or exclude overrides passed by an internal caller that differ from the repository configuration; no CLI or MCP surface sets them), HEAD read before the scan reads the repository configuration or the tree and again just before it is recorded completed (each bounded at 2 s; empty when Git, a repository or a commit is missing), and whether any other scan of the repository was running at any point while it ran. `freshness` gains `coverage` (`recorded`, `last_full_scan`, `failed_after_last_full`: failed scans that started after the last full scan or closed no earlier than it started) and each scan gains `head_at_start`, `head_at_finish` and `overlap` (`none`, `yes`, `unknown`); `scope` now reads the recorded value. A path-scoped scan never becomes `last_full_scan`, even when it advances `head_at_index`. Rows written before the migration stay unrecorded (`scope: "unknown"`, `overlap: "unknown"`, never a full scan), and a read-only handle on an unmigrated database reports `coverage.recorded: false`. A row left `running` by a crashed process cannot be told from a live writer, so every later scan reads `overlap: "yes"`. Nothing here checks uncommitted edits or unqueued watcher events, and a HEAD that moves away and back during a scan is not detected. `state` and `reasons` are unchanged.

`codegraph licenses` prints CodeGraph's license and `THIRD_PARTY_NOTICES` from copies embedded in the binary, with no network access, config or adjacent files. `THIRD_PARTY_NOTICES` gains the ICU (Unicode, Inc.) license for the `utf8.h`, `utf16.h` and `umachine.h` headers the Tree-sitter runtime compiles in.

Terraform: references inside operators are no longer dropped. The HCL grammar splits an operand's attributes and indices away from it inside a unary or binary operation, so `count = local.create && var.enabled ? 1 : 0`, `!var.disabled` or `var.a + module.m[0].x` recorded no reference at all; each operand traversal now names its own address, with the same binding rules as anywhere else (a literal index binds the declaring block, a computed index or splat stays unresolved). Profile `treesitter:hcl:v2`, so existing indexes reparse Terraform files.

Scala: damaged Scala 3 indentation syntax that the grammar parses without an error no longer moves members to another owner. In a `:` body, a member indented off the body's column or not deeper than its owner's line; at the top level, a member indented under a `:`-bodied type, or a non-type member under a bodyless type (a deleted body `:`); a top-level expression (a misspelt `package`); or an `end X` marker that does not directly follow and align with X bounds the recorded declarations like a parse error. Braced code and annotations on their own line are not column-checked, so valid, consistently indented Scala 2 and Scala 3 code records the same declarations as before. A line shifted by a whole indentation step can still form another valid program and is recorded as parsed, and a deleted `:` before a nested type on a top-level type is not detected. Profile `treesitter:scala:v3`, so existing indexes reparse Scala files.

Scala: a file with a parse error no longer records declarations under a guessed owner. Only declarations and imports that end before the first error are recorded, and a type or package body that does not end before it is skipped with its members; when the file has more `}` than `{`, a missing opening brace has moved members out of their type before any error shows, so no declaration is recorded. Broken files lose recall until fixed. Profile `treesitter:scala:v2`, so existing indexes reparse Scala files.

Dart (`.dart`), a conservative V1: CGO builds index `import`/`export`/`part`/`part of` URIs (conditional alternatives, `as`, `show`, `hide` and `deferred` kept as import evidence; `package:` URIs are never mapped to files), classes with Dart 3 modifiers, mixins, named extensions, extension types, enums and their values, typedefs, top-level functions, accessors and variables, methods, operators, fields and constructors (`Point.Point`, `Point.origin`, factory and redirecting factory), and call references (profile `treesitter:dart:v2`; non-CGO builds use `heuristic:dart:v1`). At this V1 (and `treesitter:dart:v2`) both profiles were symbols-only: no Dart call edge was emitted or resolved, because extension methods, cascades, mixins, implicit `this` and package resolution are not provable from syntax, so `codegraph doctor` listed Dart as degraded in every build (see the `treesitter:dart:v3` entry above for lexical call edges). A Dart import is never cross-language bridge evidence. In a file with a parse error only declarations and directives that end before the first error are recorded (none at all when the file has more `}` than `{` tokens, since recovery then moves members out of their type before reporting the error), and a top-level signature or variable without its body or `;` is never recorded. Null-aware elements (`[?x]`, `{?k: ?v}`, Dart 3.8), dot shorthands (`.red`, `.new()`, `case .red:`, Dart 3.10), `get`/`set` as identifiers and labeled statements parse, so declarations after them are recorded; a dot-shorthand call names no type and yields no call reference. A generic call with one type argument (`f<int>(x)`) is sometimes parsed as a comparison and yields no call reference. Grammar: UserNobody14/tree-sitter-dart `8bd7e00` (MIT); upstream publishes it as ABI 15, so its unmodified grammar is regenerated at ABI 14 with tree-sitter CLI v0.25.10 for the existing runtime, and the scanner, headers and license are vendored unmodified with a provenance record. The next upstream commit is not used: it parses `List<int>.filled(3, 0)` as a comparison. The grammar adds about 7 MB of generated C to the repository and about 1 MB of object code to CGO binaries; see `docs/dart-grammar-provenance.md`.

Release archives and the npm package now ship `THIRD_PARTY_NOTICES`, the full license text, copyright and upstream revision of every third-party component linked into the binary: the Tree-sitter runtime, go-tree-sitter, each bundled grammar (C, C#, C++, Dart, Go, HCL, Java, JavaScript, Kotlin, Lua, PHP, Python, Ruby, Rust, Scala, Swift, TypeScript; MIT, except HCL: Apache-2.0), the other Go modules (MIT and BSD-3-Clause; SQLite public domain) and the Go standard library. A test fails when a grammar package is imported or vendored without an entry.

Scala (`.scala`), a conservative V1: CGO builds index packages (chained clauses and package bodies), imports (selectors, `{A => B}` and `{A as B}` renames, `_`/`*` wildcards, `given` imports), classes, case classes, objects and companions, traits, enums, defs, vals and vars, type aliases, named givens and extension methods, and call references (profile `treesitter:scala:v1`; non-CGO builds use `heuristic:scala:v1`). At this V1 (`treesitter:scala:v1`) both profiles were symbols-only: no Scala call edge was emitted or resolved, because implicit and given scope, extension methods, inheritance, overloads and `apply` desugaring are not provable from syntax, so `codegraph doctor` listed Scala as degraded in every build (see the `treesitter:scala:v4` entry above for same-file local-def call edges). Companion objects get `object:` keys and `$`-suffixed member keys; overloads share a stable key and differ by signature and position. Declarations inside a parse-error node, local definitions (recorded from `treesitter:scala:v4`) and destructuring vals were not recorded. `.sc` scripts are intentionally not detected yet: SuperCollider shares the extension. Grammar: tree-sitter-scala v0.22.5 (MIT), shipped by the existing go-tree-sitter dependency; see `docs/scala-grammar-provenance.md`.

`graph_stats` (MCP) and `codegraph stats` gain an additive, read-only `freshness` object. `state` is `known_stale` (any reason other than `never_completed`), `unknown` (no completed scan and nothing else known) or `no_known_staleness`, never "fresh", with `reasons` (`never_completed`, `latest_scan_failed`, `scan_running_or_abandoned`, `dirty_queue_nonempty`, `head_moved`). It reports the last completed and the latest scan (`scope` is `full` only for `watch_config`; `index` and `update` share one kind with and without paths, so they read `unknown`), running scans with `liveness: "unknown"` (scans hold no lease), the dirty queue (`queued`, `in_flight`, `oldest_queued_at`), `watcher: "not_in_this_process"` (neither `serve` nor `stats` runs a watcher, and none is recorded), the worktree's canonical path, `head_at_index` (the Git history watermark: HEAD as probed by the latest scan that reached its history phase; `unknown` when history is absent or disabled), `head_now` from one Git call bounded at 2 s (`unknown` on any failure) and `head_changed` (`yes`, `no` or `unknown`), and `filesystem: "not_checked"`. Scans record neither their HEAD nor their scope, so a path-scoped update or watch flush advances `head_at_index` and becomes the latest scan: `head_changed: "no"` does not mean the whole graph was rebuilt at that HEAD, and a later completed path-scoped scan clears `latest_scan_failed` even when a failed full update's work is still missing. Existing keys are unchanged; no schema change.

Scans: a scan whose context is cancelled is now recorded `failed` with `context canceled` instead of staying `running` forever, and a scan whose final PHP Composer or SwiftPM fingerprint write fails is recorded `failed` instead of `completed`; a scan is marked completed only after all of its writes succeed. Only a process that dies mid-scan, or a failure that could not itself be recorded, still leaves a `running` row, which `freshness` reports as `scan_running_or_abandoned`.

HCL and Terraform/OpenTofu (`.tf`, `.tfvars`, `.hcl`; language `hcl`, profile `treesitter:hcl:v2`; non-CGO builds use the declarations-only `heuristic:hcl:v1`): `.tf` files record the `terraform` settings block, providers (`provider.aws`), resources (`aws_instance.web`), data sources (`data.aws_ami.base`), variables (`var.region`), locals entries (`local.tags`), outputs (`output.id`) and module calls (`module.vpc`), each with its Terraform address as the qualified name; a module's literal `source` is recorded as an import and labelled local path or remote, and is never fetched or resolved. Traversals rooted at `var`, `local`, `module`, `data` or a resource type become `references` edges (a new edge kind, not calls), and each `.tfvars` assignment references the variable it sets; `find_callers` on a declaration lists the blocks that reference it. A reference binds (`terraform_module_scope`, high) only to the one declaration of its address among the `.tf` files of the same directory; a duplicate or missing declaration, a splat or non-literal index (`[count.index]`, `[each.key]`), and every reference in a directory with a `.tf` file that did not parse completely stay unresolved, and no repository-wide strategy answers an HCL edge. `module.vpc.subnet_ids` binds to the `module "vpc"` block of the same directory, never to the child module's output. `count`, `each`, `self`, `path`, `terraform` and `ephemeral` roots, `for`-expression, template `%{ for }` and `dynamic` block iterators, `provider`/`providers` meta-arguments and lifecycle `ignore_changes` are not references. Terraform symbol names are short (`main`, `this`, `region`) and can make bare-name lookups ambiguous; query by address. The CGO profile counts as a relationship graph: a repository with Terraform files is not reported as symbols-only, and reindexing it with the non-CGO fallback is refused as a downgrade. Other `.hcl` files (Packer, Nomad, Terragrunt) record only top-level blocks and attributes. HCL files never take part in cross-language import links. `.tf.json` and `.tofu` files are not indexed, and nothing evaluates a Terraform plan. See `docs/hcl-grammar-provenance.md` and `docs/scope-models.md`.

Lua (`.lua`, standard Lua, not Luau): CGO builds index function declarations, literal `require` specifiers and call references (profile `treesitter:lua:v4`; non-CGO builds use the symbols-only `heuristic:lua:v1`). A bare call is resolved (`lua_local_function`, high) only when its innermost lexical binding is a `local function` of the same file that is never assigned to anywhere in its scope, closures included; the parser names the declaration's position and the resolver binds exactly that symbol or nothing. Globals, fields, methods, modules, parameters and other locals stay unresolved, and no repository-wide strategy answers a Lua call. A file with a parse error, or whose own code can reach the `debug` library, produces no call edges. Reaching it is decided by lexical scope, not spelling (so existing Lua files are reparsed on the next update): a free reference to `debug`, `ffi`, `_G`, `_ENV`, `package`, `getfenv` or a loader (`require`, `load`, `loadstring`, `dofile`, `loadfile`) refuses, except `debug.traceback`/`debug.getinfo`, literal-keyed `_G`/`_ENV`/`package.loaded` lookups of other names, `package.path`/`cpath`/`config`, loaders of a literal other than `"debug"`, and `load` of a literal chunk that itself passes. `log.debug(...)`, `{debug = true}`, a local or parameter named `debug`, and a `"debug"` string used as data no longer refuse the file; a computed name such as `_G["de".."bug"]`, `_G[k]`, `require(name)` or `pcall(require, m)` now does. Because the debug library rewrites the locals and upvalues of any function in the Lua state (`debug.setupvalue`/`upvaluejoin` on an exported closure, `debug.setlocal` from a hook or callback), one indexed Lua file that can reach it, or that could not be scanned (oversize, failed parse, non-CGO build), leaves every Lua call in the repository unresolved, in fresh and incremental indexing alike; a file with a parse error counts only if it spells one of the names above. Assigning to an exported table field (`M.f = other`) never affects a call to the `local function f` and is not a hazard. Not detected: code outside the index (installed modules, the host application) and the C API (`lua_setupvalue` from C). The grammar is `tree-sitter-grammars/tree-sitter-lua` v0.3.0 (MIT, ABI 14), vendored unmodified with its license in `internal/parser/treesitter/luagrammar` in place of go-tree-sitter's `lua` package (whose upstream license notice was unresolved), whose scanner kept global state and could corrupt Lua files parsed concurrently; Lua 5.4 `goto`, labels and `<const>`/`<close>` locals now parse, and a bracket callee such as `t[k]()` is no longer named `tk`. Its notice ships in `THIRD_PARTY_NOTICES` (see above). See `docs/lua-grammar-provenance.md`.

TypeScript and JavaScript: a call whose name a scope around it binds no longer binds through an import of that name. With `import { k } from "./a"`, `function caller(){ const k = () => 1; k(); }` and `function caller2(k){ k(); }` bound `a.k` (`typescript_module_scope`, high), and `function t(ns){ ns.k(); }` bound `a.k` through `import * as ns`; node runs the local in each case. The parser (profile `treesitter:typescript:v2`, so existing TypeScript and JavaScript files are reparsed on the next update) marks a call whose first name a parameter (destructured or not), a `const`, `let` or `class` in an enclosing block, a `var` or function declaration anywhere in an enclosing function, a catch parameter, a `for` binding or a named function or class expression binds, and a module-level declaration of an imported name (a redeclaration error); such a call stays unresolved. A `const` in a block that does not enclose the call, a local in a sibling function and a default parameter value still bind the import. The first `update` with this build reparses every TypeScript file, so an index written by an earlier build drops the wrong target and its call-site reference without a source change. A build with the v1 parser reparses again on its next update and restores the v1 bindings; it does not refuse the newer state.

Java: a single or on-demand static import no longer binds an unqualified call in a class that, or any class enclosing it, spells an `extends`, `implements` or interface `extends` clause (profile `treesitter:java:v11`, so existing Java files are reparsed on the next update). An inherited method of that name shadows the import whatever its arity (JLS 6.4.1, 15.12.1): with `import static app.Util.foo;`, `class Derived extends Base { void run() { foo(); } }` and `Base.foo()`, javac runs `Base.foo` but `app.Util.foo` was bound. Supertypes are not recorded, so such calls now stay unresolved, including where javac would pick the import because the inherited method is private, package-inaccessible or an interface static method. A call in an enum, record or annotation body, and a call named after a `java.lang.Object` method (`toString`, `equals`, ...), also no longer binds a static import. A class with no supertype clause still binds it. Updating a graph written by v10 drops the wrong edge target and reference identity without a forced re-index; an older binary reparses the files with its own profile and restores its own binding.

C#: a bare call no longer binds through `using static` when simple-name lookup would find a member first (C# specification, simple names and member lookup). With `using static App.Util;`, `Foo()` in `class Derived : Base` bound `App.Util.Foo` although the compiler calls the inherited `App.Base.Foo`, and `Bar()` in `Outer.Inner` bound `App.Util.Bar` instead of `App.Outer.Bar`. Inherited and enclosing-type members are not bound, so such a call now stays unresolved whenever the caller's type or any enclosing type has a base list (interfaces included, for default interface members; an external base cannot be inspected) or an enclosing type declares a member of that name. A type with no base list that is not nested still binds `using static`. Recall ceiling: a base list refuses even when no base declares the name, including a private base member the specification would skip. The parser records a type's base list as its signature (profile `treesitter:csharp:v6`, so existing C# files are reparsed on the next update), and C# resolver policy moves to epoch 4: an ordinary `codegraph update` clears the wrong edges and their reference identities without a source change, and an older binary refuses the updated graph. No C# compiler was available: expected targets are specification-backed, not compiler-verified. A call named after a `System.Object` member (`ToString`, `Equals`, `GetHashCode`, `GetType`, `MemberwiseClone`, `ReferenceEquals`, `Finalize`) never binds `using static`, because every type inherits those members.

C#: Cross-file `global using` aliases, namespace imports, and static imports are ambiguity evidence only because the graph does not record C# compilation membership. When one could change a call's target, CodeGraph leaves it unresolved rather than applying the directive across every project in the repository. This covers a call's qualifier and the declared type of a typed receiver (`void M(Util u) { u.Go(); }`, a field, `this.field`); only a type declared in the caller's own or an enclosing namespace is proven to win over such a directive. A namespace-level alias of the same name is not, because stored evidence cannot tell which body of a reopened namespace it was written in, so `namespace X { using Util = B.Util; ... Util.Run(); }` beside a cross-file `global using Util = A.Util;` stays unresolved. A C# path update re-decides C# edges repository-wide so adding, changing, or removing a global directive updates unchanged callers and their reference identities. This resolver-only change leaves parser profiles unchanged; ordinary `codegraph update` upgrades resolver policy epoch 2 to 3. The global-directive behavior is deliberately fail-closed and loses recall until trustworthy compilation membership is indexed. No C# compiler was available: semantic expectations are specification-backed, not compiler-verified.

C#: a type name now resolves level by level through the namespaces enclosing the call, as the C# specification (basic concepts, namespace and type names) orders it: a type declared in the level, then its alias, then the types its using-namespace directives import, before the next enclosing namespace. `namespace a.b { using staticns; ... Util.Run(); }` with both `a.Util` and `staticns.Util` bound `a.Util.Run`, and a root `using Util = staticns.Util;` beat a type `Util` declared in `a.b`; the latter now binds `a.b.Util.Run`. Stored evidence holds only the owner namespace of a directive, not the declaration enclosing the call (a namespace reopened in a second body, or a file-scoped namespace whose directives have an empty owner, is indistinguishable), so where an import and a type at another level would give different answers the call stays unresolved instead of guessing: an inner using or alias against a type in an enclosing namespace, a root or file-scoped using against any type outside the innermost level, an alias beside a type of the same name, and two imports supplying different types. Own-level types, a lone import, `global::` qualification and the existing refusals are unchanged. Imports owned by an enclosing namespace's declaration are still not applied. This is a resolver change with no parser profile bump: normal `codegraph update` repairs existing edges through C# resolver policy epoch 2, including graphs stamped by the earlier epoch 1. No C# compiler was available on the host: the expected targets come from the specification. A using directive or alias owned by an enclosing namespace (`namespace a.b { using other; namespace c { Util.Run(); } }`) is still not applied, because the evidence holds only the owner's name and not the declaration around the call, but it now refuses a binding to a type found in a more outer namespace when it could preempt it (an alias of that name, or a using-namespace supplying a different indexed type), so the edge stays unresolved instead of binding the outer type. A dotted qualifier whose first segment is an alias in scope (`namespace a.b { using N = other; ... N.Util.Run(); }` with `a.N.Util` and `other.Util`) is a namespace alias prefix; the call bound `a.N.Util.Run` through the declared namespace, but the specification resolves `N` through the alias first. It is now refused when an alias of that first segment is owned by the call's namespace, an enclosing namespace, or the compilation unit (including an alias of an unindexed namespace); direct type aliases (`U.Run()`) and `global::` qualification are unchanged.

C#: using-directive evidence is read from the declaration's syntax tokens (profile `treesitter:csharp:v5`, so existing C# files are reparsed on the next update; heuristic `heuristic:csharp:v5`). The v4 parser stripped a leading `static` from the imported name even without a following space, so `using staticns;` was recorded as `ns` and `Util.Run()` bound `ns.Util.Run` when both `ns.Util` and `staticns.Util` existed; the C# specification (using_namespace_directive) imports `staticns` only, so the call now binds `staticns.Util.Run`. `static` or `global` followed by a tab, a newline or a comment is now recognised, and whitespace or comments inside the name no longer change it. A directive with a syntax error, or one that combines modifiers C# forbids (`using static A = X;`, `using unsafe N;`), imports nothing. No C# compiler was available on the host: the expected targets come from the specification, not from compiling the fixtures.

Rust: a module's own `const`, `static` or extern-block item now shadows a glob-imported function of the same name, as rustc does. With `use crate::a::*;` and `const f: fn() = || {};` in the same module, `f()` bound `crate::a::f`; it now stays unresolved, as does a call through a re-exporter that declares such an item itself (`use crate::b::f;` or `crate::b::f()` over `pub use crate::a::*; pub const f: ..`). The parser records these items, and every module-level macro invocation, as syntax evidence (profile `treesitter:rust:v5`, new table `rust_value_item_evidence`, so existing Rust files are reparsed on the next update). A macro's expansion is never read: a module that holds an item-level macro invocation (`mk!(f);`, `thread_local! { .. }`, `include!(..)`) or a span the grammar could not parse refuses to bind a glob-imported function by bare name, a qualified call's first segment, or through its glob re-exports, even when the macro generates some other name, so recall drops for such modules. Invocations inside functions, impl bodies and nested modules affect only their own scope. Procedural derives and attribute macros are never expanded, so they are fail-closed the same way: any derive (built-in names included: a `#[macro_use] extern crate` in another file can make `Clone` a procedural derive, and rustc 1.98.1 then prints its `f`), or an attribute that is not a built-in non-generative one (`allow`, `inline`, `doc`, `link_name`, ...; `cfg` and `cfg_attr` are not), may emit a sibling `fn f` (rustc 1.98.1 with a real proc-macro crate prints the derive's `f`, not the glob's), so its module refuses glob-resolved calls; an attribute macro may also rename or drop the item it sits on, so that function, type, impl or module (and everything below it) binds no call, and an attributed `use` imports nothing. A block-level custom derive refuses calls in its block the way a statement macro does. A built-in attribute counts as built-in only when no `use` (an alias included) or `macro_rules!` in the file binds that name. Recall ceiling: every derive, even `#[derive(Debug)]`, refuses its module's glob-resolved calls; a glob or crate-root `#[macro_use] extern crate` that brings in a macro named like a built-in non-derive attribute (`inline`, ..) is not detected from another file, and a file-wide alias such as `use p::Gen as inline;` makes every attribute in that file unproven. An attribute macro on the function, impl, trait or inline `mod` that holds a call may rewrite the whole body (rustc 1.98.1: `#[inject] fn caller() { g(); other::h(); }` runs the injected `g` and `other::h`), so no original call inside such an item binds any target, `crate::`, `self::` and `super::` paths included; the call stays unresolved. An unevaluated `#[path = "real.rs"] mod child;` no longer records the default `child.rs` as the module's file, so an orphan `child.rs` is not bound (rustc calls `real.rs`); it declares no module, like any other attributed `mod`, and a plain `mod child;` is unchanged. `#[cfg(..)]` is unproven too: no configuration is evaluated and a disabled declaration is not a binding (rustc 1.98.1: `use provider::*; #[cfg(any())] fn f() {} fn caller() { f(); }` calls the glob's `f`), so a cfg-gated declaration or import, and a call inside a cfg-gated item, stay unresolved. Recall ceiling: every call inside an attributed item is unresolved (always-active `cfg(all())` and a complementary `cfg(unix)` / `cfg(not(unix))` import pair included), and a `#[path]` module's own items stay unreachable by path.

Resolver policy upgrade: a change to how a language's resolver decides over unchanged facts is now versioned per repository and per language, and an ordinary `update` applies it. A resolver-only fix used to leave every existing graph on its old bindings until a source file changed, because nothing recorded which resolver had decided an unchanged file's edges. Every language with a resolver (C++, C#, Go, Java, Kotlin, PHP, Python, Ruby, Rust, Swift, TypeScript) now has a policy version, so the first `update` of a graph written before policies existed decides each language's edges once and clears the wrong bindings earlier resolver-only fixes left on unchanged source (the Rust glob re-export rule, Java and C# import evidence, TypeScript development-index edges), without a re-index. C# ships at epoch 3, so graphs stamped at epochs 1 or 2 re-decide existing C# edges through an ordinary update. Markers are kept in the repository's `settings` as `resolver.policy.<repo id>.<language>`. A missing or lower marker makes the next scan decide that language's existing edges again, once, with the canonical resolvers and without parsing any source: the language's edges are cleared and rebound, the call-site reference identities of its files are reconciled, and the marker is recorded in one transaction, so a failure leaves the old bindings and the old marker. Only the selected languages are written: other languages' bindings, unresolved edges and references are never touched, even an edge an older resolver left unresolved, and a marker is re-read inside the transaction so a newer binary's stamp rolls the whole redecision back instead of being certified over. A refused scan no longer even updates the repository row. A scan that cannot touch a language (a `--languages` filter, a path-scoped request for other languages) leaves its marker alone. A full or forced `index` resolves the whole repository whatever `--languages` selected for parsing, so it honours every language's marker: a newer, unreadable or unregistered marker of any language refuses it before anything is written, and it records the policy of every language it re-resolved. The unscoped store resolve checks all markers inside its transaction too. Current markers cost one settings read. A marker above this binary's policy, one that is not a canonical version number, or one for a language this binary has no policy for refuses the scan before anything is written, in the same place as a parser downgrade. The scan summary reports `resolver_policy_languages` and `resolver_policy_ms`, and `resolve_mode` is `resolver_policy` (or `<mode>+resolver_policy`) when it ran. On a 600-file, 24 000-edge synthetic repository the one-time Rust upgrade took 1.0 s against 3.3 s for `index --force`, and a steady-state update 0.08 s. The binary's build is not a policy: only a deliberate version bump in the registry causes work. The policy repairs bindings the resolver would no longer make on unchanged facts; it does not reparse, so a fix that also changes what a parser extracts still needs its parser profile bump.

Rust: a re-export now passes on only the items the re-exporting module itself can see. With `pub(super) fn id` in `crate::a::x` and `crate::c` re-exporting `crate::a::x::*` and `std::process::*`, `crate::c::id()` called from `crate::a::y` bound `crate::a::x::id` because the caller could see it; rustc calls `std::process::id` (the `a::x` glob imports nothing). It now stays unresolved, while a re-export from `crate::a` itself still passes the item on. This is a resolver change with no parser profile bump: normal `codegraph update` re-evaluates existing Rust bindings once through resolver policy epoch 1, without requiring a source change or force indexing.

Java: import evidence is read from the declaration's syntax nodes (profile `treesitter:java:v10`, so existing Java files are reparsed on the next update). The v9 parser stripped a leading `static` from the imported name even without a following space, so `import staticpkg.Bag;` was recorded as `pkg.Bag` and `new Bag()` bound `pkg.Bag` when such a type existed (javac constructs `staticpkg.Bag`); `import staticw.*;` likewise bound `w.*` types. `static` followed by a tab, a newline or a comment is now recognised as a static import, and whitespace or comments inside the name (`import a.B /*c*/ .Box;`) no longer make the import unusable. A declaration with a syntax error still refuses.

Java: an unqualified construction in a class that, with every class enclosing it, spells no `extends` or `implements` clause now binds the package or import type of its name even when a member type elsewhere shares that name. In `package app; class Plain { void make() { new Bag(); } }` with a top-level `app.Bag` and an unrelated `Base.Bag`, the resolver left `new Bag()` unresolved because supertypes are not recorded; javac constructs `app/Bag`, since such a class inherits only `Object` (or the implicit `Enum`/`Record`), none of which declares a member type. The parser (profile `treesitter:java:v9`, so existing Java files are reparsed on the next update) marks such a construction when it sits directly in a method, constructor or lambda body and no enclosing class declares a member type of that name. A name a single-static-import also names (it may import a member type, which shadows the package type), any name in a file whose single import is spelled with spaces or comments inside its name (its imported name is not known), a class with any supertype clause, an enclosing class with one, anonymous, local and enum-constant bodies, field initializers and a package with two types of the name stay unresolved. gson 854c8255: the 7 `metrics.BagOfPrimitives` constructions now bind (constructs 1481 -> 1488 of 3312), all javac-confirmed; nothing lost or retargeted.

Git history now records, for each symbol, the newest commit of the 250-commit window that `git blame --first-parent` attributes to any line of its range at the watermark, with its committer time and mailmap-aware author email. Each symbol has a `state`: `last_commit`, `before_window` (all lines older than the window), `no_range` or `not_computed`. It is not a change count. Symbol history is withheld, with the reason on the file row, for files whose working tree differs from the watermark (`worktree_differs`: modified, staged, untracked, or an edited file flagged `assume-unchanged`/`skip-worktree`, which is hashed and compared with the blob since `git diff` skips it; this also corrects the file row's flag), files with a Git `filter` attribute such as LFS (`filtered_content`), and files parsed by a heuristic adapter, whose ranges cover only the declaration line (`untrusted_ranges`). A shallow clone does not credit pre-cut lines to its oldest commit, and a Git failure while resolving the window boundary makes history absent instead of running blame unbounded. Read it with `file_history` and `include_symbols` (CLI `--symbols`) on at most 500 explicit files; one response returns at most 500 symbols, with each file's `symbol_count` and `symbols_truncated`. Rows are keyed by path and line range; an update with an unchanged watermark blames only files whose ranges changed, and stored values equal a fresh index. Schema migration 3 adds the tables, and the history algorithm becomes `file-v1+blame-v1`, so the next scan recomputes history; a binary without migration 3 refuses the database. On this repository the history phase grows from about 1.0-1.5 s to about 2.1 s of a 15 s index. Enrichment only: the semantic graph is unchanged.

TypeScript and JavaScript: `update` no longer binds a dotted call that a fresh index leaves unresolved. TypeScript scope owns every TypeScript and JavaScript edge, but the dotted-suffix pass that `update` reruns after its language passes did not see that ownership, so `service.UserService.create()` through `import * as service from "./user.service"` bound to `user.service.UserService.create` (`dot_tail3`) after any edit and was unresolved again after `index --rebuild`. The ownership rule now reads the edge's language alone, as the incremental binder already did, and `explain` reports an unresolved TypeScript edge as refused by `typescript_scope_ownership` rather than `unknown`. The fix prevents new wrong bindings. A TypeScript edge an earlier development build already bound this way keeps that binding, and its call-site reference keeps the target, until a TypeScript file in the caller's import component changes, any change triggers a full `index`, or `index --rebuild` is run; valid module-scope and cross-language bindings are unaffected. As for other development-index defects, no repair marker is provided. The TypeScript `treesitter:typescript:v2` profile above reparses every TypeScript file on the first update with a build that has it, which also rewrites such a binding.

Re-running `codegraph index` over an existing database that has changed files now re-decides every edge, as a fresh index does; a run with no changed files resolves nothing. Unchanged files are skipped and keep their edges, and the repository-wide resolve used to decide only unbound ones, so a binding another file's change made ambiguous stayed bound: adding a second `helper` in `other.py` left `main.py`'s `helper()` bound to `util.helper`, where a fresh index and `update` leave it unresolved, and the call-site reference kept the old target. Cross-language links are kept. `update` and path-scoped runs are unchanged.

Java: edges now carry the column they start at (profile `treesitter:java:v8`, so existing Java files are reparsed on the next update), and the store credits an edge to the method or constructor whose body holds that position. Methods sharing a line no longer make an edge ambiguous and drop its caller: `void gen() { new Box<>(); } void x() { new Box(); }` credits each construction to its own method. Positions that two different declarations contain with identical ranges stay ambiguous, and an edge outside every span keeps no caller. A call-site reference is matched to its edges by file and line, so when edges on one line disagree on their source the reference keeps no context rather than a guessed one. The own-member-type marking above still decides by line and is unchanged.

Java: a construction of a member type the calling class declares itself now binds that member type. In `class Own { static class Box {} void make() { new Box(); } }` the resolver left `new Box()` unresolved whenever another class also declares a member `Box`, because the construction could sit in an anonymous or local class body, or the class could inherit another `Box`; javac constructs `Own$Box`, since a declared member type hides every inherited, enclosing, imported and package type of its name. The parser (profile `treesitter:java:v7`, so existing Java files are reparsed on the next update) now marks an unqualified `new` of such a name when it sits directly in a method, constructor or lambda body of that class, and no other method or constructor declaration covers its line; the marking is decided by line alone, which stays conservative. Constructions in anonymous, local or enum-constant class bodies, field initializers and initializer blocks, enum members, and member types of an enclosing class are still not bound; neither is a top-level class whose simple name a member type elsewhere shares, since supertypes are not recorded (gson: 7 such edges).

Python: a dotted call `C.member()` whose receiver is a class declared in a function (a nested local class) now binds to that class's own member (strategy `python_local_class_scope`, high), never to a same-named class in another module. It stays unresolved when the nearest declaration of `C` is not exactly one class, a wildcard import is in the file, the name is `global`/`nonlocal`/`del`eted, the class is decorated, or the class body rebinds the member. A module-level class is left to the other strategies. `treesitter:python:v8` and `python-regex:python:v8` reparse unchanged Python files.

Python: a direct method's `def` header now matches the class-body names it spells by their NFKC form. In `class C: full = lambda: "cls"` with `def m(self, x=ｆｕｌｌ()):`, the tree-sitter parser bound the fullwidth call to `from lib import full`, while CPython calls the class attribute; it now stays unresolved like the ASCII spelling. The regex parser reads no calls from a `def` header, so it had no wrong edge, but records the same binding. `treesitter:python:v7` and `python-regex:python:v7` reparse unchanged Python files.

New `explain` command and MCP tool `explain_edge`: why an edge is resolved or unresolved. Select by edge id, or by file and line with an optional exact destination name; multi-edge selections are paged (`limit`/`offset`) and ordered by file, line, name, kind and id. A resolved edge reports its persisted strategy, confidence and target. An unresolved edge reports every resolver inventory rule that provably refuses it, with stage and disposition, or `unknown` when no evaluated rule does; the rules that are not or only partly evaluated are listed in every answer. The explanation reflects the current graph state only; no resolution history is stored and the schema is unchanged. Read-only; the CLI output is byte-equal to the tool's data. All reads of one call come from a single read-only transaction. The tool is not listed in `tools/list` in either mode, so neither payload changes; call it by name or find it with `tool_search`. See `docs/explain.md`.

The resolver's own-module, Go local-qualifier and dotted broad-ambiguity vetoes are now applied by the Go binder from the shared rule inventory, each with a SQL/Go parity test; graphs are unchanged. A read-only internal check reports, for an unresolved edge, which inventory rules refuse it (not yet exposed).

Java generic constructions now bind like raw ones. `new Box<>(x)`, `new Box<String>(x)`, `new a.b.Box<>()` and `new Box<>() {}` previously recorded the class with its type arguments (`Box<>`), which no Java type matches, so they all stayed unresolved; they now name `Box` or `a.b.Box` and choose the constructor by argument count as a raw `new Box(x)` does. Type arguments on an outer segment (`new Outer<A>.Inner<B>()`, which javac rejects) and qualified creations (`outer.new Inner<>()`, whose class is a member of outer's type) keep their spelling and stay unresolved. A construction or qualified call whose class or owner is named by a local class, interface, enum or record -- one declared in an enclosing method, constructor, initializer or lambda body, or in an anonymous or local class body -- now stays unresolved instead of binding a recorded type of that name, raw (`new Box()`) or generic (`new Box<>()`); javac constructs the local class. A local type in a sibling method or a non-enclosing block hides nothing. `treesitter:java:v6` reparses unchanged Java files.

Index and update now record file-level Git history for the latest 250 first-parent commits reachable from `HEAD`: per file, commit count, first and last commit, mailmap-aware author count and top author, `--numstat` line churn, explicit Git reverts, and whether the working tree differs from that commit. Renames are followed inside the window. History is enrichment only and never changes the semantic graph. A missing or failing Git, a non-repository and a shallow clone are reported as distinct states and never fail the scan. Read it with the new `file_history` MCP tool (not in `tools/list`; found through `tool_search`) or `codegraph file_history`; `--no-history` skips it. Schema migration 2 adds the history tables; an earlier v2 build refuses a database this build has opened. See `docs/git-history.md`.

Java qualified class instance creations (`outer.new Inner()`, `this.new Inner()`, `Outer.this.new Inner()`) no longer bind a top-level class that shares the inner class's name. javac constructs the member class of the qualifier's type (`Outer$Inner`), but the resolver looked `Inner` up as a bare name and could bind an unrelated `app.Inner`. Qualified creations now stay unresolved. Simple type names in Java constructions and static calls now respect member types: a name that any enclosing class of the caller declares as a member type, or that any non-private member type in the project the caller could inherit uses (other than the one a single-type import names), stays unresolved instead of binding a same-package class or a single-type import. Supertypes are not recorded, an inherited member type hides the package class (`class Sub extends Base` constructing `Box` gets `Base.Box` from javac; an interface's member types are inherited too, whatever package they are in), and an edge inside an anonymous or local class body is credited to the enclosing method, whose own member types that body can hide. This costs recall: a top-level class whose simple name some accessible member type also uses is no longer bound by its simple name. Classes declared in a method body or an anonymous class body are handled by the local-type marker described above. Existing graphs pick these refusals up through the `treesitter:java:v6` reparse described above.

Rust: an incremental update no longer records a crate root on files the crate does not declare. Previously, when the changed files led to a single crate, its root was written to every changed file, including non-Rust files and Rust files no `mod` declaration reaches, and stayed there when the batch held no Rust call to re-resolve; a fresh index of the same tree records none. Membership is now recomputed from the declaration graph at that point, so `update` stores the crate roots a fresh index stores, except for a crate nested inside another crate's directory, where a touched non-root file of the inner crate can still lose its root until the next full index. Edges are unchanged: the stored root was never used to bind a call.

New `check_constraints` command and MCP tool (CLI alias `check-constraints`). It checks architectural dependency rules between path groups declared in a committed, repo-root `.codegraph-constraints.json`. There are four rule kinds: `forbidden_dependency`, `allowed_dependencies`, `allowed_dependents` and `forbidden_cycles`. Rules are evaluated over high- and medium-confidence resolved calls and constructions whose files are live. Unresolved edges, import strings, low-confidence and cross-language links, deleted files and other repositories never become violations; unresolved and excluded edges are counted in `coverage` instead. Groups use anchored segment globs (`**` spans segments; there is no basename fallback and nothing is case-folded), and a file in two groups is a `group_overlap` config error. Cycles are reported per strongly connected component of the group graph, with a shortest witness path. The CLI exits 0 when the rules hold, 1 on violations, and 2 when the result cannot be vouched for (`stale`, `not_indexed`, `not_configured`, `config_error`, invalid arguments); the JSON is written before the exit code is decided. The tool is read-only and takes `limit`/`offset`. It is listed in `full` mode (full `tools/list` grows from 12,676 to 12,974 bytes, +75 estimated tokens) and found through `tool_search` in `gateway` mode, whose payload is unchanged. See `docs/constraints.md`.

The resolver's three chosen-candidate restrictions (type scope, C++ bare namespace and member scope) are now applied by the Go binder from the shared rule inventory, with a SQL/Go parity test per rule; graphs are unchanged. The binder's resolved count now includes edges bound by the language passes in the same batch (statistics only).

Concurrency fixes: `watch` now stops and waits for its flush loop before returning an error, so a flush can no longer run after the store is closed. The dirty-file queue rolls back its write transaction even when the caller's context is already cancelled; previously the connection could return to the pool still inside the transaction, so later queue writes were never committed. Extended SQLite busy codes such as `SQLITE_BUSY_SNAPSHOT` and `SQLITE_BUSY_RECOVERY` are now treated as busy.

Relationship answers now say when the persisted graph cannot back them. Capability is read from the parser profiles stored per file, not from the running binary: `call_capable`, `symbols_only`, `unknown_provenance` or `mixed`, per language and for the graph. `supported_languages` reports it as `graph_capability`. `find_callers`, `find_callees`, `get_impact_radius`, `trace_dependencies`, `find_related_tests`, `find_dead_code`, `graph_analytics` and `context_for_task` (when it includes callers) add a `limitations` list (language, `graph_capability`, effect) when a language is not fully call-capable, for example Java indexed by a `CGO_ENABLED=0` build or Python indexed by its regex fallback. Compact responses and the CLI `callers`, `callees`, `impact` and `find_related_tests --json` commands carry the same list; `find_related_tests` in text mode prints it as one stderr line, and `codegraph doctor` reports the four states. On a call-capable graph the field is absent and responses are unchanged.

Python lambdas in a `def` parameter default, bodies written on a `def` or `class` header line, and names bound in a class body now shadow imports in both Python parsers. The tree-sitter parser previously bound `full()` to `from lib import full` in `def run(cb=lambda full: full())` (single-line, multi-line, method and nested-function headers), in `def run(): full = lambda: 1; return full()`, in a direct method's default `def m(self, x=full())` (also `async def`, and in a class outside any function) when the class body binds `full`, and in a lambda among a class header's keywords (`class C(B, cb=lambda full: full())`); both parsers bound it in a class body nested in a function (`class C: full = lambda: 1; g = full()`), and the tree-sitter parser also when that body is written on the `class` line. CPython calls the parameter, the local or the class attribute. A class body's assignments, loop targets, aliases, lambda parameters, `case` captures, imports and nested `def`/`class` names are recorded under the function the class is written in and refuse only calls attributed to that function itself; they are also recorded, for the names a direct method's `def` header spells, under that method with the header's last line, and refuse only calls on the header's lines. Method bodies and nested functions skip the class scope, so `class C: full = 1; def m(self): return full()` still binds the import, as CPython does. Over-refusals, left unresolved rather than bound: a class-body binding refuses same-named calls in the enclosing function's own body; a header binding refuses same-named calls in a one-line method's body; a lambda parameter in a default refuses same-named calls in the whole function; classes of one qualified name (one per branch of an `if`/`else` or `try`/`except`) each refuse for the bindings of all of them. A class outside every function has no calls attributed to its body. The regex parser reads no calls from a `def` or `class` header line, so it had none of the header-line wrong edges. `treesitter:python:v6` and `python-regex:python:v6` reparse unchanged Python files.

Python lambda parameters and `match`/`case` capture names now shadow imports in both Python parsers. `g = lambda full: full()` and `case full: return full()` previously bound `full()` to `from lib import full`, while CPython calls the parameter or the capture; the same held for `*args`, `**kwargs`, keyword-only and defaulted lambda parameters and for captures in sequence, mapping, class, `as` and or-patterns. A capture makes the name local to the whole function, so a call before the `match` is no longer bound to the import either. Lambdas are not lexical owners in the graph, so a lambda parameter refuses same-named calls in the whole enclosing function, not only in the lambda body: those calls stay unresolved rather than bind. `treesitter:python:v5` and `python-regex:python:v5` reparse unchanged Python files.

Python names are now recorded in the NFKC form CPython binds (PEP 3131): `ｆｕｌｌ`, `ﬁnd`, `𝐟` and `Ⅻ` are `full`, `find`, `f` and `XII` in definitions, calls, attributes, imports, parameters and local bindings, in both Python parsers. A local `ｆｕｌｌ = ...` previously failed to shadow `from lib import full`, so `full()` bound `lib.full` while CPython calls the local. String, comment and file-name text keeps its spelling. This adds the `golang.org/x/text` dependency (BSD-3-Clause) for the NFKC tables. `treesitter:python:v4` and `python-regex:python:v4` reparse unchanged Python files.

Rust: private items (and `pub(self)` items) declared at a file's module level now resolve from that module and from file modules nested inside it, for example `helper()` in the same module, or `super::helper()`, `crate::helper()` and `use super::helper` in a child module. Before this change a private Rust item never received call edges. They still never resolve from sibling or parent modules, through a re-export, or from another crate root, and the caller's module must be proven by the declaration graph rather than guessed from the file path. Paths inside an impl method now resolve from the module that holds the impl, however its type is written (`impl S`, `impl crate::a::S`), so `super::helper()` there reaches the parent module's item rather than the crate root's, and a bare `helper()` reaches the module's free function rather than the type's associated function. `pub(in path)` items, and private items of inline modules, are still left unresolved. Items of a trait impl (`impl Trait for T`) are now recorded with visibility `trait_impl`, since they take the trait's visibility, and never answer a call; only a private method of an inherent impl whose type is written as a bare identifier answers `Type::method`. This is a conservative rule, not a full model of Rust's method lookup: calls such as `Cfg::default()` through a trait impl stay unresolved. A call is left unresolved, rather than bound to a module item, when something the parser does not record may answer it instead: it sits in a `mod` declared inside a block such as a function body (unless its path starts with `crate::`), an enclosing block declares its first path segment (a nested `fn`, `mod` or other item, an `extern crate`, or a `use`, glob included), an enclosing block holds a statement macro call other than a std expression macro such as `println!` (a std macro name redefined by a macro in the same file counts; one redefined in another file, for example through `#[macro_use]`, is not detected), or, for a bare name, a parameter or pattern binding (struct-pattern shorthand `P { f }` included) of an enclosing function, closure, `let`, `for`, `match` arm, `if let` or `while let` uses that name. These scopes are over-approximated, so some calls the compiler would bind to the module item also stay unresolved. `treesitter:rust:v4` reparses unchanged Rust files.

Rust: an item declared at a file's top level is no longer treated as a member of every inline module in that file. `m::x::f()` now resolves through x's own items and re-exports. Inline modules in crate roots resolve from other files. `use super::*` (and bare `self`/`crate` paths) now import the named module, so test modules see their parent's items through the glob.

The CGO Python parser now records functions and classes defined under compound statements (`if`/`else`, `try`/`except`/`finally`, `with`, `for`, `while`, `match`/`case`), and the calls inside them; it previously skipped them. `treesitter:python:v3` reparses unchanged Python files.
Python bare calls no longer bind to a function nested in another function or a class body, which the call cannot see, and `from mod import f` no longer binds mod's fallback `def f()` when mod also imports `f` (or uses `import *`) at module level.

Scope-model documentation now covers Go, Rust, C/C++, Java, Kotlin, TypeScript/JavaScript, Python, C#, PHP and Swift alongside Ruby (`docs/scope-models.md`). Resolver behavior is unchanged.

Rust: explicit imports now take precedence over glob imports for the first segment of qualified calls (`use b::S; S::new()`), and when two imports bring in the same name, only the one that is a function is used as the call target. A call that resolves to a non-function item in a module that also has glob imports is left unresolved instead of binding to the type; re-export walks stay inside the caller's crate root.

TypeScript and JavaScript calls now bind only callable values. A type alias or interface no longer answers a call, whether it was found in the same file, through an import, or as a namespace member. `export ... from` re-exports no longer act as bindings in their own module, so a same-file `ns.bar()` or `foo()` through `export * as ns from` or `export { foo } from` stays unresolved, as does `ns.bar()` when `bar` is itself a re-exported namespace. Same-file calls to a function that is default-exported or listed in `export { … }` now bind the declaration.
A class merged with a same-named interface is called as the class.

Java calls in files with a package declaration now bind to the caller's own class: unqualified and `this.` calls, and private static methods called through their own class name, previously stayed unresolved. A single-static-import no longer binds a call that a method of the calling or an enclosing class shadows, and a private constructor of a same-named class in another package is no longer bound. Bare and `this.` calls inside anonymous classes, enum-constant bodies and local classes stay unresolved, because those classes' own members are not modelled. Inherited members remain unresolved; a static import no longer fills that gap (see the `treesitter:java:v11` entry above). `treesitter:java:v5` reparses unchanged Java files on the next complete update.

Performance: dot-suffix resolution no longer builds candidate groups for dotted names that only ordinary Ruby calls carry, since Ruby owns those calls and no generic strategy may bind them. On the Rails repository a full index dropped from about 250 s to about 77 s and a 5-file incremental update from about 200 s to about 31 s, with an identical graph.

The non-CGO Python parser no longer records keyword syntax as calls: `with x as (a, b)`
called `as`, a match statement called `match`, and a case pattern such as
`case Point(x=0)` called `Point`. Match subjects, case guards and ordinary calls named
`match` or `case` are kept. `python-regex:python:v3` reparses unchanged Python files.
Case guards and same-line case bodies keep their calls.

Rust: a bare call in a module with glob imports (`use a::*;`) no longer binds the glob's item when the module declares its own function of that name or explicitly imports one (`use b::f;`), in either declaration order. Glob imports now lose only to items in the same namespace, as in Rust. An explicit import or own item that may not be a value (a struct, whose tuple or braced form is not recorded) leaves the call unresolved rather than guessing. A trait or enum never shadows a glob-imported function. For qualified calls (`x::f()`), an own module or type shadows a glob-imported one.

TypeScript and JavaScript calls no longer bind declarations their spelling cannot name. A bare call skips same-file class methods, and a member call through a named or default import (`Foo.bar()`) no longer binds the imported symbol itself; it binds only through a re-exported namespace and otherwise stays unresolved.

Java `new` expressions now select constructors by syntactic argument and parameter counts. Commas inside nested calls, lambdas, string and char literals, annotation arguments or generic parameter types previously miscounted arity and could bind the wrong constructor; varargs constructors now accept their variable arity. Equal-arity overloads stay unresolved. The Java parser profile becomes `treesitter:java:v4`, so `codegraph update` re-parses existing Java files once.

Ruby scope documentation now states that implicit calls are recognized only in call syntax (arguments, parentheses, a block, or a `?`/`!` method name); a bare argument-less identifier is not extracted as a call because the syntax tree cannot distinguish it from a local variable.

Ruby methods defined as the only argument of `private`/`protected`/`public`, `private_class_method`/`public_class_method def self.m`, or `module_function` in a module body are now recorded with their body calls, and the wrapper call is no longer attributed to them as a call. `treesitter:ruby:v6` reparses unchanged Ruby files on the next complete update.

Python names are now read as Unicode (PEP 3131) as Python reads them. The non-CGO
parser previously dropped definitions and calls such as `café`, and could record the
ASCII tail of a name as a call (`naïve_func()` as `ve_func`). Both parsers previously
dropped Unicode import names and local bindings, so a Unicode local or parameter did
not shadow a module definition of the same name (a wrong call edge), and a nested
`def café` bound the fragment `caf`, refusing a real `caf()` import edge.
`treesitter:python:v2` and `python-regex:python:v2` reparse unchanged Python files on
the next complete update.

C++ calls the C++ evidence pass owns (`ns::foo()`, `A::foo()`, macro-unexpanded calls) no longer bind through generic name lookup on incremental updates when a fresh index refuses them.

Ruby scope documentation now states supported static resolution and deferred runtime behavior, including the separation of `require` metadata and Rails/Zeitwerk conventions from scope evidence. Resolver behavior is unchanged.

The clean v2 baseline replaces all 44 development migrations and removes one-time
resolver/reparse upgrades. Older development indexes are refused before mutation
with `codegraph index <repo-path> --rebuild` guidance. Product generation remains
`user_version=2`; parser-profile safety and current graph fingerprints remain.
Legacy v1 databases are untouched.

Symbol-target queries (callers, callees, impact, related tests, and trace) now fail
closed on ambiguous names at the selected lookup tier. Exact qualified identities
and explicit symbol IDs keep precedence; database ordering never selects a target.
Unknown callers serialize as an empty array. Full MCP tools/list grows by 141 bytes
(35 estimated tokens) to document the contract; gateway definitions are unchanged.

Incremental updates skip JVM scope-name edge scans when the repository has no
active Java or Kotlin files; mixed/JVM repositories preserve existing behavior.

Rust nested `use` groups now derive import evidence recursively from syntax nodes,
including grouped `self`, aliases and globs; malformed declarations fail closed.
`treesitter:rust:v3` reparses unchanged Rust files on the next complete update.
Multi-crate profile refresh now retains changed roots with no call edges during
bounded module discovery, keeping unchanged-file updates equivalent to fresh indexing.

PHP 8.4 property hooks and promoted setter visibility preserve class/method
ownership; conditional declarations are indexed with ambiguity refused. The
`treesitter:php:v4` profile refreshes unchanged PHP files on the next complete
index/update. Malformed signatures remain fail closed.

Heuristic Kotlin imports and C# namespaces now exclude comment/string text; duplicate
Kotlin package headers fail closed in both backends. Backticked package segments
remain unsupported. Profiles `heuristic:kotlin:v4`, `heuristic:csharp:v4`, and
`treesitter:kotlin:v11` refresh unchanged files on the next complete update.

v2 development uses separate SQLite storage: repo-local `.codegraph/codegraph.v2.sqlite`; custom/global stores use deterministic `codegraph.v2-<repo-hash>.sqlite` names. Legacy v1 `codegraph.sqlite` databases are not automatically migrated or modified; first v2 index may require a full rebuild, and v1/v2 databases can coexist safely.

C++ qualified identity now preserves namespace nesting, file-scoped anonymous namespaces, friend
function ownership at enclosing namespace scope, and explicit leading `::` call spelling. C++
qualified-name/stable-key projections changed; development databases require a fresh index. No
migration or repair marker is provided.

Java and Kotlin package identity now comes from the syntax tree (`package_declaration`,
`package_header`), so a package spelled in a comment or string no longer names the file's package,
qualified names, stable keys, or same-package bindings; an ambiguous or broken header yields no
package. Parser profiles `treesitter:java:v3`, `treesitter:kotlin:v10` and `heuristic:kotlin:v2`
make existing databases reparse those languages on the next update.

Progressive disclosure: symbol-shaped MCP results are returned at the smallest useful size by
default, and larger representations are asked for explicitly.

Query latency budget: local graph queries are measured against a deterministic ~100k-symbol
fixture, the measured bottlenecks are fixed, and the measurement is reproducible from the CLI.

Bounded task context: `context_for_task` returns real context again, ranked deterministically and
trimmed to a token budget, with an opaque cursor for the context that did not fit.

Compact bulk encoding: bulk results can be asked for as a tabular text document that states its
columns once instead of repeating a JSON key on every row.

### Added

- **`format=json|compact` on nine bulk tools.** `find_symbol`, `search_symbols`, `find_callers`,
  `find_callees`, `get_impact_radius`, `find_related_tests`, `find_dead_code`, `list_files`, and
  `trace_dependencies` accept an optional `format`. The default is `json`; newer releases may
  add metadata fields, so clients should ignore unknown additive fields. `format=compact` returns a
  `codegraph.compact/v1` document -- a version line, a tool line, and named sections of fixed
  tab-separated columns -- that avoids repeated JSON keys and is guarded by deterministic size
  and savings tests. Compact encodes `detail=card`, the default; `skeleton`,
  `excerpt`, and `full` with `format=compact` are rejected with a clear error rather than silently
  answered with cards. `get_impact_radius` keeps its graph semantics as three sections
  (`symbols`, `files`, `summary`) rather than flattening them. Escaping is exactly `\\`, `\t`,
  `\n`, `\r`, `\@`, so tabs, newlines, Unicode, empty values, leading/trailing spaces, and literal
  backslashes in paths round-trip unchanged, and output is LF-terminated on every platform.
  `search_semantic` and `graph_analytics` stay JSON-only because their row shapes are not fixed,
  and `context_for_task` stays JSON-only because its token budget is measured over the exact JSON
  document it returns.

- **`max_tokens` and `cursor` on `context_for_task`.** `max_tokens` bounds the serialized
  response (omitted or `0` uses 4000, negative is rejected, above 200000 is clamped) and is
  measured on the exact document returned -- file envelopes, counters and cursor included -- so a
  page never overshoots the budget it reports in `estimated_tokens`. (The budget covers the context
  document; the MCP `{"ok":true,"data":...}` wrapper adds ~5 estimated tokens on top.) When context is withheld the
  response carries `has_more`, `returned_symbols`, `remaining_symbols`, and an opaque
  `next_cursor`; replaying it with the same task and ranking options continues the same ranked
  stream with no duplicate and no gap. `max_tokens` may change between pages. A cursor is refused
  with a clear error when the repository, the task, a ranking option, the indexed generation
  (`scans.id`), or the ranking itself has changed since it was issued.
- **Drill-down identity in `context_for_task` results.** Every returned symbol carries
  `symbol_id` and `stable_key` alongside `qualified_name`, so any card can be passed straight to
  `find_symbol`/`find_callers` at `detail=excerpt` or `detail=full`. `context_for_task` itself
  still embeds no source, and its `signature`/`doc_summary` are bounded card-sized values.
- **Deterministic candidate ranking for `context_for_task`.** One centralized scoring model
  (relevance class, the search engine's own relevance, and a small capped file-support bonus) and
  one total order (score, class, file path, stable identity). A strong task match always outranks
  callers, callees, tests, and hub fan-in; a caller of the best seed outranks a barely relevant
  search hit; tests rank last but stay reachable. Candidates are deduplicated by stable identity
  and keep their strongest relevance label.
- `internal/tokenest`: the one deterministic token estimator (`ceil(bytes/4)`), with no external
  tokenizer and no network.
- `context_for_task` is now a `bench-queries` scenario, using the same graph-derived search term
  the other scenarios use rather than an invented task string.
- **`detail` argument** on `find_symbol`, `search_symbols`, `find_callers`, `find_callees`, and
  `get_impact_radius`, with one vocabulary shared by every tool: `card` (identity and location),
  `skeleton` (+ signature, visibility, container, doc summary, member declarations), `excerpt`
  (+ bounded source around the symbol), `full` (+ the largest bounded source window and the
  `range`/`file_id` fields the pre-P13 payload carried). All previously reachable fields remain
  reachable. An unknown level is rejected rather than
  silently downgraded.
- **`--detail card|skeleton|excerpt|full`** on `find_symbol`, `search_symbols`, `find_callers`,
  `find_callees`, and `get_impact_radius` on the CLI, with the same per-level semantics as MCP.
- Source rendering for `excerpt` and `full`: files are read from disk on demand, confined to the
  repository root (with symlinks resolved on both sides), CRLF-normalised, clamped to the file
  that actually exists, and reported rather than failed when a file is gone or its indexed range
  is stale. An excerpt longer than its ceiling, or a range clamped to a file that has shrunk, is
  marked `truncated` and reports `available_start_line`/`available_end_line`. Source read from a
  file whose size or mtime no longer matches the index carries a `source_note` instead of being
  silently misattributed.
- Explicit bounds on the expensive levels, all of them reported: a skeleton's member list is
  capped and reports `member_count`; one response looks up members for a bounded number of
  containers and renders source for a bounded number of symbols, the rest carrying a
  `skeleton_note` or `source_note`; and one symbol's source is itself capped. Traversal tools
  have no result limit of their own, so without these a single `get_impact_radius` at
  `detail=full` would read the repository. Every bound sits well above any paged request, so an
  explicit `limit` is always honoured in full.
- `--detail` and the other query flags now take effect after the positional arguments as well as
  before them. Go's flag package stopped at the first positional, so `find_symbol . Foo --exact`
  silently dropped the flag.

- **`codegraph bench-queries [PATH]`** (alias of `bench_queries`) — benchmarks the local graph
  queries against an already-indexed repository and prints a `codegraph.query_bench/v1` JSON
  report with p50/p95/max per scenario. Read-only: it opens the existing database with
  `OpenReadOnly`, never indexes, never migrates, and never writes synthetic rows. Scenario
  arguments are selected deterministically from the repository's own graph (most-called symbol,
  highest fan-out symbol, most-linked test file); a scenario the graph cannot supply a target for
  is reported as `skipped` with a reason rather than replaced by an invented query. Flags:
  `--repo-root`, `--runs`, `--warmup`, `--budget-ms`, `--fail-over-budget` (which changes the exit
  status only, after the report has been written).
- Deterministic ~100k-symbol query-latency fixture and benchmark suite in `internal/store`, plus a
  three-layer MCP benchmark (store / dispatch / dispatch+encode) in `internal/mcp`.

### Changed

- **MCP default response size.** `find_symbol`, `search_symbols`, `find_callers`, `find_callees`,
  and `get_impact_radius` now default to `detail=card`, which is 54-74% smaller than the previous
  payload on this repository. No response field became unreachable: `detail=full` returns
  everything those tools returned before. The CLI default is deliberately unchanged, because its
  JSON is script-facing.
- **`find_callers` / `find_callees`** now dedupe, order and page inside SQLite instead of
  materialising the whole neighbourhood in Go. The result set and its ordering are unchanged.
  This also removes the `IN (?, ?, …)` list that grew with fan-in: the whole caller/callee set
  used to be read into Go and bound back into a second statement, which on a hub meant tens of
  thousands of bound variables — over `SQLITE_MAX_VARIABLE_NUMBER` on drivers still using the
  historical 999-variable limit. The neighbour set now never leaves SQLite.
- **Multi-name symbol lookup** (`FindCallees`' unresolved-destination fallback) resolves its
  four-stage cascade for all names at once. The per-name stage that needs a leading-wildcard
  `LIKE` now costs one scan per call instead of one per name.
- **`architecture_overview`** computes its entry-point and hub-symbol lists by aggregating on the
  edge side rather than grouping every symbol in the repository, and derives its file/symbol/edge
  totals from the breakdowns it already computes instead of a second full count.
- **`find_dead_code`** constrains the file join to the repository, which both restores repo
  isolation on that join and lets the ordering come from indexes.
- **`architecture_overview`'s `caller_count`/`callee_count`** are now scoped to the repository
  being described. The previous `LEFT JOIN edges` had no `repo_id` predicate, so in a database
  holding several repositories the reported degrees summed across all of them.

### Added

- `DO_NOT_TRACK` and `CODEGRAPH_NO_UPDATE_CHECK` disable the GitHub Releases update check when set to a non-empty value other than `0` or `false`.

### Fixed

- **`context_for_task` returned no context at all.** Both semantic-search paths report a hit as
  `{"file": ..., "symbol": ...}`, but seed parsing read a key named `name` that neither producer
  has ever emitted, so every seed was dropped and the tool answered `{"files":null}` while
  reporting success. Hits are now parsed through one adapter that pins both producer contracts
  (and accepts the legacy `name` key, with `symbol` taking precedence), and each hit is resolved
  to a real symbol row by `(file, qualified_name)`.
- **Ambiguous caller/callee expansion for a seed.** Expansion used the seed's bare name, so a
  task that matched `billing.Renew` could be given the callers of `subscription.Renew`. Expansion
  now follows the resolved seed's `symbol_id`, and only widens to name-matched (unresolved) edges
  when that name belongs to exactly one symbol in the repository.
- Non-deterministic ordering in `HybridSearch` and `VectorSearch`: fused results were sorted by
  score alone while being collected from a map, so tied entries -- the common case -- changed
  places between runs and moved across page boundaries. Both now break ties on file path and
  qualified name.
- **Order-dependent answers in three read tools.** `detect_frameworks` listed each framework's
  evidence files in Go map order, which changes on every call; they are now sorted.
  `find_related_tests` with `files` kept whichever evidence the first-listed file produced for a
  test, so `reason` and `score` depended on argument order; it now keeps the strongest evidence
  by the single-target rule. `graph_analytics` pagerank summed floats in row-id order, so
  symbols with identical incoming contributions compared unequal in the last bit and their
  order -- and which of them made a limited page -- depended on insertion order; ranks now
  accumulate in fixed point, and such ties are broken by symbol identity. Fixed-point ranks
  differ from the float ones by far less than the printed 1e-6 (printed values are identical on
  the 100k-symbol fixture); rows that print the same rank may come back in a different, now
  identity, order.
- MCP tool errors whose message contained a double quote produced a response body the client
  could not parse (the message was spliced into a JSON literal after a `strings.Trim` that ate
  the closing escape). Error documents are now marshalled.

- **Unstable pagination in symbol search.** `search_symbols`, `find_symbol` and
  `find_symbol --exact` applied `LIMIT`/`OFFSET` with no `ORDER BY`, so which rows a page returned
  depended on storage order and consecutive pages could overlap or skip. They now use the same
  total order the rest of the symbol surface uses (`qualified_name`, `start_line`, `start_col`,
  `id`). This changes which rows a broad query returns, because previously that was arbitrary.
- **Unstable pagination in `search_semantic`.** Token-overlap search ordered by score alone, and
  weights come from a small fixed set, so ties were the rule and page boundaries fell arbitrarily.
  It now breaks ties by file path and qualified name.
- **`find_callees` variable-count limit.** The query that collects a symbol's unresolved
  destination names spliced every source symbol id into one `IN (?, ?, …)` list, which could
  exceed `SQLITE_MAX_VARIABLE_NUMBER` for an ambiguous name in a large repository. It is chunked.

### Performance

Measured on a deterministic 100k-symbol / 212k-edge fixture, p95 of 30 warm runs, same fixture
before and after:

| Query | Before | After |
|---|---|---|
| `find_callees` (300 unresolved destinations) | 8581 ms | 39.1 ms |
| `find_dead_code` (first page) | 128 ms | 0.57 ms |
| `find_callers` (hub, 18,432 distinct callers) | 236 ms | 38.2 ms |
| `architecture_overview` | 406 ms | 132 ms |
| `find_callers` (medium degree) | 42.9 ms | 14.4 ms |
| `find_callers` (low degree) | 38.2 ms | 13.4 ms |
| `search_semantic` | 35.0 ms | 24.4 ms |
| `find_callees` (hub, 4000 destinations) | 21.8 ms | 8.5 ms |

The deterministic-ordering fix costs latency where it buys correctness: `search_symbols` on a term
matching ~9% of a 100k-symbol graph went from 0.6 ms to 14.3 ms, because a page can no longer be
whatever SQLite reached first. It stays inside the 50 ms budget.

Two whole-graph operations remain over budget and are reported that way rather than narrowed:
`architecture_overview` (132 ms) aggregates every symbol and edge by definition, and
`get_impact_radius` at its default depth of 2 around a hot hub (476 ms) returns 34,564 affected
symbols — its cost tracks the size of the answer, and it has no result limit in its public
contract. Bounding either would be an API change, not an optimisation.

The `find_dead_code` figure is for the first page: the index change removes the sort, which is
what lets `LIMIT` stop the scan early, so the win depends on dead symbols appearing early in path
order. A repository whose alphabetically-first files are entirely live still scans further.

Three indexes were replaced by wider ones with the same leading columns and partiality
(migration 022), so the index count — and therefore the per-row write cost — is unchanged:
`idx_edges_repo_unresolved_name` → `idx_edges_repo_unresolved_name_src`, `idx_symbols_file_id` →
`idx_symbols_file_start`, `idx_symbol_tokens_token` → `idx_symbol_tokens_token_symbol`. Full and
incremental indexing are unchanged within noise; the database grows about 1.7%.

## v1.2.0 - 12-05-2026

C++ call graph support, DB rebuild, asset filtering, pagination correctness, and version UX improvements.

### Added

- **`codegraph index . --rebuild`** — drops the repo database before indexing; implies `--force`. Use after parser or indexer changes when stale rows must be cleared. Requires exclusive DB access (stop `codegraph serve` first if locked).
- **`codegraph version`** command and **`codegraph --version`** / **`codegraph -v`** flags — print installed version without any network calls.
- **C++ call graph:** member-pointer (`->`), member-value (`.`), static-qualified (`::`), and direct calls are now extracted as edges with structured evidence, enabling `find_callers` and `find_callees` across legacy C++ codebases.
- **Binary/asset deny-list:** the indexer now hard-skips files by extension before hashing or parsing. Skipped categories: Windows build artifacts (`.exe`, `.dll`, `.lib`, `.pdb`, `.obj`, …), archives (`.7z`, `.zip`, `.rar`), images, fonts, audio, video, game/engine formats (`.anm`, `.dff`, `.bsp`, `.dat`, `.bin`, …), and documents (`.pdf`, `.doc`, `.chm`).
- **Per-extension unknown coverage:** files landing in the `unknown` language bucket now accumulate per-extension sub-counts in `language_coverage.unknown.extensions`.
- **Versioncheck improvements:** overridable HTTP client and URL for testability, raised check timeout from 2 s to 5 s, structured `latestReleaseResult` type, comprehensive test suite.

### Changed

- **C++ qualified names** now use `Container::Name` (double-colon) instead of `Module.Container.Name` (dot). Fixes duplicated container names like `ApmMap.ApmMap::LoadWorldMap`.
- **`find_callers` / `find_callees` pagination** overhauled: all candidate IDs are collected (resolved edges + unresolved `dst_name` fallback), deduplicated, sorted once by `qualified_name / start_line / start_col / id`, then a single `offset + limit` slice is returned. Previously each path was paginated independently, producing incorrect or truncated pages when both paths had results.
- **`FindCallees` fallback lookup** is no longer N+1: distinct `dst_name` values are fetched in one query; symbol resolution is batched across all names rather than calling `lookupSymbolIDs` once per cursor row.
- **`startupVersionCheck`** is now deferred past the version flag / `version` command check, so `codegraph --version` and `codegraph version` never trigger a network round-trip.
- **`clean` command** description changed from "clean index data" to "database maintenance" to reflect its actual scope (VACUUM, FTS optimize, ANALYZE, WAL checkpoint, incremental vacuum).
- **Language coverage update** refactored into `updateLanguageCoverage` helper; unknown files now report per-extension sub-counts.
- **MCP stdio transport** simplified to newline-framed JSON (one JSON object per `\n`). Previous Content-Length header framing removed.
- README updated: build-from-source instructions, `--rebuild` usage, and `version` command documented. `version-check` references removed.

### Fixed

- Fixed `find_callers` returning empty results for valid C++ member-call references (e.g. `AgcmMinimap::F` → `pcsApmMap->LoadWorldMap` → `ApmMap::LoadWorldMap`).
- Fixed `find_callers` and `find_callees` returning wrong or duplicate results across pages when resolved and fallback result sets were independently limited and merged.
- Fixed `splitCallTarget` using a manual `strings.Cut` loop; replaced with `strings.LastIndex` for correctness on paths with multiple separators.
- Fixed C++ stable keys and qualified names including redundant container prefixes.

### Removed

- **`codegraph version-check`** command removed. Use `codegraph --version`, `codegraph -v`, or `codegraph version` for local version output. The background startup version check still runs automatically on normal commands.

### Upgrade Notes

- `--rebuild` replaces any manual "delete the DB and re-index" workflow. It is safe to run at any time on a clean working directory.
- MCP stdio protocol change (newline-framed vs Content-Length) may affect custom MCP client integrations that relied on the old framing.
- C++ unresolved-member fallback is name-based; results are included only when receiver type cannot be resolved to a symbol ID. This is best-effort for legacy codebases without full type resolution.

## v1.1.1 - 28-04-2026

Made `--repo-root` optional across all CLI commands and MCP tools. The server
now auto-detects the repository root so editor configs no longer require
hardcoded paths.

## Changes

### Features

* **Config:** Added `ResolveRepoRoot(flagValue, toolParam string)` in
  `internal/config/reporoot.go` with a 5-step fallback chain: per-call MCP
  tool parameter → CLI flag → `git rev-parse --show-toplevel` → `os.Getwd()`
  → error. Per-call tool parameter takes priority over the process-level flag.
* **CLI:** All commands that previously required `--repo-root` (`serve`,
  `watch`, `visualize`, `index`, `stats`, `query`, `doctor`, `clean`,
  `affected-tests`) now resolve the repo root through the shared helper,
  making the flag optional everywhere.
* **MCP:** `index_repo` and `update_graph` tools accept an optional `repo_root`
  parameter; when omitted, the shared resolver is used.
* **Install:** `codegraph install` no longer writes `--repo-root` into
  generated MCP config snippets for any editor (Claude Code, Cursor, Windsurf,
  Gemini CLI, Codex).

### Fixes

* Fixed macOS symlink path mismatch in `TestResolveRepoRoot_GitRootDetected`
  by normalizing paths with `filepath.EvalSymlinks` before comparison.
* Fixed incorrect resolution priority: per-call `toolParam` now correctly
  takes precedence over process-level `flagValue`.

### Docs

* Added "Repo Root Resolution" section to README.md documenting the fallback
  chain with correct priority order.
* Updated all MCP config snippets in README.md and examples/ to omit
  `--repo-root`.
* Updated `docs/ai-assistant-guidance.md`, `docs/codegraph-roadmap.md`, and
  `examples/README.md` to reflect optional `--repo-root`.

## Upgrade Notes

* No breaking changes. Existing configs with explicit `--repo-root` continue
  to work unchanged.

## v1.1.0 - 27-04-2026

A performance, correctness, and operability release focused on faster indexing, schema-backed edge resolution, streaming exports, and tighter DB lifecycle handling.

### Highlights
- **Indexing performance:** Repo-wide change detection is dramatically lighter — narrower DB projections, the existing-files load now overlaps the filesystem walk, per-row payloads are slimmed, and content-hash reads are deferred to the slow path. No-op repo updates do far less work and allocate substantially less.
- **Edge resolver scaling:** Slash-suffix, dot-tail2, and the dominant 2-dot dot-suffix paths are now schema-backed indexed joins instead of repo-wide symbol scans, removing large constant-factor and quadratic costs on bigger graphs (the 2-dot dot-suffix case is roughly three orders of magnitude faster on the large-scale benchmark). A steady-state guard short-circuits resolver work when no unresolved edges remain. Cross-file edge resolution on update runs stays path-scoped instead of going repo-wide.
- **Write-path allocations:** Symbol, edge, and batch insert SQL plus IN-placeholder strings are cached; edge source selection uses span/binary-search; residual allocations in the multi-file batch path are trimmed; tokenization allocations are reduced.
- **Streaming exports:** JSON and DOT exports stream through paged store calls. Peak memory is now O(page) instead of O(repo) for the unbounded paths, and the bounded-page CLI uses the same primitives.
- **Watch & update correctness:** Repo include/exclude is applied consistently; the dirty-file queue is crash-safe via claim/delete; chmod-only and directory-create events are ignored; `--jsonl` output is stable across `index`, `watch`, and `doctor`.
- **DB lifecycle hardening:** Per-version migrations run on a single connection under `BEGIN IMMEDIATE`; pragmas apply across the pool with a unified driver-aware busy-retry policy; `doctor` uses the same DSN/pragma path; `clean` and `doctor` add ANALYZE, WAL checkpoint, incremental vacuum, and a `--deep` integrity check.
- **Query correctness fixes:** `RelatedTests(file)` is correctly scoped via `target_file_id`; symbol lookup is deterministic; deleted-file graph rows are purged and cross-file references nullified, including ghost `test_links` rows pointing at deleted files; duplicate token-stat counting in batched writes is fixed; `FindDeadCode` is faster via dedicated indexes.
- **Operability:** New `index_smoke` runner produces compact perf-diff output; repo artifacts default to `.codegraph/` with legacy fallback; the CLI gains a command registry, per-command help, and canonical query command names with backward-compatible aliases; benchmarks capture `--sqlite-profile` and host context for reproducible perf comparisons.

## v1.0.9 - 16-04-2026

Improved Node.js repo indexing stability by hard-skipping common generated/tooling directories (for example `node_modules` and `.next`), refining default excludes, and clarifying ignore override behavior.

### Changed
- **indexer:** Enforce strict early skips for common Node.js generated directories; hardcoded skips are non-overridable via `.codegraphignore` negations. (#8)
- **config:** Centralize default exclude patterns to keep CLI/indexer behavior consistent. (#8)

### Fixed
- **sql:** Add explicit bounds/safety checks for path-filtering queries. (#8)
- **build/release:** Carry through `CGO_ENABLED=0` + tree-sitter cross-compilation fixes and release diagnostics. (#4, #5, #6)

### Docs
- **readme/changelog:** Update Node.js support status and release notes. (#8)

## v1.0.8 - 27-03-2026

### Fixed
- **build:** Restore `CGO_ENABLED=0` cross-compilation by splitting tree-sitter adapters behind `//go:build cgo` and using heuristic parsers in no-cgo builds. (#5)

### Changed
- **ci:** Add release build diagnostics to improve cross-platform release debugging. (ci/workflow)

## v1.0.7 - 27-03-2026

### Fixed
- **mcp:** Tighten MCP protocol compliance for stricter clients. (#3)

### Docs
- Add `v1.0.6` changelog entry. (docs)

## v1.0.6 - 26-03-2026

### Fixed
- **mcp:** Stop sending JSON-RPC responses to notifications; fix JSON Schema `required` handling; remove non-standard fields; route unhandled-method logging via configured stderr writer. (mcp)

### Changed
- `NewServer` accepts an `io.Writer` for error output, giving callers control over diagnostic logging. (mcp)

### Docs
- Add Claude Code MCP setup examples; add missing tools to MCP docs list; add short tool descriptions. (readme)

## v1.0.5 - 21-03-2026

### Fixed
- Default to a repo-local SQLite DB (while continuing to recognize legacy locations) and exclude repo DB artifacts from indexing. (config/store)

### Changed
- Treat prior global `db_dir` default as legacy so existing installs fall forward safely. (config)

## v1.0.4 - 18-03-2026

### Docs
- README update to include graph/export usage. (docs)

## v1.0.3 - 18-03-2026

### Added
- **cli:** `watch`, `benchmark`, `config init`, `clean`, `doctor --fix`, and `--jsonl` output for long-running/indexing workflows. (cli)
- **mcp/query:** Query commands + tools, including offset pagination and supported-languages introspection. (mcp)
- **parser:** Heuristic adapters for major languages plus a Python adapter. (parser)
- **export:** Include symbols + edges in graph exports; support export streaming. (export)

### Changed
- **indexer/scan:** `.codegraphignore` negation patterns; per-language scan coverage; best-effort parse policy; batched metadata writes and scoped edge resolution. (indexer)
- **performance:** Parallelize indexing and reduce allocation/IO overhead; improve watcher flush/coalescing; add scan phase timings; SQLite/store tuning. (perf)

### Notes
- **licensing:** Relicensed under FSL-1.1 to prevent commercial reselling. (license)

## v1.0.2 - 18-03-2026

### Fixed
- Installation hardening + README updates to unblock `go install` workflows. (install/docs)

## v1.0.1 - 18-03-2026

### Fixed
- Correct Go module path to `github.com/isink17/codegraph`; align imports and install docs accordingly. (install/docs)

## v1.0.0 - 18-03-2026

Initial public release of `codegraph`.
