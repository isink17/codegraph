package query

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// contextFixture is a small indexed Go repository shared by the
// context_for_task tests. It is deliberately shaped to exercise ranking:
//
//   - paymentsvc holds the direct semantic target (ProcessPayment) plus a
//     private helper it calls,
//   - handler calls ProcessPayment, so it is caller context,
//   - util.Log is a hub with high fan-in and no task relevance,
//   - billing.Renew and subscription.Renew are same-name symbols in different
//     packages, for the seed-identity regression,
//   - paymentsvc/service_test.go is test context.
type contextFixture struct {
	repoRoot string
	repoID   int64
	store    *store.Store
	svc      *Service
	indexer  *indexer.Indexer
}

func newContextFixture(t testing.TB) *contextFixture {
	t.Helper()
	return newContextFixtureWithEmbedder(t, nil)
}

func newContextFixtureWithEmbedder(t testing.TB, emb embedderForTest) *contextFixture {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()

	writeFixtureFile(t, repoRoot, "paymentsvc/service.go", `package paymentsvc

import "example.test/fixture/util"

// ProcessPayment charges a payment and retries the charge on failure.
func ProcessPayment(id string) error {
	util.Log("process payment")
	return chargeCard(id)
}

// chargeCard performs the payment charge attempt.
func chargeCard(id string) error {
	util.Log("charge card")
	return nil
}
`)
	writeFixtureFile(t, repoRoot, "paymentsvc/service_test.go", `package paymentsvc

import "testing"

func TestProcessPayment(t *testing.T) {
	if err := ProcessPayment("id"); err != nil {
		t.Fatal(err)
	}
}
`)
	writeFixtureFile(t, repoRoot, "handler/handler.go", `package handler

import "example.test/fixture/paymentsvc"

// HandleCheckout is the HTTP entry point for a checkout request.
func HandleCheckout(id string) error {
	return paymentsvc.ProcessPayment(id)
}
`)
	writeFixtureFile(t, repoRoot, "util/hub.go", `package util

// Log writes a message. Called from everywhere in the fixture.
func Log(msg string) {}
`)
	// A related test that is not itself a caller. Every other _test.go here calls
	// the symbol it covers, and since P22.6 those same-package bare calls resolve,
	// which makes those files ordinary callers rather than test-section entries.
	writeFixtureFile(t, repoRoot, "util/hub_test.go", `package util

import "testing"

func TestLog(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
}
`)
	writeFixtureFile(t, repoRoot, "billing/renew.go", `package billing

import "example.test/fixture/util"

// Renew renews a billing subscription payment.
func Renew(id string) error {
	util.Log("billing renew")
	return nil
}

// BillingDriver drives billing renewals.
func BillingDriver(id string) error { return Renew(id) }
`)
	writeFixtureFile(t, repoRoot, "billing/renew_test.go", `package billing

import "testing"

func TestRenew(t *testing.T) {
	if err := Renew("id"); err != nil {
		t.Fatal(err)
	}
}
`)
	writeFixtureFile(t, repoRoot, "subscription/renew.go", `package subscription

// Renew renews a subscription record.
func Renew(id string) error { return nil }

// SubscriptionDriver drives subscription renewals.
func SubscriptionDriver(id string) error { return Renew(id) }
`)
	writeFixtureFile(t, repoRoot, "go.mod", "module example.test/fixture\n\ngo 1.22\n")

	st, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	idx := indexer.New(st, parser.NewRegistry(goparser.New()), nil)
	repo, err := st.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	fx := &contextFixture{repoRoot: repoRoot, repoID: repo.ID, store: st, indexer: idx}
	if emb == nil {
		fx.svc = New(st, nil)
		return fx
	}
	fx.svc = New(st, emb)
	fx.seedEmbeddings(t, emb)
	return fx
}

func writeFixtureFile(t testing.TB, repoRoot, rel, content string) {
	t.Helper()
	path := filepath.Join(repoRoot, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

// newContextFixtureForBench is the same fixture, for benchmarks.
func newContextFixtureForBench(b *testing.B) *contextFixture { return newContextFixture(b) }
