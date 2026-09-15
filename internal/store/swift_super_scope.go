package store

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

const swiftSuperRepairSettingKey = "resolver.swift_super_method_repaired.v1"
const swiftSuperMultilevelInheritedMethodRepairSettingKey = "resolver.swift_super_multilevel_inherited_method_repaired.v1"
const swiftSuperTypeMethodRepairSettingKey = "resolver.swift_super_type_method_repaired.v1"
const swiftSuperExtensionMethodRepairSettingKey = "resolver.swift_super_extension_method_repaired.v1"

type swiftSuperSourceMode uint8

const (
	swiftSuperInstanceSource swiftSuperSourceMode = iota + 1
	swiftSuperTypeSource
)

type swiftSuperCallShape struct {
	method, strategy              string
	regularLabels, trailingLabels []string
	arity                         int
}

type swiftSuperEdge struct {
	id, file, src, startLine, startCol, endLine, endCol int64
	dst, evidence, owner, name                          string
	static, arity                                       sql.NullInt64
}

type swiftSuperExtensionMembership struct {
	file, startLine, startCol, endLine, endCol int64
	target                                     string
	generic, constrained, count                int64
}

type swiftSuperClass struct {
	id, file, startLine, startCol, endLine, endCol int64
	qname, kind, visibility                        string
}

type swiftSuperCandidate struct {
	id, file, startLine, startCol, endLine, endCol int64
	owner, name, signature, visibility, dispatch   string
	static, min, max                               sql.NullInt64
	isOverride, isFinal                            sql.NullInt64
	factCount                                      int
}

type swiftSuperRelation struct {
	file                 int64
	child, target, kind  string
	generic, constrained int64
}

type swiftSuperBlocker struct {
	file   int64
	static int64
	kind   string
}

func parseSwiftSuperCallShape(evidence, dst string, arity sql.NullInt64) (swiftSuperCallShape, bool) {
	var shape swiftSuperCallShape
	if evidence != "swift:super" && !strings.HasPrefix(evidence, "swift:super;") || !arity.Valid || arity.Int64 < 0 {
		return shape, false
	}
	seenLabels, seenTrailing := false, false
	seenNamedLabels := map[string]struct{}{}
	for _, part := range strings.Split(evidence, ";")[1:] {
		var dstLabels *[]string
		switch {
		case strings.HasPrefix(part, "labels=") && !seenLabels && !seenTrailing:
			seenLabels, dstLabels = true, &shape.regularLabels
		case strings.HasPrefix(part, "trailing_labels=") && !seenTrailing:
			seenTrailing, dstLabels = true, &shape.trailingLabels
		default:
			return swiftSuperCallShape{}, false
		}
		value := strings.TrimPrefix(part, "labels=")
		if dstLabels == &shape.trailingLabels {
			value = strings.TrimPrefix(part, "trailing_labels=")
		}
		if value == "" {
			return swiftSuperCallShape{}, false
		}
		for i, label := range strings.Split(value, ",") {
			if label != "_" && (!strings.HasSuffix(label, ":") || !swiftIdentifier(strings.TrimSuffix(label, ":"))) {
				return swiftSuperCallShape{}, false
			}
			if label != "_" {
				if _, exists := seenNamedLabels[label]; exists {
					return swiftSuperCallShape{}, false
				}
				seenNamedLabels[label] = struct{}{}
			}
			if dstLabels == &shape.trailingLabels && ((i == 0 && label != "_") || (i > 0 && label == "_")) {
				return swiftSuperCallShape{}, false
			}
			*dstLabels = append(*dstLabels, label)
		}
	}
	if !strings.HasPrefix(dst, "super.") || strings.Contains(dst[6:], ".") {
		return swiftSuperCallShape{}, false
	}
	shape.method = dst[6:]
	if !swiftIdentifier(shape.method) || shape.method == "init" || shape.method == "deinit" || shape.method == "subscript" || len(shape.regularLabels)+len(shape.trailingLabels) != int(arity.Int64) {
		return swiftSuperCallShape{}, false
	}
	shape.strategy, shape.arity = ResolutionStrategySwiftSuperScope, int(arity.Int64)
	return shape, true
}

func swiftPositionLE(al, ac, bl, bc int64) bool { return al < bl || al == bl && ac <= bc }

