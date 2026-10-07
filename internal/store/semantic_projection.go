package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SemanticGraph is the stable, database-ID-free portion of an indexed graph.
// IDs are used only to join rows while this value is built.
type SemanticGraph struct {
	SchemaVersion int64                 `json:"schema_version"`
	Files         []SemanticFile        `json:"files"`
	Declarations  []SemanticDeclaration `json:"declarations"`
	Edges         []SemanticEdge        `json:"edges"`
	TestLinks     []SemanticTestLink    `json:"test_links"`
	State         SemanticGraphState    `json:"state"`
}

type SemanticFile struct {
	Path                string `json:"path"`
	Language            string `json:"language"`
	ContentHash         string `json:"content_hash"`
	ParseState          string `json:"parse_state"`
	ParserProfile       string `json:"parser_profile"`
	ParserCallEdges     bool   `json:"parser_call_edges"`
	ParserSemanticEpoch int    `json:"parser_semantic_epoch"`
}

type SemanticDeclaration struct {
	Path          string `json:"path"`
	Language      string `json:"language"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Signature     string `json:"signature"`
	Visibility    string `json:"visibility"`
	StableKey     string `json:"stable_key"`
	StartLine     int    `json:"start_line"`
	StartCol      int    `json:"start_col"`
	EndLine       int    `json:"end_line"`
	EndCol        int    `json:"end_col"`
	IsStatic      *bool  `json:"is_static,omitempty"`
	ArityMin      *int   `json:"arity_min,omitempty"`
	ArityMax      *int   `json:"arity_max,omitempty"`
}

type SemanticEndpoint struct {
	Path          string `json:"path,omitempty"`
	Language      string `json:"language,omitempty"`
	Kind          string `json:"kind,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Signature     string `json:"signature,omitempty"`
	StableKey     string `json:"stable_key,omitempty"`
	State         string `json:"state"` // resolved, unresolved, missing, or ambiguous
}

type SemanticEdge struct {
	Source     SemanticEndpoint `json:"source"`
	Target     SemanticEndpoint `json:"target"`
	Path       string           `json:"path"`
	Kind       string           `json:"kind"`
	Evidence   string           `json:"evidence"`
	Strategy   string           `json:"strategy"`
	Confidence string           `json:"confidence"`
	Line       int              `json:"line"`
	CallArity  *int             `json:"call_arity,omitempty"`
}

type SemanticTestLink struct {
	TestFile   string           `json:"test_file"`
	TargetFile string           `json:"target_file,omitempty"`
	Test       SemanticEndpoint `json:"test"`
	Target     SemanticEndpoint `json:"target"`
	Reason     string           `json:"reason"`
	Score      float64          `json:"score"`
}

type SemanticGraphState struct {
	Capability       GraphCapability   `json:"capability"`
	Pending          string            `json:"pending,omitempty"`
	DirtyFiles       int               `json:"dirty_files"`
	ParseFailures    int               `json:"parse_failures"`
	ResolverPolicies map[string]string `json:"resolver_policies"`
	ParserSemantics  map[string]string `json:"parser_semantics"`
	JVMScope         SemanticJVMScope  `json:"jvm_scope"`
}

type SemanticJVMScope struct {
	Domain     string `json:"domain"`
	Version    int    `json:"version"`
	State      string `json:"state"`
	Provenance string `json:"provenance"`
}

