package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
)

type jvmCompilationScopeCompleteness struct {
	Source, Generated, Excluded, Dependencies, ExternalMetadata, CompilerIdentity string
}

func setTestJVMCompilationScopeCompleteness(t *testing.T, ctx context.Context, s *Store, repoID int64, scope jvmCompilationScopeCompleteness, provenance string) {
	t.Helper()
	state := "complete"
	values := []string{scope.Source, scope.Generated, scope.Excluded, scope.Dependencies, scope.ExternalMetadata, scope.CompilerIdentity}
	for _, value := range values {
		if value != "complete" {
			state = "unknown"
			break
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jvm_compilation_scope_evidence SET state=?, provenance=?, source_state=?, generated_state=?, excluded_state=?, dependency_state=?, external_metadata_state=?, compiler_identity_state=? WHERE repo_id=?`, state, provenance, scope.Source, scope.Generated, scope.Excluded, scope.Dependencies, scope.ExternalMetadata, scope.CompilerIdentity, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jvm_type_dependencies SET state='dirty' WHERE repo_id=?`, repoID); err != nil {
		t.Fatal(err)
	}
}

func TestJVMTypeDependenciesFailClosedAndConverge(t *testing.T) {
	f := newGateFixture(t)
	putClass := func(path, name, modifiers string) {
		t.Helper()
		qualified := "pkg." + name
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: name, QualifiedName: qualified, StableKey: "type:kotlin:" + qualified}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", Modifiers: modifiers, SyntaxState: "known", Provenance: "test:type"}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, modifiers+qualified, parsed); err != nil {
			t.Fatal(err)
		}
	}
	putConsumer := func() {
		t.Helper()
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Token, Other)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Token", SyntaxState: "known"}, {Position: "result", Syntax: "Other", SyntaxState: "known"}}}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Consumer.kt", "kotlin", 1, 0, "consumer", parsed); err != nil {
			t.Fatal(err)
		}
	}
	putClass("Token.kt", "Token", "")
	putConsumer()
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	assert := func(lookup, kind, target string) string {
		t.Helper()
		var gotTarget, fingerprint, state string
		err := f.store.db.QueryRowContext(f.ctx, `SELECT target_key,target_fingerprint,state FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Consumer.kt' AND lookup_key=?`, f.repoID, lookup).Scan(&gotTarget, &fingerprint, &state)
		if err != nil {
			t.Fatal(err)
		}
		var gotKind string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Consumer.kt' AND lookup_key=?`, f.repoID, lookup).Scan(&gotKind); err != nil {
			t.Fatal(err)
		}
		if gotKind != kind || gotTarget != target || state != "current" {
			t.Fatalf("dependency %q = kind %q target %q state %q, want %q %q current", lookup, gotKind, gotTarget, state, kind, target)
		}
		return fingerprint
	}
	tokenLookup := jvmTypeLookupKey("pkg", "Token")
	otherLookup := jvmTypeLookupKey("pkg", "Other")
	assert(tokenLookup, "unknown", "")
	assert(otherLookup, "unknown", "")

	complete := jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, complete, "test:complete-oracle")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	classFingerprint := assert(tokenLookup, "positive", "jvm-type/v1|kotlin|type:kotlin:pkg.Token")
	assert(otherLookup, "negative", "")
	for _, missing := range []string{"Source", "Generated", "Excluded", "Dependencies", "ExternalMetadata", "CompilerIdentity"} {
		partial := complete
		switch missing {
		case "Source":
			partial.Source = "unknown"
		case "Generated":
			partial.Generated = "unknown"
		case "Excluded":
			partial.Excluded = "unknown"
		case "Dependencies":
			partial.Dependencies = "unknown"
		case "ExternalMetadata":
			partial.ExternalMetadata = "unknown"
		case "CompilerIdentity":
			partial.CompilerIdentity = "unknown"
		}
		setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, partial, "test:missing-"+missing)
		if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
			t.Fatal(err)
		}
		assert(tokenLookup, "unknown", "")
		assert(otherLookup, "unknown", "")
	}
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, complete, "test:complete-oracle")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	classFingerprint = assert(tokenLookup, "positive", "jvm-type/v1|kotlin|type:kotlin:pkg.Token")
	assert(otherLookup, "negative", "")

	putClass("Token.kt", "Token", "value")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if next := assert(tokenLookup, "positive", "jvm-type/v1|kotlin|type:kotlin:pkg.Token"); next == classFingerprint {
		t.Fatal("target edit left a stale dependency fingerprint")
	}
	putClass("Token.kt", "Renamed", "value")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	assert(tokenLookup, "negative", "")
	putClass("Token.kt", "Token", "value")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	assert(tokenLookup, "positive", "jvm-type/v1|kotlin|type:kotlin:pkg.Token")
	if _, err := f.store.MarkFilesDeletedBatch(f.ctx, f.repoID, 2, []string{"Token.kt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PurgeDeletedFileGraphsForScan(f.ctx, f.repoID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	assert(tokenLookup, "negative", "")
	putClass("Token.kt", "Token", "value")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	assert(tokenLookup, "positive", "jvm-type/v1|kotlin|type:kotlin:pkg.Token")

	putClass("Other.kt", "Other", "")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	assert(otherLookup, "positive", "jvm-type/v1|kotlin|type:kotlin:pkg.Other")
	if _, err := f.store.MarkFilesDeletedBatch(f.ctx, f.repoID, 2, []string{"Other.kt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PurgeDeletedFileGraphsForScan(f.ctx, f.repoID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	assert(otherLookup, "negative", "")
	if n, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil || n != 0 {
		t.Fatalf("second no-op rebuild = %d, %v", n, err)
	}
}

func TestJVMTypeDependencyRecomputeFailureLeavesDirtyStateForRetry(t *testing.T) {
	f := newGateFixture(t)
	parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Token)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Token", SyntaxState: "known"}}}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Consumer.kt", "kotlin", 1, 0, "consumer", parsed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `CREATE TRIGGER fail_jvm_dependency_insert BEFORE INSERT ON jvm_type_dependencies BEGIN SELECT RAISE(FAIL, 'injected dependency failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 2, "Consumer.kt", "kotlin", 2, 0, "changed", parsed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err == nil {
		t.Fatal("expected injected recomputation failure")
	}
	var state string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT state FROM jvm_type_dependencies WHERE repo_id=? LIMIT 1`, f.repoID).Scan(&state); err != nil || state != "dirty" {
		t.Fatalf("failed recomputation state=%q err=%v", state, err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DROP TRIGGER fail_jvm_dependency_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT state FROM jvm_type_dependencies WHERE repo_id=? LIMIT 1`, f.repoID).Scan(&state); err != nil || state != "current" {
		t.Fatalf("retry state=%q err=%v", state, err)
	}
}

func TestJVMTypeAliasDependencyMutationAndDeletion(t *testing.T) {
	f := newGateFixture(t)
	putType := func(path, name, aliasTarget string) {
		t.Helper()
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}}
		if aliasTarget == "" {
			qualified := "pkg." + name
			parsed.Symbols = []graph.Symbol{{Language: "kotlin", Kind: "class", Name: name, QualifiedName: qualified, StableKey: "type:kotlin:" + qualified}}
			parsed.JVMTypeEvidence = []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:type"}}
		} else {
			parsed.JVMTypeEvidence = []graph.JVMTypeEvidence{{SymbolIndex: -1, EvidenceKey: "typealias:1:Alias", OwnerName: "pkg", Kind: "typealias", AliasTarget: aliasTarget, SyntaxState: "unknown", Provenance: "test:alias"}}
		}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, name+aliasTarget, parsed); err != nil {
			t.Fatal(err)
		}
	}
	putConsumer := func() {
		t.Helper()
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Alias)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Alias", SyntaxState: "known"}}}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Consumer.kt", "kotlin", 1, 0, "consumer", parsed); err != nil {
			t.Fatal(err)
		}
	}
	putType("Token.kt", "Token", "")
	putType("Other.kt", "Other", "")
	putConsumer()
	complete := jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, complete, "test:complete-oracle")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	aliasLookup := jvmTypeLookupKey("pkg", "Alias")
	var kind, target, oldFingerprint string
	read := func(consumerPath, lookup string) (string, string, string) {
		t.Helper()
		var k, targetKey, fp string
		err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind,target_key,target_fingerprint FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path=? AND lookup_key=? AND state='current'`, f.repoID, consumerPath, lookup).Scan(&k, &targetKey, &fp)
		if err != nil {
			t.Fatal(err)
		}
		return k, targetKey, fp
	}
	kind, target, oldFingerprint = read("Consumer.kt", aliasLookup)
	if kind != "negative" || target != "" {
		t.Fatalf("missing alias = %q %q", kind, target)
	}
	putType("Alias.kt", "", "Token")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	kind, target, oldFingerprint = read("Consumer.kt", aliasLookup)
	if kind != "positive" || target != "jvm-type/v1|kotlin|pkg.Alias|typealias" {
		t.Fatalf("alias added = %q %q", kind, target)
	}
	if kind, target, _ = read("Alias.kt", jvmTypeLookupKey("pkg", "Token")); kind != "positive" || target != "jvm-type/v1|kotlin|type:kotlin:pkg.Token" {
		t.Fatalf("alias target dependency = %q %q", kind, target)
	}
	putType("Alias.kt", "", "Other")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	kind, target, newFingerprint := read("Consumer.kt", aliasLookup)
	if kind != "positive" || target != "jvm-type/v1|kotlin|pkg.Alias|typealias" || newFingerprint == oldFingerprint {
		t.Fatalf("alias target mutation = %q %q old=%q new=%q", kind, target, oldFingerprint, newFingerprint)
	}
	if kind, target, _ = read("Alias.kt", jvmTypeLookupKey("pkg", "Other")); kind != "positive" || target != "jvm-type/v1|kotlin|type:kotlin:pkg.Other" {
		t.Fatalf("changed alias target = %q %q", kind, target)
	}
	if _, err := f.store.MarkFilesDeletedBatch(f.ctx, f.repoID, 2, []string{"Alias.kt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PurgeDeletedFileGraphsForScan(f.ctx, f.repoID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	kind, target, _ = read("Consumer.kt", aliasLookup)
	if kind != "negative" || target != "" {
		t.Fatalf("deleted alias = %q %q", kind, target)
	}

	var dbPath string
	if err := f.store.db.QueryRowContext(f.ctx, `PRAGMA database_list`).Scan(new(int), new(string), &dbPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenReadOnly(dbPath, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := normalizedJVMTypeSnapshot(t, f.store.db, f.repoID)
	if afterReopen := normalizedJVMTypeSnapshot(t, reopened.db, f.repoID); !reflect.DeepEqual(got, afterReopen) {
		t.Fatalf("reopen changed dependency state\ngot:  %v\nwant: %v", afterReopen, got)
	}

	fresh, err := OpenWithOptions(filepath.Join(t.TempDir(), RepoDatabaseFileName), OpenOptions{PerformanceProfile: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	freshRepo, err := fresh.UpsertRepo(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putFreshClass := func(path, name string) {
		t.Helper()
		qualified := "pkg." + name
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: name, QualifiedName: qualified, StableKey: "type:kotlin:" + qualified}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:type"}}}
		if err := fresh.ReplaceFileGraph(context.Background(), freshRepo.ID, 1, path, "kotlin", 1, 0, name, parsed); err != nil {
			t.Fatal(err)
		}
	}
	putFreshClass("Token.kt", "Token")
	putFreshClass("Other.kt", "Other")
	freshConsumer := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Alias)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Alias", SyntaxState: "known"}}}}}
	if err := fresh.ReplaceFileGraph(context.Background(), freshRepo.ID, 1, "Consumer.kt", "kotlin", 1, 0, "consumer", freshConsumer); err != nil {
		t.Fatal(err)
	}
	setTestJVMCompilationScopeCompleteness(t, context.Background(), fresh, freshRepo.ID, complete, "test:complete-oracle")
	if _, err := fresh.RebuildJVMTypeDependencies(context.Background(), freshRepo.ID, nil); err != nil {
		t.Fatal(err)
	}
	want := normalizedJVMTypeSnapshot(t, fresh.db, freshRepo.ID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental/fresh dependency mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

func normalizedJVMDependencySnapshot(t *testing.T, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, repoID int64) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT consumer_key,consumer_path,lookup_key,dependency_kind,target_key,target_fingerprint,source_language,type_position,type_ordinal,state,provenance FROM jvm_type_dependencies WHERE repo_id=? ORDER BY consumer_key,consumer_path,lookup_key,type_position,type_ordinal`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v [11]any
		ptrs := make([]any, len(v))
		for i := range v {
			ptrs[i] = &v[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for i, value := range v {
			if i > 0 {
				b.WriteByte('|')
			}
			fmt.Fprint(&b, value)
		}
		out = append(out, b.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func normalizedJVMTypeSnapshot(t *testing.T, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, repoID int64) []string {
	t.Helper()
	dependencies := normalizedJVMDependencySnapshot(t, db, repoID)
	rows, err := db.QueryContext(context.Background(), `SELECT evidence_key,declaration_key,lookup_key,file_path,fingerprint,source_language,declaration_kind FROM jvm_type_declarations WHERE repo_id=? ORDER BY evidence_key`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := append([]string(nil), dependencies...)
	for rows.Next() {
		var evidence, declaration, lookup, path, fingerprint, language, kind string
		if err := rows.Scan(&evidence, &declaration, &lookup, &path, &fingerprint, &language, &kind); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join([]string{"declaration", evidence, declaration, lookup, path, fingerprint, language, kind}, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestJVMTypeDependencyAmbiguityAndConflictingImports(t *testing.T) {
	f := newGateFixture(t)
	putType := func(path, pkg string) {
		t.Helper()
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: pkg}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: "Token", QualifiedName: pkg + ".Token", StableKey: "type:kotlin:" + pkg + ".Token"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: pkg, SyntaxState: "known", Provenance: "test:type"}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, path, parsed); err != nil {
			t.Fatal(err)
		}
	}
	putConsumer := func(path string, imports []graph.ScopeImport) {
		t.Helper()
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg", Imports: imports}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Token)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Token", SyntaxState: "known"}}}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, path, parsed); err != nil {
			t.Fatal(err)
		}
	}
	complete := jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, complete, "test:complete-oracle")
	lookup := jvmTypeLookupKey("pkg", "Token")
	read := func(path, key string) (string, string) {
		t.Helper()
		var kind, target string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind,target_key FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path=? AND lookup_key=? AND state='current'`, f.repoID, path, key).Scan(&kind, &target); err != nil {
			t.Fatal(err)
		}
		return kind, target
	}
	putType("One.kt", "pkg")
	putConsumer("Consumer.kt", nil)
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if kind, target := read("Consumer.kt", lookup); kind != "positive" || target == "" {
		t.Fatalf("unique declaration = %q %q", kind, target)
	}
	putType("Two.kt", "pkg")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if kind, target := read("Consumer.kt", lookup); kind != "ambiguous" || target != "" {
		t.Fatalf("ambiguous declaration = %q %q", kind, target)
	}
	if _, err := f.store.MarkFilesDeletedBatch(f.ctx, f.repoID, 2, []string{"Two.kt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PurgeDeletedFileGraphsForScan(f.ctx, f.repoID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if kind, target := read("Consumer.kt", lookup); kind != "positive" || target == "" {
		t.Fatalf("ambiguity removal = %q %q", kind, target)
	}

	imports := []graph.ScopeImport{{SourceSpecifier: "x.Token", ImportedName: "Token", LocalName: "Token", Kind: graph.ScopeImportNamed}, {SourceSpecifier: "y.Token", ImportedName: "Token", LocalName: "Token", Kind: graph.ScopeImportNamed}}
	putConsumer("Conflict.kt", imports)
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	var conflictKind string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Conflict.kt' AND lookup_key=? AND state='current'`, f.repoID, jvmTypeLookupKey("x", "Token")).Scan(&conflictKind); err != nil || conflictKind != "ambiguous" {
		t.Fatalf("conflicting imports = %q, %v", conflictKind, err)
	}
	putConsumer("Conflict.kt", imports[:1])
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if kind, target := read("Conflict.kt", jvmTypeLookupKey("x", "Token")); kind != "negative" || target != "" {
		t.Fatalf("conflict removal = %q %q", kind, target)
	}

	// A same-package alias with the same spelling shadows the ordinary type.
	shadow := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: -1, EvidenceKey: "typealias:1:Token", OwnerName: "pkg", Kind: "typealias", AliasTarget: "Other", SyntaxState: "unknown", Provenance: "test:shadow-alias"}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Shadow.kt", "kotlin", 1, 0, "shadow", shadow); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if kind, target := read("Consumer.kt", lookup); kind != "ambiguous" || target != "" {
		t.Fatalf("same-package alias shadow = %q %q", kind, target)
	}
	if _, err := f.store.MarkFilesDeletedBatch(f.ctx, f.repoID, 3, []string{"Shadow.kt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PurgeDeletedFileGraphsForScan(f.ctx, f.repoID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if kind, target := read("Consumer.kt", lookup); kind != "positive" || target == "" {
		t.Fatalf("same-package alias shadow removal = %q %q", kind, target)
	}
}

func TestJVMTypeDependencyImportOnlyChangeRedecidesConsumer(t *testing.T) {
	f := newGateFixture(t)
	put := func(path, typ string, imports []graph.ScopeImport) {
		t.Helper()
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg", Imports: imports}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(" + typ + ")", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: typ, SyntaxState: "known"}}}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, path, parsed); err != nil {
			t.Fatal(err)
		}
	}
	put("Imports.kt", "Token", nil)
	complete := jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, complete, "test:compiler-oracle")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	imported := []graph.ScopeImport{{SourceSpecifier: "external.Token", ImportedName: "Other", LocalName: "Token", Kind: graph.ScopeImportNamed}}
	put("Imports.kt", "Token", imported)
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, []string{"Imports.kt"}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT state FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Imports.kt' AND lookup_key=?`, f.repoID, jvmTypeLookupKey("external", "Token")).Scan(&state); err != nil || state != "current" {
		t.Fatalf("new import dependency state=%q err=%v", state, err)
	}
	var localKind string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Imports.kt' AND lookup_key=?`, f.repoID, jvmTypeLookupKey("pkg", "Token")).Scan(&localKind); err != nil || localKind != "negative" {
		t.Fatalf("same-package absence proof kind=%q err=%v", localKind, err)
	}
}