func swiftSuperRangeContains(outer swiftSuperClass, file, sl, sc, el, ec int64) bool {
	return outer.file == file && swiftPositionLE(outer.startLine, outer.startCol, sl, sc) && swiftPositionLE(el, ec, outer.endLine, outer.endCol)
}

// The same compatibility result is used for positive selection and every
// ambiguity veto. Unknown persisted shape is deliberately not compatible.
func swiftSuperCandidateMatches(call swiftSuperCallShape, c swiftSuperCandidate) (compatible, known bool) {
	if !c.min.Valid || !c.max.Valid || c.min.Int64 != c.max.Int64 {
		return false, false
	}
	if c.min.Int64 != int64(call.arity) {
		return false, true
	}
	labels, ok := swiftDeclarationLabels(c.signature, c.name)
	if !ok || len(labels) != call.arity {
		return false, false
	}
	if len(call.trailingLabels) == 0 {
		return c.signature == swiftSelector(call.method, call.regularLabels), true
	}
	regular := len(call.regularLabels)
	for i, label := range call.regularLabels {
		if labels[i] != swiftLabel(label) {
			return false, true
		}
	}
	for i, label := range call.trailingLabels {
		if i > 0 && labels[regular+i] != label {
			return false, true
		}
	}
	return true, true
}

// swiftSuperTypeDeclarationConflict checks declaration legality above the
// already-selected target. Unknown ancestry is not conflict evidence.
func swiftSuperTypeDeclarationConflict(selectedOwner string, selected swiftSuperCandidate, call swiftSuperCallShape, file int64, callerTest bool, tests map[int64]struct{}, relationsByChild map[string][]swiftSuperRelation, baseByKey map[string][]swiftSuperClass, candidates []swiftSuperCandidate) bool {
	current := selectedOwner
	descendant := selected
	visited := map[string]struct{}{current: {}}
	for {
		// Unknown evidence belongs to this owner only. A proven superclass
		// remains usable even when unrelated inheritance evidence is unknown.
		var supers []swiftSuperRelation
		unknownAncestry := false
		for _, relation := range relationsByChild[current] {
			switch relation.kind {
			case "conformance":
				continue
			case "unproven":
				unknownAncestry = true
				continue
			case "superclass":
				if relation.file != file {
					unknownAncestry = true
					continue
				}
				if relation.generic != 0 || relation.constrained != 0 {
					unknownAncestry = true
					continue
				}
				supers = append(supers, relation)
			default:
				unknownAncestry = true
			}
		}
		if len(supers) == 0 {
			// Only an owner with no superclass evidence is a proven root. An
			// override is invalid there; an unknown stop is target-bounded.
			return !unknownAncestry && descendant.isOverride.Valid && descendant.isOverride.Int64 == 1
		}
		if len(supers) > 1 || supers[0].target == "" {
			// Multiple or malformed proven superclass evidence is concrete
			// invalidity, not an unknown target.
			return true
		}
		current = supers[0].target
		if _, ok := visited[current]; ok {
			return true
		}
		visited[current] = struct{}{}
		owners := baseByKey[current+"\x00"+strconv.FormatInt(file, 10)]
		if len(owners) == 0 {
			return false
		}
		if len(owners) != 1 {
			return true
		}
		if owners[0].kind != "class" {
			return true
		}
		if owners[0].visibility == "private" {
			return false
		}

		matches := 0
		var ancestor swiftSuperCandidate
		for _, candidate := range candidates {
			if candidate.owner != current || candidate.name != call.method {
				continue
			}
			if _, candidateTest := tests[candidate.file]; candidateTest && !callerTest {
				continue
			}
			compatible, known := swiftSuperCandidateMatches(call, candidate)
			if !known {
				return true
			}
			if compatible {
				if candidate.file != file {
					if candidate.visibility != "private" && candidate.visibility != "fileprivate" {
						return true
					}
					continue
				}
				if candidate.visibility == "private" || !swiftSuperRangeContains(owners[0], candidate.file, candidate.startLine, candidate.startCol, candidate.endLine, candidate.endCol) {
					return true
				}
				matches++
				ancestor = candidate
			}
		}
		if matches == 0 {
			continue
		}
		if matches != 1 || ancestor.factCount != 1 || !ancestor.static.Valid || (ancestor.static.Int64 != 0 && ancestor.static.Int64 != 1) || (ancestor.static.Int64 == 0 && ancestor.dispatch != "instance") || (ancestor.static.Int64 == 1 && ancestor.dispatch != "class" && ancestor.dispatch != "static") || (ancestor.static.Int64 == 1 && (!ancestor.isFinal.Valid || !ancestor.isOverride.Valid)) {
			return true
		}
		if ancestor.static.Int64 == 0 {
			continue
		}
		if ancestor.dispatch == "static" || ancestor.isFinal.Int64 != 0 {
			return true
		}
		if !descendant.static.Valid || descendant.static.Int64 != 1 || (descendant.dispatch != "class" && descendant.dispatch != "static") || !descendant.isOverride.Valid || descendant.isOverride.Int64 != 1 {
			return true
		}
		descendant = ancestor
	}
}

