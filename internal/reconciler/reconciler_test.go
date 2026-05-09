package reconciler

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PaulOh5/flexctl/internal/container"
	"github.com/PaulOh5/flexctl/internal/db"
	"github.com/PaulOh5/flexctl/internal/environments"
	"github.com/PaulOh5/flexctl/internal/scheduler"
	"github.com/PaulOh5/flexctl/internal/users"
)

// fakePM is a controllable PairManager for tests. Set the Status the
// reconciler will see for each pair, count StopPair invocations.
type fakePM struct {
	mu           sync.Mutex
	statuses     map[string]container.PairStatus // keyed by sidecar id
	inspectErr   error
	stops        []container.Pair
	stopErr      error
}

func newFakePM() *fakePM {
	return &fakePM{statuses: map[string]container.PairStatus{}}
}

func (f *fakePM) Set(sidecarID string, st container.PairStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[sidecarID] = st
}

func (f *fakePM) InspectPair(ctx context.Context, p container.Pair) (container.PairStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return container.PairStatus{}, f.inspectErr
	}
	if st, ok := f.statuses[p.SidecarID]; ok {
		return st, nil
	}
	// Default: both missing.
	return container.PairStatus{SidecarMissing: true, WorkMissing: true}, nil
}

func (f *fakePM) StopPair(ctx context.Context, p container.Pair) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, p)
	return f.stopErr
}

// fixture wires real envs/users/scheduler stores against an in-memory
// SQLite, plus a fake pair manager and clock.
type fixture struct {
	envs  *environments.Store
	usrs  *users.Store
	sched *scheduler.Scheduler
	pm    *fakePM
	rec   *Reconciler
	clk   *clock
	store *sql.DB
	// pre-seeded test entities
	userID  string
	gpuUUID string
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	dbpath := filepath.Join(dir, "rec.db")
	d, err := db.Open(dbpath)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	clk := &clock{t: time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)}
	usrs := users.New(d, clk.Now)
	u, err := usrs.Create(context.Background(), "owner@test")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	gpuUUID := "GPU-test-0001"
	if _, err := d.Exec(`INSERT INTO gpu_inventory (uuid, device_index, name, memory_total_mb, last_seen_at)
		VALUES (?, 0, 'RTX 5090', 32768, ?)`, gpuUUID, clk.Now().Unix()); err != nil {
		t.Fatalf("seed gpu: %v", err)
	}

	envs := environments.New(d, clk.Now)
	sched := scheduler.New(d, clk.Now)
	pm := newFakePM()
	rec := New(envs, sched, pm, nil, WithClock(clk.Now), WithInterval(time.Millisecond))

	return &fixture{
		envs: envs, usrs: usrs, sched: sched, pm: pm, rec: rec, clk: clk, store: d,
		userID: u.ID, gpuUUID: gpuUUID,
	}
}

// makeEnv creates a pending env, attaches it to a GPU lease, and
// assigns containers so the reconciler will treat it as ready to
// inspect.
func (f *fixture) makeEnv(t *testing.T, image string, ttl time.Duration) environments.Environment {
	t.Helper()
	ctx := context.Background()
	env, err := f.envs.Create(ctx, f.userID, image)
	if err != nil {
		t.Fatalf("create env: %v", err)
	}
	if _, err := f.sched.Allocate(ctx, f.userID, env.ID, []string{f.gpuUUID}, ttl); err != nil {
		t.Fatalf("allocate gpu: %v", err)
	}
	sidecarID := "sc-" + env.ID[:8]
	workID := "wk-" + env.ID[:8]
	if err := f.envs.AssignContainers(ctx, env.ID, sidecarID, workID, "h-"+env.ID[:8]); err != nil {
		t.Fatalf("assign: %v", err)
	}
	got, _ := f.envs.GetByID(ctx, env.ID)
	return *got
}

func TestPendingPromotedToRunningWhenHealthy(t *testing.T) {
	f := newFixture(t)
	env := f.makeEnv(t, "img", 30*time.Second)

	f.pm.Set(env.SidecarContainerID, container.PairStatus{
		SidecarRunning: true, WorkRunning: true,
	})

	if err := f.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got, _ := f.envs.GetByID(context.Background(), env.ID)
	if got.State != environments.StateRunning {
		t.Errorf("state=%s, want running", got.State)
	}
	if got.StartedAt == nil {
		t.Errorf("started_at not set")
	}
}

func TestPendingStaysWhenStillBooting(t *testing.T) {
	f := newFixture(t)
	env := f.makeEnv(t, "img", 30*time.Second)

	// One side up, the other not yet running and not exited (still booting).
	f.pm.Set(env.SidecarContainerID, container.PairStatus{
		SidecarRunning: true,
		WorkRunning:    false,
		WorkExitCode:   0,
		// not missing — still being created
	})

	if err := f.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got, _ := f.envs.GetByID(context.Background(), env.ID)
	if got.State != environments.StatePending {
		t.Errorf("state=%s, want still pending", got.State)
	}
}

