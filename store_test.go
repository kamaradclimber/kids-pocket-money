package main

import (
	"database/sql"
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

	if _, err := st.addTransaction(k.ID, -550, "achat d'un jeu", "kid1", now, now); err != nil {
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

	manualID, err := st.addTransaction(k.ID, -300, "bonbon", "kid1", now, now)
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

func TestAddTransactionWithDate(t *testing.T) {
	st := testStore(t)
	now := time.Date(2025, time.November, 15, 10, 0, 0, 0, time.UTC)
	k := mustCreateKid(t, st, "kid1", "Alice", 0, "2025-11")

	// A backdated operation: recorded on Nov 15 but dated Oct 3.
	opDate := time.Date(2025, time.October, 3, 0, 0, 0, 0, time.UTC)
	if _, err := st.addTransaction(k.ID, -450, "bonbons", "kid1", opDate, now); err != nil {
		t.Fatalf("addTransaction: %v", err)
	}
	txs, err := st.listTransactions(k.ID)
	if err != nil {
		t.Fatalf("listTransactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("expected 1 transaction, got %d", len(txs))
	}
	if txs[0].Date != "2025-10-03" {
		t.Errorf("expected date 2025-10-03, got %q", txs[0].Date)
	}
	if txs[0].Month != "2025-10" {
		t.Errorf("expected month derived from date (2025-10), got %q", txs[0].Month)
	}
}

func TestTransactionsOrderedByDate(t *testing.T) {
	st := testStore(t)
	now := time.Date(2025, time.November, 15, 10, 0, 0, 0, time.UTC)
	k := mustCreateKid(t, st, "kid1", "Alice", 0, "2025-11")

	older := time.Date(2025, time.September, 20, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2025, time.October, 5, 0, 0, 0, 0, time.UTC)
	if _, err := st.addTransaction(k.ID, -100, "récent", "kid1", newer, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.addTransaction(k.ID, -200, "ancien", "kid1", older, now); err != nil {
		t.Fatal(err)
	}
	txs, _ := st.listTransactions(k.ID)
	if len(txs) != 2 || txs[0].Label != "récent" || txs[1].Label != "ancien" {
		t.Errorf("expected date DESC ordering, got %v then %v", txs[0].Label, txs[1].Label)
	}
}

func TestMigrationAddsDateToExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	// A pre-date-column database, as deployed by version 0.1.0.
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	const oldSchema = `
CREATE TABLE kids (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  monthly_amount INTEGER NOT NULL DEFAULT 0,
  start_month TEXT NOT NULL,
  created_at TEXT NOT NULL,
  created_by TEXT NOT NULL
);
CREATE TABLE transactions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kid_id INTEGER NOT NULL REFERENCES kids(id) ON DELETE CASCADE,
  amount INTEGER NOT NULL,
  label TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('allowance','manual')),
  month TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);`
	if _, err := old.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := old.Exec(`INSERT INTO kids (username, name, monthly_amount, start_month, created_at, created_by)
		VALUES ('kid1', 'Alice', 1000, '2025-09', '2025-09-01T10:00:00Z', 'parent')`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO transactions (kid_id, amount, label, kind, month, created_by, created_at)
		VALUES (1, 1000, 'Argent de poche', 'allowance', '2025-09', 'system', '2025-09-01T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO transactions (kid_id, amount, label, kind, month, created_by, created_at)
		VALUES (1, -250, 'livre', 'manual', '2025-09', 'kid1', '2025-09-12T18:30:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening it with the current store migrates it in place.
	st, err := newStore(path)
	if err != nil {
		t.Fatalf("migrate old db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	txs, err := st.listTransactions(1)
	if err != nil {
		t.Fatalf("listTransactions: %v", err)
	}
	if len(txs) != 2 {
		t.Fatalf("expected 2 transactions, got %d", len(txs))
	}
	for _, tx := range txs {
		if tx.Date == "" {
			t.Errorf("transaction %d (%s) was not backfilled with a date", tx.ID, tx.Label)
		}
	}
	// Allowance rows are dated to the first day of their month.
	if txs[1].Kind == txKindAllowance && txs[1].Date != "2025-09-01" {
		t.Errorf("allowance backfill: expected 2025-09-01, got %q", txs[1].Date)
	}
	// Manual rows are dated to their creation day.
	if txs[0].Kind == txKindManual && txs[0].Date != "2025-09-12" {
		t.Errorf("manual backfill: expected 2025-09-12, got %q", txs[0].Date)
	}

	// New inserts carry the date column.
	if _, err := st.addTransaction(1, -100, "date moderne", "kid1",
		time.Date(2025, time.November, 1, 0, 0, 0, 0, time.UTC), time.Now()); err != nil {
		t.Fatal(err)
	}
	txs, _ = st.listTransactions(1)
	if txs[0].Date != "2025-11-01" {
		t.Errorf("expected 2025-11-01, got %q", txs[0].Date)
	}
}
