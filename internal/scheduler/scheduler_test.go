package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PaulOh5/flexctl/internal/db"
)

// fixedClock returns the given time and lets tests advance it.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestSched opens a fresh on-disk SQLite (modernc requires a file for
// concurrent connections to share state), seeds inventory + a user +
// environments, and returns a Scheduler with a fixed clock.
func newTestSched(t *testing.T, gpuCount int) (*Scheduler, *fixedClock, *sql.DB) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	store, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	clk := &fixedClock{t: now}

	// Seed inventory.
	for i := 0; i < gpuCount; i++ {
		_, err := store.Exec(`
			INSERT INTO gpu_inventory (uuid, device_index, name, memory_total_mb, last_seen_at)
			VALUES (?, ?, 'RTX 5090', 32768, ?)
		`, gpuUUID(i), i, now.Unix())
		if err != nil {
			t.Fatalf("seed gpu %d: %v", i, err)
		}
	}

	// Seed one user and one env per slot we'll allocate (tests reuse the env id only when intentional).
	_, err = store.Exec(`INSERT INTO users (id, email, host_uid, created_at) VALUES ('u1', 'u1@test', 10001, ?)`, now.Unix())
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Seed a generous number so concurrency tests with N racers all have
	// foreign-key-valid env_ids regardless of how many goroutines run.
	const seededEnvs = 128
	for i := 0; i < seededEnvs; i++ {
		_, err := store.Exec(`
			INSERT INTO environments (id, user_id, state, image, created_at)
			VALUES (?, 'u1', 'pending', 'pytorch:2.4', ?)
		`, envID(i), now.Unix())
		if err != nil {
			t.Fatalf("seed env: %v", err)
		}
	}

	return New(store, clk.Now), clk, store
}

func gpuUUID(i int) string {
	return fmt.Sprintf("GPU-test-%04d", i)
}

func envID(i int) string {
	return fmt.Sprintf("env-%04d", i)
}

// allUUIDs returns all candidate uuids the scheduler can pick from.
func allUUIDs(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = gpuUUID(i)
	}
	return out
}

// TestAllocateBasic — happy path: one user, one GPU, one env.
func TestAllocateBasic(t *testing.T) {
	s, _, _ := newTestSched(t, 1)
	ctx := context.Background()

	a, err := s.Allocate(ctx, "u1", envID(0), allUUIDs(1), 30*time.Second)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if a.GPUUUID != gpuUUID(0) {
		t.Errorf("got %s, want %s", a.GPUUUID, gpuUUID(0))
	}
	if a.State != StateAllocated {
		t.Errorf("state: got %s", a.State)
	}
	if a.LeaseTTLSeconds != 30 {
		t.Errorf("ttl: got %d", a.LeaseTTLSeconds)
	}
}

// TestAllocateNoneAvailable — empty candidate list returns ErrNoGPUAvailable.
func TestAllocateNoneAvailable(t *testing.T) {
	s, _, _ := newTestSched(t, 0)
	_, err := s.Allocate(context.Background(), "u1", envID(0), nil, 30*time.Second)
	if err != ErrNoGPUAvailable {
		t.Errorf("got %v, want ErrNoGPUAvailable", err)
	}
}

// TestAllocateRaceSerialized (T1) — N goroutines race for 1 GPU; exactly one wins.
func TestAllocateRaceSerialized(t *testing.T) {
	const racers = 16
	s, _, _ := newTestSched(t, 1) // single GPU
	ctx := context.Background()

	var wins atomic.Int64
	var noGPU atomic.Int64
	var other atomic.Int64

	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := s.Allocate(ctx, "u1", envID(i), allUUIDs(1), 30*time.Second)
			switch {
			case err == nil:
				wins.Add(1)
			case err == ErrNoGPUAvailable:
				noGPU.Add(1)
			default:
				other.Add(1)
				t.Logf("unexpected err: %v", err)
			}
		}()
	}
	wg.Wait()

	if wins.Load() != 1 {
		t.Fatalf("wins=%d; want exactly 1 (noGPU=%d, other=%d)", wins.Load(), noGPU.Load(), other.Load())
	}
	if other.Load() != 0 {
		t.Errorf("non-NoGPU errors: %d", other.Load())
	}

	// Verify exactly one row in DB has state='allocated'.
	allocs, err := s.ListAllocated(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(allocs) != 1 {
		t.Errorf("active allocations: got %d, want 1", len(allocs))
	}
}

