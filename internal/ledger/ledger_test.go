package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func posting(hash string, entries ...Entry) Posting {
	return Posting{
		Tx: Transaction{
			ID: "txn_" + hash, Amount: 1000, Currency: "USDC",
			Status: StatusSettled, RequestHash: hash, CreatedAt: time.Unix(0, 0).UTC(),
		},
		Entries: entries,
	}
}

func debit(w string, amt int64) Entry {
	return Entry{WalletID: w, Direction: Debit, Amount: amt, Currency: "USDC"}
}
func credit(w string, amt int64) Entry {
	return Entry{WalletID: w, Direction: Credit, Amount: amt, Currency: "USDC"}
}

func TestValidate_RejectsUnbalanced(t *testing.T) {
	err := Validate(posting("h1", debit("a", 1000), credit("b", 999)))
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("want ErrUnbalanced, got %v", err)
	}
}

func TestValidate_RejectsNonPositiveAndBadDirection(t *testing.T) {
	if err := Validate(posting("h", debit("a", 0), credit("b", 0))); err == nil {
		t.Fatal("want error for zero-amount entry")
	}
	if err := Validate(posting("h", Entry{WalletID: "a", Direction: "sideways", Amount: 1, Currency: "USDC"})); err == nil {
		t.Fatal("want error for bad direction")
	}
}

func TestMemStore_PostAndDerivedBalance(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()

	_, created, err := s.Post(ctx, posting("h1", debit("wallet:a", 1000), credit("wallet:b", 1000)))
	if err != nil || !created {
		t.Fatalf("post: created=%v err=%v", created, err)
	}
	if b, _ := s.Balance(ctx, "wallet:a", "USDC"); b != -1000 {
		t.Fatalf("wallet:a balance = %d, want -1000", b)
	}
	if b, _ := s.Balance(ctx, "wallet:b", "USDC"); b != 1000 {
		t.Fatalf("wallet:b balance = %d, want 1000", b)
	}
}

func TestMemStore_IdempotentOnRequestHash(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()

	first, created, _ := s.Post(ctx, posting("dup", debit("wallet:a", 1000), credit("wallet:b", 1000)))
	if !created {
		t.Fatal("first post should be created")
	}
	second, created, _ := s.Post(ctx, posting("dup", debit("wallet:a", 1000), credit("wallet:b", 1000)))
	if created {
		t.Fatal("second post with same hash must NOT create")
	}
	if second.ID != first.ID {
		t.Fatalf("idempotent post returned different tx: %s vs %s", second.ID, first.ID)
	}
	// The replay must not move the balance.
	if b, _ := s.Balance(ctx, "wallet:b", "USDC"); b != 1000 {
		t.Fatalf("balance after replay = %d, want 1000 (no double charge)", b)
	}
}
