package store

import (
	"strconv"
	"strings"
	"testing"
)

// TestCppClassMemberKindsSQLMatchesGoTwin pins the two spellings of the set of
// kinds whose body encloses C/C++ class members against each other, the way
// every other SQL/Go twin in this package is pinned.
func TestCppClassMemberKindsSQLMatchesGoTwin(t *testing.T) {
	inner := strings.TrimSuffix(strings.TrimPrefix(cppClassMemberKindsSQL, "("), ")")
	seen := map[string]struct{}{}
	for _, part := range strings.Split(inner, ",") {
		kind := strings.Trim(strings.TrimSpace(part), "'")
		if _, ok := cppClassMemberKinds[kind]; !ok {
			t.Errorf("cppClassMemberKindsSQL lists %q, which the Go twin does not", kind)
		}
		seen[kind] = struct{}{}
	}
	for kind := range cppClassMemberKinds {
		if _, ok := seen[kind]; !ok {
			t.Errorf("cppClassMemberKinds holds %q, which the SQL twin does not", kind)
		}
	}
}

// cppClassFixture builds C/C++ symbols with real spans, which is what the
// in-class arm of sqlCppClassScope reads. The shared gateFixture helpers insert
// every symbol at line 1, so a class would enclose every sibling in its file;
// these rows carry the nesting the rule is actually about.
type cppClassFixture struct{ *gateFixture }

func newCppClassFixture(t *testing.T) *cppClassFixture {
	t.Helper()
	return &cppClassFixture{newGateFixture(t)}
}

// declare inserts one C/C++ symbol with an explicit span and container.
func (f *cppClassFixture) declare(t *testing.T, fileID int64, name, qualified, kind, container string, startLine, endLine int) int64 {
	t.Helper()
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO symbols(
			repo_id, file_id, language, kind, name, qualified_name, container_name,
			start_line, start_col, end_line, end_col, stable_key, qualified_suffix, dot_tail2, dot_tail3
		) VALUES(?, ?, 'cpp', ?, ?, ?, ?, ?, 1, ?, 1, ?, ?, ?, ?)`,
		f.repoID, fileID, kind, name, qualified, container, startLine, endLine,
		qualified+"|cpp", qualifiedSuffix(qualified), dotTail2(qualified), dotTail3(qualified))
	if err != nil {
		t.Fatalf("insert symbol(%s) error = %v", qualified, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}
	return id
}

// classScopeOf reads the rule's own answer for one symbol -- the id of the class
// symbol it belongs to, or "" -- so a scenario can be debugged as "the class fact
// was wrong" rather than "the edge did not bind".
func (f *cppClassFixture) classScopeOf(t *testing.T, symbolID int64) string {
	t.Helper()
	var class string
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT `+sqlCppClassScope("s")+` FROM symbols s WHERE s.id = ?`, symbolID).Scan(&class); err != nil {
		t.Fatalf("class scope query error = %v", err)
	}
	return class
}

type cppClassScenario struct {
	name  string
	build func(t *testing.T, f *cppClassFixture) (edgeID, wantDstID int64, srcPath, targetName string)
}

