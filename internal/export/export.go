package export

import (
	"context"
	"encoding/json"
	"fmt"
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
	return json.MarshalIndent(map[string]any{
		"repo":  stats.RepoRoot,
		"stats": stats,
	}, "", "  ")
}

func (s *Service) DOT(ctx context.Context, repoID int64, symbol string, depth int) ([]byte, error) {
	impact, err := s.query.ImpactRadius(ctx, repoID, []string{symbol}, nil, depth)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("digraph codegraph {\n")
	if symbols, ok := impact["symbols"].([]any); ok {
		for _, item := range symbols {
			b.WriteString(fmt.Sprintf("  \"%v\";\n", item))
		}
	}
	b.WriteString("}\n")
	return []byte(b.String()), nil
}