func TestJVMTypeDependencyFileRenameConvergesAndIgnoresRowIDs(t *testing.T) {
	f := newGateFixture(t)
	putClass := func(path, name string) {
		t.Helper()
		qualified := "pkg." + name
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: name, QualifiedName: qualified, StableKey: "type:kotlin:" + qualified}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:type"}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, path, parsed); err != nil {
			t.Fatal(err)
		}
	}
	putConsumer := func() {
		t.Helper()
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Token)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Token", SyntaxState: "known"}}}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Consumer.kt", "kotlin", 1, 0, "consumer", parsed); err != nil {
			t.Fatal(err)
		}
	}
	putClass("Types.kt", "Token")
	putConsumer()
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MarkFilesDeletedBatch(f.ctx, f.repoID, 2, []string{"Types.kt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PurgeDeletedFileGraphsForScan(f.ctx, f.repoID, 2); err != nil {
		t.Fatal(err)
	}
	putClass("renamed/Types.kt", "Token")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	dependencyRows := normalizedJVMDependencySnapshot(t, f.store.db, f.repoID)
	if len(dependencyRows) != 1 || !strings.Contains(dependencyRows[0], "unknown") {
		t.Fatalf("renamed-provider dependencies = %v", dependencyRows)
	}
	want := normalizedJVMTypeSnapshot(t, f.store.db, f.repoID)
	var dbPath string
	if err := f.store.db.QueryRowContext(f.ctx, `PRAGMA database_list`).Scan(new(int), new(string), &dbPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenReadOnly(dbPath, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := normalizedJVMTypeSnapshot(t, reopened.db, f.repoID); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopen changed normalized dependency identity: %v != %v", got, want)
	}

	fresh, err := OpenWithOptions(filepath.Join(t.TempDir(), RepoDatabaseFileName), OpenOptions{PerformanceProfile: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	freshRepo, err := fresh.UpsertRepo(f.ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putFresh := func(s *Store, repoID int64, path string, parsed graph.ParsedFile) {
		t.Helper()
		if err := s.ReplaceFileGraph(f.ctx, repoID, 1, path, "kotlin", 1, 0, path, parsed); err != nil {
			t.Fatal(err)
		}
	}
	typeParsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: "Token", QualifiedName: "pkg.Token", StableKey: "type:kotlin:pkg.Token"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:type"}}}
	consumerParsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Token)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Token", SyntaxState: "known"}}}}}
	putFresh(fresh, freshRepo.ID, "renamed/Types.kt", typeParsed)
	putFresh(fresh, freshRepo.ID, "Consumer.kt", consumerParsed)
	if _, err := fresh.RebuildJVMTypeDependencies(f.ctx, freshRepo.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := normalizedJVMTypeSnapshot(t, fresh.db, freshRepo.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental/fresh final dependencies differ: %v != %v", want, got)
	}
	if n, err := fresh.RebuildJVMTypeDependencies(f.ctx, freshRepo.ID, nil); err != nil || n != 0 {
		t.Fatalf("fresh second update = %d, %v", n, err)
	}
}

func TestJVMTypeDependencyFocusedInvalidationMeasurements(t *testing.T) {
	f := newGateFixture(t)
	put := func(path, name, kind string) {
		t.Helper()
		qualified := "pkg." + name
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}}
		if kind == "class" {
			parsed.Symbols = []graph.Symbol{{Language: "kotlin", Kind: "class", Name: name, QualifiedName: qualified, StableKey: "type:kotlin:" + qualified}}
			parsed.JVMTypeEvidence = []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:type"}}
		} else {
			parsed.Symbols = []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: qualified + ".use", ContainerName: qualified, Signature: "use(" + name + ")", StableKey: "func:kotlin:" + qualified + ".use"}}
			parsed.JVMTypeEvidence = []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: qualified, SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: name, SyntaxState: "known"}}}}
		}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, kind+name, parsed); err != nil {
			t.Fatal(err)
		}
	}
	put("Types.kt", "Token", "class")
	put("CallerA.kt", "Token", "function")
	put("CallerB.kt", "Token", "function")
	put("OtherType.kt", "Other", "class")
	put("Unrelated.kt", "Other", "function")
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	put("Types.kt", "Token", "class") // an unchanged parse shape still forces one provider graph replacement
	var dirtyConsumers int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(DISTINCT consumer_path) FROM jvm_type_dependencies WHERE repo_id=? AND state='dirty'`, f.repoID).Scan(&dirtyConsumers); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	updated, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, []string{"Types.kt"})
	updateDuration := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if dirtyConsumers != 2 || updated != 2 {
		t.Fatalf("focused invalidation consumers=%d dependency rows recomputed=%d, want 2 and 2", dirtyConsumers, updated)
	}
	var unrelatedCurrent int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Unrelated.kt' AND state='current'`, f.repoID).Scan(&unrelatedCurrent); err != nil || unrelatedCurrent != 1 {
		t.Fatalf("unrelated dependency rows current=%d err=%v", unrelatedCurrent, err)
	}
	start = time.Now()
	noopRows, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil)
	noopDuration := time.Since(start)
	if err != nil || noopRows != 0 {
		t.Fatalf("no-op rebuild rows=%d err=%v", noopRows, err)
	}
	t.Logf("focused invalidation: changed source files=1, invalidated consumers=%d, dependency rows recomputed=%d, unrelated JVM consumers redecided=0, reparsed=0, update=%s, no-op=%s", dirtyConsumers, updated, updateDuration, noopDuration)
}

