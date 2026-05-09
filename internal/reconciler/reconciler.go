// Package reconciler keeps the database in agreement with what Docker
// is actually doing.
//
// Why it exists: Eng review #3. The split between sidecar and work
// containers means a sidecar crash leaves a zombie work container with
// no network and a held GPU. Conversely, a work-container crash leaves
// a sidecar still consuming a tailnet identity. The reconciler runs on
// a 5-second tick, walks every active environment, asks Docker what
// the pair looks like right now, and either:
//
//   - promotes pending → running once both sides are healthy,
//   - tears down + marks failed when a running env loses either side,
//   - finalizes stopping → stopped when both sides are gone,
//   - reclaims GPU leases whose holder stopped heartbeating.
//
// All Docker mutations go through container.Manager. All DB mutations
// go through environments.Store and scheduler.Scheduler. We never call
// docker directly from this package — the small PairManager interface
// exists so tests can drive the loop with a fake.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/PaulOh5/flexctl/internal/container"
	"github.com/PaulOh5/flexctl/internal/environments"
	"github.com/PaulOh5/flexctl/internal/scheduler"
)

// PairManager is the slice of *container.Manager the reconciler needs.
// Defining it here (rather than in the container package) lets tests
// drive the reconciler without a docker daemon.
type PairManager interface {
	InspectPair(ctx context.Context, p container.Pair) (container.PairStatus, error)
	StopPair(ctx context.Context, p container.Pair) error
}

// Compile-time assertion that the real container.Manager satisfies the
// interface, so renames in container.go fail loudly here.
var _ PairManager = (*container.Manager)(nil)

// Reconciler walks active environments on a tick and brings DB state
// into agreement with Docker reality.
type Reconciler struct {
	envs     *environments.Store
	sched    *scheduler.Scheduler
	pm       PairManager
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time
}

// Option mutates a Reconciler during New.
type Option func(*Reconciler)

// WithInterval overrides the tick interval (default 5s).
func WithInterval(d time.Duration) Option {
	return func(r *Reconciler) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithClock overrides the clock used for ReclaimExpired.
func WithClock(now func() time.Time) Option {
	return func(r *Reconciler) {
		if now != nil {
			r.now = now
		}
	}
}

// New constructs a Reconciler. Pass *container.Manager (or a fake) for
// pm.
func New(
	envs *environments.Store,
	sched *scheduler.Scheduler,
	pm PairManager,
	log *slog.Logger,
	opts ...Option,
) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	r := &Reconciler{
		envs:     envs,
		sched:    sched,
		pm:       pm,
		log:      log,
		interval: 5 * time.Second,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run blocks until ctx is cancelled, calling Tick on the configured
// interval. The first tick fires immediately so a freshly-started
// control plane reconciles before sleeping.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.interval)
	defer t.Stop()

	r.log.Info("reconciler starting", "interval", r.interval)
	for {
		// Tick errors are logged but do not stop the loop. Database
		// flakes happen; killing the reconciler over them creates a
		// far worse situation (frozen state).
		if err := r.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			r.log.Error("tick", "err", err)
		}
		select {
		case <-ctx.Done():
			r.log.Info("reconciler stopping")
			return nil
		case <-t.C:
		}
	}
}

// Tick is one iteration of the reconcile loop. Exported so tests can
// drive it directly without time.NewTicker.
func (r *Reconciler) Tick(ctx context.Context) error {
	// 1. Free GPUs whose holders stopped heartbeating. Pair failures
	// will also release GPUs below, but the lease check is cheaper
	// and catches cases where the work container is gone before the
	// next inspect.
	if n, err := r.sched.ReclaimExpired(ctx, r.now()); err != nil {
		r.log.Error("reclaim expired", "err", err)
	} else if n > 0 {
		r.log.Info("reclaimed expired leases", "count", n)
	}

	// 2. Walk every active environment.
	envs, err := r.envs.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("list active: %w", err)
	}
	for _, env := range envs {
		if err := r.reconcileOne(ctx, env); err != nil {
			// One env's failure does not stop the others.
			r.log.Error("reconcile env", "env", env.ID, "err", err)
		}
	}
	return nil
}

