package store

import (
	"database/sql"
	"fmt"
	"time"
)

type User struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name,omitempty"`
	Password    string    `json:"-"`
	IsAdmin     bool      `json:"is_admin"`
	Status      string    `json:"status"`
	TelegramID  int64     `json:"telegram_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var displayName, username, status sql.NullString
	var isAdmin int
	var telegramID sql.NullInt64
	err := row.Scan(&u.ID, &username, &displayName, &u.Password, &isAdmin, &status, &telegramID, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Username = username.String
	u.DisplayName = displayName.String
	u.IsAdmin = isAdmin != 0
	u.Status = status.String
	if u.Status == "" {
		u.Status = "approved"
	}
	u.TelegramID = telegramID.Int64
	return u, nil
}

const userColumns = `id, username, display_name, password, is_admin, status, telegram_id, created_at`

func (s *Store) CreateUser(u *User) error {
	status := u.Status
	if status == "" {
		status = "approved"
	}
	_, err := s.db.Exec(`
		INSERT INTO users (id, username, display_name, password, is_admin, status, telegram_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			username = COALESCE(NULLIF(excluded.username, ''), users.username),
			display_name = COALESCE(NULLIF(excluded.display_name, ''), users.display_name),
			telegram_id = CASE WHEN excluded.telegram_id > 0 THEN excluded.telegram_id ELSE users.telegram_id END`,
		u.ID, u.Username, u.DisplayName, u.Password, boolToInt(u.IsAdmin), status, u.TelegramID)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (s *Store) GetUser(id string) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByUsername(username string) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE username = ?`, username))
	if err != nil {
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByTelegramID(telegramID int64) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE telegram_id = ? AND telegram_id > 0`, telegramID))
	if err != nil {
		return nil, fmt.Errorf("get user by telegram_id: %w", err)
	}
	return u, nil
}

// GetUserByTelegramIDCompat finds a user by telegram_id column first,
// then falls back to checking if id matches the stringified telegram ID
// (for backward compat with users created before the telegram_id column).
func (s *Store) GetUserByTelegramIDCompat(telegramID int64) (*User, error) {
	u, err := s.GetUserByTelegramID(telegramID)
	if u != nil || err != nil {
		return u, err
	}
	return s.GetUser(fmt.Sprintf("%d", telegramID))
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		if u != nil {
			users = append(users, *u)
		}
	}
	return users, rows.Err()
}

func (s *Store) UpdateUserPassword(id, password string) error {
	_, err := s.db.Exec(`UPDATE users SET password = ? WHERE id = ?`, password, id)
	return err
}

func (s *Store) UpdateUserStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE users SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) DeleteUser(id string) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

func (s *Store) UserCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}