func TestJVMTypeDependencyWildcardAndCallableProviderGuards(t *testing.T) {
	f := newGateFixture(t)
	complete := jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, complete, "test:complete-oracle")
	class := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: "Token", QualifiedName: "pkg.Token", StableKey: "type:kotlin:pkg.Token"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:type"}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Token.kt", "kotlin", 1, 0, "class", class); err != nil {
		t.Fatal(err)
	}
	function := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "Token", QualifiedName: "pkg.Token", ContainerName: "pkg", Signature: "Token()", StableKey: "func:kotlin:pkg.Token"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:function"}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "TokenFunction.kt", "kotlin", 1, 0, "function", function); err != nil {
		t.Fatal(err)
	}
	consumer := func(path, syntax string, imports []graph.ScopeImport) {
		t.Helper()
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg", Imports: imports}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use." + path, ContainerName: "pkg", Signature: "use(" + syntax + ")", StableKey: "func:kotlin:pkg.use." + path}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: syntax, SyntaxState: "known"}}}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, path, parsed); err != nil {
			t.Fatal(err)
		}
	}
	wildcard := []graph.ScopeImport{{SourceSpecifier: "external.*", Kind: "wildcard", Wildcard: true}}
	conflicting := []graph.ScopeImport{{SourceSpecifier: "x.Token", ImportedName: "Token", LocalName: "Token", Kind: graph.ScopeImportNamed}, {SourceSpecifier: "y.Token", ImportedName: "Token", LocalName: "Token", Kind: graph.ScopeImportNamed}}
	consumer("Wildcard.kt", "Token", wildcard)
	consumer("Qualified.kt", "pkg.Token", conflicting)
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	lookup := jvmTypeLookupKey("pkg", "Token")
	var wildcardKind, qualifiedKind, qualifiedTarget string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Wildcard.kt' AND lookup_key=?`, f.repoID, lookup).Scan(&wildcardKind); err != nil || wildcardKind != "unknown" {
		t.Fatalf("wildcard guard kind=%q err=%v", wildcardKind, err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind,target_key FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Qualified.kt' AND lookup_key=?`, f.repoID, lookup).Scan(&qualifiedKind, &qualifiedTarget); err != nil || qualifiedKind != "positive" || qualifiedTarget != "jvm-type/v1|kotlin|type:kotlin:pkg.Token" {
		t.Fatalf("qualified type with irrelevant conflicting imports = %q %q err=%v", qualifiedKind, qualifiedTarget, err)
	}
	var declarationCount int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM jvm_type_declarations WHERE repo_id=? AND declaration_key LIKE '%func:kotlin:pkg.Token%'`, f.repoID).Scan(&declarationCount); err != nil || declarationCount != 0 {
		t.Fatalf("callable provider declaration count=%d err=%v", declarationCount, err)
	}
}

func TestJVMTypeDependencyDuplicateAliasConsumersKeepStableIdentity(t *testing.T) {
	f := newGateFixture(t)
	complete := jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, complete, "test:complete-oracle")
	putClass := func(path, name string) {
		t.Helper()
		qualified := "pkg." + name
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: name, QualifiedName: qualified, StableKey: "type:kotlin:" + qualified}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:type"}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, path, parsed); err != nil {
			t.Fatal(err)
		}
	}
	putAlias := func(path, target string) {
		t.Helper()
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: -1, EvidenceKey: "typealias:1:Alias", OwnerName: "pkg", Kind: "typealias", AliasTarget: target, SyntaxState: "unknown", Provenance: "test:alias"}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, target, parsed); err != nil {
			t.Fatal(err)
		}
	}
	putClass("Token.kt", "Token")
	putClass("Other.kt", "Other")
	putAlias("AliasOne.kt", "Token")
	putAlias("AliasTwo.kt", "Other")
	caller := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Alias)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Alias", SyntaxState: "known"}}}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Caller.kt", "kotlin", 1, 0, "caller", caller); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	var aliasConsumerCount int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(DISTINCT consumer_key) FROM jvm_type_dependencies WHERE repo_id=? AND type_position='alias_target'`, f.repoID).Scan(&aliasConsumerCount); err != nil || aliasConsumerCount != 2 {
		t.Fatalf("duplicate alias consumers=%d err=%v", aliasConsumerCount, err)
	}
	var kind string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Caller.kt' AND lookup_key=?`, f.repoID, jvmTypeLookupKey("pkg", "Alias")).Scan(&kind); err != nil || kind != "ambiguous" {
		t.Fatalf("duplicate alias lookup kind=%q err=%v", kind, err)
	}
}

