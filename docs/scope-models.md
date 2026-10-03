# Language scope models

CodeGraph resolves a call edge only when it has repository evidence that names exactly one destination. Each language has its own partial static model. A model records the facts its parser proves, and the language-specific resolution owns the call spellings those facts govern. When an owned edge fails to bind, it stays unresolved; it is never handed to repository-wide name matching instead. Spellings a language does not own go to the generic strategies (exact qualified name, bare name, receiver and suffix matching). Those strategies bind only candidates in the caller's own language, and only when exactly one candidate survives at their evidence level. Sharing a name across languages is never evidence. The separate `cross_language_ref` edge kind is created only from an import bridge: an import in one file resolves to exactly one active file in another language, and no same-language file answers it. Those edges are low confidence. Language-specific interop paths are listed under each language. A name that a broader level already found ambiguous is never resurrected by a narrower one. A production caller never binds a test-only definition.

Missing, conflicting, unsupported and ambiguous facts leave an edge unresolved. These models describe what CodeGraph proves from source text. They are not compilers or runtimes, so a resolved edge says that the repository's static evidence names that destination, not that a particular build or execution reaches it.

Every resolution entrypoint applies the same model: full indexing, path-scoped updates, name-targeted updates and their combination. The repository-wide resolver enforces each rule in SQL and the incremental binder enforces it in Go. Where an ownership rule depends only on the edge itself, its SQL and Go forms are tested against each other. Fresh and incremental indexing of the same tree are tested to produce the same bindings.

Release archives are built with CGO and include the tree-sitter parsers. In an explicit `CGO_ENABLED=0` build, Go (via `go/ast`) and Python (via the pure fallback parser) still produce call edges. The other languages keep heuristic symbol and import navigation but produce no call edges, so none of the call-resolution behaviour below applies to them in that build. CodeGraph will not update a tree-sitter graph using one of those heuristic parsers, because the update would delete its call edges.

## Go

Go has two parsers: tree-sitter, and the `go/ast` adapter used in non-CGO builds. Both record:
- each file's package name and import paths
- package-level functions, types, values and methods, qualified as `pkg.Name` or `pkg.Receiver.Name`
- inside function bodies, the locals each block binds (receivers, parameters, named results, `var`/`const`, `:=`, range and type-switch variables), with that block's line range

A local's type is recorded only when the syntax states it: a declared type, or a `T{}` / `&T{}` literal in a one-to-one assignment. A call qualifier that is a local bound at that line is kept as written. Only a qualifier that is not a local and matches an import is rewritten to the import path. Import aliases and local qualifiers are separate facts.

Three call shapes belong to Go-specific passes. Generic repository-wide matching never answers them, even when they stay unresolved:
- **Bare calls.** `F()` binds only to the single package-level declaration of that name in the caller's own package. A package is identified by its directory plus its package name, so `foo` and `foo_test` are different packages. Methods are never bare targets. Repository-wide uniqueness, export status and builtin names are not evidence.
- **Import-path calls.** A call through an import path that maps to a `go.mod` inside the repository binds only to the single non-test package-level function or type in that exact directory. The longest module path wins. A malformed nested `go.mod` blocks its subtree.
- **Local selectors.** `x.M()` with a local qualifier binds only when all of these hold: the innermost binding has a stated type; that type's package is proven (the caller's package, or an import mapped into the repository); and that type has exactly one method `M`. A local with no stated type stays unresolved; it is never treated as a package name.

Test-file declarations answer only test callers in their own package. Other spellings go to the generic same-language strategies.

These stay unresolved:
- bare calls into dot-imported packages
- field chains such as `s.db.Query`

Not modelled:
- method calls through package-level variables reached via an import
- return-type, field-type and dataflow inference
- embedded and promoted methods
- interface dispatch
- method-set filtering

Only the `module` line of on-disk `go.mod` files is read. The toolchain, `replace` directives and modules outside the repository are not consulted.

An incremental update re-decides the changed files' edges and every edge naming a declaration the batch added or removed. A binding whose name is now declared more than once is cleared and re-decided. Both parsers build call graphs and are tested to emit the same call spellings, locals, scope ranges and stated types. One difference: `go/ast` rejects a file with syntax errors, while tree-sitter still extracts from its recovered tree. Switching between the two builds changes the parser profile, so Go files are parsed again.

## Rust

The tree-sitter Rust parser records:
- a provisional module path from each file's location
- `fn`, `struct`, `enum` and `trait` items, plus `impl` functions under the impl's type as written
- item visibility
- `use` trees, with their owner module, alias, glob and `pub use` flags
- `mod` declarations, inline or as candidate file paths

