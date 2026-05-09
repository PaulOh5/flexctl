package environments

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/PaulOh5/flexctl/internal/db"
	"github.com/PaulOh5/flexctl/internal/users"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// envFixture opens a test DB, seeds one user, and returns an env Store
// + the test user id + the clock.
func envFixture(t *testing.T) (*Store, string, *clock, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "env.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	clk := &clock{t: time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)}
	us := users.New(d, clk.Now)
	u, err := us.Create(context.Background(), "owner@test")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return New(d, clk.Now), u.ID, clk, d
}

func TestCreatePendingState(t *testing.T) {
	s, uid, _, _ := envFixture(t)
	e, err := s.Create(context.Background(), uid, "pytorch:2.4")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if e.State != StatePending {
		t.Errorf("state=%s, want pending", e.State)
	}
	if e.UserID != uid {
		t.Errorf("user_id=%s", e.UserID)
	}
	if e.SidecarContainerID != "" || e.WorkContainerID != "" {
		t.Errorf("expected empty container ids on create")
	}
	if e.StartedAt != nil || e.StoppedAt != nil {
		t.Errorf("timestamps should be nil on pending")
	}
}

func TestCreateRequiresUserAndImage(t *testing.T) {
	s, uid, _, _ := envFixture(t)
	if _, err := s.Create(context.Background(), "", "img"); err == nil {
		t.Errorf("expected error for empty user")
	}
	if _, err := s.Create(context.Background(), uid, ""); err == nil {
		t.Errorf("expected error for empty image")
	}
}

func TestCreateUnknownUserFK(t *testing.T) {
	s, _, _, _ := envFixture(t)
	if _, err := s.Create(context.Background(), "nope", "img"); err == nil {
		t.Errorf("expected FK error")
	}
}

func TestStateMachineValidTransitions(t *testing.T) {
	s, uid, clk, _ := envFixture(t)
	ctx := context.Background()

	e, _ := s.Create(ctx, uid, "img")

	// pending -> running stamps started_at
	clk.Advance(5 * time.Second)
	if err := s.SetState(ctx, e.ID, StateRunning); err != nil {
		t.Fatalf("pending->running: %v", err)
	}
	got, _ := s.GetByID(ctx, e.ID)
	if got.State != StateRunning {
		t.Errorf("state=%s", got.State)
	}
	if got.StartedAt == nil {
		t.Errorf("started_at not set on running")
	}

	// running -> stopping
	clk.Advance(2 * time.Second)
	if err := s.SetState(ctx, e.ID, StateStopping); err != nil {
		t.Fatalf("running->stopping: %v", err)
	}

	// stopping -> stopped stamps stopped_at and preserves started_at
	clk.Advance(1 * time.Second)
	if err := s.SetState(ctx, e.ID, StateStopped); err != nil {
		t.Fatalf("stopping->stopped: %v", err)
	}
	final, _ := s.GetByID(ctx, e.ID)
	if final.State != StateStopped {
		t.Errorf("state=%s", final.State)
	}
	if final.StoppedAt == nil {
		t.Errorf("stopped_at not set")
	}
	if final.StartedAt == nil {
		t.Errorf("started_at lost on transition through stopping")
	}
}

func TestStateMachineRejectsInvalidTransitions(t *testing.T) {
	s, uid, _, _ := envFixture(t)
	ctx := context.Background()
	e, _ := s.Create(ctx, uid, "img")

	// pending -> stopped not allowed (must go through stopping)
	if err := s.SetState(ctx, e.ID, StateStopped); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("pending->stopped: got %v, want ErrInvalidTransition", err)
	}

	// pending -> running OK, then running -> stopped not allowed
	_ = s.SetState(ctx, e.ID, StateRunning)
	if err := s.SetState(ctx, e.ID, StateStopped); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("running->stopped: got %v, want ErrInvalidTransition", err)
	}
}

