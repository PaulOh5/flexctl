package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// Manager creates, inspects, and removes per-env container pairs by
// shelling out to the `docker` CLI.
type Manager struct {
	docker string // path to docker binary; defaults to "docker"
	log    *slog.Logger
}

// Pair is the (sidecar, work) container ID pair for one environment.
type Pair struct {
	SidecarID string
	WorkID    string
}

// PairStatus reports the running state of each side of a pair plus
// optional exit codes when a side has stopped.
type PairStatus struct {
	SidecarRunning  bool
	WorkRunning     bool
	SidecarExitCode int // valid only when !SidecarRunning
	WorkExitCode    int
	SidecarMissing  bool // true if Docker no longer knows the container
	WorkMissing     bool
}

// Healthy is true iff both sides are running.
func (s PairStatus) Healthy() bool {
	return s.SidecarRunning && s.WorkRunning && !s.SidecarMissing && !s.WorkMissing
}

// NewManager returns a Manager that calls the docker binary on PATH.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{docker: "docker", log: log}
}

// StartPair creates and starts the sidecar and work containers for the
// given env. Idempotent: if a container with the expected name already
// exists, its ID is reused and Start is called only when needed.
func (m *Manager) StartPair(ctx context.Context, spec EnvSpec) (Pair, error) {
	if err := spec.validate(); err != nil {
		return Pair{}, fmt.Errorf("invalid spec: %w", err)
	}

	sidecarName := SidecarName(spec.EnvID)
	workName := WorkName(spec.EnvID)

	sidecarID, err := m.ensureRunning(ctx, sidecarName, SidecarRunArgs(spec))
	if err != nil {
		return Pair{}, fmt.Errorf("sidecar: %w", err)
	}

	workID, err := m.ensureRunning(ctx, workName, WorkRunArgs(spec))
	if err != nil {
		return Pair{SidecarID: sidecarID}, fmt.Errorf("work: %w", err)
	}

	m.log.Info("pair started", "env", spec.EnvID, "sidecar", short(sidecarID), "work", short(workID))
	return Pair{SidecarID: sidecarID, WorkID: workID}, nil
}

