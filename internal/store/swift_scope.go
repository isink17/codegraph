package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/isink17/codegraph/internal/graph"
)

const swiftSelfRepairSettingKey = "resolver.swift_explicit_self_repaired.v1"
const swiftClassSelfRepairSettingKey = "resolver.swift_class_self_final_repaired.v1"
const swiftClassSelfTypeRepairSettingKey = "resolver.swift_class_self_type_final_repaired.v1"
const swiftClassSelfFinalMethodRepairSettingKey = "resolver.swift_class_self_final_method_repaired.v1"
const swiftClassSelfStaticMethodRepairSettingKey = "resolver.swift_class_self_static_method_repaired.v1"
const swiftClassSelfFinalClassMethodRepairSettingKey = "resolver.swift_class_self_final_class_method_repaired.v1"
const swiftClassSelfTypeStaticMethodRepairSettingKey = "resolver.swift_class_self_type_static_method_repaired.v1"
const swiftClassSelfTypeFinalClassMethodRepairSettingKey = "resolver.swift_class_self_type_final_class_method_repaired.v1"
const swiftClassSelfInheritedFinalMethodRepairSettingKey = "resolver.swift_class_self_inherited_final_method_repaired.v1"
const swiftClassSelfTypeInheritedStaticMethodRepairSettingKey = "resolver.swift_class_self_type_inherited_static_method_repaired.v1"
const swiftTrailingRepairSettingKey = "resolver.swift_trailing_closure_repaired.v1"

var swiftSelfStrategies = []string{ResolutionStrategySwiftSelfScope, ResolutionStrategySwiftSelfTypeScope, ResolutionStrategySwiftClassSelfFinalScope, ResolutionStrategySwiftClassSelfTypeFinalScope, ResolutionStrategySwiftClassSelfFinalMethodScope, ResolutionStrategySwiftClassSelfStaticMethodScope, ResolutionStrategySwiftClassSelfFinalClassMethodScope, ResolutionStrategySwiftClassSelfTypeStaticMethodScope, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope}

// Swift v3 call facts are owned here. Unsupported Swift calls must never fall
// through to a name-based resolver.
const swiftScopeVetoSQL = `NOT (f.language = 'swift' AND edges.edge_kind = 'calls' AND substr(edges.evidence, 1, 6) = 'swift:')`

type swiftScopeEdge struct {
	id, file, src        int64
	dst, evidence, owner string
	static               sql.NullInt64
	arity                sql.NullInt64
	sourceDispatchFacts  int64
	sourceDispatchMin    string
	sourceDispatchMax    string
}

type swiftSelfCallShape struct {
	method, strategy              string
	regularLabels, trailingLabels []string
	arity                         int
}

type swiftScopeSymbol struct {
	id, file                 int64
	name, owner, sig, kind   string
	static                   sql.NullInt64
	arityMin, arityMax       sql.NullInt64
	dispatchFacts            int64
	finalFacts               int64
	dispatchMin, dispatchMax string
}

type swiftScopeFact struct {
	file, static      int64
	owner, name, kind string
}

func swiftScopeOwned(t edgeTarget) bool {
	return t.srcLanguage == "swift" && t.edgeKind == EdgeKindCalls && strings.HasPrefix(t.evidence, "swift:")
}

func swiftIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && r != '_' && !unicode.IsLetter(r) {
			return false
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func parseSwiftSelfCallShape(evidence, dst string, arity sql.NullInt64) (swiftSelfCallShape, bool) {
	shape := swiftSelfCallShape{}
	prefix, strategy := "swift:self", ResolutionStrategySwiftSelfScope
	if evidence == "swift:Self" || strings.HasPrefix(evidence, "swift:Self;") {
		prefix, strategy = "swift:Self", ResolutionStrategySwiftSelfTypeScope
	}
	if evidence != prefix && !strings.HasPrefix(evidence, prefix+";") {
		return shape, false
	}
	parts := strings.Split(evidence, ";")
	seenLabels, seenTrailing := false, false
	for _, part := range parts[1:] {
		var dstLabels *[]string
		switch {
		case strings.HasPrefix(part, "labels=") && !seenLabels && !seenTrailing:
			seenLabels, dstLabels = true, &shape.regularLabels
		case strings.HasPrefix(part, "trailing_labels=") && !seenTrailing:
			seenTrailing, dstLabels = true, &shape.trailingLabels
		default:
			return swiftSelfCallShape{}, false
		}
		value := strings.TrimPrefix(part, "labels=")
		if dstLabels == &shape.trailingLabels {
			value = strings.TrimPrefix(part, "trailing_labels=")
		}
		if value == "" {
			return swiftSelfCallShape{}, false
		}
		for _, label := range strings.Split(value, ",") {
			if label != "_" && (!strings.HasSuffix(label, ":") || !swiftIdentifier(strings.TrimSuffix(label, ":"))) {
				return swiftSelfCallShape{}, false
			}
			*dstLabels = append(*dstLabels, label)
		}
	}
	if len(shape.trailingLabels) > 0 {
		if shape.trailingLabels[0] != "_" {
			return swiftSelfCallShape{}, false
		}
		for _, label := range shape.trailingLabels[1:] {
			if label == "_" {
				return swiftSelfCallShape{}, false
			}
		}
	}
	want := strings.TrimPrefix(prefix, "swift:") + "."
	if !strings.HasPrefix(dst, want) {
		return swiftSelfCallShape{}, false
	}
	shape.method, shape.strategy = strings.TrimPrefix(dst, want), strategy
	if !swiftIdentifier(shape.method) || shape.method == "init" || shape.method == "deinit" || shape.method == "subscript" || strings.Contains(shape.method, ".") || !arity.Valid {
		return swiftSelfCallShape{}, false
	}
	shape.arity = int(arity.Int64)
	if shape.arity != len(shape.regularLabels)+len(shape.trailingLabels) {
		return swiftSelfCallShape{}, false
	}
	return shape, true
}

func swiftSelfCall(evidence, dst string, arity sql.NullInt64) (method, strategy string, labels []string, ok bool) {
	shape, parsed := parseSwiftSelfCallShape(evidence, dst, arity)
	return shape.method, shape.strategy, append(shape.regularLabels, shape.trailingLabels...), parsed && len(shape.trailingLabels) == 0
}

func (s *Store) resolveSwiftScope(ctx context.Context, q javaQuery, repoID int64, only map[int64]struct{}) (int, error) {
	var edges []swiftScopeEdge
	err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.src_symbol_id,e.dst_name,e.evidence,src.container_name,src.is_static,e.call_arity
	FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols src ON src.id=e.src_symbol_id JOIN files sf ON sf.id=src.file_id
WHERE e.repo_id=? AND f.language='swift' AND f.is_deleted=0 AND e.edge_kind='calls' AND e.dst_symbol_id IS NULL
	  AND sf.id=e.file_id AND sf.is_deleted=0 AND src.repo_id=e.repo_id AND src.language='swift' AND src.kind='function' AND src.container_name<>''`,
		" AND e.id IN (%s)", []any{repoID}, int64SliceToAny(sortedIDs(only)), len(only) > 0,
		func(rows *sql.Rows) error {
			var e swiftScopeEdge
			if err := rows.Scan(&e.id, &e.file, &e.src, &e.dst, &e.evidence, &e.owner, &e.static, &e.arity); err != nil {
				return err
			}
			edges = append(edges, e)
			return nil
		})
	if err != nil {
		return 0, err
	}
	valid := make([]swiftScopeEdge, 0, len(edges))
	candidateKeys := map[string][]any{}
	for _, e := range edges {
		shape, ok := parseSwiftSelfCallShape(e.evidence, e.dst, e.arity)
		if !ok {
			continue
		}
		if !e.static.Valid {
			continue
		}
		static := e.static.Int64
		if shape.strategy == ResolutionStrategySwiftSelfTypeScope {
			static = 1
		}
		signature := swiftSelector(shape.method, shape.regularLabels)
		if len(shape.trailingLabels) > 0 {
			signature = ""
		}
		candidateKeys[swiftCandidateKey(e.owner, shape.method, signature, static)] = []any{e.owner, shape.method, signature, static}
		valid = append(valid, e)
	}
	if len(valid) == 0 {
		return 0, nil
	}
	tests, err := testFileIDsForRepo(ctx, q, repoID)
	if err != nil {
		return 0, err
	}
	ownerCount := map[string]int{}
	allowedOwners := map[string]int{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.file_id,s.qualified_name,s.kind FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND s.language='swift' AND f.is_deleted=0 AND s.kind<>'function' AND s.qualified_name IN (`, "%s)", []any{repoID}, stringSliceToAny(swiftOwnerKeys(valid)), true, func(rows *sql.Rows) error {
		var file int64
		var owner, kind string
		if err := rows.Scan(&file, &owner, &kind); err != nil {
			return err
		}
		key := owner + "\x00" + strconv.FormatInt(file, 10)
		ownerCount[key]++
		if kind == "struct" || kind == "enum" || kind == "actor" {
			allowedOwners[key]++
		}
		return nil
	}); err != nil {
		return 0, err
	}
	// Load all relevant callable symbols and facts once. Cross-file rows are
	// intentional: module identity is unknown, so exact competitors/blockers veto.
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_swift_scope_candidates(owner TEXT NOT NULL,name TEXT NOT NULL,signature TEXT NOT NULL,is_static INTEGER NOT NULL,PRIMARY KEY(owner,name,signature,is_static)) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM tmp_swift_scope_candidates`); err != nil {
		return 0, err
	}
	candidateNames := make([]string, 0, len(candidateKeys))
	for key := range candidateKeys {
		candidateNames = append(candidateNames, key)
	}
	sort.Strings(candidateNames)
	candidateRows := make([][]any, 0, len(candidateKeys))
	for _, key := range candidateNames {
		candidateRows = append(candidateRows, candidateKeys[key])
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO tmp_swift_scope_candidates(owner,name,signature,is_static) VALUES `, "(?,?,?,?)", nil, candidateRows); err != nil {
		return 0, err
	}
	var symbols []swiftScopeSymbol
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.name,s.container_name,s.signature,s.kind,s.is_static,s.arity_min,s.arity_max,COUNT(d.id),COALESCE(SUM(CASE WHEN d.is_final=1 THEN 1 ELSE 0 END),0),COALESCE(MIN(d.dispatch_kind),''),COALESCE(MAX(d.dispatch_kind),'') FROM symbols s JOIN tmp_swift_scope_candidates c ON c.owner=s.container_name AND c.name=s.name AND (c.signature='' OR c.signature=s.signature) AND c.is_static=s.is_static JOIN files f ON f.id=s.file_id LEFT JOIN swift_declaration_facts d ON d.repo_id=s.repo_id AND d.symbol_id=s.id WHERE s.repo_id=? AND s.language='swift' AND f.is_deleted=0 AND s.kind='function' GROUP BY s.id,s.file_id,s.name,s.container_name,s.signature,s.kind,s.is_static,s.arity_min,s.arity_max`, "", []any{repoID}, nil, false, func(rows *sql.Rows) error {
		var x swiftScopeSymbol
		if err := rows.Scan(&x.id, &x.file, &x.name, &x.owner, &x.sig, &x.kind, &x.static, &x.arityMin, &x.arityMax, &x.dispatchFacts, &x.finalFacts, &x.dispatchMin, &x.dispatchMax); err != nil {
			return err
		}
		symbols = append(symbols, x)
		return nil
	}); err != nil {
		return 0, err
	}
	_, _ = q.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_swift_scope_candidates`)
	var facts []swiftScopeFact
	if err := sqliteBatchedQuery(ctx, q, `SELECT p.file_id,p.owner_module,p.local_name,p.import_kind,p.is_static FROM scope_import_evidence p JOIN files f ON f.id=p.file_id WHERE p.repo_id=? AND f.is_deleted=0 AND p.language='swift' AND p.import_kind IN ('swift_member_value','swift_enum_case') AND p.owner_module IN (`, "%s)", []any{repoID}, stringSliceToAny(swiftOwnerKeys(valid)), true, func(rows *sql.Rows) error {
		var x swiftScopeFact
		if err := rows.Scan(&x.file, &x.owner, &x.name, &x.kind, &x.static); err != nil {
			return err
		}
		facts = append(facts, x)
		return nil
	}); err != nil {
		return 0, err
	}
	blockedAny := map[string]bool{}
	blockedProd := map[string]bool{}
	for _, f := range facts {
		if f.kind != graph.ScopeImportSwiftMemberValue && f.kind != graph.ScopeImportSwiftEnumCase {
			continue
		}
		if f.kind == graph.ScopeImportSwiftEnumCase && f.static != 1 {
			continue
		}
		key := swiftCandidateKey(f.owner, f.name, "", f.static)
		blockedAny[key] = true
		if _, test := tests[f.file]; !test {
			blockedProd[key] = true
		}
	}
	res := map[int64]swiftScopeBinding{}
	for _, e := range valid {
		shape, _ := parseSwiftSelfCallShape(e.evidence, e.dst, e.arity)
		method, strategy := shape.method, shape.strategy
		selector := swiftSelector(method, shape.regularLabels)
		ownerKey := e.owner + "\x00" + strconv.FormatInt(e.file, 10)
		if ownerCount[ownerKey] != 1 || allowedOwners[ownerKey] != 1 {
			continue
		}
		wantStatic := e.static.Int64
		if strategy == ResolutionStrategySwiftSelfTypeScope {
			wantStatic = 1
		}
		var found swiftScopeSymbol
		count := 0
		for _, c := range symbols {
			if c.owner != e.owner || c.name != method || !c.static.Valid || c.static.Int64 != wantStatic {
				continue
			}
			if len(shape.trailingLabels) == 0 {
				if c.sig != selector {
					continue
				}
			} else if !swiftTrailingCandidate(shape, c) {
				continue
			}
			if _, test := tests[c.file]; test {
				if _, callerTest := tests[e.file]; !callerTest {
					continue
				}
			}
			count++
			found = c
		}
		if count != 1 || found.file != e.file {
			continue
		}
		_, callerTest := tests[e.file]
		blockKey := swiftCandidateKey(e.owner, method, "", wantStatic)
		if (callerTest && blockedAny[blockKey]) || (!callerTest && blockedProd[blockKey]) {
			continue
		}
		res[e.id] = swiftScopeBinding{dst: found.id, strategy: strategy}
	}
	return swiftScopeApply(ctx, q, res)
}

