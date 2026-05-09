# flexctl

Single-node multi-user GPU sharing platform. Bundled with GPU server hardware to give 5-person AI labs / small startups a 5-minute path from "GPU server is on" to "I have an isolated CUDA environment reachable over Tailscale."

Status: Week 1 scaffold (in progress). See `TODOS.md` for the 4-week plan and `~/.gstack/projects/PaulOh5-flexctl/paul-main-design-20260509-162932.md` for the approved design.

## Quick start (dev)

```sh
# Requires Go 1.22+, Docker, nvidia-container-toolkit
go test ./...                          # unit tests
go run ./cmd/flexctl                   # run control plane (skeleton)
TS_AUTHKEY=tskey-... scripts/poc_tailscale_container.sh   # week 1 PoC
```

## Architecture (Phase 1)

Per-user pair of containers sharing one network namespace:
- **ts-userN** — tailscale/tailscale, holds Tailscale node identity (NET_ADMIN + /dev/net/tun)
- **work-userN** — ML image (PyTorch + CUDA), `network_mode: container:ts-userN`, exclusive on one GPU

Control plane runs as a **single Go binary** under systemd on the host. SQLite (modernc.org/sqlite, no CGO) for state. Reconciliation loop watches Docker events to keep DB consistent with reality.

## Layout

```
cmd/flexctl/        # entry point (control plane binary)
internal/db/        # SQLite open + migrate
internal/scheduler/ # GPU lock + lease + reclaim (race-safe)
internal/gpu/       # nvidia-smi shell-out + UUID inventory
internal/container/ # Docker SDK wrapper (sidecar pair lifecycle)  [next]
internal/tailscale/ # OAuth + ephemeral key issuance               [next]
internal/api/       # HTTP handlers + HTMX templates               [next]
scripts/            # PoC + install
```

## Phase boundaries

Phase 1 (this design): single-node, 5 users, 4-week demo.
Phase 2 (separate product, separate codebase): own datacenter integration. **Not** a shared control plane.