func (s *Store) resolveSwiftSuperScope(ctx context.Context, q javaQuery, repoID int64, only map[int64]struct{}) (int, error) {
	var edges []swiftSuperEdge
	err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.src_symbol_id,e.dst_name,e.evidence,src.container_name,src.name,src.is_static,e.call_arity,src.start_line,src.start_col,src.end_line,src.end_col
FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols src ON src.id=e.src_symbol_id JOIN files sf ON sf.id=src.file_id
	WHERE e.repo_id=? AND f.language='swift' AND f.is_deleted=0 AND e.edge_kind='calls' AND e.dst_symbol_id IS NULL
	  AND (e.evidence='swift:super' OR e.evidence LIKE 'swift:super;%')
	  AND sf.id=e.file_id AND sf.is_deleted=0 AND src.language='swift' AND src.kind='function' AND src.container_name<>''`,
		" AND e.id IN (%s)", []any{repoID}, int64SliceToAny(sortedIDs(only)), len(only) > 0,
		func(rows *sql.Rows) error {
			var e swiftSuperEdge
			if err := rows.Scan(&e.id, &e.file, &e.src, &e.dst, &e.evidence, &e.owner, &e.name, &e.static, &e.arity, &e.startLine, &e.startCol, &e.endLine, &e.endCol); err != nil {
				return err
			}
			edges = append(edges, e)
			return nil
		})
	if err != nil {
		return 0, err
	}

	type pending struct {
		edge        swiftSuperEdge
		call        swiftSuperCallShape
		mode        swiftSuperSourceMode
		child, base swiftSuperClass
		relation    swiftSuperRelation
		extension   bool
	}
	pendingEdges := make([]pending, 0, len(edges))
	owners := map[string]struct{}{}
	for _, e := range edges {
		call, ok := parseSwiftSuperCallShape(e.evidence, e.dst, e.arity)
		if !ok || !e.static.Valid || e.name == "init" || e.name == "deinit" {
			continue
		}
		mode := swiftSuperSourceMode(0)
		if e.static.Int64 == 0 {
			mode = swiftSuperInstanceSource
		} else if e.static.Int64 == 1 {
			mode = swiftSuperTypeSource
		} else {
			continue
		}
		pendingEdges = append(pendingEdges, pending{edge: e, call: call, mode: mode})
		owners[e.owner] = struct{}{}
	}
	if len(pendingEdges) == 0 {
		return 0, nil
	}

	sourceIDs := make([]int64, 0, len(pendingEdges))
	for _, p := range pendingEdges {
		sourceIDs = append(sourceIDs, p.edge.src)
	}
	extensions, err := swiftSuperLoadExtensionMemberships(ctx, q, repoID, sourceIDs)
	if err != nil {
		return 0, err
	}
	for _, memberships := range extensions {
		for _, m := range memberships {
			if m.target != "" {
				owners[m.target] = struct{}{}
			}
		}
	}
	classes, err := swiftSuperLoadClasses(ctx, q, repoID, sortedSwiftSet(owners))
	if err != nil {
		return 0, err
	}
	classByKey := map[string][]swiftSuperClass{}
	for _, c := range classes {
		classByKey[c.qname+"\x00"+strconv.FormatInt(c.file, 10)] = append(classByKey[c.qname+"\x00"+strconv.FormatInt(c.file, 10)], c)
	}
	valid := pendingEdges[:0]
	sourceFacts, err := swiftSuperLoadSourceFacts(ctx, q, repoID, sourceIDs)
	if err != nil {
		return 0, err
	}
	for _, p := range pendingEdges {
		fact, ok := sourceFacts[p.edge.src]
		if !ok || fact.count != 1 || (p.mode == swiftSuperInstanceSource && fact.dispatch != "instance") || (p.mode == swiftSuperTypeSource && (fact.dispatch != "class" && fact.dispatch != "static")) {
			continue
		}
		cs := classByKey[p.edge.owner+"\x00"+strconv.FormatInt(p.edge.file, 10)]
		if len(cs) == 1 && cs[0].kind == "class" && swiftSuperRangeContains(cs[0], p.edge.file, p.edge.startLine, p.edge.startCol, p.edge.endLine, p.edge.endCol) {
			p.child = cs[0]
		} else {
			members := extensions[p.edge.src]
			if len(members) != 1 {
				continue
			}
			m := members[0]
			if m.file != p.edge.file || m.target != p.edge.owner || m.target == "" || m.generic < 0 || m.generic > 1 || m.constrained < 0 || m.constrained > 1 || m.generic != 0 || m.constrained != 0 || !swiftPositionLE(m.startLine, m.startCol, p.edge.startLine, p.edge.startCol) || !swiftPositionLE(p.edge.endLine, p.edge.endCol, m.endLine, m.endCol) {
				continue
			}
			cs = classByKey[m.target+"\x00"+strconv.FormatInt(p.edge.file, 10)]
			if len(cs) != 1 || cs[0].kind != "class" {
				continue
			}
			p.child, p.extension = cs[0], true
		}
		valid = append(valid, p)
	}
	if len(valid) == 0 {
		return 0, nil
	}

	// Load the complete Swift hierarchy once. The walk below is target-bounded,
	// but its evidence is shared by every unresolved super edge.
	relations, err := swiftSuperLoadRelations(ctx, q, repoID, nil)
	if err != nil {
		return 0, err
	}
	valid2 := valid[:0]
	relationsByChild := map[string][]swiftSuperRelation{}
	classNames := map[string]struct{}{}
	for _, r := range relations {
		relationsByChild[r.child] = append(relationsByChild[r.child], r)
		classNames[r.child] = struct{}{}
		classNames[r.target] = struct{}{}
	}
	for _, p := range valid {
		var direct []swiftSuperRelation
		unproven := false
		for _, r := range relationsByChild[p.child.qname] {
			if r.file != p.edge.file {
				continue
			}
			if r.kind == "unproven" {
				unproven = true
			}
			if r.kind == "superclass" {
				direct = append(direct, r)
			}
		}
		if unproven || len(direct) != 1 || direct[0].generic != 0 || direct[0].constrained != 0 {
			continue
		}
		p.relation = direct[0]
		valid2 = append(valid2, p)
	}
	if len(valid2) == 0 {
		return 0, nil
	}

	for _, p := range valid2 {
		classNames[p.child.qname] = struct{}{}
	}
	baseClasses, err := swiftSuperLoadClasses(ctx, q, repoID, sortedSwiftSet(classNames))
	if err != nil {
		return 0, err
	}
	baseByKey := map[string][]swiftSuperClass{}
	for _, c := range baseClasses {
		baseByKey[c.qname+"\x00"+strconv.FormatInt(c.file, 10)] = append(baseByKey[c.qname+"\x00"+strconv.FormatInt(c.file, 10)], c)
	}
	keys := make([][]any, 0)
	seen := map[string]struct{}{}
	for _, p := range valid2 {
		for owner := range classNames {
			key := owner + "\x00" + p.call.method
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				keys = append(keys, []any{owner, p.call.method})
			}
		}
	}
	if len(keys) == 0 {
		return 0, nil
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_swift_super_candidates(owner TEXT NOT NULL,name TEXT NOT NULL,PRIMARY KEY(owner,name)) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	defer q.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_swift_super_candidates`)
	if _, err := q.ExecContext(ctx, `DELETE FROM tmp_swift_super_candidates`); err != nil {
		return 0, err
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO tmp_swift_super_candidates(owner,name) VALUES `, `(?,?)`, nil, keys); err != nil {
		return 0, err
	}

	var candidates []swiftSuperCandidate
	err = sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.container_name,s.name,s.signature,s.visibility,s.is_static,s.arity_min,s.arity_max,s.start_line,s.start_col,s.end_line,s.end_col,COUNT(d.id),COALESCE(MAX(d.dispatch_kind),''),MAX(d.is_override),MAX(d.is_final)
FROM symbols s JOIN files f ON f.id=s.file_id JOIN tmp_swift_super_candidates k ON k.owner=s.container_name AND k.name=s.name
LEFT JOIN swift_declaration_facts d ON d.repo_id=s.repo_id AND d.symbol_id=s.id
WHERE s.repo_id=? AND s.language='swift' AND s.kind='function' AND f.is_deleted=0 GROUP BY s.id`, "", []any{repoID}, nil, false, func(rows *sql.Rows) error {
		var c swiftSuperCandidate
		if err := rows.Scan(&c.id, &c.file, &c.owner, &c.name, &c.signature, &c.visibility, &c.static, &c.min, &c.max, &c.startLine, &c.startCol, &c.endLine, &c.endCol, &c.factCount, &c.dispatch, &c.isOverride, &c.isFinal); err != nil {
			return err
		}
		candidates = append(candidates, c)
		return nil
	})
	if err != nil {
		return 0, err
	}
	tests, err := testFileIDsForRepo(ctx, q, repoID)
	if err != nil {
		return 0, err
	}
	blockers, err := swiftSuperLoadBlockers(ctx, q, repoID, sortedSwiftSet(classNames))
	if err != nil {
		return 0, err
	}

	res := map[int64]swiftScopeBinding{}
	for _, p := range valid2 {
		_, callerTest := tests[p.edge.file]
		current := p.relation.target
		visited := map[string]struct{}{p.child.qname: {}, current: {}}
		for depth := 1; ; depth++ {
			owners := baseByKey[current+"\x00"+strconv.FormatInt(p.edge.file, 10)]
			if len(owners) != 1 || owners[0].kind != "class" || owners[0].visibility == "private" {
				break
			}
			owner := owners[0]
			veto, matches := false, 0
			var found swiftSuperCandidate
			for _, blocker := range blockers[current+"\x00"+p.call.method] {
				if p.mode == swiftSuperInstanceSource && blocker.kind == graph.ScopeImportSwiftMemberValue && blocker.static != 0 {
					continue
				}
				if p.mode == swiftSuperTypeSource && blocker.kind == graph.ScopeImportSwiftMemberValue && blocker.static == 0 {
					continue
				}
				if _, blockerTest := tests[blocker.file]; blockerTest && !callerTest {
					continue
				}
				veto = true
			}
			for _, c := range candidates {
				if c.owner != current || c.name != p.call.method {
					continue
				}
				if _, candidateTest := tests[c.file]; candidateTest && !callerTest {
					continue
				}
				if c.file != p.edge.file && (c.visibility == "private" || c.visibility == "fileprivate") {
					continue
				}
				compatible, known := swiftSuperCandidateMatches(p.call, c)
				if !known {
					veto = true
					continue
				}
				if !compatible {
					continue
				}
				validTarget := c.factCount == 1 && c.static.Valid && c.static.Int64 == 0 && c.dispatch == "instance" && c.visibility != "private" && swiftSuperRangeContains(owner, c.file, c.startLine, c.startCol, c.endLine, c.endCol)
				if p.mode == swiftSuperTypeSource {
					validTarget = c.factCount == 1 && c.static.Valid && c.static.Int64 == 1 && (c.dispatch == "class" || c.dispatch == "static") && c.visibility != "private" && c.file == p.edge.file && swiftSuperRangeContains(owner, c.file, c.startLine, c.startCol, c.endLine, c.endCol)
				}
				if !validTarget {
					veto = true
					continue
				}
				matches++
				found = c
			}
			if veto || matches > 1 {
				break
			}
			if matches == 1 {
				if swiftSuperConcreteAncestryConflict(current, p.edge.file, visited, relationsByChild) {
					break
				}
				if p.mode == swiftSuperTypeSource && swiftSuperTypeDeclarationConflict(current, found, p.call, p.edge.file, callerTest, tests, relationsByChild, baseByKey, candidates) {
					break
				}
				strategy := ResolutionStrategySwiftSuperScope
				if p.mode == swiftSuperTypeSource {
					strategy = ResolutionStrategySwiftSuperTypeScope
				}
				if p.extension {
					if p.mode == swiftSuperTypeSource {
						strategy = ResolutionStrategySwiftSuperExtensionTypeScope
					} else {
						strategy = ResolutionStrategySwiftSuperExtensionScope
					}
				}
				if depth >= 2 {
					if p.mode == swiftSuperTypeSource {
						strategy = ResolutionStrategySwiftSuperMultilevelTypeScope
					} else {
						strategy = ResolutionStrategySwiftSuperMultilevelInheritedMethodScope
					}
					if p.extension {
						if p.mode == swiftSuperTypeSource {
							strategy = ResolutionStrategySwiftSuperExtensionMultilevelTypeScope
						} else {
							strategy = ResolutionStrategySwiftSuperExtensionMultilevelInheritedMethodScope
						}
					}
				}
				res[p.edge.id] = swiftScopeBinding{dst: found.id, strategy: strategy}
				break
			}
			var supers []swiftSuperRelation
			for _, r := range relationsByChild[current] {
				if r.file != p.edge.file {
					continue
				}
				if r.kind == "unproven" {
					veto = true
				}
				if r.kind == "superclass" {
					supers = append(supers, r)
				}
			}
			if veto || len(supers) != 1 || supers[0].generic != 0 || supers[0].constrained != 0 || supers[0].target == "" {
				break
			}
			current = supers[0].target
			if _, ok := visited[current]; ok {
				break
			}
			visited[current] = struct{}{}
		}
	}
	return swiftScopeApply(ctx, q, res)
}