func cppClassScopeScenarios() []cppClassScenario {
	return []cppClassScenario{
		{
			// The caller and the target are members of one class: the only class
			// evidence CodeGraph holds, and the recall the rule keeps.
			name: "same_class_binds",
			build: func(t *testing.T, f *cppClassFixture) (int64, int64, string, string) {
				file := f.file(t, "a.cpp", "cpp")
				f.declare(t, file, "A", "a::A", "class", "a", 1, 20)
				dst := f.declare(t, file, "foo", "A::foo", "function", "A", 2, 3)
				src := f.declare(t, file, "caller", "A::caller", "function", "A", 4, 6)
				return f.edge(t, file, src, "foo"), dst, "a.cpp", "foo"
			},
		},
		{
			// Another class's member, and the only `foo` in the repository.
			// Repository-global uniqueness is not class scope.
			name: "other_class_refused",
			build: func(t *testing.T, f *cppClassFixture) (int64, int64, string, string) {
				file := f.file(t, "a.cpp", "cpp")
				f.declare(t, file, "A", "a::A", "class", "a", 1, 10)
				src := f.declare(t, file, "caller", "A::caller", "function", "A", 2, 4)
				f.declare(t, file, "B", "a::B", "class", "a", 11, 20)
				f.declare(t, file, "foo", "B::foo", "function", "B", 12, 13)
				return f.edge(t, file, src, "foo"), 0, "a.cpp", "foo"
			},
		},
		{
			// A free function has no class, so it reaches no member.
			name: "free_caller_refused",
			build: func(t *testing.T, f *cppClassFixture) (int64, int64, string, string) {
				file := f.file(t, "a.cpp", "cpp")
				f.declare(t, file, "A", "a::A", "class", "a", 1, 10)
				f.declare(t, file, "foo", "A::foo", "function", "A", 2, 3)
				src := f.declare(t, file, "caller", "a::caller", "function", "a", 11, 13)
				return f.edge(t, file, src, "foo"), 0, "a.cpp", "foo"
			},
		},
		{
			// An edge with no source symbol has no class either, and must fail
			// closed rather than compare '' against the member's class.
			name: "unknown_caller_refused",
			build: func(t *testing.T, f *cppClassFixture) (int64, int64, string, string) {
				file := f.file(t, "a.cpp", "cpp")
				f.declare(t, file, "A", "a::A", "class", "a", 1, 10)
				f.declare(t, file, "foo", "A::foo", "function", "A", 2, 3)
				return f.edge(t, file, 0, "foo"), 0, "a.cpp", "foo"
			},
		},
		{
			// The control: a free function target is not a class member, so the
			// rule does not touch P22.13's same-file recall -- not even for a
			// caller that IS a member.
			name: "member_calls_free_function_binds",
			build: func(t *testing.T, f *cppClassFixture) (int64, int64, string, string) {
				file := f.file(t, "a.cpp", "cpp")
				f.declare(t, file, "A", "a::A", "class", "a", 1, 10)
				src := f.declare(t, file, "caller", "A::caller", "function", "A", 2, 4)
				dst := f.declare(t, file, "helper", "a::helper", "function", "a", 11, 13)
				return f.edge(t, file, src, "helper"), dst, "a.cpp", "helper"
			},
		},
		{
			// The out-of-line form, so the second arm of sqlCppClassScope is
			// exercised through the Go twin as well as through the SQL: the
			// caller's own class comes from its declarator text matched against
			// the class this file declares.
			name: "out_of_line_caller_same_class_binds",
			build: func(t *testing.T, f *cppClassFixture) (int64, int64, string, string) {
				file := f.file(t, "a.cpp", "cpp")
				f.declare(t, file, "A", "a::A", "class", "a", 1, 10)
				dst := f.declare(t, file, "foo", "A::foo", "function", "A", 2, 3)
				src := f.declare(t, file, "A::caller", "a::A::caller", "function", "a", 11, 14)
				return f.edge(t, file, src, "foo"), dst, "a.cpp", "foo"
			},
		},
		{
			// Two classes of one name in one file -- the shape a namespace makes,
			// which the current identity cannot name apart. The in-class arm tells
			// them apart by span; the out-of-line arm cannot, so it abstains and
			// the call is refused rather than pointed at an arbitrary one.
			name: "duplicate_class_name_out_of_line_refused",
			build: func(t *testing.T, f *cppClassFixture) (int64, int64, string, string) {
				file := f.file(t, "a.cpp", "cpp")
				f.declare(t, file, "A", "a::A", "class", "a", 1, 10)
				f.declare(t, file, "foo", "A::foo", "function", "A", 2, 3)
				f.declare(t, file, "A", "a::A", "class", "a", 11, 20)
				src := f.declare(t, file, "A::caller", "a::A::caller", "function", "a", 21, 24)
				return f.edge(t, file, src, "foo"), 0, "a.cpp", "foo"
			},
		},
		{
			// Same shape, in-class: the span says which `A` each symbol is in, so
			// the sibling pair binds and the other `A`'s member is not reachable.
			name: "duplicate_class_name_in_class_uses_span",
			build: func(t *testing.T, f *cppClassFixture) (int64, int64, string, string) {
				file := f.file(t, "a.cpp", "cpp")
				f.declare(t, file, "A", "a::A", "class", "a", 1, 10)
				f.declare(t, file, "foo", "A::foo", "function", "A", 2, 3)
				f.declare(t, file, "A", "a::A", "class", "a", 11, 20)
				src := f.declare(t, file, "caller", "A::caller", "function", "A", 12, 14)
				return f.edge(t, file, src, "foo"), 0, "a.cpp", "foo"
			},
		},
		{
			// A qualifier the source itself wrote is evidence the rule must not
			// consume: `A::foo` is not a bare spelling.
			name: "qualified_spelling_untouched",
			build: func(t *testing.T, f *cppClassFixture) (int64, int64, string, string) {
				file := f.file(t, "a.cpp", "cpp")
				f.declare(t, file, "A", "a::A", "class", "a", 1, 10)
				dst := f.declare(t, file, "foo", "A::foo", "function", "A", 2, 3)
				src := f.declare(t, file, "caller", "a::caller", "function", "a", 11, 13)
				return f.edge(t, file, src, "A::foo"), dst, "a.cpp", "A::foo"
			},
		},
	}
}

