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
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Store) CreateUser(u *User) error {
	_, err := s.db.Exec(`
		INSERT INTO users (id, username, display_name, password, is_admin, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			username = COALESCE(NULLIF(excluded.username, ''), users.username),
			display_name = COALESCE(NULLIF(excluded.display_name, ''), users.display_name)`,
		u.ID, u.Username, u.DisplayName, u.Password, boolToInt(u.IsAdmin))
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (s *Store) GetUser(id string) (*User, error) {
	u := &User{}
	var displayName, username sql.NullString
	var isAdmin int
	err := s.db.QueryRow(`SELECT id, username, display_name, password, is_admin, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &username, &displayName, &u.Password, &isAdmin, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	u.Username = username.String
	u.DisplayName = displayName.String
	u.IsAdmin = isAdmin != 0
	return u, nil
}

func (s *Store) GetUserByUsername(username string) (*User, error) {
	u := &User{}
	var displayName, uname sql.NullString
	var isAdmin int
	err := s.db.QueryRow(`SELECT id, username, display_name, password, is_admin, created_at FROM users WHERE username = ?`, username).
		Scan(&u.ID, &uname, &displayName, &u.Password, &isAdmin, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	u.Username = uname.String
	u.DisplayName = displayName.String
	u.IsAdmin = isAdmin != 0
	return u, nil
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, username, display_name, password, is_admin, created_at FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		var displayName, username sql.NullString
		var isAdmin int
		if err := rows.Scan(&u.ID, &username, &displayName, &u.Password, &isAdmin, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.Username = username.String
		u.DisplayName = displayName.String
		u.IsAdmin = isAdmin != 0
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) UpdateUserPassword(id, password string) error {
	_, err := s.db.Exec(`UPDATE users SET password = ? WHERE id = ?`, password, id)
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

