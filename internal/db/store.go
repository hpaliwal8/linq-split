package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open creates or opens the SQLite database and runs migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS groups (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id     TEXT UNIQUE NOT NULL,  -- Linq chat_id
			name        TEXT DEFAULT '',
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS members (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id    INTEGER NOT NULL REFERENCES groups(id),
			handle      TEXT NOT NULL,         -- phone number (E.164)
			name        TEXT DEFAULT '',
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(group_id, handle)
		);

		CREATE TABLE IF NOT EXISTS expenses (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id    INTEGER NOT NULL REFERENCES groups(id),
			payer_id    INTEGER NOT NULL REFERENCES members(id),
			amount      REAL NOT NULL,
			description TEXT DEFAULT '',
			category    TEXT DEFAULT '',
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
			voided_at   DATETIME
		);

		-- Each expense has splits: who owes what portion.
		CREATE TABLE IF NOT EXISTS expense_splits (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			expense_id  INTEGER NOT NULL REFERENCES expenses(id),
			member_id   INTEGER NOT NULL REFERENCES members(id),
			amount      REAL NOT NULL          -- how much this member owes for this expense
		);

		CREATE TABLE IF NOT EXISTS settlements (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id    INTEGER NOT NULL REFERENCES groups(id),
			from_id     INTEGER NOT NULL REFERENCES members(id),
			to_id       INTEGER NOT NULL REFERENCES members(id),
			amount      REAL NOT NULL,
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		return err
	}
	// Add voided_at to existing databases (no-op if column already exists)
	s.db.Exec(`ALTER TABLE expenses ADD COLUMN voided_at DATETIME`)
	return nil
}

// ── Group + Member operations ────────────────────────────────────────

// EnsureGroup creates a group if it doesn't exist, returns its ID.
func (s *Store) EnsureGroup(chatID string) (int64, error) {
	_, err := s.db.Exec(
		`INSERT INTO groups (chat_id) VALUES (?) ON CONFLICT(chat_id) DO NOTHING`, chatID,
	)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRow(`SELECT id FROM groups WHERE chat_id = ?`, chatID).Scan(&id)
	return id, err
}

// EnsureMember creates a member if they don't exist, returns their ID.
func (s *Store) EnsureMember(groupID int64, handle, name string) (int64, error) {
	_, err := s.db.Exec(
		`INSERT INTO members (group_id, handle, name) VALUES (?, ?, ?)
		 ON CONFLICT(group_id, handle) DO UPDATE SET name = excluded.name`,
		groupID, handle, name,
	)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRow(
		`SELECT id FROM members WHERE group_id = ? AND handle = ?`, groupID, handle,
	).Scan(&id)
	return id, err
}

// GetMemberByHandle looks up a member by their phone handle.
func (s *Store) GetMemberByHandle(groupID int64, handle string) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM members WHERE group_id = ? AND handle = ?`, groupID, handle,
	).Scan(&id)
	return id, err
}

// UpdateMemberName sets the display name for a member.
func (s *Store) UpdateMemberName(memberID int64, name string) error {
	_, err := s.db.Exec(`UPDATE members SET name = ? WHERE id = ?`, name, memberID)
	return err
}

// GetGroupMembers returns all member handles for a group.
func (s *Store) GetGroupMembers(groupID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT handle FROM members WHERE group_id = ?`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var handles []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		handles = append(handles, h)
	}
	return handles, nil
}

// ── Expense operations ───────────────────────────────────────────────

// AddExpense records an expense and its splits. Returns the expense ID.
func (s *Store) AddExpense(groupID, payerID int64, amount float64, desc, category string, splits map[int64]float64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO expenses (group_id, payer_id, amount, description, category) VALUES (?, ?, ?, ?, ?)`,
		groupID, payerID, amount, desc, category,
	)
	if err != nil {
		return 0, err
	}

	expenseID, _ := res.LastInsertId()

	for memberID, splitAmt := range splits {
		_, err := tx.Exec(
			`INSERT INTO expense_splits (expense_id, member_id, amount) VALUES (?, ?, ?)`,
			expenseID, memberID, splitAmt,
		)
		if err != nil {
			return 0, err
		}
	}

	return expenseID, tx.Commit()
}

// AddSettlement records a payment from one person to another.
func (s *Store) AddSettlement(groupID, fromID, toID int64, amount float64) error {
	_, err := s.db.Exec(
		`INSERT INTO settlements (group_id, from_id, to_id, amount) VALUES (?, ?, ?, ?)`,
		groupID, fromID, toID, amount,
	)
	return err
}

// ── Balance calculations ─────────────────────────────────────────────

// Balance represents what one member owes another.
type Balance struct {
	FromHandle string
	FromName   string
	ToHandle   string
	ToName     string
	Amount     float64
}

// GetBalances calculates net balances for a group.
// Positive = they're owed money. Negative = they owe money.
func (s *Store) GetNetBalances(groupID int64) (map[int64]float64, error) {
	balances := make(map[int64]float64)

	// Money paid out (they're owed this much)
	rows, err := s.db.Query(`
		SELECT payer_id, SUM(amount) FROM expenses
		WHERE group_id = ? AND voided_at IS NULL GROUP BY payer_id
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var memberID int64
		var total float64
		if err := rows.Scan(&memberID, &total); err != nil {
			return nil, err
		}
		balances[memberID] += total
	}

	// Money they owe (subtract their splits)
	rows2, err := s.db.Query(`
		SELECT es.member_id, SUM(es.amount)
		FROM expense_splits es
		JOIN expenses e ON es.expense_id = e.id
		WHERE e.group_id = ? AND e.voided_at IS NULL
		GROUP BY es.member_id
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()

	for rows2.Next() {
		var memberID int64
		var total float64
		if err := rows2.Scan(&memberID, &total); err != nil {
			return nil, err
		}
		balances[memberID] -= total
	}

	// Factor in settlements
	rows3, err := s.db.Query(`
		SELECT from_id, to_id, SUM(amount)
		FROM settlements WHERE group_id = ?
		GROUP BY from_id, to_id
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows3.Close()

	for rows3.Next() {
		var fromID, toID int64
		var total float64
		if err := rows3.Scan(&fromID, &toID, &total); err != nil {
			return nil, err
		}
		balances[fromID] += total // paid out → reduces what they owe (balance moves toward 0)
		balances[toID] -= total   // received → reduces what they're owed (balance moves toward 0)
	}

	return balances, nil
}

