// Package scheduler is the race-safe GPU allocator.
//
// Invariant: at any instant, at most one row in gpu_allocation has
// state='allocated' for a given gpu_uuid. Enforced by the
// `gpu_alloc_active` UNIQUE partial index plus IMMEDIATE-locked
// transactions (see internal/db.Open).
//
// Allocations carry a lease (heartbeat + TTL). If the holder stops
// heartbeating, ReclaimExpired releases the GPU so it does not stay
// stuck because a process crashed mid-job.
package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Common errors.
var (
	ErrNoGPUAvailable = errors.New("no GPU available among candidates")
	ErrNotFound       = errors.New("allocation not found")
)

// Allocation mirrors a row in gpu_allocation.
type Allocation struct {
	ID              int64
	GPUUUID         string
	UserID          string
	EnvID           string
	State           string
	LeasedAt        time.Time
	HeartbeatAt     time.Time
	LeaseTTLSeconds int
	ReleasedAt      *time.Time
}

// State constants.
const (
	StateAllocated = "allocated"
	StateReleased  = "released"
	StateFailed    = "failed"
)

// Scheduler is the GPU allocator.
type Scheduler struct {
	db  *sql.DB
	now func() time.Time
}

// New returns a Scheduler backed by db. The now function is used for
// timestamps; pass time.Now in production and a fake clock in tests.
func New(db *sql.DB, now func() time.Time) *Scheduler {
	if now == nil {
		now = time.Now
	}
	return &Scheduler{db: db, now: now}
}

// Allocate reserves the first GPU from candidateUUIDs that is currently
// free. Returns ErrNoGPUAvailable if none is free.
//
// Concurrent callers may race: the IMMEDIATE transaction lock plus the
// UNIQUE partial index ensures at most one wins. Other callers see
// either ErrNoGPUAvailable (if all candidates were taken in the gap) or
// the SQLite UNIQUE error (if a writer slipped in between SELECT and
// INSERT, in which case we retry once).
func (s *Scheduler) Allocate(
	ctx context.Context,
	userID, envID string,
	candidateUUIDs []string,
	ttl time.Duration,
) (*Allocation, error) {
	if len(candidateUUIDs) == 0 {
		return nil, ErrNoGPUAvailable
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	ttlSec := int(ttl.Seconds())
	if ttlSec < 1 {
		ttlSec = 1
	}

	// Up to 2 attempts: a UNIQUE collision means another tx grabbed
	// the same UUID while we were committing; the second try will
	// see it and pick the next candidate or return ErrNoGPUAvailable.
	for attempt := 0; attempt < 2; attempt++ {
		alloc, err := s.allocateOnce(ctx, userID, envID, candidateUUIDs, ttlSec)
		if err == nil {
			return alloc, nil
		}
		if errors.Is(err, ErrNoGPUAvailable) {
			return nil, err
		}
		// Likely a UNIQUE collision; retry once.
	}
	return nil, fmt.Errorf("allocate: exhausted retries")
}

func (s *Scheduler) allocateOnce(
	ctx context.Context,
	userID, envID string,
	candidateUUIDs []string,
	ttlSec int,
) (*Allocation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// SELECT first candidate not currently allocated.
	placeholders, args := buildIn(candidateUUIDs)
	pickQuery := fmt.Sprintf(`
		SELECT uuid FROM gpu_inventory
		WHERE uuid IN (%s)
		  AND uuid NOT IN (
		    SELECT gpu_uuid FROM gpu_allocation WHERE state = 'allocated'
		  )
		ORDER BY device_index
		LIMIT 1
	`, placeholders)

	var picked string
	if err := tx.QueryRowContext(ctx, pickQuery, args...).Scan(&picked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoGPUAvailable
		}
		return nil, fmt.Errorf("pick: %w", err)
	}

	now := s.now().Unix()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO gpu_allocation (
		  gpu_uuid, user_id, env_id, state,
		  leased_at, heartbeat_at, lease_ttl_seconds
		) VALUES (?, ?, ?, 'allocated', ?, ?, ?)
	`, picked, userID, envID, now, now, ttlSec)
	if err != nil {
		return nil, fmt.Errorf("insert: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return s.GetByID(ctx, id)
}

// Heartbeat updates heartbeat_at to now for an active allocation.
// Returns ErrNotFound if the allocation does not exist or is no longer
// active.
func (s *Scheduler) Heartbeat(ctx context.Context, id int64) error {
	now := s.now().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE gpu_allocation
		SET heartbeat_at = ?
		WHERE id = ? AND state = 'allocated'
	`, now, id)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Release marks an allocation released or failed. Idempotent: calling
