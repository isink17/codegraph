package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/isink17/codegraph/internal/limits"
)

// Edge explanation statuses and verdicts.
const (
	ExplainStatusResolved   = "resolved"
	ExplainStatusUnresolved = "unresolved"

	// ExplainPersisted: the edge is bound; strategy and confidence are the
	// values the resolver persisted, not a reconstruction.
	ExplainPersisted = "persisted_resolution"
	// ExplainRefused: at least one inventory rule provably refuses the edge.
	ExplainRefused = "refusing_rules"
	// ExplainUnknown: no evaluated rule refuses the edge. That is not a proof
	// that nothing refused it; see ExplainResult.NotEvaluated.
	ExplainUnknown = "unknown"
)

// ErrNoEdgeMatches is returned when a selector names no edge of the repository.
var ErrNoEdgeMatches = errors.New("no edge matches the selector")

// EdgeSelector picks edges by id, or by file and line with an optional exact
// destination name. Exactly one form must be given.
type EdgeSelector struct {
	EdgeID int64
	File   string
	Line   int
	Name   string
	Limit  int
	Offset int
}

// Validate rejects a selector that names no edge form, both forms, or a page
// outside the shared bounds.
func (sel EdgeSelector) Validate() error {
	byID := sel.EdgeID != 0
	byPos := sel.File != "" || sel.Line != 0 || sel.Name != ""
	switch {
	case sel.EdgeID < 0:
		return fmt.Errorf("invalid edge_id %d: want a positive id", sel.EdgeID)
	case byID && byPos:
		return errors.New("give edge_id, or file and line, not both")
	case !byID && (sel.File == "" || sel.Line <= 0):
		return errors.New("give edge_id, or file and a positive line")
	case sel.Limit < 0 || sel.Limit > limits.MaxPage:
		return fmt.Errorf("invalid limit %d: want 0..%d", sel.Limit, limits.MaxPage)
	case sel.Offset < 0:
		return fmt.Errorf("invalid offset %d: want 0 or more", sel.Offset)
	}
	return nil
}

