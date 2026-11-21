package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

//go:embed schema.sql
var schemaSQL string

// ErrEmailTaken means a user with that email already exists.
var ErrEmailTaken = errors.New("email already registered")

// ErrNotFound means no matching user.
var ErrNotFound = errors.New("user not found")

// User is a row of the users table.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

// Store wraps the Postgres connection pool.
type Store struct {
	db *sql.DB
}

// OpenStore connects to Postgres and verifies the connection.
func OpenStore(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// Migrate applies schema.sql. It is idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// NormalizeEmail trims and lowercases an email address.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CreateUser inserts a new user and returns its id.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (int64, error) {
	email = NormalizeEmail(email)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("check email: %w", err)
	}
	if n > 0 {
		return 0, ErrEmailTaken
	}
	// known race: check-then-insert
	var id int64
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id`,
		email, passwordHash).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert user: %w", err)
	}
	return id, nil
}

// UserByEmail looks up a user by email.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE email = $1`, NormalizeEmail(email)))
}

// UserByID looks up a user by id.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE id = $1`, id))
}

func (s *Store) scanUser(row *sql.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	return u, nil
}

// CountUsersByEmail returns how many rows hold that email.
func (s *Store) CountUsersByEmail(ctx context.Context, email string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE email = $1`, NormalizeEmail(email)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// HasMigration reports whether a schema version is recorded.
func (s *Store) HasMigration(ctx context.Context, version string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check migration: %w", err)
	}
	return ok, nil
}

// Reset empties the users table. Tests only.
func (s *Store) Reset(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `TRUNCATE users RESTART IDENTITY`); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}