// Release twice on the same id returns ErrNotFound the second time.
func (s *Scheduler) Release(ctx context.Context, id int64, finalState string) error {
	if finalState != StateReleased && finalState != StateFailed {
		return fmt.Errorf("invalid final state %q", finalState)
	}
	now := s.now().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE gpu_allocation
		SET state = ?, released_at = ?
		WHERE id = ? AND state = 'allocated'
	`, finalState, now, id)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReclaimExpired releases allocations whose heartbeat_at + ttl is
// before the supplied now. Returns the count reclaimed.
//
// Call this periodically (e.g., every 10s) from the reconciliation
// loop. Reclaimed allocations are marked 'failed' since the holder
// stopped heartbeating.
func (s *Scheduler) ReclaimExpired(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE gpu_allocation
		SET state = 'failed', released_at = ?
		WHERE state = 'allocated'
		  AND (heartbeat_at + lease_ttl_seconds) < ?
	`, cutoff, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reclaim: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ReleaseByEnvID marks all active allocations belonging to envID as
// finalState (released or failed). Used by the reconciler when an env
// fails or stops: one call cleans up however many GPUs the env held.
// Returns the count released. Idempotent: calling on an env with no
// active allocations returns 0 with nil error.
func (s *Scheduler) ReleaseByEnvID(ctx context.Context, envID, finalState string) (int, error) {
	if finalState != StateReleased && finalState != StateFailed {
		return 0, fmt.Errorf("invalid final state %q", finalState)
	}
	now := s.now().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE gpu_allocation
		SET state = ?, released_at = ?
		WHERE env_id = ? AND state = 'allocated'
	`, finalState, now, envID)
	if err != nil {
		return 0, fmt.Errorf("release by env: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// GetByID loads an allocation by id.
func (s *Scheduler) GetByID(ctx context.Context, id int64) (*Allocation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, gpu_uuid, user_id, env_id, state,
		       leased_at, heartbeat_at, lease_ttl_seconds, released_at
		FROM gpu_allocation WHERE id = ?
	`, id)
	return scanAllocation(row)
}

// GPUStatus joins a row from gpu_inventory with its active allocation
// (if any). Used by the dashboard to render the GPU availability strip.
type GPUStatus struct {
	UUID          string
	DeviceIndex   int
	Name          string
	MemoryTotalMB int
	// Empty when free; populated when state='allocated'.
	AllocationID int64
	UserID       string
	EnvID        string
	LeasedAt     time.Time
}

// Free is true when no environment currently holds this GPU.
func (g GPUStatus) Free() bool { return g.AllocationID == 0 }

// ListGPUs returns one row per GPU in the inventory, with allocation
// fields populated for GPUs currently in use. Ordered by device_index
// for stable rendering.
func (s *Scheduler) ListGPUs(ctx context.Context) ([]GPUStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
		    inv.uuid,
		    inv.device_index,
		    inv.name,
		    inv.memory_total_mb,
		    COALESCE(a.id, 0)        AS alloc_id,
		    COALESCE(a.user_id, '')  AS user_id,
		    COALESCE(a.env_id, '')   AS env_id,
		    COALESCE(a.leased_at, 0) AS leased_at
		FROM gpu_inventory inv
		LEFT JOIN gpu_allocation a
		    ON a.gpu_uuid = inv.uuid AND a.state = 'allocated'
		ORDER BY inv.device_index
	`)
	if err != nil {
		return nil, fmt.Errorf("list gpus: %w", err)
	}
	defer rows.Close()

	var out []GPUStatus
	for rows.Next() {
		var g GPUStatus
		var leasedAt int64
		if err := rows.Scan(
			&g.UUID, &g.DeviceIndex, &g.Name, &g.MemoryTotalMB,
			&g.AllocationID, &g.UserID, &g.EnvID, &leasedAt,
		); err != nil {
			return nil, err
		}
		if leasedAt > 0 {
			g.LeasedAt = time.Unix(leasedAt, 0)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListAllocated returns all currently active allocations.
func (s *Scheduler) ListAllocated(ctx context.Context) ([]Allocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, gpu_uuid, user_id, env_id, state,
		       leased_at, heartbeat_at, lease_ttl_seconds, released_at
		FROM gpu_allocation WHERE state = 'allocated'
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Allocation
	for rows.Next() {
		a, err := scanAllocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// rowScanner is implemented by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAllocation(rs rowScanner) (*Allocation, error) {
	var a Allocation
	var leasedAt, heartbeatAt int64
	var releasedAt sql.NullInt64

	err := rs.Scan(
		&a.ID,
		&a.GPUUUID,
		&a.UserID,
		&a.EnvID,
		&a.State,
		&leasedAt,
		&heartbeatAt,
		&a.LeaseTTLSeconds,
		&releasedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.LeasedAt = time.Unix(leasedAt, 0)
	a.HeartbeatAt = time.Unix(heartbeatAt, 0)
	if releasedAt.Valid {
		t := time.Unix(releasedAt.Int64, 0)
		a.ReleasedAt = &t
	}
	return &a, nil
}

// buildIn returns ("?,?,?", args) for use in WHERE x IN (?,?,?).
func buildIn(uuids []string) (string, []any) {
	args := make([]any, len(uuids))
	ph := make([]byte, 0, 2*len(uuids))
	for i, u := range uuids {
		args[i] = u
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
	}
	return string(ph), args
}