// TestCppBareMemberScope_AllEntrypoints is §19: the repo-wide resolver, the
// path-scoped one and the name-scoped one apply one policy, never two.
func TestCppBareMemberScope_AllEntrypoints(t *testing.T) {
	resolvers := []struct {
		name string
		run  func(t *testing.T, f *cppClassFixture, srcPath, targetName string)
	}{
		{
			name: "repo",
			run: func(t *testing.T, f *cppClassFixture, _, _ string) {
				if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
					t.Fatalf("ResolveEdges() error = %v", err)
				}
			},
		},
		{
			name: "paths",
			run: func(t *testing.T, f *cppClassFixture, srcPath, _ string) {
				if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{srcPath}); err != nil {
					t.Fatalf("ResolveEdgesForPaths() error = %v", err)
				}
			},
		},
		{
			name: "names",
			run: func(t *testing.T, f *cppClassFixture, _, targetName string) {
				if _, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, []string{targetName}); err != nil {
					t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
				}
			},
		},
	}

	for _, scenario := range cppClassScopeScenarios() {
		for _, resolver := range resolvers {
			t.Run(scenario.name+"/"+resolver.name, func(t *testing.T) {
				f := newCppClassFixture(t)
				edgeID, wantDstID, srcPath, targetName := scenario.build(t, f)
				resolver.run(t, f, srcPath, targetName)
				gotDstID, resolved := f.dstSymbolID(t, edgeID)
				switch {
				case wantDstID == 0 && resolved:
					t.Fatalf("edge resolved to %s; want unresolved", f.qualifiedNameOf(t, gotDstID))
				case wantDstID != 0 && !resolved:
					t.Fatalf("edge unresolved; want %s", f.qualifiedNameOf(t, wantDstID))
				case wantDstID != 0 && gotDstID != wantDstID:
					t.Fatalf("edge resolved to %s; want %s",
						f.qualifiedNameOf(t, gotDstID), f.qualifiedNameOf(t, wantDstID))
				}
			})
		}
	}
}

// TestCppClassScopeIdentifiesBothDeclarationForms pins the two arms directly, so
// a regression reports which class fact broke rather than which edge moved.
func TestCppClassScopeIdentifiesBothDeclarationForms(t *testing.T) {
	f := newCppClassFixture(t)
	file := f.file(t, "a.cpp", "cpp")
	classA := f.declare(t, file, "A", "a::A", "class", "a", 1, 10)
	member := f.declare(t, file, "foo", "A::foo", "function", "A", 2, 3)
	free := f.declare(t, file, "helper", "a::helper", "function", "a", 11, 13)
	outOfLine := f.declare(t, file, "A::caller", "a::A::caller", "function", "a", 14, 16)
	// The declarator text carries a return type as well, which is why the class
	// is matched against declared class symbols rather than split out of it.
	returnTyped := f.declare(t, file, "std::string Join", "a::std::string Join", "function", "a", 17, 19)
	// A struct whose own name contains `::` inside template arguments: the
	// prefix match handles it, a `::` split does not.
	classFmt := f.declare(t, file, "fmt<x::y>", "a::fmt<x::y>", "struct", "a", 20, 30)
	templated := f.declare(t, file, "fmt<x::y>::format", "a::fmt<x::y>::format", "function", "a", 31, 33)

	for _, tc := range []struct {
		name string
		id   int64
		want string
	}{
		{"in-class member", member, strconv.FormatInt(classA, 10)},
		{"free function", free, ""},
		{"out-of-line member", outOfLine, strconv.FormatInt(classA, 10)},
		{"return type is not a class", returnTyped, ""},
		{"template arguments are not a class boundary", templated, strconv.FormatInt(classFmt, 10)},
	} {
		if got := f.classScopeOf(t, tc.id); got != tc.want {
			t.Errorf("%s: class scope = %q, want %q", tc.name, got, tc.want)
		}
	}
}