func TestJVMTypeDependencySamePackageShadowInvalidatesImportedAbsenceProof(t *testing.T) {
	f := newGateFixture(t)
	complete := jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, complete, "test:complete-oracle")
	putClass := func(path, pkg, name string) {
		t.Helper()
		qualified := pkg + "." + name
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: pkg}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: name, QualifiedName: qualified, StableKey: "type:kotlin:" + qualified}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: pkg, SyntaxState: "known", Provenance: "test:type"}}}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, path, "kotlin", 1, 0, path, parsed); err != nil {
			t.Fatal(err)
		}
	}
	putClass("ExternalToken.kt", "ext", "Token")
	putClass("Other.kt", "pkg", "Other")
	imported := []graph.ScopeImport{{SourceSpecifier: "ext.Token", ImportedName: "Token", LocalName: "Token", Kind: graph.ScopeImportNamed}}
	caller := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg", Imports: imported}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Token)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Token", SyntaxState: "known"}}}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Caller.kt", "kotlin", 1, 0, "caller", caller); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	read := func(key string) string {
		t.Helper()
		var kind string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Caller.kt' AND lookup_key=? AND state='current'`, f.repoID, key).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		return kind
	}
	if read(jvmTypeLookupKey("ext", "Token")) != "positive" || read(jvmTypeLookupKey("pkg", "Token")) != "negative" {
		t.Fatal("initial import lookup did not retain imported target and same-package absence proof")
	}
	localAlias := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: -1, EvidenceKey: "typealias:1:Token", OwnerName: "pkg", Kind: "typealias", AliasTarget: "Other", SyntaxState: "unknown", Provenance: "test:shadow-alias"}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 2, "LocalAlias.kt", "kotlin", 1, 0, "alias", localAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if read(jvmTypeLookupKey("ext", "Token")) != "ambiguous" || read(jvmTypeLookupKey("pkg", "Token")) != "ambiguous" {
		t.Fatal("same-package alias addition did not invalidate imported lookup")
	}
	if _, err := f.store.MarkFilesDeletedBatch(f.ctx, f.repoID, 3, []string{"LocalAlias.kt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PurgeDeletedFileGraphsForScan(f.ctx, f.repoID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if read(jvmTypeLookupKey("ext", "Token")) != "positive" || read(jvmTypeLookupKey("pkg", "Token")) != "negative" {
		t.Fatal("same-package alias removal did not restore imported target and absence proof")
	}
}

