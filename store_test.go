package main

import (
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *store {
	t.Helper()
	st, err := newStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mustCreateKid(t *testing.T, st *store, username, name string, monthly int64, startMonth string) kid {
	t.Helper()
	k, err := st.createKid(username, name, monthly, startMonth, "parent", time.Now())
	if err != nil {
		t.Fatalf("createKid %s: %v", username, err)
	}
	return k
}

func TestEnsureMonthlyAllowances(t *testing.T) {
	st := testStore(t)
	now := time.Date(2025, time.November, 15, 10, 0, 0, 0, time.UTC)
	k := mustCreateKid(t, st, "kid1", "Alice", 1000, "2025-09")

	if _, err := st.ensureMonthlyAllowances(now); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	balance, err := st.balance(k.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != 3000 { // Sep + Oct + Nov
		t.Errorf("expected 3000 cents after 3 months, got %d", balance)
	}

	// Idempotent: a second pass must not double-credit.
	if _, err := st.ensureMonthlyAllowances(now); err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	balance, _ = st.balance(k.ID)
	if balance != 3000 {
		t.Errorf("expected idempotent balance of 3000, got %d", balance)
	}
}

func TestEnsureMonthlyAllowancesFutureStartAndZero(t *testing.T) {
	st := testStore(t)
	now := time.Date(2025, time.November, 15, 10, 0, 0, 0, time.UTC)

	// Start month in the future: nothing credited yet.
	k := mustCreateKid(t, st, "kid1", "Alice", 1000, "2025-12")
	if _, err := st.ensureMonthlyAllowances(now); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	balance, _ := st.balance(k.ID)
	if balance != 0 {
		t.Errorf("expected 0 for future start month, got %d", balance)
	}

	// Zero monthly amount: no allowance rows.
	k2 := mustCreateKid(t, st, "kid2", "Bob", 0, "2025-09")
	if _, err := st.ensureMonthlyAllowances(now); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	balance2, _ := st.balance(k2.ID)
	if balance2 != 0 {
		t.Errorf("expected 0 for zero monthly amount, got %d", balance2)
	}
}

func TestManualTransactionsAndBalance(t *testing.T) {
	st := testStore(t)
	now := time.Date(2025, time.November, 15, 10, 0, 0, 0, time.UTC)
	k := mustCreateKid(t, st, "kid1", "Alice", 1000, "2025-11")
	if _, err := st.ensureMonthlyAllowances(now); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	if _, err := st.addTransaction(k.ID, -550, "achat d'un jeu", "kid1", now); err != nil {
		t.Fatalf("addTransaction: %v", err)
	}
	balance, _ := st.balance(k.ID)
	if balance != 450 {
		t.Errorf("expected 450, got %d", balance)
	}

	txs, err := st.listTransactions(k.ID)
	if err != nil {
		t.Fatalf("listTransactions: %v", err)
	}
	if len(txs) != 2 {
		t.Fatalf("expected 2 transactions, got %d", len(txs))
	}
	if txs[0].Kind != txKindManual || txs[1].Kind != txKindAllowance {
		t.Errorf("expected manual first (newest first), got %s then %s", txs[0].Kind, txs[1].Kind)
	}
}

func TestDeleteTransaction(t *testing.T) {
	st := testStore(t)
	now := time.Date(2025, time.November, 15, 10, 0, 0, 0, time.UTC)
	k := mustCreateKid(t, st, "kid1", "Alice", 1000, "2025-11")
	_, _ = st.ensureMonthlyAllowances(now)

	manualID, err := st.addTransaction(k.ID, -300, "bonbon", "kid1", now)
	if err != nil {
		t.Fatalf("addTransaction: %v", err)
	}
	txs, _ := st.listTransactions(k.ID)
	allowanceID := int64(0)
	for _, tx := range txs {
		if tx.Kind == txKindAllowance {
			allowanceID = tx.ID
		}
	}
	if allowanceID == 0 {
		t.Fatal("no allowance transaction found")
	}

	// Manual transactions are deletable.
	if err := st.deleteTransaction(manualID); err != nil {
		t.Fatalf("delete manual: %v", err)
	}
	balance, _ := st.balance(k.ID)
	if balance != 1000 {
		t.Errorf("expected 1000 after deleting manual tx, got %d", balance)
	}

	// Allowance transactions are not deletable.
	if err := st.deleteTransaction(allowanceID); err == nil {
		t.Error("expected error when deleting an allowance transaction")
	}
	// Deleting twice fails: the row is already gone.
	if err := st.deleteTransaction(manualID); err == nil {
		t.Error("expected error when deleting twice")
	}
}

func TestKidByUsername(t *testing.T) {
	st := testStore(t)
	mustCreateKid(t, st, "kid1", "Alice", 1000, "2025-11")

	if _, found, err := st.kidByUsername("kid1"); err != nil || !found {
		t.Errorf("expected kid1 to be found, got found=%v err=%v", found, err)
	}
	if _, found, err := st.kidByUsername("unknown"); err != nil || found {
		t.Errorf("expected unknown to not be found, got found=%v err=%v", found, err)
	}
}

func TestNextMonth(t *testing.T) {
	cases := map[string]string{
		"2025-01": "2025-02",
		"2025-12": "2026-01",
		"2024-02": "2024-03",
	}
	for in, want := range cases {
		if got := nextMonth(in); got != want {
			t.Errorf("nextMonth(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestOverviews(t *testing.T) {
	st := testStore(t)
	now := time.Date(2025, time.November, 15, 10, 0, 0, 0, time.UTC)
	mustCreateKid(t, st, "kid1", "Alice", 1000, "2025-11")
	mustCreateKid(t, st, "kid2", "Zoé", 500, "2025-10")
	_, _ = st.ensureMonthlyAllowances(now)

	overviews, err := st.overviews()
	if err != nil {
		t.Fatalf("overviews: %v", err)
	}
	if len(overviews) != 2 {
		t.Fatalf("expected 2 kids, got %d", len(overviews))
	}
	byName := map[string]int64{}
	for _, o := range overviews {
		byName[o.Name] = o.Balance
	}
	if byName["Alice"] != 1000 || byName["Zoé"] != 1000 { // Zoé: Oct + Nov
		t.Errorf("unexpected balances: %v", byName)
	}
}
