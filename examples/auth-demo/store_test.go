package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

var (
	dbOnce  sync.Once
	dbStore *Store
	dbErr   error
)

// testStore returns a migrated, empty store, or skips when Postgres is unreachable.
func testStore(t *testing.T) *Store {
	t.Helper()
	dbOnce.Do(func() {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			dsn = defaultDSN
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		dbStore, dbErr = OpenStore(ctx, dsn)
		if dbErr == nil {
			dbErr = dbStore.Migrate(ctx)
		}
	})
	if dbErr != nil {
		t.Skipf("skipping: Postgres unreachable at DATABASE_URL (%v); run inside the branch VM", dbErr)
	}
	if err := dbStore.Reset(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	return dbStore
}

func TestCreateUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cases := []struct{ email string }{{"a@example.com"}, {"b@example.com"}, {"c@example.com"}}
	for i, tc := range cases {
		id, err := s.CreateUser(ctx, tc.email, "h")
		if err != nil {
			t.Fatalf("%s: %v", tc.email, err)
		}
		if id != int64(i+1) {
			t.Fatalf("%s: id %d, want %d", tc.email, id, i+1)
		}
	}
}

func TestCreateUserDuplicate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "dup@example.com", "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "dup@example.com", "h2"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err %v, want ErrEmailTaken", err)
	}
}

func TestEmailCaseNormalized(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "  Mixed@Example.COM ", "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "mixed@example.com", "h"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err %v, want ErrEmailTaken", err)
	}
}

func TestUserByEmail(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateUser(ctx, "find@example.com", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.UserByEmail(ctx, "FIND@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != id || u.Email != "find@example.com" || u.PasswordHash != "hash1" || u.CreatedAt.IsZero() {
		t.Fatalf("got %+v", u)
	}
}

func TestUserByID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateUser(ctx, "byid@example.com", "h")
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.UserByID(ctx, id)
	if err != nil || u.Email != "byid@example.com" {
		t.Fatalf("got %+v, %v", u, err)
	}
}

func TestUserNotFound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		get  func() error
	}{
		{"by email", func() error { _, err := s.UserByEmail(ctx, "none@example.com"); return err }},
		{"by id", func() error { _, err := s.UserByID(ctx, 999); return err }},
	}
	for _, tc := range cases {
		if err := tc.get(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: err %v, want ErrNotFound", tc.name, err)
		}
	}
}

func TestSchemaMigrationsRecorded(t *testing.T) {
	s := testStore(t)
	ok, err := s.HasMigration(context.Background(), "0001_init")
	if err != nil || !ok {
		t.Fatalf("migration recorded=%v err=%v", ok, err)
	}
}

// TestSignupConcurrentDuplicate races many signups for one email. Exactly one
// must succeed, every other caller must get ErrEmailTaken, and one row must exist.
func TestSignupConcurrentDuplicate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const workers = 20
	for round := 0; round < 5; round++ {
		email := "race" + string(rune('a'+round)) + "@example.com"
		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			mu      sync.Mutex
			success int
			other   []error
		)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := s.CreateUser(ctx, email, "h")
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					success++
				case !errors.Is(err, ErrEmailTaken):
					other = append(other, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		n, err := s.CountUsersByEmail(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		if success != 1 || n != 1 || len(other) > 0 {
			t.Fatalf("round %d: successes=%d rows=%d unexpected errors=%d (first: %v)",
				round, success, n, len(other), firstErr(other))
		}
	}
}

func firstErr(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}
