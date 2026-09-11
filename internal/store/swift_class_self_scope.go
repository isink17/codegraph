package store

import (
	"context"
	"database/sql"
	"sort"
	"strconv"

	"github.com/isink17/codegraph/internal/graph"
)

type swiftClassSelfOwner struct {
	id, file, facts, finals int64
	qname, kind             string
}

type swiftClassSelfRelation struct {
	file, test int64
	child      string
}

type swiftInheritedSelfCandidate struct {
	id, file                  int64
	name, owner, sig          string
	visibility                string
	static                    sql.NullInt64
	arityMin, arityMax        sql.NullInt64
	dispatchFacts, finalFacts int64
	dispatchMin, dispatchMax  string
}

func swiftIsTestFile(tests map[int64]struct{}, file int64) bool {
	_, ok := tests[file]
	return ok
}

func swiftInheritedLoadRelations(ctx context.Context, q javaQuery, repoID int64, names []string) ([]swiftSuperRelation, error) {
	var out []swiftSuperRelation
	err := sqliteBatchedQuery(ctx, q, `SELECT r.file_id,r.child_qualified_name,r.target_qualified_name,r.relation_kind,r.is_generic,r.is_constrained
		FROM swift_inheritance_relations r JOIN files f ON f.id=r.file_id AND f.is_deleted=0
		WHERE r.repo_id=? AND r.child_qualified_name IN (`, `%s)`, []any{repoID}, stringSliceToAny(names), true, func(rows *sql.Rows) error {
		var r swiftSuperRelation
		if err := rows.Scan(&r.file, &r.child, &r.target, &r.kind, &r.generic, &r.constrained); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

func swiftInheritedTrailingCandidate(call swiftSelfCallShape, candidate swiftInheritedSelfCandidate) bool {
	if !candidate.arityMin.Valid || !candidate.arityMax.Valid || candidate.arityMin.Int64 != int64(call.arity) || candidate.arityMax.Int64 != int64(call.arity) {
		return false
	}
	labels, ok := swiftDeclarationLabels(candidate.sig, candidate.name)
	if !ok || len(labels) != call.arity {
		return false
	}
	regular := len(call.regularLabels)
	for i := 0; i < regular; i++ {
		if labels[i] != swiftLabel(call.regularLabels[i]) {
			return false
		}
	}
	for i, label := range call.trailingLabels {
		if i > 0 && labels[regular+i] != label {
			return false
		}
	}
	return true
}

// resolveSwiftClassSelf is deliberately separate from value-type self scope:
// finality and a complete absence of active ancestry/conformance evidence are
// proofs this resolver alone owns.
func (s *Store) resolveSwiftClassSelf(ctx context.Context, q javaQuery, repoID int64, only map[int64]struct{}) (int, error) {
	var edges []swiftScopeEdge
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.src_symbol_id,e.dst_name,e.evidence,src.container_name,src.is_static,e.call_arity,
		COALESCE(sd.facts,0),COALESCE(sd.dispatch_min,''),COALESCE(sd.dispatch_max,'')
		FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols src ON src.id=e.src_symbol_id JOIN files sf ON sf.id=src.file_id
		LEFT JOIN (SELECT symbol_id,COUNT(*) AS facts,MIN(dispatch_kind) AS dispatch_min,MAX(dispatch_kind) AS dispatch_max
			FROM swift_declaration_facts WHERE repo_id=? GROUP BY symbol_id) sd ON sd.symbol_id=src.id
		WHERE e.repo_id=? AND f.language='swift' AND f.is_deleted=0 AND e.edge_kind='calls' AND e.dst_symbol_id IS NULL
		  AND sf.id=e.file_id AND sf.is_deleted=0 AND src.repo_id=e.repo_id AND src.language='swift' AND src.kind='function' AND src.container_name<>''`,
		" AND e.id IN (%s)", []any{repoID, repoID}, int64SliceToAny(sortedIDs(only)), len(only) > 0,
		func(rows *sql.Rows) error {
			var e swiftScopeEdge
			if err := rows.Scan(&e.id, &e.file, &e.src, &e.dst, &e.evidence, &e.owner, &e.static, &e.arity, &e.sourceDispatchFacts, &e.sourceDispatchMin, &e.sourceDispatchMax); err != nil {
				return err
			}
			shape, ok := parseSwiftSelfCallShape(e.evidence, e.dst, e.arity)
			if ok && (shape.strategy == ResolutionStrategySwiftSelfScope || shape.strategy == ResolutionStrategySwiftSelfTypeScope) && e.static.Valid {
				edges = append(edges, e)
			}
			return nil
		}); err != nil {
		return 0, err
	}
	if len(edges) == 0 {
		return 0, nil
	}
	tests, err := testFileIDsForRepo(ctx, q, repoID)
	if err != nil {
		return 0, err
	}

	owners := swiftOwnerKeys(edges)
	var ownerRows []swiftClassSelfOwner
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.qualified_name,s.kind,COUNT(d.id),COALESCE(SUM(CASE WHEN d.is_final=1 THEN 1 ELSE 0 END),0)
		FROM symbols s JOIN files f ON f.id=s.file_id LEFT JOIN swift_declaration_facts d ON d.repo_id=s.repo_id AND d.symbol_id=s.id
		WHERE s.repo_id=? AND s.language='swift' AND f.is_deleted=0 AND s.kind<>'function' AND s.qualified_name IN (`, `%s) GROUP BY s.id`,
		[]any{repoID}, stringSliceToAny(owners), true, func(rows *sql.Rows) error {
			var o swiftClassSelfOwner
			if err := rows.Scan(&o.id, &o.file, &o.qname, &o.kind, &o.facts, &o.finals); err != nil {
				return err
			}
			ownerRows = append(ownerRows, o)
			return nil
		}); err != nil {
		return 0, err
	}

	var relations []swiftClassSelfRelation
	if err := sqliteBatchedQuery(ctx, q, `SELECT r.file_id,r.child_qualified_name FROM swift_inheritance_relations r JOIN files f ON f.id=r.file_id
		WHERE r.repo_id=? AND f.is_deleted=0 AND r.child_qualified_name IN (`, `%s)`, []any{repoID}, stringSliceToAny(owners), true, func(rows *sql.Rows) error {
		var r swiftClassSelfRelation
		if err := rows.Scan(&r.file, &r.child); err != nil {
			return err
		}
		if _, ok := tests[r.file]; ok {
			r.test = 1
		}
		relations = append(relations, r)
		return nil
	}); err != nil {
		return 0, err
	}

	keys := map[string][]any{}
	for _, e := range edges {
		shape, _ := parseSwiftSelfCallShape(e.evidence, e.dst, e.arity)
		signature := swiftSelector(shape.method, shape.regularLabels)
		if len(shape.trailingLabels) > 0 {
			signature = ""
		}
		wantStatic := e.static.Int64
		if shape.strategy == ResolutionStrategySwiftSelfTypeScope {
			wantStatic = 1
		}
		key := swiftCandidateKey(e.owner, shape.method, signature, wantStatic)
		keys[key] = []any{e.owner, shape.method, signature, wantStatic}
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_swift_class_self_candidates(owner TEXT NOT NULL,name TEXT NOT NULL,signature TEXT NOT NULL,is_static INTEGER NOT NULL,PRIMARY KEY(owner,name,signature,is_static)) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	defer q.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_swift_class_self_candidates`)
	if _, err := q.ExecContext(ctx, `DELETE FROM tmp_swift_class_self_candidates`); err != nil {
		return 0, err
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	rows := make([][]any, 0, len(ordered))
	for _, k := range ordered {
		rows = append(rows, keys[k])
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO tmp_swift_class_self_candidates(owner,name,signature,is_static) VALUES `, "(?,?,?,?)", nil, rows); err != nil {
		return 0, err
	}
	var candidates []swiftScopeSymbol
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.name,s.container_name,s.signature,s.kind,s.is_static,s.arity_min,s.arity_max,COUNT(d.id),COALESCE(SUM(CASE WHEN d.is_final=1 THEN 1 ELSE 0 END),0),COALESCE(MIN(d.dispatch_kind),''),COALESCE(MAX(d.dispatch_kind),'')
		FROM symbols s JOIN tmp_swift_class_self_candidates c ON c.owner=s.container_name AND c.name=s.name AND (c.signature='' OR c.signature=s.signature) AND c.is_static=s.is_static
		JOIN files f ON f.id=s.file_id LEFT JOIN swift_declaration_facts d ON d.repo_id=s.repo_id AND d.symbol_id=s.id WHERE s.repo_id=? AND s.language='swift' AND f.is_deleted=0 AND s.kind='function' GROUP BY s.id,s.file_id,s.name,s.container_name,s.signature,s.kind,s.is_static,s.arity_min,s.arity_max`, "", []any{repoID}, nil, false, func(rows *sql.Rows) error {
		var c swiftScopeSymbol
		if err := rows.Scan(&c.id, &c.file, &c.name, &c.owner, &c.sig, &c.kind, &c.static, &c.arityMin, &c.arityMax, &c.dispatchFacts, &c.finalFacts, &c.dispatchMin, &c.dispatchMax); err != nil {
			return err
		}
		candidates = append(candidates, c)
		return nil
	}); err != nil {
		return 0, err
	}

	var blockers []swiftScopeFact
	if err := sqliteBatchedQuery(ctx, q, `SELECT p.file_id,p.owner_module,p.local_name,p.import_kind,p.is_static FROM scope_import_evidence p JOIN files f ON f.id=p.file_id
		WHERE p.repo_id=? AND f.is_deleted=0 AND p.language='swift' AND p.import_kind IN ('swift_member_value','swift_enum_case') AND p.owner_module IN (`, "%s)", []any{repoID}, stringSliceToAny(owners), true, func(rows *sql.Rows) error {
		var b swiftScopeFact
		if err := rows.Scan(&b.file, &b.owner, &b.name, &b.kind, &b.static); err != nil {
			return err
		}
		blockers = append(blockers, b)
		return nil
	}); err != nil {
		return 0, err
	}

	identity := map[string][]swiftClassSelfOwner{}
	for _, o := range ownerRows {
		identity[o.qname] = append(identity[o.qname], o)
	}
	res := map[int64]swiftScopeBinding{}
	for _, e := range edges {
		callerTest := swiftIsTestFile(tests, e.file)
		ownersForName := identity[e.owner]
		count := 0
		var owner swiftClassSelfOwner
		for _, o := range ownersForName {
			if !callerTest {
				if _, test := tests[o.file]; test {
					continue
				}
			}
			count++
			if o.file == e.file {
				owner = o
			}
		}
		if count != 1 || owner.kind != "class" || owner.facts != 1 {
			continue
		}
		hazard := false
		for _, r := range relations {
			if r.child == e.owner && (callerTest || r.test == 0) {
				hazard = true
				break
			}
		}
		if hazard {
			continue
		}
		shape, _ := parseSwiftSelfCallShape(e.evidence, e.dst, e.arity)
		typeContext := shape.strategy == ResolutionStrategySwiftSelfScope && e.static.Int64 == 1 &&
			e.sourceDispatchFacts == 1 && e.sourceDispatchMin == e.sourceDispatchMax &&
			(e.sourceDispatchMin == "static" || e.sourceDispatchMin == "class")
		selector := swiftSelector(shape.method, shape.regularLabels)
		matches := 0
		var found swiftScopeSymbol
		for _, c := range candidates {
			wantStatic := e.static.Int64
			if shape.strategy == ResolutionStrategySwiftSelfTypeScope {
				wantStatic = 1
			}
			if c.owner != e.owner || c.name != shape.method || c.file != e.file || !c.static.Valid || c.static.Int64 != wantStatic {
				continue
			}
			if shape.strategy == ResolutionStrategySwiftSelfTypeScope && (c.dispatchFacts != 1 || c.dispatchMin != c.dispatchMax || (c.dispatchMin != "static" && c.dispatchMin != "class")) {
				continue
			}
			finalClassMethod := c.dispatchFacts == 1 && c.dispatchMin == "class" && c.dispatchMax == "class" && c.finalFacts == 1
			staticMethod := c.dispatchFacts == 1 && c.dispatchMin == "static" && c.dispatchMax == "static"
			if shape.strategy == ResolutionStrategySwiftSelfTypeScope && owner.finals == 0 && !staticMethod && !finalClassMethod {
				continue
			}
			if typeContext && owner.finals == 0 && !staticMethod && !finalClassMethod {
				continue
			}
			if len(shape.trailingLabels) == 0 && c.sig != selector {
				continue
			}
			if len(shape.trailingLabels) > 0 && !swiftTrailingCandidate(shape, c) {
				continue
			}
			matches++
			found = c
		}
		if matches != 1 {
			continue
		}
		staticTypeMethod := shape.strategy == ResolutionStrategySwiftSelfTypeScope && owner.finals == 0 && found.dispatchFacts == 1 && found.dispatchMin == "static" && found.dispatchMax == "static"
		staticTypeFinalClassMethod := shape.strategy == ResolutionStrategySwiftSelfTypeScope && owner.finals == 0 && found.dispatchFacts == 1 && found.finalFacts == 1 && found.dispatchMin == "class" && found.dispatchMax == "class"
		finalMethod := shape.strategy == ResolutionStrategySwiftSelfScope && e.static.Int64 == 0 && owner.finals == 0 && found.dispatchFacts == 1 && found.finalFacts == 1 && found.dispatchMin == "instance" && found.dispatchMax == "instance"
		if owner.finals == 0 && !finalMethod && !typeContext && !staticTypeMethod && !staticTypeFinalClassMethod {
			continue
		}
		blocked := false
		for _, b := range blockers {
			_, blockerTest := tests[b.file]
			wantStatic := e.static.Int64
			if shape.strategy == ResolutionStrategySwiftSelfTypeScope {
				wantStatic = 1
			}
			if b.owner != e.owner || b.name != shape.method || b.static != wantStatic || (b.kind == graph.ScopeImportSwiftEnumCase && b.static != 1) || (!callerTest && blockerTest) {
				continue
			}
			blocked = true
			break
		}
		if !blocked {
			strategy := ResolutionStrategySwiftClassSelfFinalScope
			if shape.strategy == ResolutionStrategySwiftSelfTypeScope {
				if staticTypeMethod {
					strategy = ResolutionStrategySwiftClassSelfTypeStaticMethodScope
				} else if staticTypeFinalClassMethod {
					strategy = ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope
				} else {
					strategy = ResolutionStrategySwiftClassSelfTypeFinalScope
				}
			} else if finalMethod {
				strategy = ResolutionStrategySwiftClassSelfFinalMethodScope
			} else if typeContext && owner.finals == 0 {
				if found.dispatchMin == "class" {
					strategy = ResolutionStrategySwiftClassSelfFinalClassMethodScope
				} else {
					strategy = ResolutionStrategySwiftClassSelfStaticMethodScope
				}
			}
			res[e.id] = swiftScopeBinding{dst: found.id, strategy: strategy}
		}
	}
	n, err := swiftScopeApply(ctx, q, res)
	if err != nil {
		return 0, err
	}
	inherited, err := s.resolveSwiftClassSelfInheritedFinalMethod(ctx, q, repoID, only)
	return n + inherited, err
}

// resolveSwiftClassSelfInheritedFinalMethod is intentionally narrower than the
// ordinary class-self pass: one exact child class, one direct superclass
// relation, and one same-file final instance method. It never walks the
// hierarchy; any missing or competing fact leaves the edge unresolved.
func (s *Store) resolveSwiftClassSelfInheritedFinalMethod(ctx context.Context, q javaQuery, repoID int64, only map[int64]struct{}) (int, error) {
	var edges []swiftScopeEdge
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.src_symbol_id,e.dst_name,e.evidence,src.container_name,src.is_static,e.call_arity,
		COALESCE(sd.facts,0),COALESCE(sd.dispatch_min,''),COALESCE(sd.dispatch_max,'')
		FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols src ON src.id=e.src_symbol_id JOIN files sf ON sf.id=src.file_id
		LEFT JOIN (SELECT symbol_id,COUNT(*) AS facts,MIN(dispatch_kind) AS dispatch_min,MAX(dispatch_kind) AS dispatch_max
			FROM swift_declaration_facts WHERE repo_id=? GROUP BY symbol_id) sd ON sd.symbol_id=src.id
		WHERE e.repo_id=? AND f.language='swift' AND f.is_deleted=0 AND e.edge_kind='calls' AND e.dst_symbol_id IS NULL
		  AND sf.id=e.file_id AND sf.is_deleted=0 AND src.repo_id=e.repo_id AND src.language='swift' AND src.kind='function' AND src.container_name<>'' AND src.is_static=0`,
		" AND e.id IN (%s)", []any{repoID, repoID}, int64SliceToAny(sortedIDs(only)), len(only) > 0,
		func(rows *sql.Rows) error {
			var e swiftScopeEdge
			if err := rows.Scan(&e.id, &e.file, &e.src, &e.dst, &e.evidence, &e.owner, &e.static, &e.arity, &e.sourceDispatchFacts, &e.sourceDispatchMin, &e.sourceDispatchMax); err != nil {
				return err
			}
			shape, ok := parseSwiftSelfCallShape(e.evidence, e.dst, e.arity)
			if ok && shape.strategy == ResolutionStrategySwiftSelfScope && e.static.Valid && e.static.Int64 == 0 {
				edges = append(edges, e)
			}
			return nil
		}); err != nil {
		return 0, err
	}
	if len(edges) == 0 {
		return 0, nil
	}
	tests, err := testFileIDsForRepo(ctx, q, repoID)
	if err != nil {
		return 0, err
	}

	owners := swiftOwnerKeys(edges)
	var classes []swiftClassSelfOwner
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.qualified_name,s.kind,COUNT(d.id),COALESCE(SUM(CASE WHEN d.is_final=1 THEN 1 ELSE 0 END),0)
		FROM symbols s JOIN files f ON f.id=s.file_id LEFT JOIN swift_declaration_facts d ON d.repo_id=s.repo_id AND d.symbol_id=s.id
		WHERE s.repo_id=? AND s.language='swift' AND f.is_deleted=0 AND s.kind='class' AND s.qualified_name IN (`, `%s) GROUP BY s.id`,
		[]any{repoID}, stringSliceToAny(owners), true, func(rows *sql.Rows) error {
			var c swiftClassSelfOwner
			if err := rows.Scan(&c.id, &c.file, &c.qname, &c.kind, &c.facts, &c.finals); err != nil {
				return err
			}
			classes = append(classes, c)
			return nil
		}); err != nil {
		return 0, err
	}

	childNames := map[string]struct{}{}
	for _, e := range edges {
		childNames[e.owner] = struct{}{}
	}
	relations, err := swiftInheritedLoadRelations(ctx, q, repoID, sortedSwiftSet(childNames))
	if err != nil {
		return 0, err
	}

	type pending struct {
		edge     swiftScopeEdge
		relation swiftSuperRelation
	}
	valid := make([]pending, 0, len(edges))
	baseNames := map[string]struct{}{}
	for _, e := range edges {
		callerTest := false
		_, callerTest = tests[e.file]
		var child swiftClassSelfOwner
		count := 0
		for _, c := range classes {
			if c.qname != e.owner || (!callerTest && swiftIsTestFile(tests, c.file)) {
				continue
			}
			count++
			if c.file == e.file {
				child = c
			}
		}
		if count != 1 || child.kind != "class" || child.facts != 1 || child.finals != 0 {
			continue
		}
		var direct []swiftSuperRelation
		hazard := false
		for _, r := range relations {
			if r.child != e.owner || (!callerTest && swiftIsTestFile(tests, r.file)) {
				continue
			}
			if r.generic != 0 || r.constrained != 0 || r.kind == "conformance" || r.kind == "unproven" || r.kind != "superclass" {
				hazard = true
			}
			if r.kind == "superclass" {
				direct = append(direct, r)
			}
		}
		if hazard || len(direct) != 1 || direct[0].file != e.file {
			continue
		}
		baseNames[direct[0].target] = struct{}{}
		valid = append(valid, pending{edge: e, relation: direct[0]})
	}
	if len(valid) == 0 {
		return 0, nil
	}

	bases, err := swiftSuperLoadClasses(ctx, q, repoID, sortedSwiftSet(baseNames))
	if err != nil {
		return 0, err
	}
	baseByName := map[string][]swiftSuperClass{}
	for _, b := range bases {
		baseByName[b.qname] = append(baseByName[b.qname], b)
	}
	valid2 := valid[:0]
	for _, p := range valid {
		callerTest := swiftIsTestFile(tests, p.edge.file)
		count := 0
		var base swiftSuperClass
		for _, b := range baseByName[p.relation.target] {
			if !callerTest {
				if _, ok := tests[b.file]; ok {
					continue
				}
			}
			count++
			if b.file == p.edge.file {
				base = b
			}
		}
		if count == 1 && base.kind == "class" && base.file == p.edge.file {
			valid2 = append(valid2, p)
		}
	}
	if len(valid2) == 0 {
		return 0, nil
	}

	keys := map[string][]any{}
	for _, p := range valid2 {
		shape, _ := parseSwiftSelfCallShape(p.edge.evidence, p.edge.dst, p.edge.arity)
		signature := swiftSelector(shape.method, shape.regularLabels)
		if len(shape.trailingLabels) > 0 {
			signature = ""
		}
		key := swiftCandidateKey(p.relation.target, shape.method, signature, 0) + "\x00" + strconv.FormatInt(p.edge.file, 10)
		keys[key] = []any{p.edge.file, p.relation.target, shape.method, signature}
		childKey := swiftCandidateKey(p.edge.owner, shape.method, signature, 0) + "\x00" + strconv.FormatInt(p.edge.file, 10)
		keys[childKey] = []any{p.edge.file, p.edge.owner, shape.method, signature}
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_swift_inherited_self_candidates(file_id INTEGER NOT NULL,owner TEXT NOT NULL,name TEXT NOT NULL,signature TEXT NOT NULL,PRIMARY KEY(file_id,owner,name,signature)) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	defer q.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_swift_inherited_self_candidates`)
	if _, err := q.ExecContext(ctx, `DELETE FROM tmp_swift_inherited_self_candidates`); err != nil {
		return 0, err
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	rows := make([][]any, 0, len(ordered))
	for _, key := range ordered {
		rows = append(rows, keys[key])
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO tmp_swift_inherited_self_candidates(file_id,owner,name,signature) VALUES `, "(?,?,?,?)", nil, rows); err != nil {
		return 0, err
	}
	var candidates []swiftInheritedSelfCandidate
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.name,s.container_name,s.signature,s.visibility,s.is_static,s.arity_min,s.arity_max,COUNT(d.id),COALESCE(SUM(CASE WHEN d.is_final=1 THEN 1 ELSE 0 END),0),COALESCE(MIN(d.dispatch_kind),''),COALESCE(MAX(d.dispatch_kind),'')
		FROM symbols s JOIN tmp_swift_inherited_self_candidates c ON c.file_id=s.file_id AND c.owner=s.container_name AND c.name=s.name AND (c.signature='' OR c.signature=s.signature)
		JOIN files f ON f.id=s.file_id LEFT JOIN swift_declaration_facts d ON d.repo_id=s.repo_id AND d.symbol_id=s.id
		WHERE s.repo_id=? AND s.language='swift' AND f.is_deleted=0 AND s.kind='function' AND s.is_static=0
		GROUP BY s.id,s.file_id,s.name,s.container_name,s.signature,s.visibility,s.kind,s.is_static,s.arity_min,s.arity_max`, "", []any{repoID}, nil, false, func(rows *sql.Rows) error {
		var c swiftInheritedSelfCandidate
		if err := rows.Scan(&c.id, &c.file, &c.name, &c.owner, &c.sig, &c.visibility, &c.static, &c.arityMin, &c.arityMax, &c.dispatchFacts, &c.finalFacts, &c.dispatchMin, &c.dispatchMax); err != nil {
			return err
		}
		candidates = append(candidates, c)
		return nil
	}); err != nil {
		return 0, err
	}

	blockers := map[string][]swiftScopeFact{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT p.file_id,p.owner_module,p.local_name,p.import_kind,p.is_static FROM scope_import_evidence p JOIN files f ON f.id=p.file_id
		WHERE p.repo_id=? AND f.is_deleted=0 AND p.language='swift' AND p.import_kind IN ('swift_member_value','swift_enum_case') AND p.owner_module IN (`, "%s)", []any{repoID}, stringSliceToAny(owners), true, func(rows *sql.Rows) error {
		var b swiftScopeFact
		if err := rows.Scan(&b.file, &b.owner, &b.name, &b.kind, &b.static); err != nil {
			return err
		}
		blockers[b.owner+"\x00"+b.name] = append(blockers[b.owner+"\x00"+b.name], b)
		return nil
	}); err != nil {
		return 0, err
	}

	res := map[int64]swiftScopeBinding{}
	for _, p := range valid2 {
		shape, _ := parseSwiftSelfCallShape(p.edge.evidence, p.edge.dst, p.edge.arity)
		matches := 0
		veto := false
		var found swiftInheritedSelfCandidate
		for _, c := range candidates {
			if c.file != p.edge.file || c.name != shape.method || !c.static.Valid || c.static.Int64 != 0 {
				continue
			}
			selector := swiftSelector(shape.method, shape.regularLabels)
			if len(shape.trailingLabels) == 0 && c.sig != selector {
				continue
			}
			if len(shape.trailingLabels) > 0 && !swiftInheritedTrailingCandidate(shape, c) {
				continue
			}
			if c.owner == p.edge.owner {
				veto = true
				continue
			}
			if c.owner != p.relation.target {
				continue
			}
			if c.visibility == "private" || c.dispatchFacts != 1 || c.finalFacts != 1 || c.dispatchMin != "instance" || c.dispatchMax != "instance" {
				veto = true
				continue
			}
			matches++
			found = c
		}
		if veto || matches != 1 {
			continue
		}
		blocked := false
		for _, b := range blockers[p.edge.owner+"\x00"+shape.method] {
			if b.static != 0 || b.kind == graph.ScopeImportSwiftEnumCase || (!swiftIsTestFile(tests, p.edge.file) && swiftIsTestFile(tests, b.file)) {
				continue
			}
			blocked = true
			break
		}
		if !blocked {
			res[p.edge.id] = swiftScopeBinding{dst: found.id, strategy: ResolutionStrategySwiftClassSelfInheritedFinalMethodScope}
		}
	}
	return swiftScopeApply(ctx, q, res)
}
