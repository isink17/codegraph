package export

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
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

// JSONPaged returns a JSON-encoded subset of the graph. When the caller
// requests a bounded page over the whole repo (symbol == "" && limit > 0)
// rows are streamed straight from the paging helpers so peak memory is
// O(page), not O(repo). The unbounded (limit <= 0) and focused (symbol != "")
// paths still go through GraphSnapshot since they intentionally want the full
// snapshot or a bounded impact subgraph respectively.
func (s *Service) JSONPaged(ctx context.Context, repoID int64, symbol string, depth, limit, offset int) ([]byte, error) {
	stats, err := s.query.Stats(ctx, repoID)
	if err != nil {
		return nil, err
	}
	var symbols []graph.Symbol
	var edges []store.ExportEdge
	if symbol == "" && limit > 0 {
		symbols, err = s.query.ExportSymbolsPage(ctx, repoID, limit, offset)
		if err != nil {
			return nil, err
		}
		edges, err = s.query.ExportEdgesPage(ctx, repoID, limit, offset)
		if err != nil {
			return nil, err
		}
	} else {
		symbols, edges, err = s.query.GraphSnapshot(ctx, repoID, symbol, depth)
		if err != nil {
			return nil, err
		}
		// GraphSnapshot returns slices in unspecified order (the no-focus
		// loaders have no ORDER BY, and loadEdgesForExport's dedup map
		// further randomises focused-edge order). Sort by ID ASC to match
		// the optimized paging branch and make pageSlice deterministic.
		sort.Slice(symbols, func(i, j int) bool { return symbols[i].ID < symbols[j].ID })
		sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
		symbols = pageSlice(symbols, limit, offset)
		edges = pageSlice(edges, limit, offset)
	}
	return json.MarshalIndent(map[string]any{
		"repo":    stats.RepoRoot,
		"stats":   stats,
		"symbols": symbols,
		"edges":   edges,
	}, "", "  ")
}

func (s *Service) StreamJSONL(ctx context.Context, w io.Writer, repoID int64, pageSize int) error {
	if pageSize <= 0 {
		pageSize = 500
	}
	stats, err := s.query.Stats(ctx, repoID)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(map[string]any{
		"type":  "graph_meta",
		"stats": stats,
	}); err != nil {
		return err
	}
	offset := 0
	for {
		symbols, err := s.query.ExportSymbolsPage(ctx, repoID, pageSize, offset)
		if err != nil {
			return err
		}
		if len(symbols) == 0 {
			break
		}
		for _, sym := range symbols {
			if err := enc.Encode(map[string]any{"type": "symbol", "data": sym}); err != nil {
				return err
			}
		}
		offset += len(symbols)
	}
	offset = 0
	for {
		edges, err := s.query.ExportEdgesPage(ctx, repoID, pageSize, offset)
		if err != nil {
			return err
		}
		if len(edges) == 0 {
			break
		}
		for _, edge := range edges {
			if err := enc.Encode(map[string]any{"type": "edge", "data": edge}); err != nil {
				return err
			}
		}
		offset += len(edges)
	}
	return enc.Encode(map[string]any{"type": "graph_done"})
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

func pageSlice[T any](items []T, limit, offset int) []T {
	start := clampOffset(offset, len(items))
	end := clampEnd(start, limit, len(items))
	return items[start:end]
}

func clampOffset(offset, length int) int {
	if offset < 0 {
		offset = 0
	}
	if offset > length {
		offset = length
	}
	return offset
}

func clampEnd(start, limit, length int) int {
	if limit <= 0 {
		return length
	}
	end := start + limit
	if end > length {
		end = length
	}
	return end
}