// ExplainSymbol is one end of an edge.
type ExplainSymbol struct {
	SymbolID      int64  `json:"symbol_id"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	File          string `json:"file,omitempty"`
	Line          int    `json:"line,omitempty"`
}

// ExplainRule is one inventory rule that refuses an edge.
type ExplainRule struct {
	ID          string `json:"id"`
	Stage       string `json:"stage"`
	Disposition string `json:"disposition"`
}

// EdgeExplanation is the current-state explanation of one edge.
type EdgeExplanation struct {
	EdgeID   int64          `json:"edge_id"`
	Source   *ExplainSymbol `json:"source"`
	DstName  string         `json:"dst_name"`
	Kind     string         `json:"kind"`
	File     string         `json:"file"`
	Line     int            `json:"line"`
	Language string         `json:"language"`
	Status   string         `json:"status"`
	// Explanation is ExplainPersisted, ExplainRefused or ExplainUnknown.
	Explanation          string         `json:"explanation"`
	Target               *ExplainSymbol `json:"target,omitempty"`
	ResolutionStrategy   string         `json:"resolution_strategy,omitempty"`
	ResolutionConfidence string         `json:"resolution_confidence,omitempty"`
	RefusingRules        []ExplainRule  `json:"refusing_rules,omitempty"`
}

// ExplainResult is one page of edge explanations.
type ExplainResult struct {
	// Scope is always "current_graph_state": the answer reads the graph as it
	// is now and promises nothing about how an edge was decided in the past.
	Scope string `json:"scope"`
	// NotEvaluated lists the inventory rules no explanation consults, and
	// PartiallyEvaluated those consulted for only part of their domain. An
	// unresolved edge reported as unknown may be refused by one of them.
	NotEvaluated       []string          `json:"not_evaluated"`
	PartiallyEvaluated []string          `json:"partially_evaluated"`
	Total              int               `json:"total"`
	Limit              int               `json:"limit"`
	Offset             int               `json:"offset"`
	Edges              []EdgeExplanation `json:"edges"`
}

var explainStageNames = map[resolverRuleStage]string{
	resolverStagePopulation:      "population",
	resolverStageOwnership:       "ownership",
	resolverStageChosenCandidate: "chosen_candidate",
	resolverStageBroadAmbiguity:  "broad_ambiguity",
	resolverStageOwnModule:       "own_module",
}

var explainDispositionNames = map[resolverRuleDisposition]string{
	resolverDispositionIneligible: "ineligible",
	resolverDispositionOwned:      "owned",
	resolverDispositionAmbiguous:  "ambiguous",
}

// explainNotEvaluated derives, from the inventory, the rules edgeRefusalReasons
// never consults (see edgeRefusalEvaluates).
func explainNotEvaluated() []string {
	var out []string
	for _, rule := range resolverBindGateRules {
		if !edgeRefusalEvaluates(rule) {
			out = append(out, string(rule.id))
		}
	}
	return out
}

// ExplainEdges explains the selected edges of a repository from its current
// state: a resolved edge by its persisted strategy and confidence, an
// unresolved one by the inventory rules edgeRefusalReasons proves refuse it.
// It only reads, and reads everything -- the edges and every fact the rules
// are judged on -- in one read-only transaction, so a concurrent writer cannot
// pair rows from one graph state with facts from another. Edges are ordered by
// file, line, destination name, kind and id.
func (s *Store) ExplainEdges(ctx context.Context, repoID int64, sel EdgeSelector) (ExplainResult, error) {
	if err := sel.Validate(); err != nil {
		return ExplainResult{}, err
	}
	limit, offset := limits.PageRows(sel.Limit), sel.Offset
	res := ExplainResult{
		Scope: "current_graph_state", NotEvaluated: explainNotEvaluated(),
		PartiallyEvaluated: []string{string(ruleBroadAmbiguity)},
		Limit:              limit, Offset: offset, Edges: []EdgeExplanation{},
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	where := `e.repo_id = ? AND e.id = ?`
	args := []any{repoID, sel.EdgeID}
	if sel.EdgeID == 0 {
		where = `e.repo_id = ? AND f.path = ? AND e.line = ? AND (? = '' OR e.dst_name = ?)`
		args = []any{repoID, CanonicalRelPath(sel.File), sel.Line, sel.Name, sel.Name}
	}
	const from = ` FROM edges e JOIN files f ON f.id = e.file_id AND f.repo_id = e.repo_id AND f.is_deleted = 0`
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)`+from+` WHERE `+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}
	if res.Total == 0 {
		return res, ErrNoEdgeMatches
	}

	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.dst_name, e.edge_kind, f.path, e.line, f.language,
			e.resolution_strategy, e.resolution_confidence, e.dst_symbol_id IS NOT NULL,
			src.id, src.name, src.qualified_name,
			dst.id, dst.name, dst.qualified_name, dstf.path, dst.start_line`+
		from+`
		LEFT JOIN symbols src ON src.id = e.src_symbol_id AND src.repo_id = e.repo_id
		LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id AND dst.repo_id = e.repo_id
		LEFT JOIN files dstf ON dstf.id = dst.file_id
		WHERE `+where+`
		ORDER BY f.path, e.line, e.dst_name, e.edge_kind, e.id LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return res, err
	}
	var unresolved []int64
	for rows.Next() {
		var x EdgeExplanation
		var resolved bool
		var srcID, dstID, dstLine *int64
		var srcName, srcQN, dstName, dstQN, dstFile *string
		if err = rows.Scan(&x.EdgeID, &x.DstName, &x.Kind, &x.File, &x.Line, &x.Language,
			&x.ResolutionStrategy, &x.ResolutionConfidence, &resolved,
			&srcID, &srcName, &srcQN, &dstID, &dstName, &dstQN, &dstFile, &dstLine); err != nil {
			_ = rows.Close()
			return res, err
		}
		if srcID != nil {
			x.Source = &ExplainSymbol{SymbolID: *srcID, Name: *srcName, QualifiedName: *srcQN}
		}
		if resolved {
			x.Status, x.Explanation = ExplainStatusResolved, ExplainPersisted
			if dstID != nil {
				x.Target = &ExplainSymbol{SymbolID: *dstID, Name: *dstName, QualifiedName: *dstQN, Line: int(*dstLine)}
				if dstFile != nil {
					x.Target.File = *dstFile
				}
			}
		} else {
			// Strategy and confidence describe a binding; an unresolved
			// edge has none to report.
			x.Status, x.Explanation = ExplainStatusUnresolved, ExplainUnknown
			x.ResolutionStrategy, x.ResolutionConfidence = "", ""
			// A cross-language reference is decided by its own pass from
			// import-bridge evidence, not by the strategies the inventory
			// gates, so a gate rule refusing it would not explain why it is
			// unresolved. It is left unknown rather than attributed.
			if x.Kind != EdgeKindCrossLanguageRef {
				unresolved = append(unresolved, x.EdgeID)
			}
		}
		res.Edges = append(res.Edges, x)
	}
	// Close before the next query: the transaction holds one connection.
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return res, err
	}
	if len(unresolved) == 0 {
		return res, nil
	}
	reasons, err := edgeRefusalReasonsQuery(ctx, tx, repoID, unresolved)
	if err != nil {
		return res, err
	}
	rules := make(map[resolverRuleID]resolverGateRule, len(resolverBindGateRules))
	for _, rule := range resolverBindGateRules {
		rules[rule.id] = rule
	}
	for i := range res.Edges {
		for _, id := range reasons[res.Edges[i].EdgeID] {
			rule := rules[id]
			res.Edges[i].RefusingRules = append(res.Edges[i].RefusingRules, ExplainRule{
				ID: string(id), Stage: explainStageNames[rule.stage], Disposition: explainDispositionNames[rule.disposition]})
		}
		if len(res.Edges[i].RefusingRules) > 0 {
			res.Edges[i].Explanation = ExplainRefused
		}
	}
	return res, nil
}
