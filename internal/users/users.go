// Package users manages flexctl user records and host UID assignment.
//
// Each user gets a unique host UID starting at 10001, used for filesystem
// ownership inside the work container's mounted home (Eng review #11:
// avoid UID collisions across users on shared host volumes).
package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// firstHostUID-1 is the seed for COALESCE(MAX, ...). Real users get
// firstHostUID and up.
const firstHostUID = 10001

// Common errors.
var (
	ErrNotFound   = errors.New("user not found")
	ErrEmailTaken = errors.New("email already in use")
)

// User mirrors a row in users.
type User struct {
	ID        string
	Email     string
	HostUID   int
	CreatedAt time.Time
}

// Store is the user CRUD layer over SQLite.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// New returns a Store backed by db. now is used for created_at; pass
// time.Now in production and a fake clock in tests.
func New(db *sql.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

// Create inserts a user with auto-assigned host_uid. Concurrent calls
// are safe: IMMEDIATE-locked transactions (db.Open) serialize writers,
// and the UNIQUE constraint on host_uid + email are the final guards.
func (s *Store) Create(ctx context.Context, email string) (*User, error) {
	if email == "" {
		return nil, fmt.Errorf("email required")
	}
	id := uuid.NewString()
	nowUnix := s.now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, email, host_uid, created_at)
		VALUES (
			?, ?,
			COALESCE((SELECT MAX(host_uid) FROM users), ?) + 1,
			?
		)
	`, id, email, firstHostUID-1, nowUnix)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("create user: %w", err)
	}
	return s.GetByID(ctx, id)
}

// GetByID returns the user with the given id, or ErrNotFound.
func (s *Store) GetByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `
		SELECT id, email, host_uid, created_at FROM users WHERE id = ?
	`, id))
}

// GetByEmail returns the user with the given email, or ErrNotFound.
func (s *Store) GetByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `
		SELECT id, email, host_uid, created_at FROM users WHERE email = ?
	`, email))
}

// List returns all users ordered by host_uid (creation order).
func (s *Store) List(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, email, host_uid, created_at FROM users ORDER BY host_uid
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// Delete removes a user. Caller is responsible for ensuring the user
// has no active environments (the environments table FK will block
// otherwise).
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(rs rowScanner) (*User, error) {
	var u User
	var createdAt int64
	err := rs.Scan(&u.ID, &u.Email, &u.HostUID, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = time.Unix(createdAt, 0)
	return &u, nil
}

// modernc.org/sqlite returns "constraint failed: UNIQUE constraint failed: ..."
// for unique violations. Matching the substring is fragile in theory but
// stable in practice for this driver.
func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