// TestAllocateMultipleGPUs — N concurrent allocations succeed when N GPUs available.
func TestAllocateMultipleGPUs(t *testing.T) {
	const gpus = 4
	s, _, _ := newTestSched(t, gpus)
	ctx := context.Background()

	var wins atomic.Int64
	var wg sync.WaitGroup
	wg.Add(gpus * 2) // twice the requesters of GPU count

	for i := 0; i < gpus*2; i++ {
		i := i
		go func() {
			defer wg.Done()
			if _, err := s.Allocate(ctx, "u1", envID(i), allUUIDs(gpus), 30*time.Second); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()

	if wins.Load() != int64(gpus) {
		t.Errorf("wins=%d, want %d", wins.Load(), gpus)
	}
	allocs, err := s.ListAllocated(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(allocs) != gpus {
		t.Errorf("active=%d want %d", len(allocs), gpus)
	}
}

// TestHeartbeatUpdatesTimestamp.
func TestHeartbeatUpdatesTimestamp(t *testing.T) {
	s, clk, _ := newTestSched(t, 1)
	ctx := context.Background()

	a, err := s.Allocate(ctx, "u1", envID(0), allUUIDs(1), 30*time.Second)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	t0 := a.HeartbeatAt

	clk.Advance(15 * time.Second)
	if err := s.Heartbeat(ctx, a.ID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	got, err := s.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.HeartbeatAt.After(t0) {
		t.Errorf("heartbeat not advanced: %v -> %v", t0, got.HeartbeatAt)
	}
}

// TestHeartbeatAfterRelease — heartbeating a released allocation returns ErrNotFound.
func TestHeartbeatAfterRelease(t *testing.T) {
	s, _, _ := newTestSched(t, 1)
	ctx := context.Background()

	a, err := s.Allocate(ctx, "u1", envID(0), allUUIDs(1), 30*time.Second)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := s.Release(ctx, a.ID, StateReleased); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := s.Heartbeat(ctx, a.ID); err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestReleaseFreesGPU — after Release, the same GPU can be allocated again.
func TestReleaseFreesGPU(t *testing.T) {
	s, _, _ := newTestSched(t, 1)
	ctx := context.Background()

	a, err := s.Allocate(ctx, "u1", envID(0), allUUIDs(1), 30*time.Second)
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if err := s.Release(ctx, a.ID, StateReleased); err != nil {
		t.Fatalf("release: %v", err)
	}
	b, err := s.Allocate(ctx, "u1", envID(1), allUUIDs(1), 30*time.Second)
	if err != nil {
		t.Fatalf("re-allocate: %v", err)
	}
	if b.GPUUUID != a.GPUUUID {
		t.Errorf("expected same GPU, got %s vs %s", b.GPUUUID, a.GPUUUID)
	}
	if b.ID == a.ID {
		t.Errorf("expected new allocation row")
	}
}

// TestReleaseTwiceErrNotFound — Release is idempotent; second call ErrNotFound.
func TestReleaseTwiceErrNotFound(t *testing.T) {
	s, _, _ := newTestSched(t, 1)
	ctx := context.Background()

	a, _ := s.Allocate(ctx, "u1", envID(0), allUUIDs(1), 30*time.Second)
	if err := s.Release(ctx, a.ID, StateReleased); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := s.Release(ctx, a.ID, StateReleased); err != ErrNotFound {
		t.Errorf("second release: got %v, want ErrNotFound", err)
	}
}

// TestReleaseInvalidState — only 'released' or 'failed' are valid finals.
func TestReleaseInvalidState(t *testing.T) {
	s, _, _ := newTestSched(t, 1)
	a, _ := s.Allocate(context.Background(), "u1", envID(0), allUUIDs(1), 30*time.Second)
	if err := s.Release(context.Background(), a.ID, "allocated"); err == nil {
		t.Errorf("expected error for invalid final state")
	}
}

// TestReclaimExpired (T2) — lease past TTL is reclaimed and the GPU becomes free.
func TestReclaimExpired(t *testing.T) {
	s, clk, _ := newTestSched(t, 1)
	ctx := context.Background()

	a, err := s.Allocate(ctx, "u1", envID(0), allUUIDs(1), 30*time.Second)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	// Before TTL: nothing reclaimed.
	clk.Advance(20 * time.Second)
	if n, _ := s.ReclaimExpired(ctx, clk.Now()); n != 0 {
		t.Errorf("early reclaim n=%d, want 0", n)
	}

	// Past TTL: reclaimed.
	clk.Advance(20 * time.Second) // total +40s, TTL was 30
	n, err := s.ReclaimExpired(ctx, clk.Now())
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaim n=%d, want 1", n)
	}

	got, err := s.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateFailed {
		t.Errorf("state=%s, want failed", got.State)
	}
	if got.ReleasedAt == nil {
		t.Errorf("released_at not set")
	}

	// The GPU is now free; a new allocation succeeds.
	if _, err := s.Allocate(ctx, "u1", envID(1), allUUIDs(1), 30*time.Second); err != nil {
		t.Errorf("after reclaim allocate: %v", err)
	}
}

// TestReclaimSurvivesHeartbeat — heartbeating before TTL keeps the lease alive.
func TestReclaimSurvivesHeartbeat(t *testing.T) {
	s, clk, _ := newTestSched(t, 1)
	ctx := context.Background()

	a, err := s.Allocate(ctx, "u1", envID(0), allUUIDs(1), 30*time.Second)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	clk.Advance(20 * time.Second)
	if err := s.Heartbeat(ctx, a.ID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	clk.Advance(20 * time.Second) // 20s after heartbeat, still inside TTL

	n, err := s.ReclaimExpired(ctx, clk.Now())
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 0 {
		t.Errorf("reclaim n=%d, want 0", n)
	}
}

// TestReleaseByEnvID — reconciler shortcut: one call frees all GPUs an env held.
func TestReleaseByEnvID(t *testing.T) {
	s, _, _ := newTestSched(t, 4)
	ctx := context.Background()

	// One env grabs two GPUs (e.g., distributed training).
	a1, err := s.Allocate(ctx, "u1", envID(0), allUUIDs(4)[:2], 30*time.Second)
	if err != nil {
		t.Fatalf("alloc 1: %v", err)
	}
	a2, err := s.Allocate(ctx, "u1", envID(0), allUUIDs(4)[1:3], 30*time.Second)
	if err != nil {
		t.Fatalf("alloc 2: %v", err)
	}
	// And another env grabs one GPU (must not be touched).
	other, err := s.Allocate(ctx, "u1", envID(1), allUUIDs(4)[3:], 30*time.Second)
	if err != nil {
		t.Fatalf("alloc other: %v", err)
	}

	n, err := s.ReleaseByEnvID(ctx, envID(0), StateFailed)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if n != 2 {
		t.Errorf("released n=%d, want 2", n)
	}

	// Both env-0 allocations are now failed.
	for _, id := range []int64{a1.ID, a2.ID} {
		got, _ := s.GetByID(ctx, id)
		if got.State != StateFailed {
			t.Errorf("alloc %d state=%s, want failed", id, got.State)
		}
	}
	// Other env's allocation untouched.
	got, _ := s.GetByID(ctx, other.ID)
	if got.State != StateAllocated {
		t.Errorf("other alloc state=%s, want allocated", got.State)
	}

	// Idempotent: second call releases nothing.
	if n, _ := s.ReleaseByEnvID(ctx, envID(0), StateFailed); n != 0 {
		t.Errorf("second release n=%d, want 0", n)
	}
}

func TestReleaseByEnvIDInvalidState(t *testing.T) {
	s, _, _ := newTestSched(t, 1)
	if _, err := s.ReleaseByEnvID(context.Background(), envID(0), "allocated"); err == nil {
		t.Errorf("expected error for invalid final state")
	}
}

// TestUUIDStabilityAcrossReboot (T10) — DB-side allocations are keyed by UUID,
// so a host reboot that re-orders device indexes does not invalidate them.
func TestUUIDStabilityAcrossReboot(t *testing.T) {
	s, _, store := newTestSched(t, 2)
	ctx := context.Background()

	// Allocate GPU 0 (UUID stable).
	target := gpuUUID(0)
	a, err := s.Allocate(ctx, "u1", envID(0), []string{target}, 30*time.Second)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	// Simulate a host reboot that swapped device indexes:
	// the UUID still refers to the same physical GPU, only the index changed.
	if _, err := store.Exec(`UPDATE gpu_inventory SET device_index = 99 WHERE uuid = ?`, target); err != nil {
		t.Fatalf("simulate reorder: %v", err)
	}

	// The allocation is still tied to the same UUID.
	got, err := s.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GPUUUID != target {
		t.Errorf("uuid drifted: %s vs %s", got.GPUUUID, target)
	}

	// And re-allocating the same UUID still fails (it's still held).
	if _, err := s.Allocate(ctx, "u1", envID(1), []string{target}, 30*time.Second); err != ErrNoGPUAvailable {
		t.Errorf("expected ErrNoGPUAvailable, got %v", err)
	}
}
