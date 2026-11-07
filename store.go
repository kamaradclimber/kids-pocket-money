package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const txKindAllowance = "allowance"
const txKindManual = "manual"

var errKidNotFound = errors.New("kid not found")

// store wraps a SQLite database. Money amounts are integer cents.
type store struct {
	db *sql.DB
}

type kid struct {
	ID            int64
	Username      string // matches Remote-User
	Name          string
	MonthlyAmount int64  // cents, >= 0
	StartMonth    string // "YYYY-MM", first month the allowance is credited
}

type transaction struct {
	ID        int64
	KidID     int64
	Amount    int64 // cents, signed
	Label     string
	Kind      string // allowance | manual
	Month     string
	CreatedBy string
	CreatedAt time.Time
}

type kidOverview struct {
	kid
	Balance int64
}

func newStore(path string) (*store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	st := &store{db: db}
	if err := st.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

func (s *store) Close() error { return s.db.Close() }

func (s *store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS kids (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  monthly_amount INTEGER NOT NULL DEFAULT 0,
  start_month TEXT NOT NULL,
  created_at TEXT NOT NULL,
  created_by TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS transactions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kid_id INTEGER NOT NULL REFERENCES kids(id) ON DELETE CASCADE,
  amount INTEGER NOT NULL,
  label TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('allowance','manual')),
  month TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_transactions_allowance
  ON transactions(kid_id, month) WHERE kind = 'allowance';

CREATE INDEX IF NOT EXISTS idx_transactions_kid ON transactions(kid_id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func (s *store) createKid(username, name string, monthlyAmount int64, startMonth, createdBy string, now time.Time) (kid, error) {
	res, err := s.db.Exec(
		`INSERT INTO kids (username, name, monthly_amount, start_month, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		username, name, monthlyAmount, startMonth, now.Format(time.RFC3339), createdBy)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return kid{}, fmt.Errorf("Un compte existe déjà pour %s.", username)
		}
		return kid{}, fmt.Errorf("insert kid: %w", err)
	}
	id, _ := res.LastInsertId()
	return kid{ID: id, Username: username, Name: name, MonthlyAmount: monthlyAmount, StartMonth: startMonth}, nil
}

func scanKid(row interface{ Scan(...any) error }) (kid, error) {
	var k kid
	err := row.Scan(&k.ID, &k.Username, &k.Name, &k.MonthlyAmount, &k.StartMonth)
	return k, err
}

const kidColumns = `id, username, name, monthly_amount, start_month`

func (s *store) listKids() ([]kid, error) {
	rows, err := s.db.Query(`SELECT ` + kidColumns + ` FROM kids ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list kids: %w", err)
	}
	defer rows.Close()
	var kids []kid
	for rows.Next() {
		k, err := scanKid(rows)
		if err != nil {
			return nil, fmt.Errorf("list kids: %w", err)
		}
		kids = append(kids, k)
	}
	return kids, rows.Err()
}

func (s *store) getKid(id int64) (kid, error) {
	row := s.db.QueryRow(`SELECT `+kidColumns+` FROM kids WHERE id = ?`, id)
	k, err := scanKid(row)
	if errors.Is(err, sql.ErrNoRows) {
		return kid{}, errKidNotFound
	}
	if err != nil {
		return kid{}, fmt.Errorf("get kid: %w", err)
	}
	return k, nil
}

func (s *store) kidByUsername(username string) (kid, bool, error) {
	row := s.db.QueryRow(`SELECT `+kidColumns+` FROM kids WHERE username = ?`, username)
	k, err := scanKid(row)
	if errors.Is(err, sql.ErrNoRows) {
		return kid{}, false, nil
	}
	if err != nil {
		return kid{}, false, fmt.Errorf("kid by username: %w", err)
	}
	return k, true, nil
}

func (s *store) updateMonthlyAmount(id int64, amount int64) error {
	_, err := s.db.Exec(`UPDATE kids SET monthly_amount = ? WHERE id = ?`, amount, id)
	if err != nil {
		return fmt.Errorf("update monthly amount: %w", err)
	}
	return nil
}

// addTransaction inserts a manual transaction. Amount is signed cents.
func (s *store) addTransaction(kidID, amount int64, label, createdBy string, now time.Time) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO transactions (kid_id, amount, label, kind, month, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		kidID, amount, label, txKindManual, now.Format("2006-01"), createdBy, now.Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("insert transaction: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (s *store) kidIDOfTransaction(id int64) (int64, error) {
	row := s.db.QueryRow(`SELECT kid_id FROM transactions WHERE id = ? AND kind = ?`, id, txKindManual)
	var kidID int64
	if err := row.Scan(&kidID); err != nil {
		return 0, err
	}
	return kidID, nil
}

func (s *store) deleteTransaction(id int64) error {
	// Allowance rows are immutable: a deleted row would be re-created by
	// the next ensureMonthlyAllowances pass. Parents adjust with a manual
	// transaction instead.
	res, err := s.db.Exec(`DELETE FROM transactions WHERE id = ? AND kind = ?`, id, txKindManual)
	if err != nil {
		return fmt.Errorf("delete transaction: %w", err)
	}
	deleted, _ := res.RowsAffected()
	if deleted == 0 {
		return fmt.Errorf("transaction not found")
	}
	return nil
}

func (s *store) listTransactions(kidID int64) ([]transaction, error) {
	rows, err := s.db.Query(
		`SELECT id, kid_id, amount, label, kind, month, created_by, created_at
		 FROM transactions WHERE kid_id = ? ORDER BY created_at DESC, id DESC`, kidID)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	defer rows.Close()
	var txs []transaction
	for rows.Next() {
		var tx transaction
		var createdAt string
		err := rows.Scan(&tx.ID, &tx.KidID, &tx.Amount, &tx.Label, &tx.Kind, &tx.Month, &tx.CreatedBy, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("list transactions: %w", err)
		}
		tx.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		txs = append(txs, tx)
	}
	return txs, rows.Err()
}

func (s *store) balance(kidID int64) (int64, error) {
	row := s.db.QueryRow(`SELECT COALESCE(SUM(amount), 0) FROM transactions WHERE kid_id = ?`, kidID)
	var balance int64
	if err := row.Scan(&balance); err != nil {
		return 0, fmt.Errorf("balance: %w", err)
	}
	return balance, nil
}

func (s *store) overview(k kid) (kidOverview, error) {
	balance, err := s.balance(k.ID)
	if err != nil {
		return kidOverview{}, err
	}
	return kidOverview{kid: k, Balance: balance}, nil
}

func (s *store) overviews() ([]kidOverview, error) {
	kids, err := s.listKids()
	if err != nil {
		return nil, err
	}
	// Newest kid first is nicer for the parent dashboard, but listKids
	// orders by name; keep that order for overviews too.
	result := make([]kidOverview, 0, len(kids))
	for _, k := range kids {
		o, err := s.overview(k)
		if err != nil {
			return nil, err
		}
		result = append(result, o)
	}
	return result, nil
}

// ensureMonthlyAllowances materializes missing allowance transactions from
// each kid's start month up to now. It relies on the partial unique index
// (kid_id, month) WHERE kind = 'allowance' and is therefore idempotent and
// safe to call concurrently.
func (s *store) ensureMonthlyAllowances(now time.Time) (int, error) {
	kids, err := s.listKids()
	if err != nil {
		return 0, err
	}
	currentMonth := now.Format("2006-01")
	inserted := 0
	for _, k := range kids {
		if k.MonthlyAmount == 0 || k.StartMonth > currentMonth {
			continue
		}
		for month := k.StartMonth; month <= currentMonth; month = nextMonth(month) {
			res, err := s.db.Exec(
				`INSERT OR IGNORE INTO transactions (kid_id, amount, label, kind, month, created_by, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				k.ID, k.MonthlyAmount, "Argent de poche", txKindAllowance, month, "system", now.Format(time.RFC3339))
			if err != nil {
				return 0, fmt.Errorf("insert allowance for kid %d month %s: %w", k.ID, month, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				inserted++
				slog.Info("allowance credited", "kid", k.Name, "month", month, "amount", k.MonthlyAmount)
			}
		}
	}
	return inserted, nil
}

func nextMonth(month string) string {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		// Should not happen: validated at creation time.
		return month
	}
	return t.AddDate(0, 1, 0).Format("2006-01")
}