func TestJVMTypeDependencyConvergenceMatrix(t *testing.T) {
	type file struct {
		path, pkg, kind, name, target, modifiers, syntax string
		imports                                          []graph.ScopeImport
	}
	class := func(path, pkg, name, modifiers string) file {
		return file{path: path, pkg: pkg, kind: "class", name: name, modifiers: modifiers}
	}
	alias := func(path, name, target string) file {
		return file{path: path, pkg: "pkg", kind: "alias", name: name, target: target}
	}
	caller := func(syntax string, imports ...graph.ScopeImport) file {
		return file{path: "Caller.kt", pkg: "pkg", kind: "callable", name: "use", syntax: syntax, imports: imports}
	}
	imp := func(source string) graph.ScopeImport {
		return graph.ScopeImport{SourceSpecifier: source, ImportedName: "Token", LocalName: "Token", Kind: graph.ScopeImportNamed}
	}
	copyTree := func(in []file) map[string]file {
		out := make(map[string]file, len(in))
		for _, item := range in {
			out[item.path] = item
		}
		return out
	}
	tests := []struct {
		name  string
		base  []file
		final []file
	}{
		{name: "positive target add", base: []file{class("Other.kt", "pkg", "Other", ""), caller("Token")}, final: []file{class("Other.kt", "pkg", "Other", ""), class("Token.kt", "pkg", "Token", ""), caller("Token")}},
		{name: "positive target change", base: []file{class("Token.kt", "pkg", "Token", ""), caller("Token")}, final: []file{class("Token.kt", "pkg", "Token", "value"), caller("Token")}},
		{name: "positive target delete", base: []file{class("Token.kt", "pkg", "Token", ""), caller("Token")}, final: []file{caller("Token")}},
		{name: "declaration rename", base: []file{class("Token.kt", "pkg", "Token", ""), caller("Token")}, final: []file{class("Token.kt", "pkg", "Renamed", ""), caller("Token")}},
		{name: "provider file rename", base: []file{class("Types.kt", "pkg", "Token", ""), caller("Token")}, final: []file{class("moved/Types.kt", "pkg", "Token", ""), caller("Token")}},
		{name: "alias add", base: []file{class("Token.kt", "pkg", "Token", ""), caller("Alias")}, final: []file{class("Token.kt", "pkg", "Token", ""), alias("Alias.kt", "Alias", "Token"), caller("Alias")}},
		{name: "alias target change", base: []file{class("Token.kt", "pkg", "Token", ""), class("Other.kt", "pkg", "Other", ""), alias("Alias.kt", "Alias", "Token"), caller("Alias")}, final: []file{class("Token.kt", "pkg", "Token", ""), class("Other.kt", "pkg", "Other", ""), alias("Alias.kt", "Alias", "Other"), caller("Alias")}},
		{name: "alias delete", base: []file{class("Token.kt", "pkg", "Token", ""), alias("Alias.kt", "Alias", "Token"), caller("Alias")}, final: []file{class("Token.kt", "pkg", "Token", ""), caller("Alias")}},
		{name: "same package shadow add", base: []file{class("Token.kt", "pkg", "Token", ""), class("Other.kt", "pkg", "Other", ""), caller("Token")}, final: []file{class("Token.kt", "pkg", "Token", ""), class("Other.kt", "pkg", "Other", ""), alias("Shadow.kt", "Token", "Other"), caller("Token")}},
		{name: "same package shadow remove", base: []file{class("Token.kt", "pkg", "Token", ""), class("Other.kt", "pkg", "Other", ""), alias("Shadow.kt", "Token", "Other"), caller("Token")}, final: []file{class("Token.kt", "pkg", "Token", ""), class("Other.kt", "pkg", "Other", ""), caller("Token")}},
		{name: "conflicting import add", base: []file{caller("Token", imp("x.Token"))}, final: []file{caller("Token", imp("x.Token"), imp("y.Token"))}},
		{name: "conflicting import remove", base: []file{caller("Token", imp("x.Token"), imp("y.Token"))}, final: []file{caller("Token", imp("x.Token"))}},
		{name: "ambiguity add", base: []file{class("One.kt", "pkg", "Token", ""), caller("Token")}, final: []file{class("One.kt", "pkg", "Token", ""), class("Two.kt", "pkg", "Token", ""), caller("Token")}},
		{name: "ambiguity remove", base: []file{class("One.kt", "pkg", "Token", ""), class("Two.kt", "pkg", "Token", ""), caller("Token")}, final: []file{class("One.kt", "pkg", "Token", ""), caller("Token")}},
	}
	parse := func(item file) graph.ParsedFile {
		parsed := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: item.pkg, Imports: item.imports}}
		switch item.kind {
		case "class":
			qualified := item.pkg + "." + item.name
			parsed.Symbols = []graph.Symbol{{Language: "kotlin", Kind: "class", Name: item.name, QualifiedName: qualified, StableKey: "type:kotlin:" + qualified}}
			parsed.JVMTypeEvidence = []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: item.pkg, Modifiers: item.modifiers, SyntaxState: "known", Provenance: "test:type"}}
		case "alias":
			parsed.JVMTypeEvidence = []graph.JVMTypeEvidence{{SymbolIndex: -1, EvidenceKey: "typealias:1:" + item.name, OwnerName: item.pkg, Kind: "typealias", AliasTarget: item.target, SyntaxState: "unknown", Provenance: "test:alias"}}
		case "callable":
			parsed.Symbols = []graph.Symbol{{Language: "kotlin", Kind: "function", Name: item.name, QualifiedName: "pkg." + item.name, ContainerName: "pkg", Signature: item.name + "(" + item.syntax + ")", StableKey: "func:kotlin:pkg." + item.name}}
			parsed.JVMTypeEvidence = []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: item.pkg, SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: item.syntax, SyntaxState: "known"}}}}
		default:
			panic("unknown fixture kind " + item.kind)
		}
		return parsed
	}
	openTree := func(t *testing.T, label string, tree map[string]file) (*Store, int64) {
		t.Helper()
		s, err := OpenWithOptions(filepath.Join(t.TempDir(), label+".sqlite"), OpenOptions{PerformanceProfile: "fast"})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := s.UpsertRepo(context.Background(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		paths := make([]string, 0, len(tree))
		for path := range tree {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			item := tree[path]
			if err := s.ReplaceFileGraph(context.Background(), repo.ID, 1, path, "kotlin", 1, 0, path+item.name+item.target+item.modifiers, parse(item)); err != nil {
				s.Close()
				t.Fatal(err)
			}
		}
		setTestJVMCompilationScopeCompleteness(t, context.Background(), s, repo.ID, jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}, "test:complete-oracle")
		if _, err := s.RebuildJVMTypeDependencies(context.Background(), repo.ID, nil); err != nil {
			s.Close()
			t.Fatal(err)
		}
		return s, repo.ID
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base, final := copyTree(tc.base), copyTree(tc.final)
			incremental, repoID := openTree(t, "incremental", base)
			defer incremental.Close()
			var deleted []string
			for path := range base {
				if _, ok := final[path]; !ok {
					deleted = append(deleted, path)
				}
			}
			sort.Strings(deleted)
			if len(deleted) > 0 {
				if _, err := incremental.MarkFilesDeletedBatch(context.Background(), repoID, 2, deleted); err != nil {
					t.Fatal(err)
				}
				if _, err := incremental.PurgeDeletedFileGraphsForScan(context.Background(), repoID, 2); err != nil {
					t.Fatal(err)
				}
			}
			paths := make([]string, 0, len(final))
			for path := range final {
				if old, ok := base[path]; !ok || !reflect.DeepEqual(old, final[path]) {
					paths = append(paths, path)
				}
			}
			sort.Strings(paths)
			for _, path := range paths {
				item := final[path]
				if err := incremental.ReplaceFileGraph(context.Background(), repoID, 2, path, "kotlin", 2, 0, path+item.name+item.target+item.modifiers, parse(item)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := incremental.RebuildJVMTypeDependencies(context.Background(), repoID, nil); err != nil {
				t.Fatal(err)
			}
			fresh, freshRepoID := openTree(t, "fresh", final)
			defer fresh.Close()
			want := normalizedJVMTypeSnapshot(t, fresh.db, freshRepoID)
			got := normalizedJVMTypeSnapshot(t, incremental.db, repoID)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("incremental/fresh semantic snapshots differ\ngot:  %v\nwant: %v", got, want)
			}
			if n, err := incremental.RebuildJVMTypeDependencies(context.Background(), repoID, nil); err != nil || n != 0 {
				t.Fatalf("second incremental update = %d, %v", n, err)
			}
			if n, err := fresh.RebuildJVMTypeDependencies(context.Background(), freshRepoID, nil); err != nil || n != 0 {
				t.Fatalf("second fresh update = %d, %v", n, err)
			}
		})
	}
}

