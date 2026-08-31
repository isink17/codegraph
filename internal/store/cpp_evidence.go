package store

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
)

// C/C++ declaration evidence (P22.17). A declaration is persisted as a
// symbol row with kind=declaration. It is never a call target; its qualified
// identity grants visibility to matching definition rows in another file.
// Definitions remain separate rows so locations, duplicate definitions and
// overloads stay observable and fail closed.
type cppEvidenceCandidate struct {
	id         int64
	fileID     int64
	kind       string
	name       string
	qualified  string
	signature  string
	visibility string
}

type cppEvidenceExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// cppEvidenceTarget is the shared routing predicate for C++ spellings whose
// identity can be checked by declaration/definition evidence. Template-like
// spellings are included even when punctuation makes them unlike a bare name.
func cppEvidenceTarget(target edgeTarget) bool {
	return target.srcLanguage == "cpp" &&
		(goBareCallName(target.dstName) ||
			strings.Contains(target.dstName, "::") ||
			strings.ContainsAny(target.dstName, "<>()"))
}

func unresolvedCppEvidenceTargets(ctx context.Context, q queryContexter, repoID int64) ([]edgeTarget, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT e.id, e.dst_name, e.file_id, e.evidence
		FROM edges e JOIN files f ON f.id = e.file_id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND f.language = 'cpp'`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []edgeTarget
	for rows.Next() {
		var target edgeTarget
		if err := rows.Scan(&target.edgeID, &target.dstName, &target.srcFileID, &target.evidence); err != nil {
			return nil, err
		}
		target.srcLanguage = "cpp"
		out = append(out, target)
	}
	return out, rows.Err()
}

func (s *Store) resolveCppEvidenceEdges(ctx context.Context, repoID int64, targets []edgeTarget) (int, error) {
	if len(targets) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := resolveCppEvidenceEdgesWith(ctx, tx, tx, repoID, targets)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func resolveCppEvidenceEdgesWith(ctx context.Context, q queryContexter, exec cppEvidenceExecutor, repoID int64, targets []edgeTarget) (int, error) {
	cppTargets := make([]edgeTarget, 0, len(targets))
	nameSet := map[string]struct{}{}
	edgeIDs := make([]int64, 0, len(targets))
	for _, target := range targets {
		if strings.HasPrefix(target.evidence, "macro_unexpanded:") {
			continue
		}
		if !cppEvidenceTarget(target) {
			continue
		}
		cppTargets = append(cppTargets, target)
		nameSet[target.dstName] = struct{}{}
		if strings.HasPrefix(target.dstName, "::") {
			nameSet[strings.TrimPrefix(target.dstName, "::")] = struct{}{}
		}
		edgeIDs = append(edgeIDs, target.edgeID)
	}
	if len(cppTargets) == 0 {
		return 0, nil
	}

	imports, err := importScopeForRepo(ctx, q, repoID)
	if err != nil {
		return 0, err
	}
	testFiles, err := testFileIDsForRepo(ctx, q, repoID)
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(names)), ",")
	args := make([]any, 1+2*len(names))
	args[0] = repoID
	for i, name := range names {
		args[i+1] = name
		args[i+1+len(names)] = name
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, file_id, kind, name, qualified_name, signature, visibility
		FROM symbols
		WHERE repo_id = ? AND language = 'cpp'
		  AND (name IN (`+placeholders+`) OR qualified_name IN (`+placeholders+`))`, args...)
	if err != nil {
		return 0, err
	}
	var candidates []cppEvidenceCandidate
	declarations := map[string][]int64{}
	declarationSignatures := map[string]map[string]struct{}{}
	for rows.Next() {
		var c cppEvidenceCandidate
		if err := rows.Scan(&c.id, &c.fileID, &c.kind, &c.name, &c.qualified, &c.signature, &c.visibility); err != nil {
			return 0, err
		}
		if c.kind == "declaration" {
			key := cppEvidenceKey(c.qualified, c.signature)
			declarations[key] = append(declarations[key], c.fileID)
			if declarationSignatures[c.qualified] == nil {
				declarationSignatures[c.qualified] = map[string]struct{}{}
			}
			declarationSignatures[c.qualified][cppNormalizedSignature(c.signature)] = struct{}{}
			continue
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	// Out-of-line definitions keep their qualified declarator in `name`
	// (`A::foo`), while the header declaration has the callable tail (`foo`).
	// Load those exact identities from the declarations we just found; avoid a
	// repository-wide suffix scan.
	qualifiedSet := map[string]struct{}{}
	for key := range declarations {
		qualifiedSet[strings.SplitN(key, "\x00", 2)[0]] = struct{}{}
	}
	if len(qualifiedSet) > 0 {
		qualifiedNames := make([]string, 0, len(qualifiedSet))
		for qualified := range qualifiedSet {
			qualifiedNames = append(qualifiedNames, qualified)
		}
		sort.Strings(qualifiedNames)
		qualifiedPlaceholders := strings.TrimRight(strings.Repeat("?,", len(qualifiedNames)), ",")
		qualifiedArgs := make([]any, 1, len(qualifiedNames)+1)
		qualifiedArgs[0] = repoID
		for _, qualified := range qualifiedNames {
			qualifiedArgs = append(qualifiedArgs, qualified)
		}
		rows, err := q.QueryContext(ctx, `
			SELECT id, file_id, kind, name, qualified_name, signature, visibility
			FROM symbols
			WHERE repo_id = ? AND language = 'cpp' AND qualified_name IN (`+qualifiedPlaceholders+`)`, qualifiedArgs...)
		if err != nil {
			return 0, err
		}
		seen := make(map[int64]struct{}, len(candidates))
		for _, candidate := range candidates {
			seen[candidate.id] = struct{}{}
		}
		for rows.Next() {
			var candidate cppEvidenceCandidate
			if err := rows.Scan(&candidate.id, &candidate.fileID, &candidate.kind, &candidate.name, &candidate.qualified, &candidate.signature, &candidate.visibility); err != nil {
				rows.Close()
				return 0, err
			}
			if candidate.kind == "declaration" {
				continue
			}
			if _, ok := seen[candidate.id]; !ok {
				candidates = append(candidates, candidate)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, err
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
	}

	classScopes, err := cppClassScopesByName(ctx, q, repoID, names)
	if err != nil {
		return 0, err
	}
	namespaceScopes, err := cppNamespaceScopesByName(ctx, q, repoID, names)
	if err != nil {
		return 0, err
	}
	callerClasses, err := cppCallerClassScopesByEdge(ctx, q, repoID, edgeIDs)
	if err != nil {
		return 0, err
	}
	callerNamespaces, err := cppNamespaceScopesByEdge(ctx, q, repoID, edgeIDs)
	if err != nil {
		return 0, err
	}
	if err := augmentCppOutOfLineScopes(ctx, q, repoID, cppTargets, candidates, imports, classScopes, callerClasses, callerNamespaces, namespaceScopes); err != nil {
		return 0, err
	}
	byName := map[string][]cppEvidenceCandidate{}
	byQualified := map[string][]cppEvidenceCandidate{}
	for _, candidate := range candidates {
		byQualified[candidate.qualified] = append(byQualified[candidate.qualified], candidate)
		name := candidate.name
		if i := strings.LastIndex(candidate.qualified, "::"); i >= 0 {
			name = candidate.qualified[i+2:]
		}
		byName[name] = append(byName[name], candidate)
	}

	type resolution struct {
		edgeID   int64
		dstID    int64
		strategy string
	}
	var resolutions []resolution
	for _, edge := range cppTargets {
		isQualified := strings.Contains(edge.dstName, "::")
		eligible := make([]cppEvidenceCandidate, 0, 2)
		pool := byName[edge.dstName]
		if isQualified {
			pool = byQualified[strings.TrimPrefix(edge.dstName, "::")]
		}
		if !isQualified {
			hiddenFriendVisible := false
			for _, candidate := range pool {
				if candidate.visibility != "hidden_friend" {
					continue
				}
				if candidate.fileID == edge.srcFileID {
					hiddenFriendVisible = true
					break
				}
				if _, ok := imports[edge.srcFileID][candidate.fileID]; ok {
					hiddenFriendVisible = true
					break
				}
			}
			if hiddenFriendVisible {
				continue
			}
		}
		for _, candidate := range pool {
			if candidate.kind == "enum" || candidate.visibility == "hidden_friend" {
				continue
			}
			if _, isTest := testFiles[candidate.fileID]; isTest {
				if _, callerIsTest := testFiles[edge.srcFileID]; !callerIsTest {
					continue
				}
			}
			if candidate.fileID != edge.srcFileID {
				if strings.HasPrefix(candidate.signature, "static:") {
					continue
				}
				_, visible := imports[edge.srcFileID][candidate.fileID]
				if !visible {
					for _, declarationFile := range declarations[cppEvidenceKey(candidate.qualified, candidate.signature)] {
						if _, ok := imports[edge.srcFileID][declarationFile]; ok {
							visible = true
							break
						}
					}
				}
				if !visible {
					continue
				}
			}
			if !isQualified {
				if class := classScopes[candidate.id]; class != "" && callerClasses[edge.edgeID] != class {
					continue
				}
				if namespace := namespaceScopes[candidate.id]; namespace == "" || callerNamespaces[edge.edgeID] != namespace {
					continue
				}
			}
			eligible = append(eligible, candidate)
		}
		if len(eligible) != 1 {
			continue
		}
		// A single indexed definition is not proof of a unique C++ callable:
		// declarations may expose several overload signatures. Argument-type
		// resolution is not modeled here, so keep only a semantically unique
		// qualified family. Duplicate rows of one signature are harmless.
		if !isQualified {
			familySignatures := map[string]struct{}{}
			for signature := range declarationSignatures[eligible[0].qualified] {
				familySignatures[signature] = struct{}{}
			}
			for _, candidate := range byQualified[eligible[0].qualified] {
				familySignatures[cppNormalizedSignature(candidate.signature)] = struct{}{}
			}
			if len(familySignatures) != 1 {
				continue
			}
		}
		strategy := ResolutionStrategyExactName
		if isQualified {
			strategy = ResolutionStrategyExactQualified
		}
		resolutions = append(resolutions, resolution{edgeID: edge.edgeID, dstID: eligible[0].id, strategy: strategy})
	}
	if len(resolutions) == 0 {
		return 0, nil
	}
	if _, err := exec.ExecContext(ctx, `DROP TABLE IF EXISTS temp.tmp_cpp_evidence_resolution`); err != nil {
		return 0, err
	}
	if _, err := exec.ExecContext(ctx, `CREATE TEMP TABLE tmp_cpp_evidence_resolution(edge_id INTEGER PRIMARY KEY, dst_symbol_id INTEGER NOT NULL, strategy TEXT NOT NULL)`); err != nil {
		return 0, err
	}
	for start := 0; start < len(resolutions); start += 300 {
		end := min(start+300, len(resolutions))
		var b strings.Builder
		b.WriteString(`INSERT INTO tmp_cpp_evidence_resolution(edge_id, dst_symbol_id, strategy) VALUES `)
		args := make([]any, 0, 3*(end-start))
		for i, resolution := range resolutions[start:end] {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?,?,?)")
			args = append(args, resolution.edgeID, resolution.dstID, resolution.strategy)
		}
		if _, err := exec.ExecContext(ctx, b.String(), args...); err != nil {
			return 0, err
		}
	}
	if _, err := exec.ExecContext(ctx, `
		UPDATE edges
		SET dst_symbol_id = r.dst_symbol_id,
		    resolution_strategy = r.strategy,
		    resolution_confidence = 'high'
		FROM tmp_cpp_evidence_resolution r
		WHERE edges.id = r.edge_id`); err != nil {
		return 0, err
	}
	_, _ = exec.ExecContext(ctx, `DROP TABLE tmp_cpp_evidence_resolution`)
	return len(resolutions), nil
}

func cppEvidenceKey(qualified, signature string) string {
	return qualified + "\x00" + cppNormalizedSignature(signature)
}

// cppNormalizedSignature compares declaration/definition evidence at the
// callable-type level. Parameter names and default expressions are not part of
// overload identity, while the remaining spelling is kept conservative so
// distinct types still fail closed.
func cppNormalizedSignature(signature string) string {
	var b strings.Builder
	depth := 0
	defaultExpr := false
	for _, r := range strings.TrimSpace(signature) {
		switch r {
		case '(':
			depth++
			defaultExpr = false
		case ')':
			depth--
			defaultExpr = false
		case ',':
			defaultExpr = false
		case '=':
			if depth > 0 {
				defaultExpr = true
				continue
			}
		}
		if defaultExpr {
			continue
		}
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func augmentCppOutOfLineScopes(ctx context.Context, q queryContexter, repoID int64, edges []edgeTarget, candidates []cppEvidenceCandidate, imports map[int64]map[int64]struct{}, classScopes, callerClasses, callerNamespaces, namespaceScopes map[int64]string) error {
	prefixes := map[string]struct{}{}
	for _, candidate := range candidates {
		if prefix := cppOwnerPrefix(candidate.qualified); prefix != "" {
			prefixes[prefix] = struct{}{}
		}
	}
	edgeSources := map[int64]struct {
		fileID int64
		name   string
	}{}
	edgeIDs := make([]int64, 0, len(edges))
	for _, edge := range edges {
		edgeIDs = append(edgeIDs, edge.edgeID)
	}
	if len(edgeIDs) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(edgeIDs)), ",")
		args := make([]any, 1, len(edgeIDs)+1)
		args[0] = repoID
		for _, id := range edgeIDs {
			args = append(args, id)
		}
		rows, err := q.QueryContext(ctx, `SELECT e.id, s.file_id, s.qualified_name FROM edges e JOIN symbols s ON s.id = e.src_symbol_id WHERE e.repo_id = ? AND e.id IN (`+placeholders+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, fileID int64
			var name string
			if err := rows.Scan(&id, &fileID, &name); err != nil {
				rows.Close()
				return err
			}
			edgeSources[id] = struct {
				fileID int64
				name   string
			}{fileID: fileID, name: name}
			if prefix := cppOwnerPrefix(name); prefix != "" {
				prefixes[prefix] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if len(prefixes) == 0 {
		return nil
	}
	keys := make([]string, 0, len(prefixes))
	for prefix := range prefixes {
		keys = append(keys, prefix)
	}
	sort.Strings(keys)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 1, len(keys)+1)
	args[0] = repoID
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := q.QueryContext(ctx, `SELECT id, file_id, name, qualified_name FROM symbols WHERE repo_id = ? AND kind IN ('class','struct','union') AND (name IN (`+placeholders+`) OR qualified_name IN (`+placeholders+`))`, append(args, func() []any {
		out := make([]any, len(keys))
		for i, key := range keys {
			out[i] = key
		}
		return out
	}()...)...)
	if err != nil {
		return err
	}
	type classRow struct {
		id, fileID      int64
		name, qualified string
	}
	var classes []classRow
	for rows.Next() {
		var c classRow
		if err := rows.Scan(&c.id, &c.fileID, &c.name, &c.qualified); err != nil {
			rows.Close()
			return err
		}
		classes = append(classes, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	choose := func(fileID int64, prefix string) (classRow, bool) {
		var found classRow
		count := 0
		for _, c := range classes {
			if c.name != prefix && c.qualified != prefix {
				continue
			}
			if c.fileID != fileID {
				if _, ok := imports[fileID][c.fileID]; !ok {
					continue
				}
			}
			found, count = c, count+1
		}
		return found, count == 1
	}
	for _, candidate := range candidates {
		if prefix := cppOwnerPrefix(candidate.qualified); prefix != "" {
			if c, ok := choose(candidate.fileID, prefix); ok {
				if classScopes[candidate.id] == "" {
					classScopes[candidate.id] = strconv.FormatInt(c.id, 10)
				}
				if namespaceScopes[candidate.id] == "" {
					namespaceScopes[candidate.id] = cppClassNamespace(c.qualified, prefix)
				}
			}
		}
	}
	for edgeID, source := range edgeSources {
		if prefix := cppOwnerPrefix(source.name); prefix != "" {
			if c, ok := choose(source.fileID, prefix); ok {
				if callerClasses[edgeID] == "" {
					callerClasses[edgeID] = strconv.FormatInt(c.id, 10)
				}
				if callerNamespaces[edgeID] == "" {
					callerNamespaces[edgeID] = cppClassNamespace(source.name, prefix)
				}
			}
		}
	}
	return nil
}

func cppOwnerPrefix(name string) string {
	if i := strings.LastIndex(name, "::"); i > 0 && !strings.ContainsAny(name[:i], " ()<>") {
		return name[:i]
	}
	return ""
}

func cppClassNamespace(member, owner string) string {
	if i := strings.LastIndex(member, "::"); i >= 0 {
		className := member[:i]
		if j := strings.LastIndex(className, "::"); j >= 0 {
			return "cpp:" + className[:j]
		}
	}
	if i := strings.LastIndex(owner, "::"); i >= 0 {
		return "cpp:" + owner[:i]
	}
	return "cpp:"
}
