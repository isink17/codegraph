package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// These are the persisted helper identities from the missing-helper report.
func calleeContinuationFixture(t *testing.T, n int) (*Store, int64, int64) {
	t.Helper()
	s, repo := openBudgetStore(t)
	ctx := context.Background()
	file := must(insertTestFile(ctx, s, repo.ID, "store.go"))
	src := must(insertTestSymbol(ctx, s, repo.ID, file, "ImpactRadius", "store.Store.ImpactRadius"))
	names := []string{"beginParserSemanticGraphRead", "impactClosureWithPresence", "safeLimit", "safeOffset"}
	for i := n - 1; i >= 0; i-- {
		name := fmt.Sprintf("helper%04d", i)
		if n == 4 {
			name = names[i]
		}
		dst := must(insertTestSymbol(ctx, s, repo.ID, file, name, "store."+name))
		for range 2 {
			insertResolvedTestEdge(t, s, repo.ID, file, src, dst, "store."+name)
		}
	}
	must(insertTestEdge(ctx, s, repo.ID, file, src, "unresolved"))
	return s, repo.ID, src
}

func TestCalleeContinuationBoundaries(t *testing.T) {
	for _, n := range []int{0, 2, 3, 4, 8, 2001} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s, repo, src := calleeContinuationFixture(t, n)
			before := persistentGraphSnapshot(t, s)
			for _, p := range [][2]int{{3, 0}, {3, 3}, {3, 6}, {3, n}, {3, n + 10}, {0, -1}, {-1, -1}, {2000, 0}, {int(^uint(0) >> 1), 0}, {3, int(^uint(0) >> 1)}} {
				limit, offset := safeLimit(p[0]), safeOffset(p[1])
				got := must(s.FindCalleesResult(context.Background(), repo, "", src, p[0], p[1]))
				repeat := must(s.FindCalleesResult(context.Background(), repo, "", src, p[0], p[1]))
				legacy := must(s.FindCallees(context.Background(), repo, "", src, p[0], p[1]))
				if !reflect.DeepEqual(got, repeat) || !reflect.DeepEqual(got.Callees, legacy) {
					t.Fatal("page identity/order changed")
				}
				// Decode the additive contract so this regression runs against the old types.
				wire := map[string]json.RawMessage{}
				raw := must(json.Marshal(got))
				mustOK(t, json.Unmarshal(raw, &wire))
				var more bool
				if _, ok := wire["has_more"]; !ok {
					t.Fatal("missing has_more continuation metadata")
				}
				mustOK(t, json.Unmarshal(wire["has_more"], &more))
				wantMore := offset < n && n-offset > limit
				if more != wantMore {
					t.Fatalf("n=%d page=%v more=%v", n, p, more)
				}
				if more {
					var next int
					mustOK(t, json.Unmarshal(wire["next_offset"], &next))
					if next != offset+len(got.Callees) {
						t.Fatal("wrong next offset")
					}
				} else if _, ok := wire["next_offset"]; ok {
					t.Fatal("terminal page advertises next offset")
				}
			}
			if persistentGraphSnapshot(t, s) != before {
				t.Fatal("query mutated graph")
			}
		})
	}
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCalleeContinuationWalkAndSnapshot(t *testing.T) {
	s, repo, src := calleeContinuationFixture(t, 4)
	ctx := context.Background()
	readCtx, tx, err := s.beginParserSemanticGraphRead(ctx, repo)
	mustOK(t, err)
	defer tx.Rollback()
	// A later commit must not change either the page or lookahead in the pinned read.
	mustOK(t, func() error { _, err := s.db.ExecContext(ctx, `DELETE FROM edges WHERE repo_id=?`, repo); return err }())
	var names []string
	offset := 0
	for {
		got := must(s.FindCalleesResult(readCtx, repo, "", src, 3, offset))
		for _, sym := range got.Callees {
			names = append(names, sym.Name)
		}
		wire := map[string]json.RawMessage{}
		mustOK(t, json.Unmarshal(must(json.Marshal(got)), &wire))
		var more bool
		mustOK(t, json.Unmarshal(wire["has_more"], &more))
		if !more {
			break
		}
		mustOK(t, json.Unmarshal(wire["next_offset"], &offset))
	}
	want := []string{"beginParserSemanticGraphRead", "impactClosureWithPresence", "safeLimit", "safeOffset"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("helpers=%v", names)
	}
	fresh := must(s.FindCalleesResult(ctx, repo, "", src, 3, 0))
	if len(fresh.Callees) != 0 {
		t.Fatal("fresh snapshot retained removed edges")
	}
}

func TestCalleeContinuationFiltersBeforeLookahead(t *testing.T) {
	s, repo, src := calleeContinuationFixture(t, 3)
	ctx := context.Background()
	deletedFile := must(insertTestFile(ctx, s, repo, "a-deleted.go"))
	deleted := must(insertTestSymbol(ctx, s, repo, deletedFile, "Ghost", "a.Ghost"))
	insertResolvedTestEdge(t, s, repo, deletedFile, src, deleted, "a.Ghost")
	_, err := s.db.ExecContext(ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, deletedFile)
	mustOK(t, err)
	foreign := must(s.UpsertRepo(ctx, t.TempDir()))
	file := must(insertTestFile(ctx, s, foreign.ID, "a-foreign.go"))
	dst := must(insertTestSymbol(ctx, s, foreign.ID, file, "Foreign", "a.Foreign"))
	insertResolvedTestEdge(t, s, repo, deletedFile, src, dst, "a.Foreign")
	got := must(s.FindCalleesResult(ctx, repo, "", src, 3, 0))
	if got.HasMore || len(got.Callees) != 3 {
		t.Fatalf("filtered boundary=%+v", got)
	}
	for _, id := range []int64{dst, src + 999999} {
		missing := must(s.FindCalleesResult(ctx, repo, "", id, 3, 0))
		if missing.TargetFound || missing.HasMore || len(missing.Callees) != 0 {
			t.Fatalf("foreign/missing seed=%+v", missing)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.FindCalleesResult(cancelled, repo, "", src, 3, 0); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

// Diagnostic measurements are observations, never performance assertions.
func TestCalleeContinuationQueryCost(t *testing.T) {
	for _, n := range []int{4, 201, 2001} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s, repo, src := calleeContinuationFixture(t, n)
			ctx := context.Background()
			guard := guardNeighborStatements(s)
			must(s.FindCalleesResult(ctx, repo, "", src, 3, 0))
			if len(guard.statements) != 1 {
				t.Fatalf("page SELECT statements=%d", len(guard.statements))
			}
			for _, stmt := range guard.statements {
				t.Logf("bounded page statement: %v", stmt)
			}
			s.neighborStatementWrap = nil
			const runs = 50
			for _, lookahead := range []bool{false, true} {
				start := time.Now()
				for range runs {
					if lookahead {
						must(s.FindCalleesResult(ctx, repo, "", src, 3, 0))
					} else {
						must(s.FindCallees(ctx, repo, "", src, 3, 0))
					}
				}
				t.Logf("n=%d limit=3 lookahead=%t runs=%d total=%s", n, lookahead, runs, time.Since(start))
			}
		})
	}
}