Malformed `use` declarations are skipped.

Only the Rust module pass resolves Rust calls, and generic repository-wide matching never sees a Rust edge. Crate membership comes from primary evidence only:
- Any `lib.rs` or `main.rs` is a crate root, identified by its full stored path. A matching basename never makes two roots the same crate.
- Membership spreads through `mod` declarations to exactly one of `name.rs` or `name/mod.rs`.
- Two candidate files, or a file claimed by two crates, prove nothing.
- Stored membership is a cache, not proof. A file whose `mod` declaration disappears loses its membership.
- Finding roots by directory prefix only decides which rows to load.
- Module paths are keyed by crate, so a symbol in one crate is never a candidate for a caller in another.

Lookup starts from the calling symbol's container, or otherwise from the file's module:
1. A `use` with a matching local name takes precedence.
2. Otherwise, `crate::`, `self::`, `super::` and relative paths resolve against the starting module.
3. A candidate must be visible, and either in the caller's file or in a module proven to belong to the caller's crate.
4. If none qualifies, named and glob `pub use` re-exports are followed, with cycle protection.

Exactly one candidate binds. Known defect: if a glob import supplies an eligible candidate for a bare name, it currently wins over the module's own item of that name. Rust gives the own item precedence.

Visibility works as follows:
- `pub` items are always eligible.
- `pub(crate)` items are eligible within the crate.
- `pub(super)` items are eligible from the parent module and its descendants.
- Private items, `pub(self)` and `pub(in path)` never bind, even from the same file.

These stay unresolved:
- method calls (`x.m()`)
- `Self::` paths, and bare calls inside `impl` method bodies
- trait methods and trait dispatch
- items and `use` declarations inside function bodies
- cross-file paths into inline `mod` blocks
- paths into other crates or dependencies

Macro invocations produce no call edges. `#[path]`, `cfg` and Cargo manifests are not read.

An incremental update maps changed files to the crates they affect, clears every Rust binding in those crates, and re-decides all of their edges. Tests pin the result to a fresh index across added and removed `use`, `mod` and `pub use` declarations, aliases, renames, moves and visibility changes.

## C and C++

`.c`, `.h`, `.cc`, `.cpp`, `.cxx`, `.hpp`, `.hh`, `.hxx` and `.ipp` files are all indexed as one language, `cpp`. The tree-sitter parser uses the C grammar for `.c` files and the C++ grammar for the rest.

It records:
- function definitions, and separate declaration rows from headers and class bodies, each with a canonical parameter signature and any `static` or `extern "C"` linkage
- classes, structs, unions and enums with namespace-qualified names
- friend functions, owned by the enclosing namespace
- `#include` paths and call spellings

No build configuration is known, so declarations in every preprocessor arm are recorded.

C/C++ resolution owns every call spelled bare or with `::`, and every call written inside a macro invocation's arguments. Generic repository-wide name matching never answers these, even when they stay unresolved, on every entrypoint. Receiver calls (`p.f()`, `p->f()`) go to the generic strategies. A variable receiver is not type evidence, so a unique member name alone does not bind them, and `this->f()` stays unresolved.

A bare call binds a target only under these conditions:
- The target is in the caller's file, or in a file the caller reaches through a project `#include` of the target's definition or a matching declaration.
- A class member also requires the caller to belong to that same class declaration. An include does not make the includer a member, and two classes with the same name are not the same class.
- A namespace-level target must be in exactly the caller's namespace. There is no outward search to enclosing or global scope.
- A visible friend function defined inside a class with the same name refuses the call.

A qualified call (`ns::f()`, `::f()`) matches the exact qualified identity, with the same file and include visibility.

In both cases exactly one eligible candidate must remain, and every declaration and definition of that identity must share one signature, so overloads are refused. `static` functions in other files, enums, and test-only definitions called from production code are never targets.

Not modelled:
- macro expansion
- inheritance (a bare call to an inherited base member stays unresolved)
- argument-dependent lookup
- choosing an overload by argument type
- template instantiation
- `using` and namespace aliases

A class-qualified call such as `A::foo()` stays unresolved when the static member's body is written inside the class definition in another file. It binds when the header only declares the member and the definition is out of line.

## Java

The tree-sitter Java parser records:
- the package from a single `package` declaration (a missing, repeated or malformed one records no package)
- single-type, on-demand and static imports
- top-level and nested types
- methods and constructors, with their visibility and `static` modifiers
- the argument count of each call

Every Java file gets a persisted scope record. The Java resolver owns every call and `new` expression in such a file. A call it refuses stays unresolved; generic matching never fills it in.

