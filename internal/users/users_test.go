package users

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PaulOh5/flexctl/internal/db"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

func fixedNow() func() time.Time {
	t := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func TestCreateAssignsFirstHostUID(t *testing.T) {
	s := New(openTestDB(t), fixedNow())
	u, err := s.Create(context.Background(), "alice@test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.HostUID != firstHostUID {
		t.Errorf("host_uid=%d, want %d", u.HostUID, firstHostUID)
	}
	if u.Email != "alice@test" {
		t.Errorf("email=%s", u.Email)
	}
	if u.ID == "" {
		t.Errorf("empty id")
	}
}

func TestCreateIncrementsHostUID(t *testing.T) {
	s := New(openTestDB(t), fixedNow())
	ctx := context.Background()

	for i, email := range []string{"a@test", "b@test", "c@test"} {
		u, err := s.Create(ctx, email)
		if err != nil {
			t.Fatalf("create %s: %v", email, err)
		}
		want := firstHostUID + i
		if u.HostUID != want {
			t.Errorf("user %d host_uid=%d, want %d", i, u.HostUID, want)
		}
	}
}

func TestCreateDuplicateEmail(t *testing.T) {
	s := New(openTestDB(t), fixedNow())
	ctx := context.Background()

	if _, err := s.Create(ctx, "dup@test"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.Create(ctx, "dup@test")
	if err != ErrEmailTaken {
		t.Errorf("got %v, want ErrEmailTaken", err)
	}
}

func TestCreateEmptyEmail(t *testing.T) {
	s := New(openTestDB(t), fixedNow())
	if _, err := s.Create(context.Background(), ""); err == nil {
		t.Errorf("expected error for empty email")
	}
}

func TestGetByIDAndEmail(t *testing.T) {
	s := New(openTestDB(t), fixedNow())
	ctx := context.Background()

	created, err := s.Create(ctx, "find@test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	byID, err := s.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("get id: %v", err)
	}
	if byID.Email != "find@test" {
		t.Errorf("email=%s", byID.Email)
	}

	byEmail, err := s.GetByEmail(ctx, "find@test")
	if err != nil {
		t.Fatalf("get email: %v", err)
	}
	if byEmail.ID != created.ID {
		t.Errorf("id mismatch: %s vs %s", byEmail.ID, created.ID)
	}
}

func TestGetMissing(t *testing.T) {
	s := New(openTestDB(t), fixedNow())
	if _, err := s.GetByID(context.Background(), "no-such"); err != ErrNotFound {
		t.Errorf("by id: got %v want ErrNotFound", err)
	}
	if _, err := s.GetByEmail(context.Background(), "no@test"); err != ErrNotFound {
		t.Errorf("by email: got %v want ErrNotFound", err)
	}
}

func TestListSortedByHostUID(t *testing.T) {
	s := New(openTestDB(t), fixedNow())
	ctx := context.Background()

	for _, e := range []string{"c@test", "a@test", "b@test"} {
		if _, err := s.Create(ctx, e); err != nil {
			t.Fatalf("create %s: %v", e, err)
		}
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	want := []string{"c@test", "a@test", "b@test"} // creation order = host_uid order
	for i, u := range got {
		if u.Email != want[i] {
			t.Errorf("pos %d: %s want %s", i, u.Email, want[i])
		}
	}
}

func TestDelete(t *testing.T) {
	s := New(openTestDB(t), fixedNow())
	ctx := context.Background()

	u, _ := s.Create(ctx, "del@test")
	if err := s.Delete(ctx, u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, u.ID); err != ErrNotFound {
		t.Errorf("second delete: got %v want ErrNotFound", err)
	}
}

// Concurrent Creates should all succeed with distinct host_uids.
func TestCreateConcurrent(t *testing.T) {
	const n = 20
	s := New(openTestDB(t), fixedNow())

	var ok atomic.Int64
	uids := make([]int, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			u, err := s.Create(context.Background(), fakeEmail(i))
			if err != nil {
				t.Errorf("create %d: %v", i, err)
				return
			}
			uids[i] = u.HostUID
			ok.Add(1)
		}()
	}
	wg.Wait()

	if ok.Load() != n {
		t.Fatalf("ok=%d want %d", ok.Load(), n)
	}
	// All host_uids distinct, all in [firstHostUID, firstHostUID+n).
	seen := make(map[int]bool, n)
	for _, u := range uids {
		if u < firstHostUID || u >= firstHostUID+n {
			t.Errorf("host_uid out of range: %d", u)
		}
		if seen[u] {
			t.Errorf("duplicate host_uid: %d", u)
		}
		seen[u] = true
	}
}

func fakeEmail(i int) string { return "u" + itoa3(i) + "@test" }
func itoa3(i int) string {
	// always 3 digits, e.g. "007"
	d := []byte{'0', '0', '0'}
	for k := 2; k >= 0 && i > 0; k-- {
		d[k] = byte('0' + i%10)
		i /= 10
	}
	return string(d)
}