// MemberInfo holds display info for a member.
type MemberInfo struct {
	ID     int64
	Handle string
	Name   string
}

// GetMemberInfo returns member info by ID.
func (s *Store) GetMemberInfo(memberID int64) (*MemberInfo, error) {
	m := &MemberInfo{ID: memberID}
	err := s.db.QueryRow(
		`SELECT handle, name FROM members WHERE id = ?`, memberID,
	).Scan(&m.Handle, &m.Name)
	return m, err
}

// GetAllMembers returns all members in a group.
func (s *Store) GetAllMembers(groupID int64) ([]*MemberInfo, error) {
	rows, err := s.db.Query(
		`SELECT id, handle, name FROM members WHERE group_id = ?`, groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []*MemberInfo
	for rows.Next() {
		m := &MemberInfo{}
		if err := rows.Scan(&m.ID, &m.Handle, &m.Name); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, nil
}

// GetSpendingSince returns total spending by category since a given time.
func (s *Store) GetSpendingSince(groupID int64, since time.Time) (map[string]float64, float64, error) {
	rows, err := s.db.Query(`
		SELECT category, SUM(amount) FROM expenses
		WHERE group_id = ? AND created_at >= ? AND voided_at IS NULL
		GROUP BY category
	`, groupID, since)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	categories := make(map[string]float64)
	var total float64
	for rows.Next() {
		var cat string
		var amt float64
		if err := rows.Scan(&cat, &amt); err != nil {
			return nil, 0, err
		}
		categories[cat] = amt
		total += amt
	}
	return categories, total, nil
}

// ── Expense editing ──────────────────────────────────────────────────

// ExpenseRecord holds the full data for a single expense including its splits.
type ExpenseRecord struct {
	ID          int64
	PayerID     int64
	Amount      float64
	Description string
	Category    string
	Splits      map[int64]float64
}

// GetLastExpense returns the most recent non-voided expense for a group.
func (s *Store) GetLastExpense(groupID int64) (*ExpenseRecord, error) {
	row := s.db.QueryRow(`
		SELECT id, payer_id, amount, description, category
		FROM expenses
		WHERE group_id = ? AND voided_at IS NULL
		ORDER BY created_at DESC LIMIT 1
	`, groupID)

	e := &ExpenseRecord{}
	if err := row.Scan(&e.ID, &e.PayerID, &e.Amount, &e.Description, &e.Category); err != nil {
		return nil, err
	}
	splits, err := s.loadSplits(e.ID)
	if err != nil {
		return nil, err
	}
	e.Splits = splits
	return e, nil
}

// FindExpenseByRef finds the most recent non-voided expense matching a description or amount string.
func (s *Store) FindExpenseByRef(groupID int64, ref string) (*ExpenseRecord, error) {
	row := s.db.QueryRow(`
		SELECT id, payer_id, amount, description, category
		FROM expenses
		WHERE group_id = ? AND voided_at IS NULL
		  AND (description LIKE ? OR CAST(amount AS TEXT) = ?)
		ORDER BY created_at DESC LIMIT 1
	`, groupID, "%"+ref+"%", ref)

	e := &ExpenseRecord{}
	if err := row.Scan(&e.ID, &e.PayerID, &e.Amount, &e.Description, &e.Category); err != nil {
		return nil, err
	}
	splits, err := s.loadSplits(e.ID)
	if err != nil {
		return nil, err
	}
	e.Splits = splits
	return e, nil
}

func (s *Store) loadSplits(expenseID int64) (map[int64]float64, error) {
	rows, err := s.db.Query(
		`SELECT member_id, amount FROM expense_splits WHERE expense_id = ?`, expenseID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	splits := make(map[int64]float64)
	for rows.Next() {
		var memberID int64
		var amt float64
		if err := rows.Scan(&memberID, &amt); err != nil {
			return nil, err
		}
		splits[memberID] = amt
	}
	return splits, nil
}

// VoidExpense soft-deletes an expense by setting voided_at.
func (s *Store) VoidExpense(expenseID int64) error {
	_, err := s.db.Exec(
		`UPDATE expenses SET voided_at = CURRENT_TIMESTAMP WHERE id = ?`, expenseID,
	)
	return err
}

// EditExpense updates an expense's amount/description and replaces its splits in one transaction.
func (s *Store) EditExpense(expenseID int64, newAmount float64, newDescription string, newSplits map[int64]float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`UPDATE expenses SET amount = ?, description = ? WHERE id = ?`,
		newAmount, newDescription, expenseID,
	)
	if err != nil {
		return err
	}

	if _, err = tx.Exec(`DELETE FROM expense_splits WHERE expense_id = ?`, expenseID); err != nil {
		return err
	}

	for memberID, amt := range newSplits {
		if _, err = tx.Exec(
			`INSERT INTO expense_splits (expense_id, member_id, amount) VALUES (?, ?, ?)`,
			expenseID, memberID, amt,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}
