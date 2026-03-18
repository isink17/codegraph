package export

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/query"
)

type Service struct {
	query *query.Service
}

func New(q *query.Service) *Service {
	return &Service{query: q}
}

func (s *Service) JSON(ctx context.Context, repoID int64) ([]byte, error) {
	stats, err := s.query.Stats(ctx, repoID)
	if err != nil {
		return nil, err
	}
	symbols, edges, err := s.query.GraphSnapshot(ctx, repoID, "", 0)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(map[string]any{
		"repo":    stats.RepoRoot,
		"stats":   stats,
		"symbols": symbols,
		"edges":   edges,
	}, "", "  ")
}

func (s *Service) DOT(ctx context.Context, repoID int64, symbol string, depth int) ([]byte, error) {
	symbols, edges, err := s.query.GraphSnapshot(ctx, repoID, symbol, depth)
	if err != nil {
		return nil, err
	}
	nodeSet := map[string]struct{}{}
	for _, sym := range symbols {
		if sym.QualifiedName != "" {
			nodeSet[sym.QualifiedName] = struct{}{}
		}
	}
	var b strings.Builder
	b.WriteString("digraph codegraph {\n")
	nodes := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		b.WriteString(fmt.Sprintf("  %q;\n", n))
	}
	for _, edge := range edges {
		src := edge.SrcQualifiedName
		if src == "" {
			src = fmt.Sprintf("symbol#%d", edge.SrcSymbolID)
		}
		dst := edge.DstQualifiedName
		attrs := []string{}
		if dst == "" {
			dst = edge.DstName
			if dst == "" {
				continue
			}
			attrs = append(attrs, `style="dashed"`)
		}
		attrs = append(attrs, fmt.Sprintf(`label=%q`, edge.Kind))
		b.WriteString(fmt.Sprintf("  %q -> %q [%s];\n", src, dst, strings.Join(attrs, ",")))
	}
	b.WriteString("}\n")
	return []byte(b.String()), nil
}
