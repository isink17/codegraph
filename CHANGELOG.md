# Changelog

## Unreleased

The resolver's own-module, Go local-qualifier and dotted broad-ambiguity vetoes are now applied by the Go binder from the shared rule inventory, each with a SQL/Go parity test; graphs are unchanged. A read-only internal check reports, for an unresolved edge, which inventory rules refuse it (not yet exposed).

Index and update now record file-level Git history for the latest 250 first-parent commits reachable from `HEAD`: per file, commit count, first and last commit, mailmap-aware author count and top author, `--numstat` line churn, explicit Git reverts, and whether the working tree differs from that commit. Renames are followed inside the window. History is enrichment only and never changes the semantic graph. A missing or failing Git, a non-repository and a shallow clone are reported as distinct states and never fail the scan. Read it with the new `file_history` MCP tool (not in `tools/list`; found through `tool_search`) or `codegraph file_history`; `--no-history` skips it. Schema migration 2 adds the history tables; an earlier v2 build refuses a database this build has opened. See `docs/git-history.md`.

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

Java calls in files with a package declaration now bind to the caller's own class: unqualified and `this.` calls, and private static methods called through their own class name, previously stayed unresolved. A single-static-import no longer binds a call that a method of the calling or an enclosing class shadows, and a private constructor of a same-named class in another package is no longer bound. Bare and `this.` calls inside anonymous classes, enum-constant bodies and local classes stay unresolved, because those classes' own members are not modelled. Inherited members remain unresolved. `treesitter:java:v5` reparses unchanged Java files on the next complete update.

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
