package store

import (
	"context"
	"sort"
	"strings"
)

// Bare C/C++ calls and class members (P22.15 closeout).
//
// P22.15 made the C++ extractor complete: declarations hidden behind
// `preproc_if`, `template_declaration`, `linkage_specification` and `ERROR`
// wrappers became symbols, and `gtest_unittest.cc` went from 15 symbols to 729.
// That is a parser win, and it exposed a resolver defect the extraction gap had
// been masking:
//
//	`exact_name` matches `symbols.name` for every callable kind, so a bare
//	`close()` bound `ostream::close` and a bare `impl()` bound
//	`PolymorphicMatcher::impl` -- neither call site named a class at all.
//
// Measured on fmt + googletest: bare-call bindings whose target is a class
// member went from 66 to 704 once the members behind the wrappers became
// visible (608 same-file, 96 cross-file), and a hand audit of the 96 cross-file
// ones found 60 source-WRONG (`impl` -> `PolymorphicMatcher::impl` x28,
// `GTEST_DISABLE_MSC_WARNINGS_POP_` -> `UniversalPrinter` x9, `Elements` -> the
// wrong matcher class x11, `ExpectSpecProperty` x4, `Join` x2, `days` x3,
// `close` x2, `rhs` x1). So completeness increased the active false-positive
// population, and the phase is not shippable until this class is closed.
//
// The rule:
//
//	In C and C++, a call spelled with NO receiver and NO qualifier may bind a
//	CLASS MEMBER only when CodeGraph can prove the calling symbol is a member
//	of that same class.
//
// What is deliberately NOT evidence, each of them a fixture:
//
//   - Repository-global uniqueness of the spelling. `B::foo` being the only
//     `foo` in the repository says nothing about whether a caller in `A` can
//     name it. This is the same non-evidence P22.6 removed for Go and P22.9
//     removed for types.
//   - The same FILE. P22.13 trusts same-file as FILE visibility, and that is
//     correct for a free function; it is not class visibility. Two classes in
//     one header see each other's *names*, not each other's members.
//   - An `#include`. Including the header that declares `class A` makes `A`
//     visible; it does not put the includer inside `A`.
//
// And what the rule does NOT try to model, because the graph holds no fact for
// it -- each is a recall loss stated rather than guessed at:
//
//   - Inheritance. `class Derived : Base { void f() { inherited(); } }` is
//     source-correct and stays unresolved: CodeGraph models no base-class edge,
//     so admitting it would mean admitting every same-name member anywhere.
//     This is where the 36 source-correct cross-file bindings went
//     (`GetParam` -> `WithParamInterface::GetParam`, inherited by every
//     `TEST_P` fixture).
//   - An out-of-line definition whose class is declared in ANOTHER file
//     (`void A::caller()` in `a.cc`, `class A` in `a.h`). The same-file class
//     declaration is the fact; an include is not, per the paragraph above.
//     Note which side that abstention is safe on: as a CALLER it costs recall,
//     and as a TARGET it would cost precision -- but an out-of-line definition's
//     `symbols.name` is `A::foo`, which no bare spelling matches, so no bare call
//     can choose one. That is load-bearing rather than lucky, and it stops holding
//     the moment P22.17 emits header declarations or `field_declaration` members
//     with a bare `name` whose class lives in another file.
//   - Namespaces, leading `::`, friend lookup, ADL, overload resolution: all
//     P22.16, none of them started here.
//
// The out-of-line form IS recognised, and how matters. The adapter stores the
// declarator text verbatim in `symbols.name`, so an out-of-line definition is
// named `A::caller` -- but that string cannot be split on `::` to recover `A`:
// the same field carries the return type for some declarations
// (`std::string JoinAsTuple`, `WithoutMatchers WithoutMatchers::Get`) and a
// template class carries `::` inside its own arguments
// (`formatter<detail::streamed_view<T>, Char>::format`). Splitting on `::`
// misreads 465 of 7465 googletest symbols and 595 of 5246 fmt symbols. So the
// prefix is matched against the CLASS SYMBOLS the same file actually declares
// instead: `A::caller` is a member of `A` because this file declares
// `class A` and the name begins with exactly `A::`. That reads
// `formatter<detail::streamed_view<T>, Char>::format` correctly, and it reads
// `std::string JoinAsTuple` as a free function -- there is no `class std` here,
// and even if there were, the name does not begin with `std::`.

// cppClassMemberKindsSQL are the symbol kinds whose BODY encloses class
// members. The C++ adapter emits exactly these three as containers a member can
// be declared in (`cppAddType` recurses into the body of `struct_specifier`,
// `class_specifier` and `union_specifier`, passing the type's own name down as
// the container); `enum` also reaches cppAddType but its body holds enumerators,
// which are not symbols, so a member can never claim it.
const cppClassMemberKindsSQL = `('class', 'struct', 'union')`

// cppClassMemberKinds is the Go twin of cppClassMemberKindsSQL.
// TestCppClassMemberKindsSQLMatchesGoTwin pins that they agree.
var cppClassMemberKinds = map[string]struct{}{
	"class":  {},
	"struct": {},
	"union":  {},
}