// StopPair stops and removes both members. Idempotent: missing
// containers are not an error.
func (m *Manager) StopPair(ctx context.Context, p Pair) error {
	// Order matters: stop work first so it does not lose its netns
	// while still running. Sidecar second.
	var firstErr error
	for _, id := range []string{p.WorkID, p.SidecarID} {
		if id == "" {
			continue
		}
		if err := m.stopAndRemove(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	m.log.Info("pair stopped", "sidecar", short(p.SidecarID), "work", short(p.WorkID))
	return nil
}

// InspectPair returns the current status of both members.
func (m *Manager) InspectPair(ctx context.Context, p Pair) (PairStatus, error) {
	var st PairStatus

	scRunning, scExit, scMissing, err := m.inspect(ctx, p.SidecarID)
	if err != nil {
		return st, fmt.Errorf("inspect sidecar: %w", err)
	}
	st.SidecarRunning, st.SidecarExitCode, st.SidecarMissing = scRunning, scExit, scMissing

	wRunning, wExit, wMissing, err := m.inspect(ctx, p.WorkID)
	if err != nil {
		return st, fmt.Errorf("inspect work: %w", err)
	}
	st.WorkRunning, st.WorkExitCode, st.WorkMissing = wRunning, wExit, wMissing

	return st, nil
}

// FindPairByEnvID looks up the (possibly partial) pair owned by envID
// using container names. Useful on startup when we want to reconcile
// against whatever Docker currently knows.
func (m *Manager) FindPairByEnvID(ctx context.Context, envID string) (Pair, error) {
	sidecarID, err := m.findByName(ctx, SidecarName(envID))
	if err != nil {
		return Pair{}, err
	}
	workID, err := m.findByName(ctx, WorkName(envID))
	if err != nil {
		return Pair{SidecarID: sidecarID}, err
	}
	return Pair{SidecarID: sidecarID, WorkID: workID}, nil
}

// ensureRunning returns the ID of a running container matching the
// name, creating it via runArgs if missing or starting it if stopped.
func (m *Manager) ensureRunning(ctx context.Context, name string, runArgs []string) (string, error) {
	id, err := m.findByName(ctx, name)
	if err != nil {
		return "", err
	}
	if id == "" {
		// Create + start in one shot via `docker run -d`.
		out, err := m.docker3(ctx, runArgs...)
		if err != nil {
			return "", fmt.Errorf("docker run %s: %w", name, err)
		}
		return strings.TrimSpace(out), nil
	}
	// Container exists. If stopped, start it.
	running, _, missing, err := m.inspect(ctx, id)
	if err != nil {
		return id, err
	}
	if missing {
		// Race: was just removed. Recurse to recreate.
		return m.ensureRunning(ctx, name, runArgs)
	}
	if !running {
		if _, err := m.docker3(ctx, "start", id); err != nil {
			return id, fmt.Errorf("docker start %s: %w", short(id), err)
		}
	}
	return id, nil
}

// findByName returns the ID of the first container whose name exactly
// matches, or "" if none.
func (m *Manager) findByName(ctx context.Context, name string) (string, error) {
	out, err := m.docker3(ctx,
		"ps", "-aq",
		"--filter", "name=^"+name+"$",
		"--no-trunc",
	)
	if err != nil {
		return "", fmt.Errorf("docker ps %s: %w", name, err)
	}
	id := strings.TrimSpace(out)
	// Filter is a regex but we anchor it; the result is at most one ID.
	if i := strings.Index(id, "\n"); i >= 0 {
		id = id[:i]
	}
	return id, nil
}

// inspect returns (running, exitCode, missing, err) for one container.
// Empty id is treated as missing without erroring.
func (m *Manager) inspect(ctx context.Context, id string) (bool, int, bool, error) {
	if id == "" {
		return false, 0, true, nil
	}
	out, err := m.docker3(ctx,
		"inspect", "--format", "{{.State.Running}} {{.State.ExitCode}}",
		id,
	)
	if err != nil {
		// The CLI prints "No such container" + exits non-zero. We
		// detect that and translate to missing=true.
		if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "no such") {
			return false, 0, true, nil
		}
		return false, 0, false, fmt.Errorf("docker inspect %s: %w", short(id), err)
	}
	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) != 2 {
		return false, 0, false, fmt.Errorf("docker inspect %s: unexpected output %q", short(id), out)
	}
	running := parts[0] == "true"
	var exitCode int
	if _, err := fmt.Sscanf(parts[1], "%d", &exitCode); err != nil {
		return running, 0, false, nil
	}
	return running, exitCode, false, nil
}

// stopAndRemove is idempotent: missing containers are not an error.
func (m *Manager) stopAndRemove(ctx context.Context, id string) error {
	if _, err := m.docker3(ctx, "stop", "-t", "10", id); err != nil {
		if !isNotFoundErr(err) {
			return fmt.Errorf("docker stop %s: %w", short(id), err)
		}
	}
	if _, err := m.docker3(ctx, "rm", "-f", id); err != nil {
		if !isNotFoundErr(err) {
			return fmt.Errorf("docker rm %s: %w", short(id), err)
		}
	}
	return nil
}

// docker3 runs `docker <args...>` and returns combined stdout. Stderr
// is folded into the returned error on failure so callers see exactly
// what dockerd said.
func (m *Manager) docker3(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, m.docker, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), fmt.Errorf("exit %d: %s", ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), err
	}
	return string(out), nil
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "No such") || strings.Contains(s, "no such")
}

// validate makes sure callers passed the fields we cannot synthesize.
func (s EnvSpec) validate() error {
	switch {
	case s.EnvID == "":
		return errors.New("EnvID required")
	case s.TailscaleHostname == "":
		return errors.New("TailscaleHostname required")
	case s.TailscaleAuthKey == "":
		return errors.New("TailscaleAuthKey required")
	case s.WorkImage == "":
		return errors.New("WorkImage required")
	case s.GPUUUID == "":
		return errors.New("GPUUUID required")
	case s.HostUID < 1000:
		return fmt.Errorf("HostUID must be >= 1000, got %d", s.HostUID)
	case s.HostHomeDir == "":
		return errors.New("HostHomeDir required")
	}
	return nil
}

// short returns the first 12 chars of a container ID for log lines.
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