func TestJVMTypeDependencyRetirementLeavesRetryableInvalidation(t *testing.T) {
	f := newGateFixture(t)
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}, "test:complete-oracle")
	provider := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: "Token", QualifiedName: "pkg.Token", StableKey: "type:kotlin:pkg.Token"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:type"}}}
	caller := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Token)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Token", SyntaxState: "known"}}}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Token.kt", "kotlin", 1, 0, "provider", provider); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Caller.kt", "kotlin", 1, 0, "caller", caller); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	retired, err := f.store.RetireFileGraphsBatch(f.ctx, f.repoID, 2, []FileMetadataUpdate{{Path: "Token.kt", Language: "kotlin", SizeBytes: 9, ContentHash: "failed"}}, ParseStateFailed, nil)
	if err != nil || retired != 1 {
		t.Fatalf("retire provider = %d, %v", retired, err)
	}
	var pending, dirty int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM jvm_type_dependency_pending_paths WHERE repo_id=? AND path='Token.kt'`, f.repoID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Caller.kt' AND state='dirty'`, f.repoID).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || dirty == 0 {
		t.Fatalf("retirement recovery marker pending=%d dirty consumers=%d", pending, dirty)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Caller.kt' AND lookup_key=? AND state='current'`, f.repoID, jvmTypeLookupKey("pkg", "Token")).Scan(&kind); err != nil || kind != "negative" {
		t.Fatalf("retired provider retry dependency=%q err=%v", kind, err)
	}
}