// sqlCppClassScope renders the CLASS SYMBOL a C/C++ symbol is a member of, as
// its `symbols.id` rendered as text, or '' when the symbol is not a class member
// at all. Two arms, one per form the adapter emits.
//
// The value is the class ROW, not the class name, and that is the whole point:
// CodeGraph does not model namespaces yet (P22.16), so `class A` in two
// namespaces -- even in one header -- are two rows answering to one name. Two
// members would compare equal on the name and let a caller in one `A` claim a
// member of the other. Comparing the row makes that pair fail closed, and it
// costs nothing measured, since both arms below anchor on a class the member's
// OWN file declares.
//
// IN-CLASS form (`class A { void foo(); }`). Two facts, and it needs both:
//
//   - `container_name` is the adapter's own ownership claim. For a member
//     declared in a class body it is the class name; for a free function it is
//     the MODULE (the file's base name), because the extractor uses the module
//     as the default container.
//   - a class of that name in the same file whose span encloses the symbol.
//     This is what separates the two cases above, and it has to be checked:
//     `format.h` declaring a `class format` would otherwise make every free
//     function in the file look like a member of it, and every member of it look
//     free. It is also what tells two same-named classes apart: only one of them
//     encloses this member.
//
// The innermost enclosing class wins (`ORDER BY start_line DESC`), which is what
// a nested class needs, and the span test alone makes the answer unique -- a
// forward declaration `struct A;` spans one line and encloses nothing.
//
// OUT-OF-LINE form (`void A::caller() { ... }`). Its `container_name` is the
// module and its span is outside every class, so the first arm abstains. Here
// the class comes from the declarator text, matched against the class symbols
// this file declares rather than split out of the string -- see the header
// comment for why the string cannot be trusted. `substr` equality, never LIKE:
// a class name holding `_` or `%` would make a LIKE pattern match names it must
// not.
//
// This arm has no span to disambiguate with, so it demands a UNIQUE match and
// abstains otherwise: two same-named classes in the file, or a prefix that two
// different class names both satisfy, leave the definition with no provable class
// rather than an arbitrary one.
//
// The `instr(name, '::')` guard is load-bearing for cost, not just for meaning:
// it keeps the second arm's per-file scan off every ordinary declaration, which
// is nearly all of them.
//
// `alias` must be a `symbols` row. Every caller guards the whole expression
// behind a language test and a bare-spelling test first.
func sqlCppClassScope(alias string) string {
	inClass := `(
		SELECT CAST(cc.id AS TEXT) FROM symbols cc
		WHERE cc.file_id = ` + alias + `.file_id
		  AND cc.start_line <= ` + alias + `.start_line
		  AND cc.name = ` + alias + `.container_name
		  AND cc.kind IN ` + cppClassMemberKindsSQL + `
		  AND cc.end_line >= ` + alias + `.end_line
		  AND cc.id <> ` + alias + `.id
		ORDER BY cc.start_line DESC LIMIT 1
	)`
	outOfLine := `(CASE WHEN instr(` + alias + `.name, '::') > 0 THEN (
		SELECT CASE WHEN COUNT(*) = 1 THEN CAST(MIN(oc.id) AS TEXT) END
		FROM symbols oc
		WHERE oc.file_id = ` + alias + `.file_id
		  AND oc.kind IN ` + cppClassMemberKindsSQL + `
		  AND oc.id <> ` + alias + `.id
		  AND substr(` + alias + `.name, 1, length(oc.name) + 2) = oc.name || '::'
	) END)`
	return `COALESCE(` + inClass + `, ` + outOfLine + `, '')`
}

// sqlCppClassScopeIfCpp is sqlCppClassScope behind a language test, for the
// query-side loaders: they read every language's symbols in one statement, and
// the derivation is meaningless -- but not free -- for a Go or Python row.
// Measured: without the guard, BenchmarkStoreFindCallers_Hub went 1.27 -> 2.00
// ms/op on a graph with no C++ in it at all.
func sqlCppClassScopeIfCpp(alias string) string {
	return `(CASE WHEN ` + alias + `.language IN ` + bareNameScopeAllKindsSQL +
		` THEN ` + sqlCppClassScope(alias) + ` ELSE '' END)`
}