A dotted qualifier must match exactly one fully qualified type. For a simple type name, a matching single-type import decides on its own. Otherwise, same-package types, member types of the caller's class and on-demand imports compete, and two visible candidates leave the call unresolved. That includes a same-package type that collides with an on-demand-imported type.

Calls bind as follows:
- `Type.m()` binds only a unique, visible static method. Overloads are not chosen between.
- `new T(...)` binds a unique Java constructor with the call's argument count. An implicit default constructor is not a target.
  Known defect: argument and parameter counts are currently read from source text. Commas inside nested calls or generic parameter types are counted too, which can select the wrong constructor.
- An unqualified call can bind through a static import.

Visibility is checked as follows:
- Private targets are visible only from their owner.
- Package-private and protected targets are visible only within the package.
- Access through subclassing is not modelled.

The following stay unresolved:
- In a file that declares a package, unqualified and `this.` calls to the caller's own methods are not currently bound.
- `super.` calls.
- Variable, field and expression receivers.
- Inheritance and interface dispatch.
- Generic type arguments.

Java reaches Kotlin only through the JVM shapes listed under Kotlin. Incremental updates apply the same rules when declarations, imports or peer files change.

## Kotlin

The tree-sitter Kotlin parser records:
- the package header
- imports, including `as` aliases and wildcards
- classes, objects, interfaces, companion objects and functions, with their visibility modifiers

For `.kt` files whose preamble parses cleanly, it also records JVM evidence:
- the file facade class (`FileKt`, or a proven `@file:JvmName`) and `@JvmMultifileClass`
- per-function `@JvmName`
- Java-visible arity from defaults and `@JvmOverloads`

Every Kotlin file gets a persisted scope record, and the Kotlin resolver owns every call in it, including calls it refuses.

An unqualified call that names a visible, constructible top-level Java type binds to that type as a constructor call, before the function pool is consulted. Otherwise Kotlin lookup does not rank scopes: these candidates compete as one pool:
- same-package top-level functions
- members of the caller's container
- explicitly imported, aliased and wildcard-imported functions

Exactly one candidate binds. Any competitor leaves the call unresolved, including an explicit import against a same-package function. Extension functions are excluded.

For `Q.m()`, `Q` must resolve the same way to a Kotlin class, interface, object or enum, or to a top-level Java type. `m` must then be the unique member declared directly on that type; for a Java owner it must be a unique visible static method.

Visibility is modelled as follows:
- `private` means the same file.
- `protected` means the same file or the same container.
- `internal` is treated as visible, because module boundaries are not modelled.

Java reaches Kotlin only through these modelled JVM shapes:
- file facades (when several files share one, every file must be `@JvmMultifileClass`)
- `@JvmStatic` object members, and `Obj.INSTANCE.f()` otherwise
- companion members through `Outer.Companion.f()`, or `Outer.f()` when `@JvmStatic`
- explicit static imports of those owners

The JVM target must meet all of these:
- It is public.
- It is not an extension, `suspend`, `expect`, reified or `@JvmSynthetic` function.
- A call with arguments needs a proven fixed arity.
- A known `@JvmName` literal binds under the JVM name. An unprovable rename refuses.

Java `new` of a Kotlin class is not bound. Also not modelled:
- Kotlin class constructor calls
- `this.` calls
- companion members reached through the class name
- value and expression receivers
- inheritance and extension dispatch
- typealiases, default imports and builtins

Incremental updates follow the fresh-index rules, including facade renames and peer additions and removals.

## TypeScript and JavaScript

`.ts`, `.tsx`, `.js`, `.jsx` and `.mjs` files are indexed as one language, `typescript`.

The tree-sitter parser records:
- functions, classes, class methods, interfaces, type aliases, and `const`/`let`/`var` declarations whose value is a function, each marked exported or not
- `import` statements (named, default, namespace, side-effect and `import type`)
- `export … from`, `export *`, `export * as ns` and local `export { … }`

A call is recorded only when its callee is a plain name or a chain of plain property accesses. `require()` strings are import metadata only.

TypeScript resolution owns every TypeScript and JavaScript call edge. A call it cannot decide stays unresolved. Bindings work as follows:
- A bare call binds through a non-type-only import of that local name.
- With no import binding for the name, a bare call binds only to a unique same-file symbol of that name.
- `ns.f()` binds through `import * as ns`, or through an imported `export * as ns`.
- Every binding needs exactly one target symbol.

Known defects:
- The same-file fallback also considers class methods, so a bare `run()` can bind a method `run`.
- A member call whose head is a named or default import (`Foo.bar()`, `Foo.a.b()`) currently binds to the imported symbol itself rather than to the member.

