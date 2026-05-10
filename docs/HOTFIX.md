# Hotfix SOP

How vendor support pushes a fix to a customer's flexctl box without
asking them to do anything beyond keeping the box on.

## Prerequisites (one-time per customer)

For this SOP to work, both must already be true:

1. **The customer's tailnet authorizes the vendor's support tag.** During
   the OAuth setup wizard the customer added a tailnet ACL clause like:

   ```jsonc
   "ssh": [
     { "action": "accept",
       "src":    ["tag:flexctl-vendor-support"],
       "dst":    ["tag:flexctl-user"],
       "users":  ["root"] }
   ]
   ```

2. **The vendor has a Tailscale node tagged `tag:flexctl-vendor-support`.**
   This is the support engineer's laptop or a shared bastion. It must
   be peered with the customer's tailnet via federation or a shared
   identity provider.

If either is missing, this SOP cannot run; the customer has to update
their ACL or grant access first.

## When to use

- A bug landed that affects this customer's box.
- A security fix needs to roll out before the next scheduled image cut.
- The customer reports an issue and we want to inspect live state
  rather than ask them for `journalctl` dumps.

For routine upgrades that wait for the next image, do not use this SOP;
let the shipping pipeline pick the new build.

## Diagnostic flow (read-only)

```sh
# 1. Reach the box. Hostname follows the customer's tailnet naming.
tailscale ssh root@<customer-host>

# 2. Service health
systemctl is-active flexctl
journalctl -u flexctl -n 200 --no-pager

# 3. Control plane health (from the box)
curl -s http://127.0.0.1:8080/healthz | jq
# expect: db_ok=true, reconcile_ticks growing, gpu_count matches nvidia-smi

# 4. Reconciler liveness
journalctl -u flexctl --since "5 minutes ago" | grep "reconcil"

# 5. Docker pair state for any running env
docker ps --filter "label=com.flexctl.env" --format \
  "table {{.Names}}\t{{.Status}}\t{{.Image}}"

# 6. SQLite snapshot (read-only; safe to run live thanks to WAL)
sqlite3 /var/lib/flexctl/flexctl.db \
  "SELECT id, state, image, last_exit_reason FROM environments
   ORDER BY created_at DESC LIMIT 10;"
```

If diagnosis is enough, log what you saw in the support ticket and
disconnect. Stop here.

## Hotfix procedure

```sh
# 1. Stage the new binary in /tmp on the customer's box (over Tailscale,
#    using your local copy compiled for linux/amd64).
scp -i tailscale flexctl-new root@<customer-host>:/tmp/flexctl-new

# 2. SSH in and finish from the box.
tailscale ssh root@<customer-host>

# 3. Verify the binary is intact + the right architecture.
sha256sum /tmp/flexctl-new           # match the value from CI
file     /tmp/flexctl-new           # ELF 64-bit LSB pie executable, x86-64

# 4. Snapshot the previous binary so we can roll back instantly.
cp /usr/local/bin/flexctl /usr/local/bin/flexctl.prev-$(date +%Y%m%d-%H%M)

# 5. Atomic swap.
install -m 0755 /tmp/flexctl-new /usr/local/bin/flexctl

# 6. Reload the service. systemd will SIGTERM, wait up to TimeoutStopSec,
#    then start the new binary. live-restore=true on dockerd keeps user
#    containers untouched across the bounce.
systemctl restart flexctl

# 7. Verify the new binary started clean.
sleep 2
systemctl is-active flexctl                                  # must be "active"
curl -fsS http://127.0.0.1:8080/healthz | jq -r '.db_ok'    # must be "true"
journalctl -u flexctl -n 30 --no-pager                       # no level=ERROR

# 8. Watch one full reconciler tick to make sure the loop survived
#    the swap. The number must increase between calls.
curl -s http://127.0.0.1:8080/healthz | jq -r '.reconcile_ticks'
sleep 7
curl -s http://127.0.0.1:8080/healthz | jq -r '.reconcile_ticks'
```

## Rollback

If `/healthz` does not return `db_ok=true` within 30 seconds of the
restart, or if the journal shows a panic:

```sh
# Grab the most recent .prev backup and reinstall it.
PREV=$(ls -t /usr/local/bin/flexctl.prev-* 2>/dev/null | head -1)
[ -n "$PREV" ] || { echo "no prev binary found"; exit 1; }

install -m 0755 "$PREV" /usr/local/bin/flexctl
systemctl restart flexctl
sleep 2
systemctl is-active flexctl

# Capture the failed binary for forensics before deleting.
mv /tmp/flexctl-new /tmp/flexctl-new.failed-$(date +%Y%m%d-%H%M)
```

Open a ticket pointing at `/tmp/flexctl-new.failed-*` and the journal
range; the binary that failed in the field is the most valuable
artifact we can have.

## Things to NOT do

- **Do not edit `/var/lib/flexctl/flexctl.db` directly** unless the
  service is stopped. Concurrent writes to a SQLite WAL by an external
  process can corrupt the file.
- **Do not docker-rm a `flexctl-*` container by hand** while the
  service is running; the reconciler will mark its env failed and
  release the GPU. If you want the env stopped, hit
  `POST /envs/<id>/stop` from the UI or use the future CLI subcommand.
- **Do not send the OAuth `client_secret` or the contents of any
  `oauth.env` over a non-tailnet channel.** Slack, email, and pasted
  screenshots are non-tailnet channels.
- **Do not skip the rollback drill.** A hotfix path that has never
  been rolled back is half a hotfix path.
