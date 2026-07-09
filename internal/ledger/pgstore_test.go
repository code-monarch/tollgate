package ledger_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tollgate/tollgate/internal/ledger"
)

// TestPGStore runs the Store contract against a real Postgres. It is skipped
// unless TOLLGATE_TEST_DB is set to a DSN whose database already has the schema
// from db/schema.sql applied. See scripts/pgtest.ps1.
func TestPGStore(t *testing.T) {
	dsn := os.Getenv("TOLLGATE_TEST_DB")
	if dsn == "" {
		t.Skip("set TOLLGATE_TEST_DB to run the Postgres store test")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE ledger_entries, transactions RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate (is the schema applied?): %v", err)
	}

	store := ledger.NewPGStoreFromPool(pool)
	now := time.Now().UTC()

	mkPosting := func(hash string) ledger.Posting {
		return ledger.Posting{
			Tx: ledger.Transaction{
				ID: "txn_" + hash, AgentID: "agt_1", Amount: 1000, Currency: "USDC",
				Status: ledger.StatusSettled, RequestHash: hash, CreatedAt: now, SettledAt: &now,
			},
			Entries: []ledger.Entry{
				{WalletID: "wallet:a", Direction: ledger.Debit, Amount: 1000, Currency: "USDC"},
				{WalletID: "wallet:b", Direction: ledger.Credit, Amount: 1000, Currency: "USDC"},
			},
		}
	}

	// Post + derived balances.
	if _, created, err := store.Post(ctx, mkPosting("h1")); err != nil || !created {
		t.Fatalf("post h1: created=%v err=%v", created, err)
	}
	if b, _ := store.Balance(ctx, "wallet:a", "USDC"); b != -1000 {
		t.Fatalf("wallet:a = %d, want -1000", b)
	}
	if b, _ := store.Balance(ctx, "wallet:b", "USDC"); b != 1000 {
		t.Fatalf("wallet:b = %d, want 1000", b)
	}

	// Idempotency: same hash → no new rows, created=false, balance unchanged.
	if _, created, err := store.Post(ctx, mkPosting("h1")); err != nil || created {
		t.Fatalf("replay h1: created=%v err=%v (want created=false)", created, err)
	}
	if b, _ := store.Balance(ctx, "wallet:b", "USDC"); b != 1000 {
		t.Fatalf("wallet:b after replay = %d, want 1000 (no double charge)", b)
	}

	// Lookup by request hash.
	if tx, ok, err := store.TransactionByRequestHash(ctx, "h1"); err != nil || !ok || tx.ID != "txn_h1" {
		t.Fatalf("lookup h1: ok=%v id=%s err=%v", ok, tx.ID, err)
	}
	if _, ok, _ := store.TransactionByRequestHash(ctx, "missing"); ok {
		t.Fatal("lookup of missing hash returned ok=true")
	}

	// Unbalanced posting is rejected before any write.
	bad := mkPosting("h_bad")
	bad.Entries[1].Amount = 999
	if _, _, err := store.Post(ctx, bad); err == nil {
		t.Fatal("unbalanced posting should error")
	}

	// Concurrent identical posts: exactly one creates, balance moves once.
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	creates := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := store.Post(ctx, mkPosting("h_race"))
			if err != nil {
				t.Errorf("concurrent post: %v", err)
				return
			}
			if created {
				mu.Lock()
				creates++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if creates != 1 {
		t.Fatalf("concurrent identical posts created %d times, want 1", creates)
	}
}
