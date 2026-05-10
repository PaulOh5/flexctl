# flexctl

Single-node multi-user GPU sharing platform. Bundled with GPU server hardware to give 5-person AI labs / small startups a 5-minute path from "GPU server is on" to "I have an isolated CUDA environment reachable over Tailscale."

Status: Week 4 (shipping line + ops). See `TODOS.md` for the 4-week plan and `~/.gstack/projects/PaulOh5-flexctl/paul-main-design-20260509-162932.md` for the approved design.

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
cmd/flexctl/         # entry point (control plane binary)
internal/db/         # SQLite open + migrate (modernc.org/sqlite, no CGO)
internal/scheduler/  # GPU lock + lease + reclaim (race-safe partial UNIQUE index)
internal/gpu/        # nvidia-smi inventory sync
internal/container/  # docker CLI wrapper, sidecar pair lifecycle
internal/tailscale/  # OAuth client_credentials + ephemeral key issuance
internal/environments/ # env state machine (pending → running → stopping → stopped/failed)
internal/users/      # user CRUD + auto host UID
internal/reconciler/ # 5s tick reconciles DB with docker reality
internal/web/        # HTML+HTMX UI, embedded templates + Linear-style CSS
scripts/             # install.sh, uninstall.sh, flexctl.service, PoC
docs/                # SHIPPING.md, HOTFIX.md, DEMO.md
```

## Operational docs

- `docs/SHIPPING.md` — what the shipping line operator does to ship a box.
- `docs/HOTFIX.md` — vendor support's runbook for fixing a customer's box
  over Tailscale SSH.
- `docs/DEMO.md` — the 30-second sales demo flow.

## Install on a real GPU host

```sh
go build -o ./bin/flexctl ./cmd/flexctl
sudo cp ./bin/flexctl scripts/flexctl.service scripts/install.sh /tmp/shipping/
cd /tmp/shipping && sudo ./install.sh
```

The installer is idempotent and has a `verify` mode (`./install.sh verify`)
that prints what it would do without changes.

## Phase boundaries

Phase 1 (this design): single-node, 5 users, 4-week demo.
Phase 2 (separate product, separate codebase): own datacenter integration. **Not** a shared control plane.