// SemanticGraph returns a deterministic graph projection. It refuses pending
// parser transitions, because the stored graph can contain mixed semantics.
func (s *Store) SemanticGraph(ctx context.Context, repoID int64) (SemanticGraph, error) {
	var out SemanticGraph
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&out.SchemaVersion); err != nil {
		return out, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dirty_files WHERE repo_id=?`, repoID).Scan(&out.State.DirtyFiles); err != nil {
		return out, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE repo_id=? AND is_deleted=0 AND parse_state IN ('failed','oversize','pending')`, repoID).Scan(&out.State.ParseFailures); err != nil {
		return out, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT path,language,content_sha256,parse_state,parser_profile,parser_call_edges,parser_semantic_epoch FROM files WHERE repo_id=? AND is_deleted=0 ORDER BY path`, repoID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var f SemanticFile
		var call int
		if err := rows.Scan(&f.Path, &f.Language, &f.ContentHash, &f.ParseState, &f.ParserProfile, &call, &f.ParserSemanticEpoch); err != nil {
			rows.Close()
			return out, err
		}
		f.ParserCallEdges = call != 0
		out.Files = append(out.Files, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()

	declByID := map[int64]SemanticEndpoint{}
	declIdentityByID := map[int64]string{}
	declIdentityCounts := map[string]int{}
	rows, err = tx.QueryContext(ctx, `SELECT s.id,f.path,s.language,s.kind,s.name,s.qualified_name,s.signature,s.visibility,s.stable_key,s.start_line,s.start_col,s.end_line,s.end_col,s.is_static,s.arity_min,s.arity_max FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.is_deleted=0 ORDER BY f.path,s.language,s.kind,s.qualified_name,s.signature,s.stable_key,s.start_line,s.start_col`, repoID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id int64
		var d SemanticDeclaration
		var stat, amin, amax sql.NullInt64
		if err := rows.Scan(&id, &d.Path, &d.Language, &d.Kind, &d.Name, &d.QualifiedName, &d.Signature, &d.Visibility, &d.StableKey, &d.StartLine, &d.StartCol, &d.EndLine, &d.EndCol, &stat, &amin, &amax); err != nil {
			rows.Close()
			return out, err
		}
		if stat.Valid {
			v := stat.Int64 != 0
			d.IsStatic = &v
		}
		if amin.Valid {
			v := int(amin.Int64)
			d.ArityMin = &v
		}
		if amax.Valid {
			v := int(amax.Int64)
			d.ArityMax = &v
		}
		out.Declarations = append(out.Declarations, d)
		key := semanticDeclarationIdentity(d)
		declByID[id] = SemanticEndpoint{Path: d.Path, Language: d.Language, Kind: d.Kind, QualifiedName: d.QualifiedName, Signature: d.Signature, StableKey: d.StableKey, State: "resolved"}
		declIdentityByID[id] = key
		declIdentityCounts[key]++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	for id, key := range declIdentityByID {
		if declIdentityCounts[key] > 1 {
			endpoint := declByID[id]
			endpoint.State = "ambiguous"
			declByID[id] = endpoint
		}
	}

	rows, err = tx.QueryContext(ctx, `SELECT e.src_symbol_id,e.dst_symbol_id,e.dst_name,e.edge_kind,e.evidence,f.path,e.line,e.resolution_strategy,e.resolution_confidence,e.call_arity FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.is_deleted=0 ORDER BY f.path,e.line,e.edge_kind,e.dst_name,e.evidence,e.resolution_strategy,e.resolution_confidence`, repoID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var src int64
		var dst sql.NullInt64
		var name, kind, evidence, path, strategy, confidence string
		var line int
		var arity sql.NullInt64
		if err := rows.Scan(&src, &dst, &name, &kind, &evidence, &path, &line, &strategy, &confidence, &arity); err != nil {
			rows.Close()
			return out, err
		}
		source, ok := declByID[src]
		if !ok {
			return out, fmt.Errorf("semantic projection: edge source declaration missing")
		}
		target := SemanticEndpoint{State: "unresolved", QualifiedName: name}
		if dst.Valid {
			if d, found := declByID[dst.Int64]; found {
				target = d
			} else {
				target.State = "missing"
			}
		}
		edge := SemanticEdge{Source: source, Target: target, Path: path, Kind: kind, Evidence: evidence, Strategy: strategy, Confidence: confidence, Line: line}
		if arity.Valid {
			v := int(arity.Int64)
			edge.CallArity = &v
		}
		out.Edges = append(out.Edges, edge)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()

	rows, err = tx.QueryContext(ctx, `SELECT tf.path,ts.id,ts.language,ts.kind,ts.qualified_name,ts.signature,ts.stable_key,lf.path,ls.id,ls.language,ls.kind,ls.qualified_name,ls.signature,ls.stable_key,t.reason,t.score FROM test_links t JOIN files tf ON tf.id=t.test_file_id LEFT JOIN symbols ts ON ts.id=t.test_symbol_id LEFT JOIN files lf ON lf.id=t.target_file_id LEFT JOIN symbols ls ON ls.id=t.target_symbol_id WHERE t.repo_id=? AND tf.is_deleted=0 ORDER BY tf.path,lf.path,t.reason,t.score`, repoID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var link SemanticTestLink
		var tsi, lsi sql.NullInt64
		var tsl, tst, tsq, tss, tsk sql.NullString
		var lsl, lst, lsq, lss, lsk sql.NullString
		var targetFile sql.NullString
		if err := rows.Scan(&link.TestFile, &tsi, &tsl, &tst, &tsq, &tss, &tsk, &targetFile, &lsi, &lsl, &lst, &lsq, &lss, &lsk, &link.Reason, &link.Score); err != nil {
			rows.Close()
			return out, err
		}
		if targetFile.Valid {
			link.TargetFile = targetFile.String
		}
		link.Test = SemanticEndpoint{State: "missing"}
		if tsi.Valid {
			link.Test = SemanticEndpoint{Path: link.TestFile, Language: tsl.String, Kind: tst.String, QualifiedName: tsq.String, Signature: tss.String, StableKey: tsk.String, State: "resolved"}
		}
		link.Target = SemanticEndpoint{State: "missing"}
		if lsi.Valid {
			link.Target = SemanticEndpoint{Path: link.TargetFile, Language: lsl.String, Kind: lst.String, QualifiedName: lsq.String, Signature: lss.String, StableKey: lsk.String, State: "resolved"}
		} else if targetFile.Valid {
			link.Target = SemanticEndpoint{Path: link.TargetFile, State: "unresolved"}
		}
		out.TestLinks = append(out.TestLinks, link)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()

	var groups []FileParserProfileGroup
	profileRows, err := tx.QueryContext(ctx, `SELECT language,parser_profile,parser_call_edges,COUNT(*) FROM files WHERE repo_id=? AND is_deleted=0 GROUP BY language,parser_profile,parser_call_edges ORDER BY language,parser_profile,parser_call_edges`, repoID)
	if err != nil {
		return out, err
	}
	for profileRows.Next() {
		var g FileParserProfileGroup
		if err := profileRows.Scan(&g.Language, &g.Profile, &g.CallEdges, &g.Files); err != nil {
			profileRows.Close()
			return out, err
		}
		groups = append(groups, g)
	}
	if err := profileRows.Err(); err != nil {
		profileRows.Close()
		return out, err
	}
	profileRows.Close()
	out.State.Capability = ClassifyGraphCapability(groups)
	out.State.ResolverPolicies = map[string]string{}
	out.State.ParserSemantics = map[string]string{}
	settings, err := tx.QueryContext(ctx, `SELECT key,value FROM settings WHERE key LIKE ? OR key LIKE ? OR key LIKE ? ORDER BY key`, fmt.Sprintf("resolver.policy.%d.%%", repoID), fmt.Sprintf("parser.semantic.%d.%%", repoID), fmt.Sprintf("parser.semantic.pending.%d.%%", repoID))
	if err != nil {
		return out, err
	}
	for settings.Next() {
		var k, v string
		if err := settings.Scan(&k, &v); err != nil {
			settings.Close()
			return out, err
		}
		if len(k) >= len("resolver.policy.") && k[:len("resolver.policy.")] == "resolver.policy." {
			out.State.ResolverPolicies[k] = v
		} else {
			out.State.ParserSemantics[k] = v
		}
	}
	if err := settings.Err(); err != nil {
		settings.Close()
		return out, err
	}
	settings.Close()
	if out.State.DirtyFiles > 0 {
		out.State.Pending = "dirty files remain"
	}
	for _, v := range out.State.ParserSemantics {
		if len(v) >= 3 && v[:3] == "v1:" {
			out.State.Pending = "parser semantic transition pending"
			break
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT evidence_domain,evidence_version,state,provenance FROM jvm_compilation_scope_evidence WHERE repo_id=?`, repoID).Scan(&out.State.JVMScope.Domain, &out.State.JVMScope.Version, &out.State.JVMScope.State, &out.State.JVMScope.Provenance)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	return out, nil
}

func semanticDeclarationIdentity(d SemanticDeclaration) string {
	return fmt.Sprintf("%q\x00%q\x00%q\x00%q\x00%q\x00%q", d.Path, d.Language, d.Kind, d.QualifiedName, d.Signature, d.StableKey)
}