func TestTerminalIsTerminal(t *testing.T) {
	s, uid, _, _ := envFixture(t)
	ctx := context.Background()
	e, _ := s.Create(ctx, uid, "img")
	_ = s.SetState(ctx, e.ID, StateFailed) // pending -> failed allowed

	// no transitions allowed out of failed
	for _, to := range []string{StatePending, StateRunning, StateStopping, StateStopped} {
		if err := s.SetState(ctx, e.ID, to); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("failed->%s: got %v, want ErrInvalidTransition", to, err)
		}
	}
}

func TestAssignContainers(t *testing.T) {
	s, uid, _, _ := envFixture(t)
	ctx := context.Background()
	e, _ := s.Create(ctx, uid, "img")

	if err := s.AssignContainers(ctx, e.ID, "ts-1", "work-1", "u1-flexctl"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	got, _ := s.GetByID(ctx, e.ID)
	if got.SidecarContainerID != "ts-1" || got.WorkContainerID != "work-1" || got.TailscaleHostname != "u1-flexctl" {
		t.Errorf("assign mismatch: %+v", got)
	}
}

func TestAssignContainersBlockedAfterTerminal(t *testing.T) {
	s, uid, _, _ := envFixture(t)
	ctx := context.Background()
	e, _ := s.Create(ctx, uid, "img")
	_ = s.SetState(ctx, e.ID, StateFailed)

	if err := s.AssignContainers(ctx, e.ID, "ts", "work", "h"); err != ErrNotFound {
		t.Errorf("assign on terminal: got %v, want ErrNotFound", err)
	}
}

// TestAssignContainersAllowedInStopping covers the race where the user
// hits Stop while StartPair is still placing the pair: we must still
// record the IDs so the reconciler can tear them down.
func TestAssignContainersAllowedInStopping(t *testing.T) {
	s, uid, _, _ := envFixture(t)
	ctx := context.Background()
	e, _ := s.Create(ctx, uid, "img")
	_ = s.SetState(ctx, e.ID, StateRunning)
	_ = s.SetState(ctx, e.ID, StateStopping)

	if err := s.AssignContainers(ctx, e.ID, "ts-1", "work-1", "h"); err != nil {
		t.Errorf("assign in stopping: %v", err)
	}
	got, _ := s.GetByID(ctx, e.ID)
	if got.SidecarContainerID != "ts-1" {
		t.Errorf("ids not recorded in stopping: %+v", got)
	}
}

func TestSetExitReason(t *testing.T) {
	s, uid, _, _ := envFixture(t)
	ctx := context.Background()
	e, _ := s.Create(ctx, uid, "img")
	if err := s.SetExitReason(ctx, e.ID, "OOM in worker container"); err != nil {
		t.Fatalf("set exit: %v", err)
	}
	got, _ := s.GetByID(ctx, e.ID)
	if got.LastExitReason != "OOM in worker container" {
		t.Errorf("got %q", got.LastExitReason)
	}
}

func TestListByUserNewestFirst(t *testing.T) {
	s, uid, clk, _ := envFixture(t)
	ctx := context.Background()

	first, _ := s.Create(ctx, uid, "img-1")
	clk.Advance(time.Second)
	second, _ := s.Create(ctx, uid, "img-2")

	got, err := s.ListByUser(ctx, uid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].ID != second.ID || got[1].ID != first.ID {
		t.Errorf("order: %s,%s want %s,%s", got[0].ID, got[1].ID, second.ID, first.ID)
	}
}

func TestListActiveExcludesTerminal(t *testing.T) {
	s, uid, _, _ := envFixture(t)
	ctx := context.Background()

	a, _ := s.Create(ctx, uid, "img-a")
	b, _ := s.Create(ctx, uid, "img-b")
	c, _ := s.Create(ctx, uid, "img-c")

	_ = s.SetState(ctx, a.ID, StateRunning)
	_ = s.SetState(ctx, a.ID, StateStopping)
	_ = s.SetState(ctx, a.ID, StateStopped) // terminal
	_ = s.SetState(ctx, b.ID, StateFailed)  // terminal
	// c stays pending

	active, err := s.ListActive(ctx)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("len=%d, want 1", len(active))
	}
	if active[0].ID != c.ID {
		t.Errorf("got %s, want %s", active[0].ID, c.ID)
	}
}