Only relative specifiers inside the repository are followed. Package names, paths that leave the repository, trailing-slash specifiers and directory `index` files are not resolved. An extensionless specifier names `x.ts` or `x.tsx`, and having both makes it ambiguous.

Explicit runtime spellings are tried in order, and the first file that exists wins even if it lacks the name:
- `./x.js` tries `x.ts`, `x.tsx`, `x.js`, `x.jsx`
- `./x.jsx` tries `x.tsx`, `x.jsx`

Other spellings name only themselves, and a `.d.ts` file is never selected through a `.js` or `.jsx` spelling.

Inside a module, lookup goes in this order:
1. explicit re-exports and local `export { … }`
2. the module's own exported declarations
3. `export *`

A `default` export is never taken through `export *`, and conflicting re-exports stay unresolved.

`import type` is not evidence for an implementation call, so it never binds a call. It still counts as binding the name, so the call does not fall back to a same-file declaration. Side-effect imports bind nothing.

Not modelled:
- CommonJS `require` / `module.exports`
- package resolution
- `paths` / `baseUrl` aliases
- member calls deeper than one level through a namespace import

When a file changes, CodeGraph re-decides the TypeScript call edges of that file and of every file that depends on it through relative imports, following runtime spellings and re-export chains.

## Python

Both Python parsers, tree-sitter and the pure fallback, read PEP 3131 Unicode identifiers. They record:
- classes, functions and methods with their lexical nesting
- `import` / `from … import` statements, with the scope they were written in
- what each scope binds locally: parameters; assignment, loop, `with`/`except` and walrus targets; and nested `def`/`class` names inside functions

Imports written inside a class body are not recorded.

Python resolution claims only calls whose leading name the calling scope already gives a meaning, through a visible import or a local binding. A claimed call binds or stays unresolved; the generic strategies cannot override it. A bare call with no such evidence goes to the generic strategies. Those strategies bind a class in another file only when the caller declares it or imports the declaring file.

Module identity is a repository path, never a basename:
- `pkg.mod` names `pkg/mod.py` or `pkg/mod/__init__.py`; if both exist, the import stays unresolved.
- Absolute imports are anchored at the repository root.
- Relative imports are anchored at the importing file's package.
- `sys.path`, `src/` layouts and packaging configuration are not read.

Lookup works as follows:
- The nearest visible import wins, and within one scope the longest matching spelling wins.
- Two different imports at the same distance leave the call unresolved.
- A local binding at least as near as the import shadows it, and the call stays unresolved.
- A function-local import never binds a call; it only blocks other bindings.
- A module-level import binds when the target module declares the name exactly once at module level.
- For `from pkg import helpers` followed by `helpers.load()`, the `pkg.helpers` submodule is tried unless `pkg/__init__.py` binds `helpers` itself or has a module-level `from … import *`.
- A bare call with no import and no local binding binds to a unique module-level `def` in its own file.

Python has no export control, and leading underscores do not affect binding. The following remain unresolved:
- duplicate declarations
- competing imports
- wildcard imports
- calls more than one member deep

Names that a package's `__init__.py` re-exports from another module are not followed.

On a change, these Python call edges are re-decided:
- the changed file and its direct importers
- for a changed `__init__.py`, every Python file under that package
- the bare class bindings of files that import a changed file
- after a deletion, every such binding in the repository, checked against the current imports

The non-CGO fallback parser extracts calls only inside functions; tree-sitter also extracts module-level calls.

## C#

The tree-sitter C# parser records:
- block and file-scoped namespaces, and nested type identities
- declared visibility; with no modifier, a member is private inside a type and internal at the top level
- method staticness and parameter-count ranges
- the argument count of a call, when it is safe to read
- using directives with their kind (namespace, alias, static, global) and the namespace they are declared in
- parameters, locals, `foreach` and `catch` variables, local functions, fields and properties, with their type when it is a plain or dotted name

The C# resolver owns every C# call edge. A call it cannot prove stays unresolved.

A bare call looks first at the caller's own type, and stops there if that type declares the name. A static caller, or one whose staticness is unknown, sees only static methods. Otherwise only `using static` types are tried. `this.M()` binds only instance methods of the caller's type.

A type qualifier is resolved through these levels, in order:
1. `global::`
2. a using alias that spells the whole qualifier
3. the enclosing namespaces, innermost first
4. `using` namespaces
5. the qualifier as written

The first level that applies decides, and the result must be exactly one accessible type. A matching alias decides even when it names no type.

Receivers follow these rules:
- A receiver variable, field or property needs exactly one recorded type, and binds only instance methods declared on that type.
- A local name that shadows the call, an untyped receiver, or competing overloads leave the call unresolved.
- Arity is the last filter. When the call's argument count is known and any candidate's arity is unknown, nothing binds.