func swiftTrailingCandidate(call swiftSelfCallShape, candidate swiftScopeSymbol) bool {
	if !candidate.arityMin.Valid || !candidate.arityMax.Valid || candidate.arityMin.Int64 != int64(call.arity) || candidate.arityMax.Int64 != int64(call.arity) {
		return false
	}
	labels, ok := swiftDeclarationLabels(candidate.sig, candidate.name)
	if !ok || len(labels) != call.arity {
		return false
	}
	r := len(call.regularLabels)
	for i := 0; i < r; i++ {
		if labels[i] != swiftLabel(call.regularLabels[i]) {
			return false
		}
	}
	for i, label := range call.trailingLabels {
		if i > 0 && labels[r+i] != label {
			return false
		}
	}
	return true
}

func swiftLabel(label string) string {
	if label == "_" {
		return "_:"
	}
	return label
}

func swiftDeclarationLabels(signature, method string) ([]string, bool) {
	prefix := method + "("
	if !strings.HasPrefix(signature, prefix) || !strings.HasSuffix(signature, ")") {
		return nil, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(signature, prefix), ")")
	if body == "" {
		return nil, true
	}
	labels := strings.Split(body, ",")
	for _, label := range labels {
		if label != "_:" && (!strings.HasSuffix(label, ":") || !swiftIdentifier(strings.TrimSuffix(label, ":"))) {
			return nil, false
		}
	}
	return labels, true
}

