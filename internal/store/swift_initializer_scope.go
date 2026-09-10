package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

const swiftInitializerRepairSettingKey = "resolver.swift_initializer_repaired.v1"
const swiftTrailingInitializerRepairSettingKey = "resolver.swift_trailing_initializer_repaired.v1"

type swiftInitializerCallShape struct {
	owner                         string
	regularLabels, trailingLabels []string
	generic                       bool
	selector                      string
	arity                         int64
}

func parseSwiftInitializerCall(evidence, dst string, arity sql.NullInt64) (swiftInitializerCallShape, bool) {
	var shape swiftInitializerCallShape
	if evidence != "swift:initializer" && !strings.HasPrefix(evidence, "swift:initializer;") || !arity.Valid || arity.Int64 < 0 || !swiftTypePathStore(dst) {
		return shape, false
	}
	parts := strings.Split(evidence, ";")
	seenGeneric, seenLabels, seenTrailing := false, false, false
	seenNamedLabels := map[string]struct{}{}
	for _, part := range parts[1:] {
		switch {
		case part == "generic_specialization=true" && !seenGeneric && !seenLabels && !seenTrailing:
			seenGeneric = true
			shape.generic = true
		case strings.HasPrefix(part, "labels=") && !seenLabels && !seenTrailing:
			seenLabels = true
			value := strings.TrimPrefix(part, "labels=")
			if value == "" {
				return swiftInitializerCallShape{}, false
			}
			for _, label := range strings.Split(value, ",") {
				if label != "_" && (!strings.HasSuffix(label, ":") || !swiftIdentifier(strings.TrimSuffix(label, ":"))) {
					return swiftInitializerCallShape{}, false
				}
				if label != "_" {
					name := strings.TrimSuffix(label, ":")
					if _, exists := seenNamedLabels[name]; exists {
						return swiftInitializerCallShape{}, false
					}
					seenNamedLabels[name] = struct{}{}
				}
				shape.regularLabels = append(shape.regularLabels, label)
			}
		case strings.HasPrefix(part, "trailing_labels=") && !seenTrailing:
			seenTrailing = true
			value := strings.TrimPrefix(part, "trailing_labels=")
			if value == "" {
				return swiftInitializerCallShape{}, false
			}
			for i, label := range strings.Split(value, ",") {
				if (i == 0 && label != "_") || (i > 0 && (label == "_" || !strings.HasSuffix(label, ":") || !swiftIdentifier(strings.TrimSuffix(label, ":")))) {
					return swiftInitializerCallShape{}, false
				}
				shape.trailingLabels = append(shape.trailingLabels, label)
			}
		default:
			return swiftInitializerCallShape{}, false
		}
	}
	if len(shape.regularLabels)+len(shape.trailingLabels) != int(arity.Int64) {
		return swiftInitializerCallShape{}, false
	}
	shape.owner, shape.arity = dst, arity.Int64
	shape.selector = swiftSelector("init", shape.regularLabels)
	return shape, true
}

func swiftTypePathStore(value string) bool {
	if value == "" || strings.ContainsAny(value, "<>?!()[]:=,") {
		return false
	}
	parts := strings.Split(value, ".")
	if parts[0] == "self" || parts[0] == "Self" {
		return false
	}
	for _, part := range parts {
		if part == "init" || !swiftASCIIIdentifier(part) {
			return false
		}
	}
	return true
}

func swiftASCIIIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 && c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

type swiftInitializerCandidate struct {
	id, file, min, max           int64
	minValid, maxValid           bool
	owner, signature, visibility string
}

