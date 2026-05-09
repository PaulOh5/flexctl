// Package environments holds environment lifecycle records and the
// state machine that governs them.
//
// State transitions:
//
//	pending  -> running, failed
//	running  -> stopping, failed
//	stopping -> stopped, failed
//	stopped, failed are terminal.
//
// AssignContainers is called once Docker has placed both pair members,
// SetTailscaleHostname when ts-sidecar advertises an identity. The
// reconciler keeps the row in sync with reality and writes
// SetExitReason when a transition lands in stopped/failed.
package environments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// State constants.
const (
	StatePending  = "pending"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateStopped  = "stopped"
	StateFailed   = "failed"
)

// Common errors.
var (
	ErrNotFound          = errors.New("environment not found")
	ErrInvalidTransition = errors.New("invalid state transition")
)

// Environment mirrors a row in environments.
type Environment struct {
	ID                 string
	UserID             string
	State              string
	Image              string
	SidecarContainerID string
	WorkContainerID    string
	TailscaleHostname  string
	CreatedAt          time.Time
	StartedAt          *time.Time
	StoppedAt          *time.Time
	LastExitReason     string
}

// Store is the env CRUD layer.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// New returns a Store backed by db. now is used for timestamps.
func New(db *sql.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

// Create inserts a new environment in 'pending' state. The user FK
// must exist.
func (s *Store) Create(ctx context.Context, userID, image string) (*Environment, error) {
	if userID == "" || image == "" {
		return nil, fmt.Errorf("userID and image required")
	}
	id := uuid.NewString()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO environments (id, user_id, state, image, created_at)
		VALUES (?, ?, 'pending', ?, ?)
	`, id, userID, image, s.now().Unix())
	if err != nil {
		return nil, fmt.Errorf("create env: %w", err)
	}
	return s.GetByID(ctx, id)
}

// AssignContainers sets the sidecar/work container IDs and the tailnet
// hostname after Docker has placed the pair. Allowed in pending or
// running state.
func (s *Store) AssignContainers(ctx context.Context, id, sidecarID, workID, tsHostname string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE environments
		SET sidecar_container_id = ?, work_container_id = ?, tailscale_hostname = ?
		WHERE id = ? AND state IN ('pending','running')
	`, sidecarID, workID, tsHostname, id)
	if err != nil {
		return fmt.Errorf("assign containers: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetState transitions the env. Auto-stamps started_at on first
// 'running' and stopped_at on 'stopped'/'failed'. Returns
// ErrInvalidTransition if the move is not in the allowed graph.
func (s *Store) SetState(ctx context.Context, id, newState string) error {
	cur, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if !validTransition(cur.State, newState) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, cur.State, newState)
	}

	now := s.now().Unix()
	startedAt, stoppedAt := timeStamps(cur, newState, now)

	_, err = s.db.ExecContext(ctx, `
		UPDATE environments
		SET state = ?, started_at = COALESCE(started_at, ?), stopped_at = ?
		WHERE id = ?
	`, newState, nullableInt64(startedAt), nullableInt64(stoppedAt), id)
	if err != nil {
		return fmt.Errorf("update state: %w", err)
	}
	return nil
}

// SetExitReason records why the env entered a terminal state. Safe to
// call multiple times; later calls overwrite.
func (s *Store) SetExitReason(ctx context.Context, id, reason string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE environments SET last_exit_reason = ? WHERE id = ?
	`, reason, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetByID returns the environment with the given id, or ErrNotFound.
func (s *Store) GetByID(ctx context.Context, id string) (*Environment, error) {
	return scanEnv(s.db.QueryRowContext(ctx, baseSelect+` WHERE id = ?`, id))
}

// ListByUser returns all envs owned by userID, newest first.
func (s *Store) ListByUser(ctx context.Context, userID string) ([]Environment, error) {
	return s.queryMany(ctx, baseSelect+` WHERE user_id = ? ORDER BY created_at DESC`, userID)
}

// ListActive returns envs in non-terminal states (pending/running/stopping).
// The reconciler iterates these every tick.
func (s *Store) ListActive(ctx context.Context) ([]Environment, error) {
	return s.queryMany(ctx, baseSelect+` WHERE state IN ('pending','running','stopping') ORDER BY created_at`)
}

func (s *Store) queryMany(ctx context.Context, q string, args ...any) ([]Environment, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

const baseSelect = `
SELECT id, user_id, state, image,
       sidecar_container_id, work_container_id, tailscale_hostname,
       created_at, started_at, stopped_at, last_exit_reason
FROM environments`

type rowScanner interface{ Scan(dest ...any) error }

func scanEnv(rs rowScanner) (*Environment, error) {
	var e Environment
	var sidecar, work, tsHost, exitReason sql.NullString
	var createdAt int64
	var startedAt, stoppedAt sql.NullInt64

	err := rs.Scan(
		&e.ID, &e.UserID, &e.State, &e.Image,
		&sidecar, &work, &tsHost,
		&createdAt, &startedAt, &stoppedAt, &exitReason,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	e.SidecarContainerID = sidecar.String
	e.WorkContainerID = work.String
	e.TailscaleHostname = tsHost.String
	e.LastExitReason = exitReason.String
	e.CreatedAt = time.Unix(createdAt, 0)
	if startedAt.Valid {
		t := time.Unix(startedAt.Int64, 0)
		e.StartedAt = &t
	}
	if stoppedAt.Valid {
		t := time.Unix(stoppedAt.Int64, 0)
		e.StoppedAt = &t
	}
	return &e, nil
}

// validTransition encodes the state graph defined at the top of this file.
func validTransition(from, to string) bool {
	allowed := map[string][]string{
		StatePending:  {StateRunning, StateFailed},
		StateRunning:  {StateStopping, StateFailed},
		StateStopping: {StateStopped, StateFailed},
	}
	for _, ok := range allowed[from] {
		if ok == to {
			return true
		}
	}
	return false
}

// timeStamps returns the (started_at, stopped_at) pair to write for a
// transition. started_at is set on first move to 'running'; stopped_at
// on entry to stopped/failed.
func timeStamps(cur *Environment, newState string, now int64) (startedAt, stoppedAt int64) {
	// COALESCE(started_at, ?) in SQL means "use existing if already set,
	// else this value". We pass `now` only when transitioning to running
	// for the first time; otherwise pass 0 which COALESCE treats as
	// "set" (because 0 is not NULL). To keep COALESCE semantics simple
	// we instead unconditionally pass current started_at if set.
	if cur.StartedAt != nil {
		startedAt = cur.StartedAt.Unix()
	} else if newState == StateRunning {
		startedAt = now
	}
	if newState == StateStopped || newState == StateFailed {
		stoppedAt = now
	} else if cur.StoppedAt != nil {
		stoppedAt = cur.StoppedAt.Unix()
	}
	return
}

// nullableInt64 maps 0 -> NULL so COALESCE/started_at semantics stay
// correct. (Genuine 0 timestamps are pre-1970 and we never write them.)
func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