// swiftSuperConcreteAncestryConflict rejects a proven cycle that closes through
// an already-traversed owner. Unknown ancestry above the selected target is
// deliberately target-bounded and does not conflict.
func swiftSuperConcreteAncestryConflict(selectedOwner string, file int64, visited map[string]struct{}, relationsByChild map[string][]swiftSuperRelation) bool {
	current := selectedOwner
	seen := map[string]struct{}{current: {}}
	for {
		var supers []swiftSuperRelation
		for _, relation := range relationsByChild[current] {
			switch relation.kind {
			case "conformance":
				continue
			case "superclass":
				if relation.file != file || relation.generic != 0 || relation.constrained != 0 {
					continue
				}
				supers = append(supers, relation)
			case "unproven":
				continue
			}
		}
		if len(supers) == 0 {
			return false
		}
		if len(supers) != 1 {
			return true
		}
		next := supers[0].target
		if next == "" {
			return true
		}
		if _, ok := visited[next]; ok {
			return true
		}
		if _, ok := seen[next]; ok {
			return true
		}
		seen[next] = struct{}{}
		current = next
	}
}

type swiftSuperFact struct {
	count    int
	dispatch string
}

func swiftSuperLoadSourceFacts(ctx context.Context, q javaQuery, repoID int64, ids []int64) (map[int64]swiftSuperFact, error) {
	// Kept as a separate query so source dispatch is never inferred from the
	// symbol's static bit alone.
	result := map[int64]swiftSuperFact{}
	if len(ids) == 0 {
		return result, nil
	}
	err := sqliteBatchedIDQuery(ctx, q, ids,
		`SELECT symbol_id,dispatch_kind FROM swift_declaration_facts WHERE repo_id=? AND symbol_id IN (`,
		[]any{repoID}, func(scan func(...any) error) error {
			var id int64
			var dispatch string
			if err := scan(&id, &dispatch); err != nil {
				return err
			}
			fact := result[id]
			fact.count++
			fact.dispatch = dispatch
			result[id] = fact
			return nil
		})
	return result, err
}