func (s *Store) resolveSwiftInitializerScope(ctx context.Context, q javaQuery, repoID int64, only map[int64]struct{}) (int, error) {
	var edges []swiftScopeEdge
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.src_symbol_id,e.dst_name,e.evidence,src.container_name,src.is_static,e.call_arity
		FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols src ON src.id=e.src_symbol_id JOIN files sf ON sf.id=src.file_id
		WHERE e.repo_id=? AND f.language='swift' AND f.is_deleted=0 AND e.edge_kind='calls' AND e.dst_symbol_id IS NULL
		  AND sf.id=e.file_id AND sf.is_deleted=0 AND src.repo_id=e.repo_id AND src.language='swift' AND src.kind='function'`,
		" AND e.id IN (%s)", []any{repoID}, int64SliceToAny(sortedIDs(only)), len(only) > 0,
		func(rows *sql.Rows) error {
			var e swiftScopeEdge
			if err := rows.Scan(&e.id, &e.file, &e.src, &e.dst, &e.evidence, &e.owner, &e.static, &e.arity); err != nil {
				return err
			}
			edges = append(edges, e)
			return nil
		}); err != nil {
		return 0, err
	}
	shapes := make(map[int64]swiftInitializerCallShape)
	owners := make([]string, 0)
	seenOwners := map[string]struct{}{}
	for _, e := range edges {
		shape, ok := parseSwiftInitializerCall(e.evidence, e.dst, e.arity)
		if !ok {
			continue
		}
		shapes[e.id] = shape
		if _, ok := seenOwners[shape.owner]; !ok {
			seenOwners[shape.owner] = struct{}{}
			owners = append(owners, shape.owner)
		}
	}
	if len(shapes) == 0 {
		return 0, nil
	}
	tests, err := testFileIDsForRepo(ctx, q, repoID)
	if err != nil {
		return 0, err
	}
	type ownerFact struct {
		file             int64
		kind, visibility string
	}
	ownerCounts := map[string]int{}
	ownerAllowed := map[string]ownerFact{}
	err = sqliteBatchedQuery(ctx, q, `SELECT s.file_id,s.qualified_name,s.kind,s.visibility FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND s.language='swift' AND f.is_deleted=0 AND s.kind<>'function' AND s.qualified_name IN (`, `%s)`, []any{repoID}, stringSliceToAny(owners), true, func(rows *sql.Rows) error {
		var file int64
		var qname, kind, vis string
		if err := rows.Scan(&file, &qname, &kind, &vis); err != nil {
			return err
		}
		key := qname + "\x00" + strconvI(file)
		ownerCounts[key]++
		if kind == "struct" || kind == "enum" || kind == "actor" {
			ownerAllowed[key] = ownerFact{file, kind, vis}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	candidates := map[string][]swiftInitializerCandidate{}
	err = sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.container_name,s.signature,s.visibility,s.arity_min,s.arity_max FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND s.language='swift' AND f.is_deleted=0 AND s.kind='function' AND s.name='init' AND s.container_name IN (`, `%s)`, []any{repoID}, stringSliceToAny(owners), true, func(rows *sql.Rows) error {
		var c swiftInitializerCandidate
		var min, max sql.NullInt64
		if err := rows.Scan(&c.id, &c.file, &c.owner, &c.signature, &c.visibility, &min, &max); err != nil {
			return err
		}
		c.min, c.max, c.minValid, c.maxValid = min.Int64, max.Int64, min.Valid, max.Valid
		candidates[c.owner] = append(candidates[c.owner], c)
		return nil
	})
	if err != nil {
		return 0, err
	}
	res := map[int64]swiftScopeBinding{}
	for _, e := range edges {
		shape, ok := shapes[e.id]
		if !ok {
			continue
		}
		ownerKey := shape.owner + "\x00" + strconvI(e.file)
		fact, ownerOK := ownerAllowed[ownerKey]
		if ownerCounts[ownerKey] != 1 || !ownerOK || fact.visibility == "private" {
			continue
		}
		matches := 0
		var found swiftInitializerCandidate
		for _, c := range candidates[shape.owner] {
			if !swiftInitializerCandidateMatches(shape, c) || c.visibility == "private" {
				continue
			}
			if _, test := tests[c.file]; test {
				if _, callerTest := tests[e.file]; !callerTest {
					continue
				}
			}
			matches++
			found = c
		}
		if matches != 1 || found.file != e.file || found.id == 0 {
			continue
		}
		res[e.id] = swiftScopeBinding{dst: found.id, strategy: ResolutionStrategySwiftInitializerScope}
	}
	return swiftScopeApply(ctx, q, res)
}

func swiftInitializerCandidateMatches(call swiftInitializerCallShape, candidate swiftInitializerCandidate) bool {
	if !candidate.minValid || !candidate.maxValid || candidate.min != candidate.max || candidate.min != call.arity {
		return false
	}
	if len(call.trailingLabels) == 0 {
		return candidate.signature == call.selector
	}
	labels, ok := swiftDeclarationLabels(candidate.signature, "init")
	if !ok || int64(len(labels)) != call.arity {
		return false
	}
	for i, label := range call.regularLabels {
		if labels[i] != swiftLabel(label) {
			return false
		}
	}
	regular := len(call.regularLabels)
	for i, label := range call.trailingLabels {
		if i > 0 && labels[regular+i] != label {
			return false
		}
	}
	return true
}

func strconvI(v int64) string { return strconv.FormatInt(v, 10) }

func (s *Store) redecideSwiftBindings(ctx context.Context, repoID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+` WHERE repo_id=? AND edge_kind='calls' AND (evidence LIKE 'swift:self%' OR evidence LIKE 'swift:Self%' OR evidence='swift:initializer' OR evidence LIKE 'swift:initializer;%')`, repoID); err != nil {
		return err
	}
	if _, err := s.resolveSwiftInitializerScope(ctx, tx, repoID, nil); err != nil {
		return err
	}
	if _, err := s.resolveSwiftScope(ctx, tx, repoID, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) swiftInitializerRepairApplies(ctx context.Context, repoID int64) (bool, error) {
	return s.swiftInitializerRepairEvidenceApplies(ctx, repoID, false)
}

func (s *Store) swiftTrailingInitializerRepairApplies(ctx context.Context, repoID int64) (bool, error) {
	return s.swiftInitializerRepairEvidenceApplies(ctx, repoID, true)
}

func (s *Store) swiftInitializerRepairEvidenceApplies(ctx context.Context, repoID int64, trailing bool) (bool, error) {
	condition := `e.evidence NOT LIKE '%;trailing_labels=%'`
	if trailing {
		condition = `e.evidence LIKE '%;trailing_labels=%'`
	}
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM edges e JOIN files f ON f.id=e.file_id
		WHERE e.repo_id=? AND f.language='swift' AND f.is_deleted=0 AND e.edge_kind='calls'
		  AND (e.evidence='swift:initializer' OR e.evidence LIKE 'swift:initializer;%')
		  AND `+condition+`
	)`, repoID).Scan(&found)
	return found, err
}