func TestPendingFailsOnContainerExit(t *testing.T) {
	f := newFixture(t)
	env := f.makeEnv(t, "img", 30*time.Second)

	f.pm.Set(env.SidecarContainerID, container.PairStatus{
		SidecarRunning:  true,
		WorkRunning:     false,
		WorkExitCode:    137, // OOM-killed during apt-get, say
	})

	if err := f.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got, _ := f.envs.GetByID(context.Background(), env.ID)
	if got.State != environments.StateFailed {
		t.Errorf("state=%s, want failed", got.State)
	}
	if got.LastExitReason == "" {
		t.Errorf("exit reason not recorded")
	}

	// GPU released back to the pool.
	allocs, _ := f.sched.ListAllocated(context.Background())
	if len(allocs) != 0 {
		t.Errorf("active allocations after fail: %d, want 0", len(allocs))
	}

	// StopPair was called for cleanup.
	if len(f.pm.stops) != 1 {
		t.Errorf("StopPair called %d times, want 1", len(f.pm.stops))
	}
}

func TestRunningFailsWhenSidecarVanishes(t *testing.T) {
	f := newFixture(t)
	env := f.makeEnv(t, "img", 30*time.Second)

	// First, drive it to running.
	f.pm.Set(env.SidecarContainerID, container.PairStatus{
		SidecarRunning: true, WorkRunning: true,
	})
	_ = f.rec.Tick(context.Background())

	// Now the sidecar disappears.
	f.pm.Set(env.SidecarContainerID, container.PairStatus{
		SidecarMissing: true, WorkRunning: true,
	})
	if err := f.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	got, _ := f.envs.GetByID(context.Background(), env.ID)
	if got.State != environments.StateFailed {
		t.Errorf("state=%s, want failed", got.State)
	}
	if got.LastExitReason == "" || !contains(got.LastExitReason, "sidecar") {
		t.Errorf("reason should mention sidecar, got %q", got.LastExitReason)
	}
}

func TestStoppingFinalizesWhenBothGone(t *testing.T) {
	f := newFixture(t)
	env := f.makeEnv(t, "img", 30*time.Second)

	ctx := context.Background()
	// Drive: pending -> running -> stopping (manual via SetState as the
	// API handler would).
	_ = f.envs.SetState(ctx, env.ID, environments.StateRunning)
	_ = f.envs.SetState(ctx, env.ID, environments.StateStopping)

	// Both stopped.
	f.pm.Set(env.SidecarContainerID, container.PairStatus{
		SidecarRunning: false, WorkRunning: false,
	})
	if err := f.rec.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	got, _ := f.envs.GetByID(ctx, env.ID)
	if got.State != environments.StateStopped {
		t.Errorf("state=%s, want stopped", got.State)
	}
	if got.StoppedAt == nil {
		t.Errorf("stopped_at not set")
	}
	allocs, _ := f.sched.ListAllocated(ctx)
	if len(allocs) != 0 {
		t.Errorf("expected GPU released, got %d active", len(allocs))
	}
}

func TestStoppingDoesNotFinalizeWhileSidecarRunning(t *testing.T) {
	f := newFixture(t)
	env := f.makeEnv(t, "img", 30*time.Second)
	ctx := context.Background()

	_ = f.envs.SetState(ctx, env.ID, environments.StateRunning)
	_ = f.envs.SetState(ctx, env.ID, environments.StateStopping)

	// Work down, sidecar still running (mid-stop)
	f.pm.Set(env.SidecarContainerID, container.PairStatus{
		SidecarRunning: true, WorkRunning: false,
	})
	_ = f.rec.Tick(ctx)
	got, _ := f.envs.GetByID(ctx, env.ID)
	if got.State != environments.StateStopping {
		t.Errorf("state=%s, want still stopping", got.State)
	}
}

func TestPendingWithoutContainersIsSkipped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	env, _ := f.envs.Create(ctx, f.userID, "img")
	// No AssignContainers; reconciler must not try to inspect.

	// Default fakePM behavior is "both missing" — if the reconciler did
	// inspect, it would mark the env failed. We assert it stays pending.
	if err := f.rec.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got, _ := f.envs.GetByID(ctx, env.ID)
	if got.State != environments.StatePending {
		t.Errorf("state=%s, want pending", got.State)
	}
}

func TestReclaimExpiredOnTick(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Create a stale allocation: short TTL, no heartbeats, advance time.
	env, _ := f.envs.Create(ctx, f.userID, "img")
	if _, err := f.sched.Allocate(ctx, f.userID, env.ID, []string{f.gpuUUID}, 5*time.Second); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	f.clk.Advance(30 * time.Second)

	if err := f.rec.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	allocs, _ := f.sched.ListAllocated(ctx)
	if len(allocs) != 0 {
		t.Errorf("stale alloc not reclaimed: %d active", len(allocs))
	}
}

func TestRunErrorsDontStopLoop(t *testing.T) {
	f := newFixture(t)
	env := f.makeEnv(t, "img", 30*time.Second)

	// Make Inspect always error briefly, then recover.
	f.pm.inspectErr = errors.New("synthetic")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- f.rec.Run(ctx) }()

	// Recover after a few ticks.
	time.Sleep(10 * time.Millisecond)
	f.pm.mu.Lock()
	f.pm.inspectErr = nil
	f.pm.mu.Unlock()
	f.pm.Set(env.SidecarContainerID, container.PairStatus{
		SidecarRunning: true, WorkRunning: true,
	})

	if err := <-done; err != nil {
		t.Errorf("Run returned err: %v", err)
	}
	got, _ := f.envs.GetByID(context.Background(), env.ID)
	if got.State != environments.StateRunning {
		t.Errorf("after recovery state=%s, want running", got.State)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