Another type's method, and every type enclosing it, must be `public`. Internal and protected access is not modelled. Global usings are ignored.

Not modelled:
- inheritance
- `base.` calls
- interface and extension-method dispatch
- `?.`
- generic and named arguments, except as an arity-safety check

Calls bind only to C# declarations. Fresh and incremental indexing are tested to produce the same edges.

## PHP

The tree-sitter PHP parser records:
- braced and semicolon namespaces
- namespaced type and function identities
- method visibility (public when unwritten) and staticness
- `use` imports by kind (type, function, const), in the exact namespace they apply to
- trait declarations
- properties, including constructor-promoted ones; a property has a type only when it is non-static and declared with a named type

Call spellings keep their syntax (`Foo::run`, `\Foo\run`, `namespace\Foo::run`, `self::`, `static::`, `parent::`, `$x->run`, `$x?->run`). `$this->` inside closures, arrow functions and nested functions is marked.

The PHP resolver owns every `::` and `->` call. An owned call it cannot prove stays unresolved. Bare and namespaced function calls (`run()`, `\App\run()`) are not owned and go to the generic strategies. Those strategies never treat a PHP method as a candidate.

The caller's namespace comes from its own declaration. The type is decided before the method, by the first rule that matches the spelling:
1. a leading `\`
2. `namespace\`
3. a `use` alias for a type in the caller's exact namespace
4. otherwise, the caller's namespace plus the spelling

There is no parent-namespace walk and no global fallback, and a missing method never sends the call on to another rule. An alias that matches only when case is ignored leaves the call unresolved.

Calls bind as follows:
- `self::` and `Type::` bind only static methods.
- `$this->m()` binds only from an instance method, and not from inside a trait or closure.
- `$this->prop->m()` needs exactly one typed declaration of the property.
- Another type's method must be `public`.
- When one name has several type declarations, the root `composer.json` `autoload.psr-4` mapping may pick one. Otherwise the call stays unresolved.

Not modelled:
- `static::` and `parent::`
- inheritance and traits
- `$var->` receivers and `?->`
- dynamic names

Calls bind only to PHP declarations. Fresh and incremental indexing are tested to produce the same edges, and a changed Composer manifest updates the edges that used it.

## Swift

The tree-sitter Swift parser records:
- classes, structs, enums, protocols and actors with dotted qualified names
- functions and initializers, with their argument-label selector, `static`/`class` modifier, arity, and `final`, `override` and dispatch facts
- stored properties and enum cases
- extension membership and module imports
- inheritance clauses, classified as superclass or conformance only when the parent is declared in the same file, and unproven otherwise
- local bindings that block a name (parameters, generic parameters, local functions and types, typealiases, pattern bindings)
- every call, with its form (bare, member, `self.`, `Self.`, `super.`, optional, forced, chained, initializer), its labels and argument count

Calls inside nested local functions are not recorded.

Swift resolution owns every recorded Swift call, including forms it does not support. Generic repository-wide matching never answers them, so bare, value-member, type-member, optional, forced and chained calls stay unresolved. Each pass needs a recorded calling function and binds only a target in the caller's file.

Calls bind as follows:
- `T(...)` binds when all of these hold: `T` names exactly one non-private struct, enum or actor in the file; no local binding shadows it; and exactly one non-private `init` has the same labels and a fixed arity. Class initializers are not bound.
- In a struct, enum or actor, `self.m()` and `Self.m()` bind a unique method of that exact type with matching staticness and labels.
- In a class, `self.m()` and `Self.m()` bind only where overriding is impossible: in a `final` class, or to a `final`, `static` or `final class` method. They bind directly when the class has no recorded inheritance or conformance, or through same-file proven superclasses.
- `super.m()` walks the same-file proven superclasses and stops at the first ancestor with a compatible member.

Any of the following refuses the edge, with no fallback:
- a same-named property or enum case
- a second compatible candidate
- an unproven, generic or constrained relation
- an unrecognized call shape

Literal targets in `Package.swift` map files to packages and modules; dynamically built manifests map nothing. Module, import and access-level facts only exclude competitors; they never admit a target in another file. Test declarations are visible only to tests.

Not modelled:
- calls through protocols
- implicit-self calls
- choosing an overload by argument type
- synthesized initializers

A Swift call never binds a symbol in another language. A change to any Swift file re-decides the repository's `self`, `Self`, initializer and `super` bindings, and a `Package.swift` change triggers a full scan.

## Ruby

See [Ruby scope and limitations](ruby-scope.md).