func TestJVMTypeDependencyLanguageTransitionInvalidatesOldProvider(t *testing.T) {
	f := newGateFixture(t)
	setTestJVMCompilationScopeCompleteness(t, f.ctx, f.store, f.repoID, jvmCompilationScopeCompleteness{Source: "complete", Generated: "complete", Excluded: "complete", Dependencies: "complete", ExternalMetadata: "complete", CompilerIdentity: "complete"}, "test:complete-oracle")
	provider := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "class", Name: "Token", QualifiedName: "pkg.Token", StableKey: "type:kotlin:pkg.Token"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "class", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:type"}}}
	caller := graph.ParsedFile{Language: "kotlin", Scope: graph.ScopeEvidence{Package: "pkg"}, Symbols: []graph.Symbol{{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "pkg.use", ContainerName: "pkg", Signature: "use(Token)", StableKey: "func:kotlin:pkg.use"}}, JVMTypeEvidence: []graph.JVMTypeEvidence{{SymbolIndex: 0, Kind: "function", OwnerName: "pkg", SyntaxState: "known", Provenance: "test:callable", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Token", SyntaxState: "known"}}}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Provider.kt", "kotlin", 1, 0, "provider", provider); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Caller.kt", "kotlin", 1, 0, "caller", caller); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	retagged := graph.ParsedFile{Language: "python", Symbols: []graph.Symbol{{Language: "python", Kind: "class", Name: "Token", QualifiedName: "pkg.Token", StableKey: "class:python:pkg.Token"}}}
	if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 2, "Provider.kt", "python", 1, 0, "retagged", retagged); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RebuildJVMTypeDependencies(f.ctx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dependency_kind FROM jvm_type_dependencies WHERE repo_id=? AND consumer_path='Caller.kt' AND lookup_key=? AND state='current'`, f.repoID, jvmTypeLookupKey("pkg", "Token")).Scan(&kind); err != nil || kind != "negative" {
		t.Fatalf("language transition dependency=%q err=%v", kind, err)
	}
}