// resolverCppBareMemberScopeSQL is the repo-wide half of the rule: a predicate
// on the *chosen* candidate, for the same reason resolverBareNameTypeScopeSQL is
// one -- whichever id the caller-kind chooser landed on has to satisfy the
// scope, and a strategy that reaches a foreign class member must bind nothing
// rather than fall through to a weaker level.
//
// It lives in resolverBindableCandidateSQL, so every repo-wide strategy carries
// it. That matters here as much as it did for P22.9: `receiver_method` matches
// the same bare `symbols.name` and asks only that the candidate have a
// container, which every class member has, so a rule sitting on `exact_name`
// alone would be answered one strategy later.
//
// The caller's class comes from `edges.src_symbol_id`. An edge with no source
// symbol has no class, so the COALESCE fails closed rather than matching ''
// against a member's non-empty class.
//
// Ordering is what keeps this cheap: the language test and sqlNotBareName
// short-circuit before either correlated lookup, so a qualified spelling, a Go
// edge or a Python edge never touches `symbols` at all, and the caller's class
// is derived only once the chosen candidate turns out to be a member.
var resolverCppBareMemberScopeSQL = `(
		f.language NOT IN ` + bareNameScopeAllKindsSQL + `
		OR ` + sqlNotBareName("edges.dst_name") + `
		OR NOT EXISTS (
			SELECT 1 FROM (
				SELECT ` + sqlCppClassScope("ms") + ` AS target_class
				FROM symbols ms WHERE ms.id = (` + resolverChosenCandidateSQL + `)
			) t
			WHERE t.target_class <> ''
			AND t.target_class <> COALESCE((
				SELECT ` + sqlCppClassScope("cs") + `
				FROM symbols cs WHERE cs.id = edges.src_symbol_id
			), '')
		)
	)`

// -- Go twin ----------------------------------------------------------------
//
// The incremental binder (resolveEdgeTargets) decides the same population in Go,
// and P22.9's parity tests exist precisely because a rule enforced on one path
// only makes `index` and `update` disagree about the same tree. The two loaders
// below are its inputs; both are batched, neither is per-edge.

// cppClassScopesByName returns, for every C/C++ symbol in the repo whose `name`
// OR `qualified_name` is one of `names`, the class it is a member of. A symbol
// absent from the result is not a class member, which is the answer the binder
// needs for the overwhelming majority of candidates.
//
// Keyed by spelling rather than by candidate id because the binder's candidate
// maps are built before any edge is decided. Both columns are matched because a
// bare spelling reaches two evidence levels there -- the bare-name lookup, which
// matches `name`, and the qualified lookup, which matches `qualified_name` and
// can also answer a bare spelling when a symbol's qualified identity carries no
// container. Matching only `name` would leave the second level unchecked in Go
// while the SQL predicate still refuses it, which is exactly the full/incremental
// split the parity tests exist to catch. Two indexed lookups per name
// (idx_symbols_repo_name, idx_symbols_repo_qname).
func cppClassScopesByName(ctx context.Context, q queryContexter, repoID int64, names []string) (map[int64]string, error) {
	out := map[int64]string{}
	if len(names) == 0 {
		return out, nil
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for start := 0; start < len(sorted); start += nameLookupChunk {
		end := min(start+nameLookupChunk, len(sorted))
		chunk := sorted[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, 2*len(chunk)+1)
		args = append(args, repoID)
		for range 2 {
			for _, name := range chunk {
				args = append(args, name)
			}
		}
		rows, err := q.QueryContext(ctx, `
			SELECT s.id, `+sqlCppClassScope("s")+`
			FROM symbols s
			WHERE s.repo_id = ? AND s.language IN `+bareNameScopeAllKindsSQL+`
			  AND (s.name IN (`+placeholders+`) OR s.qualified_name IN (`+placeholders+`))
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var class string
			if err := rows.Scan(&id, &class); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if class == "" {
				continue
			}
			out[id] = class
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// cppCallerClassScopesByEdge returns the class each edge's SOURCE symbol is a
// member of, keyed by edge id. An edge whose source symbol is missing, is not
// C/C++, or is not a class member simply has no entry -- all three mean "no
// class scope", which is the fail-closed answer.
//
// It is the C/C++ twin of goSymbolScopesByEdgeSource and is called with the
// batch's C/C++ bare-name edges only, never with the repository.
func cppCallerClassScopesByEdge(ctx context.Context, q queryContexter, repoID int64, edgeIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(edgeIDs))
	if len(edgeIDs) == 0 {
		return out, nil
	}
	for _, chunk := range chunkInt64s(edgeIDs, sqliteInClauseBatchSize) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		args = append(args, int64SliceToAny(chunk)...)
		rows, err := q.QueryContext(ctx, `
			SELECT e.id, `+sqlCppClassScope("s")+`
			FROM edges e
			JOIN symbols s ON s.id = e.src_symbol_id
			WHERE e.repo_id = ? AND s.language IN `+bareNameScopeAllKindsSQL+`
			  AND e.id IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var edgeID int64
			var class string
			if err := rows.Scan(&edgeID, &class); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if class == "" {
				continue
			}
			out[edgeID] = class
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// cppMemberTargetOutOfClassScope is the Go twin of the NOT EXISTS arm of
// resolverCppBareMemberScopeSQL: it reports whether binding `dstID` from this
// edge would claim a class member the caller has no class evidence for.
//
// A candidate that is not a class member is never blocked -- this rule governs
// class members and nothing else, so P22.13's free-function recall is untouched.
func cppMemberTargetOutOfClassScope(edgeID, dstID int64, targetClasses, callerClasses map[int64]string) bool {
	targetClass, isMember := targetClasses[dstID]
	if !isMember || targetClass == "" {
		return false
	}
	return callerClasses[edgeID] != targetClass
}
