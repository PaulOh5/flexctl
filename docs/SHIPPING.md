# Shipping checklist

For the line operator burning flexctl into a customer GPU server. One
page; if any step is red, do not ship the box.

## What "ready to ship" means

Per the approved design (`~/.gstack/projects/PaulOh5-flexctl/paul-main-design-*.md`,
Success Criteria), a shippable box satisfies all six:

- [ ] **Tests P1 are green** on the matching binary
      (`go test ./... -race -count=1`; T1 GPU lock race, T2 lease reclaim,
      T3-T4 sidecar reconciliation, T5 Tailscale ephemeral key, T6 e2e smoke).
- [ ] **30-second sales demo runs in under 60 seconds wall-clock** on this
      host (`docs/DEMO.md`). Five users created, environment created,
      SSH-from-tailnet works, nvidia-smi inside the work container shows
      the assigned GPU.
- [ ] **`install.sh` is idempotent on a clean Ubuntu 22.04+ image.**
      Running it twice in a row produces the same result with no errors.
- [ ] **OAuth wizard completes once** during install. After completion
      `/etc/flexctl/oauth.env` exists with mode 0640, owner root:flexctl.
- [ ] **Manual WCAG AA pass.** Keyboard-only navigation reaches every
      action. Color contrast checked with the browser dev tools (text
      vs `--bg` ≥ 4.5:1, large text ≥ 3:1). `prefers-reduced-motion` is
      honored.
- [ ] **Hotfix SOP rehearsed** on this exact box (`docs/HOTFIX.md`).
      The rehearsal proves the vendor's tailnet has authorized SSH
      access to this customer's box.

## Preflight (before install.sh)

The shipping image must already have:

| item                  | check                                                    |
|-----------------------|----------------------------------------------------------|
| Ubuntu 22.04+         | `cat /etc/os-release` shows VERSION_ID 22.04 or newer    |
| nvidia driver ≥ 535   | `nvidia-smi --query-gpu=driver_version --format=csv`    |
| GPU count             | `nvidia-smi -L` lists the SKU we shipped                 |
| systemd               | `systemctl --version` returns                             |
| network egress        | `curl -fsSI https://nvidia.github.io` returns 2xx        |
| internet date         | `date` is within a minute (TLS will reject if skewed)    |

Driver upgrade if needed:
```sh
apt-get update && apt-get install -y nvidia-driver-535-server
reboot
```

## Install

The `install.sh` script and the `flexctl` binary live next to each
other in the shipping bundle:

```
shipping/
├── install.sh
├── flexctl              # `go build -o ./bin/flexctl ./cmd/flexctl`
├── flexctl.service
└── uninstall.sh
```

Run from inside that directory:

```sh
sudo ./install.sh
```

The script writes nothing to `/usr/lib`, only:

- `/usr/local/bin/flexctl` — the binary (mode 0755)
- `/etc/systemd/system/flexctl.service` — unit file
- `/etc/flexctl/oauth.env` — Tailscale creds (mode 0640, root:flexctl)
- `/var/lib/flexctl/` — data dir (owner flexctl:flexctl, mode 0750)
- A `flexctl` system user and group, added to `docker`

Run `sudo ./install.sh verify` to dry-run without making changes.

## OAuth wizard

The interactive wizard prompts for:

1. **Client ID** — from <https://login.tailscale.com/admin/settings/oauth>
2. **Client secret** — never logged anywhere; written only to `oauth.env`
3. **Tailnet name** *(optional)* — e.g. `tail1234.ts.net`. Used to
   render copy-pasteable SSH commands in the UI.

Required tailnet ACL clauses (paste into the customer's tailnet):

```jsonc
{
  "tagOwners": {
    "tag:flexctl-user": ["autogroup:admin"]
  },
  "acls": [
    { "action": "accept", "src": ["*"], "dst": ["tag:flexctl-user:*"] }
  ],
  "ssh": [
    { "action": "accept",
      "src": ["autogroup:member"],
      "dst": ["tag:flexctl-user"],
      "users": ["root"] }
  ]
}
```

If the customer wants to defer OAuth setup, run the install script with
`--no-oauth`. Until creds are written the web UI shows a clear alert
and environment creation is disabled.

## Post-install validation

```sh
# 1. service is running and healthy
systemctl is-active flexctl
curl -fsS http://127.0.0.1:8080/healthz | jq

# 2. expected keys: db_ok=true, gpu_count == nvidia-smi count, reconcile_ticks > 0
# 3. one-shot end-to-end check: open http://<host>:8080 from the operator's
#    laptop (over the customer's tailnet), create a user, create an
#    environment using the prewarmed image, ssh into it, run `nvidia-smi`.
#    Should take well under 5 minutes from "click + New environment" to
#    "GPU listed in the work container".
```

## First-shipment rollback (E20)

The image is dual-partitioned during the burn step. If `flexctl` does
not come up cleanly after install:

1. Boot into the rescue partition (hold `Esc` at POST, pick `flexctl-rescue`).
2. The rescue partition has a known-good earlier flexctl build at
   `/usr/local/bin/flexctl-rescue` and an `install.sh.previous` snapshot.
3. From rescue: `flexctl-rescue dump-state` writes a tarball to
   `/root/flexctl-state.tgz` (DB + journal + daemon.json) for the
   vendor to inspect.
4. If the customer needs to start using the box anyway, swap partitions
   and re-image at the next maintenance window.

(Setting up the rescue partition itself is out of scope for the install
script — it lives in the shipping-line image-burn workflow.)

## What this script does NOT install

- **nvidia driver**: must already be present (or the customer chose to
  install themselves later — flexctl can wait).
- **Tailscale on the host**: not needed. flexctl talks to Tailscale via
  the OAuth API; the tailnet identity for each environment is held by
  its sidecar container, not by the host.
- **A reverse proxy / TLS terminator**: the listener is bound to
  `0.0.0.0:8080`; the customer is expected to front it with a tailnet
  ACL or their own proxy. Phase 1 assumes the tailnet is the security
  boundary.