// reconcileOne advances a single environment's state machine based on
// what Docker reports.
func (r *Reconciler) reconcileOne(ctx context.Context, env environments.Environment) error {
	// Pending without containers means the API handler hasn't called
	// StartPair yet (or the call is in flight). Nothing to do.
	if env.SidecarContainerID == "" || env.WorkContainerID == "" {
		return nil
	}

	pair := container.Pair{
		SidecarID: env.SidecarContainerID,
		WorkID:    env.WorkContainerID,
	}
	status, err := r.pm.InspectPair(ctx, pair)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}

	switch env.State {
	case environments.StatePending:
		// Healthy -> running. Either side dead with non-zero exit ->
		// fail (something blew up during startup).
		if status.Healthy() {
			if err := r.envs.SetState(ctx, env.ID, environments.StateRunning); err != nil {
				return fmt.Errorf("set running: %w", err)
			}
			r.log.Info("env running", "env", env.ID)
			return nil
		}
		if startupFailed(status) {
			return r.markFailed(ctx, env, pair, statusReason(status))
		}
		// Still booting; check again next tick.
		return nil

	case environments.StateRunning:
		if status.Healthy() {
			return nil
		}
		// Either side gone or stopped. Tear down + mark failed.
		return r.markFailed(ctx, env, pair, statusReason(status))

	case environments.StateStopping:
		// Wait until both sides are actually gone, then finalize.
		if !status.SidecarRunning && !status.WorkRunning {
			return r.markStopped(ctx, env, pair)
		}
		return nil
	}
	return nil
}

// markFailed tears the pair down and updates DB rows so the GPU is
// returned to the pool and the user sees an exit reason.
func (r *Reconciler) markFailed(
	ctx context.Context,
	env environments.Environment,
	pair container.Pair,
	reason string,
) error {
	if err := r.pm.StopPair(ctx, pair); err != nil {
		// Best-effort. Log and proceed; we still want the DB cleaned up.
		r.log.Warn("stop pair on failure", "env", env.ID, "err", err)
	}
	if n, err := r.sched.ReleaseByEnvID(ctx, env.ID, scheduler.StateFailed); err != nil {
		r.log.Warn("release gpus", "env", env.ID, "err", err)
	} else if n > 0 {
		r.log.Info("released gpus", "env", env.ID, "count", n)
	}
	if err := r.envs.SetExitReason(ctx, env.ID, reason); err != nil {
		r.log.Warn("set exit reason", "env", env.ID, "err", err)
	}
	if err := r.envs.SetState(ctx, env.ID, environments.StateFailed); err != nil {
		return fmt.Errorf("set failed: %w", err)
	}
	r.log.Info("env failed", "env", env.ID, "reason", reason)
	return nil
}

// markStopped finalizes a clean shutdown. Both sides are already gone
// by the time we get here.
func (r *Reconciler) markStopped(
	ctx context.Context,
	env environments.Environment,
	pair container.Pair,
) error {
	// Defensive: stopPair on already-stopped is a no-op for our shell-out
	// manager (idempotent). Doing it here ensures docker rm runs even
	// when the container exited on its own.
	if err := r.pm.StopPair(ctx, pair); err != nil {
		r.log.Warn("stop pair on shutdown", "env", env.ID, "err", err)
	}
	if n, err := r.sched.ReleaseByEnvID(ctx, env.ID, scheduler.StateReleased); err != nil {
		r.log.Warn("release gpus", "env", env.ID, "err", err)
	} else if n > 0 {
		r.log.Info("released gpus", "env", env.ID, "count", n)
	}
	if err := r.envs.SetState(ctx, env.ID, environments.StateStopped); err != nil {
		return fmt.Errorf("set stopped: %w", err)
	}
	r.log.Info("env stopped", "env", env.ID)
	return nil
}

// startupFailed is true when a pending env's containers have already
// exited with a real error (not still booting).
func startupFailed(s container.PairStatus) bool {
	scExited := !s.SidecarRunning && !s.SidecarMissing && s.SidecarExitCode != 0
	wExited := !s.WorkRunning && !s.WorkMissing && s.WorkExitCode != 0
	scGone := s.SidecarMissing
	wGone := s.WorkMissing
	return scExited || wExited || scGone || wGone
}

// statusReason synthesizes a human-readable reason from a status that
// indicated trouble.
func statusReason(s container.PairStatus) string {
	switch {
	case s.SidecarMissing && s.WorkMissing:
		return "both containers disappeared from docker"
	case s.SidecarMissing:
		return "tailscale sidecar disappeared from docker"
	case s.WorkMissing:
		return "work container disappeared from docker"
	case !s.SidecarRunning && s.SidecarExitCode != 0:
		return fmt.Sprintf("tailscale sidecar exited %d", s.SidecarExitCode)
	case !s.WorkRunning && s.WorkExitCode != 0:
		return fmt.Sprintf("work container exited %d", s.WorkExitCode)
	case !s.SidecarRunning:
		return "tailscale sidecar stopped"
	case !s.WorkRunning:
		return "work container stopped"
	default:
		return "pair unhealthy"
	}
}