func swiftSuperLoadExtensionMemberships(ctx context.Context, q javaQuery, repoID int64, ids []int64) (map[int64][]swiftSuperExtensionMembership, error) {
	out := map[int64][]swiftSuperExtensionMembership{}
	if len(ids) == 0 {
		return out, nil
	}
	err := sqliteBatchedIDQuery(ctx, q, ids, `SELECT symbol_id,file_id,target_qualified_name,extension_start_line,extension_start_col,extension_end_line,extension_end_col,is_generic,is_constrained FROM swift_extension_memberships WHERE repo_id=? AND symbol_id IN (`, []any{repoID}, func(scan func(...any) error) error {
		var symbol int64
		var m swiftSuperExtensionMembership
		if err := scan(&symbol, &m.file, &m.target, &m.startLine, &m.startCol, &m.endLine, &m.endCol, &m.generic, &m.constrained); err != nil {
			return err
		}
		out[symbol] = append(out[symbol], m)
		return nil
	})
	for id, rows := range out {
		for i := range rows {
			rows[i].count = int64(len(rows))
		}
		out[id] = rows
	}
	return out, err
}

func sortedSwiftSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func swiftSuperLoadClasses(ctx context.Context, q javaQuery, repoID int64, names []string) ([]swiftSuperClass, error) {
	var out []swiftSuperClass
	err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.qualified_name,s.kind,s.visibility,s.start_line,s.start_col,s.end_line,s.end_col FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND s.language='swift' AND f.is_deleted=0 AND s.qualified_name IN (`, `%s)`, []any{repoID}, stringSliceToAny(names), true, func(rows *sql.Rows) error {
		var c swiftSuperClass
		if err := rows.Scan(&c.id, &c.file, &c.qname, &c.kind, &c.visibility, &c.startLine, &c.startCol, &c.endLine, &c.endCol); err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

func swiftSuperLoadRelations(ctx context.Context, q javaQuery, repoID int64, names []string) ([]swiftSuperRelation, error) {
	var out []swiftSuperRelation
	query := `SELECT file_id,child_qualified_name,target_qualified_name,relation_kind,is_generic,is_constrained FROM swift_inheritance_relations WHERE repo_id=?`
	suffix := ""
	args := []any{repoID}
	values := []any(nil)
	if len(names) > 0 {
		query += ` AND child_qualified_name IN (`
		suffix = `%s)`
		values = stringSliceToAny(names)
	}
	err := sqliteBatchedQuery(ctx, q, query, suffix, args, values, len(names) > 0, func(rows *sql.Rows) error {
		var r swiftSuperRelation
		if err := rows.Scan(&r.file, &r.child, &r.target, &r.kind, &r.generic, &r.constrained); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

func swiftSuperLoadBlockers(ctx context.Context, q javaQuery, repoID int64, owners []string) (map[string][]swiftSuperBlocker, error) {
	out := map[string][]swiftSuperBlocker{}
	err := sqliteBatchedQuery(ctx, q, `SELECT p.owner_module,p.local_name,p.file_id,p.is_static,p.import_kind FROM scope_import_evidence p JOIN files f ON f.id=p.file_id WHERE p.repo_id=? AND f.is_deleted=0 AND p.language='swift' AND p.import_kind IN (?,?) AND p.owner_module IN (`, `%s)`, []any{repoID, graph.ScopeImportSwiftMemberValue, graph.ScopeImportSwiftEnumCase}, stringSliceToAny(owners), true, func(rows *sql.Rows) error {
		var owner, name string
		var blocker swiftSuperBlocker
		if err := rows.Scan(&owner, &name, &blocker.file, &blocker.static, &blocker.kind); err != nil {
			return err
		}
		out[owner+"\x00"+name] = append(out[owner+"\x00"+name], blocker)
		return nil
	})
	return out, err
}

func (s *Store) redecideSwiftSuperBindings(ctx context.Context, repoID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+` WHERE repo_id=? AND edge_kind='calls' AND (evidence='swift:super' OR evidence LIKE 'swift:super;%')`, repoID); err != nil {
		return err
	}
	if _, err := s.resolveSwiftSuperScope(ctx, tx, repoID, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) swiftSuperRepairApplies(ctx context.Context, repoID int64) (bool, error) {
	return s.swiftSuperEvidenceApplies(ctx, repoID)
}

func (s *Store) swiftSuperEvidenceApplies(ctx context.Context, repoID int64) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='swift' AND f.is_deleted=0 AND e.edge_kind='calls' AND (e.evidence='swift:super' OR e.evidence LIKE 'swift:super;%'))`, repoID).Scan(&found)
	return found, err
}