func swiftOwnerKeys(edges []swiftScopeEdge) []string {
	set := map[string]struct{}{}
	for _, e := range edges {
		set[e.owner] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func swiftCandidateKey(owner, name, signature string, static int64) string {
	return owner + "\x00" + name + "\x00" + signature + "\x00" + strconv.FormatInt(static, 10)
}
func swiftSelector(method string, labels []string) string {
	if len(labels) == 0 {
		return method + "()"
	}
	labels = append([]string(nil), labels...)
	for i, label := range labels {
		if label == "_" {
			labels[i] = "_:"
		}
	}
	return method + "(" + strings.Join(labels, ",") + ")"
}
func strconvNull(v sql.NullInt64) string {
	if !v.Valid {
		return "?"
	}
	return fmt.Sprint(v.Int64)
}

type swiftScopeBinding struct {
	dst      int64
	strategy string
}

func swiftScopeApply(ctx context.Context, q javaQuery, res map[int64]swiftScopeBinding) (int, error) {
	if len(res) == 0 {
		return 0, nil
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_swift_scope_resolution(edge_id INTEGER PRIMARY KEY,dst_symbol_id INTEGER NOT NULL,strategy TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM tmp_swift_scope_resolution`); err != nil {
		return 0, err
	}
	ids := make([]int64, 0, len(res))
	for id := range res {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows := make([][]any, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, []any{id, res[id].dst, res[id].strategy})
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO tmp_swift_scope_resolution(edge_id,dst_symbol_id,strategy) VALUES `, "(?,?,?)", nil, rows); err != nil {
		return 0, err
	}
	_, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT dst_symbol_id FROM tmp_swift_scope_resolution r WHERE r.edge_id=edges.id),resolution_strategy=(SELECT strategy FROM tmp_swift_scope_resolution r WHERE r.edge_id=edges.id),resolution_confidence='high' WHERE id IN (SELECT edge_id FROM tmp_swift_scope_resolution)`)
	_, _ = q.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_swift_scope_resolution`)
	return len(res), err
}

func (s *Store) resolveSwiftScopeStandalone(ctx context.Context, repoID int64, only map[int64]struct{}) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := s.resolveSwiftInitializerScope(ctx, tx, repoID, only)
	if err == nil {
		var self int
		self, err = s.resolveSwiftScope(ctx, tx, repoID, only)
		n += self
		if err == nil {
			var classSelf int
			classSelf, err = s.resolveSwiftClassSelf(ctx, tx, repoID, only)
			n += classSelf
		}
		if err == nil {
			var super int
			super, err = s.resolveSwiftSuperScope(ctx, tx, repoID, only)
			n += super
		}
	}
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
func (s *Store) repairSwiftSelfBindings(ctx context.Context, repoID int64) error {
	return s.redecideSwiftSelfBindings(ctx, repoID)
}

func (s *Store) repairSwiftTrailingBindings(ctx context.Context, repoID int64) error {
	return s.redecideSwiftSelfBindings(ctx, repoID)
}

func (s *Store) swiftTrailingRepairApplies(ctx context.Context, repoID int64) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM edges e JOIN files f ON f.id=e.file_id
		WHERE e.repo_id=? AND f.repo_id=e.repo_id AND f.language='swift' AND f.is_deleted=0
		  AND e.edge_kind='calls'
		  AND (e.evidence LIKE 'swift:self;trailing_labels=%' OR e.evidence LIKE 'swift:self;labels=%;trailing_labels=%'
		       OR e.evidence LIKE 'swift:Self;trailing_labels=%' OR e.evidence LIKE 'swift:Self;labels=%;trailing_labels=%')
	)`, repoID).Scan(&found)
	return found, err
}

func (s *Store) swiftPathsChanged(ctx context.Context, repoID int64, paths []string) (bool, error) {
	if len(paths) == 0 {
		return false, nil
	}
	var changed bool
	stored := make([]string, 0, len(paths)*3)
	for _, path := range paths {
		stored = append(stored, storedPathVariants(CanonicalRelPath(path))...)
	}
	err := sqliteBatchedQuery(ctx, s.db, `SELECT EXISTS(SELECT 1 FROM files WHERE repo_id=? AND language='swift' AND path IN (`, "%s))", []any{repoID}, stringSliceToAny(stored), true, func(rows *sql.Rows) error {
		var hit bool
		if err := rows.Scan(&hit); err != nil {
			return err
		}
		changed = changed || hit
		return nil
	})
	return changed, err
}

func (s *Store) redecideSwiftSelfBindings(ctx context.Context, repoID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+` WHERE repo_id=? AND edge_kind='calls' AND (evidence LIKE 'swift:self%' OR evidence LIKE 'swift:Self%')`, repoID); err != nil {
		return err
	}
	if _, err := s.resolveSwiftScope(ctx, tx, repoID, nil); err != nil {
		return err
	}
	if _, err := s.resolveSwiftClassSelf(ctx, tx, repoID, nil); err != nil {
		return err
	}
	return tx.Commit()
}
