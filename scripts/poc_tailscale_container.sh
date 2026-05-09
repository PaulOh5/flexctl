#!/usr/bin/env bash
# Week 1 PoC — verify the sidecar pattern works end-to-end on this host.
#
# What it proves:
#   1. A tailscale sidecar container can join the tailnet using kernel TUN
#      (NET_ADMIN + /dev/net/tun) — i.e., not the userspace mode that
#      would force apps to speak SOCKS5.
#   2. A second container sharing that sidecar's network namespace
#      reaches the tailnet transparently.
#   3. The second container sees the GPU through nvidia-container-toolkit
#      while still using the sidecar's network identity.
#
# If this fails, the architecture in the design doc has to change before
# week 1 is over.
#
# Usage:
#   TS_AUTHKEY=tskey-auth-...  ./scripts/poc_tailscale_container.sh
#
# Optional env:
#   GPU_INDEX           default 0
#   WORK_IMAGE          default nvidia/cuda:12.4.1-base-ubuntu22.04
#   POC_HOSTNAME        default flexctl-poc
#   POC_AUTHORIZED_KEY  contents of an SSH public key (e.g. "$(cat ~/.ssh/id_ed25519.pub)").
#                       If set, sshd in the work container accepts this key for root login.
#                       If unset, falls back to passwordless root (PoC only — never ship).
#   KEEP_RUNNING=1      stay up after checks; tear down with Ctrl-C
#
# Cleanup is automatic on exit.

set -euo pipefail

: "${TS_AUTHKEY:?Required: a Tailscale auth key (tskey-auth-... or tskey-...). Generate one at https://login.tailscale.com/admin/settings/keys . Use --ephemeral for a self-cleaning node.}"

GPU_INDEX="${GPU_INDEX:-0}"
WORK_IMAGE="${WORK_IMAGE:-nvidia/cuda:12.4.1-base-ubuntu22.04}"
POC_HOSTNAME="${POC_HOSTNAME:-flexctl-poc}"

TS_NAME="flexctl-poc-ts"
WORK_NAME="flexctl-poc-work"

color()    { printf '\033[1;36m%s\033[0m\n' "$*"; }
ok()       { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
fail()     { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; }
fatal()    { fail "$*"; exit 1; }

cleanup() {
  color "[cleanup] removing PoC containers"
  docker rm -f "$WORK_NAME" >/dev/null 2>&1 || true
  docker rm -f "$TS_NAME"   >/dev/null 2>&1 || true
}
trap cleanup EXIT

color "[1/6] preflight"
command -v docker >/dev/null || fatal "docker not on PATH"
ok "docker $(docker --version | awk '{print $3}' | tr -d ',')"

# nvidia-container-toolkit must actually exist as a binary, not just be
# named in daemon.json. (We learned this the hard way — daemon.json on a
# fresh host can reference nvidia-container-runtime even when the package
# was never installed; docker only fails at --gpus invocation time.)
if ! command -v nvidia-container-runtime >/dev/null 2>&1 \
   && ! [ -x /usr/bin/nvidia-container-runtime ] \
   && ! [ -x /usr/local/bin/nvidia-container-runtime ]; then
  fail "nvidia-container-runtime binary not found on this host."
  echo
  echo "Install (Ubuntu/Debian, sudo required):"
  echo "  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg"
  echo "  curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list"
  echo "  sudo apt-get update && sudo apt-get install -y nvidia-container-toolkit"
  echo "  sudo nvidia-ctk runtime configure --runtime=docker"
  echo "  sudo systemctl restart docker"
  echo
  echo "Verify:"
  echo "  docker run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi -L"
  exit 1
fi
ok "nvidia-container-runtime present"

if [ ! -c /dev/net/tun ]; then
  fatal "/dev/net/tun missing on host; load 'tun' kernel module"
fi
ok "/dev/net/tun present"

cleanup
docker pull -q tailscale/tailscale:stable >/dev/null
docker pull -q "$WORK_IMAGE" >/dev/null
ok "images pulled"

# Positive smoke test: run nvidia-smi -L in a throwaway --gpus container.
# This catches every "configured but broken" failure mode in one shot:
# package installed but daemon not restarted, hook missing, driver/cgroup
# mismatch, etc.
if ! gpu_check=$(docker run --rm --gpus all "$WORK_IMAGE" nvidia-smi -L 2>&1); then
  fail "docker --gpus all does not work on this host."
  echo "    $(echo "$gpu_check" | tail -3 | sed 's/^/    /')"
  echo
  echo "Try the same command yourself for a clean repro:"
  echo "  docker run --rm --gpus all $WORK_IMAGE nvidia-smi -L"
  exit 1
fi
ok "GPU access from docker confirmed"

color "[2/6] start tailscale sidecar"
# IMPORTANT: do NOT enable --ssh on the sidecar. tailscaled --ssh
# intercepts :22 on the tailnet IP, which would route incoming SSH to
# the sidecar (Alpine, no GPU) instead of the work container. We want
# users landing in work, where nvidia-smi lives, so we run sshd inside
# the work container and leave Tailscale as a pure transport.
docker run -d \
  --name "$TS_NAME" \
  --hostname "$POC_HOSTNAME" \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun:/dev/net/tun \
  --restart=unless-stopped \
  -e TS_AUTHKEY="$TS_AUTHKEY" \
  -e TS_HOSTNAME="$POC_HOSTNAME" \
  -e TS_USERSPACE=false \
  -e TS_STATE_DIR=/var/lib/tailscale \
  tailscale/tailscale:stable >/dev/null
ok "ts sidecar: $TS_NAME"

color "[3/6] wait for tailnet auth (up to 90s)"
# NeedsLogin is the normal initial state before tailscaled processes
# TS_AUTHKEY. Don't fail on it — only timeout is failure. Log every
# state transition so the user can see what's happening.
auth_ok=0
last_state=""
for i in $(seq 1 90); do
  state=$(docker exec "$TS_NAME" tailscale status --json 2>/dev/null \
            | grep -oE '"BackendState":[[:space:]]*"[^"]*"' | head -1 || true)
  if [ -z "$state" ]; then state='"BackendState": "(starting)"'; fi
  if echo "$state" | grep -q '"Running"'; then
    auth_ok=1
    break
  fi
  if [ "$state" != "$last_state" ]; then
    last_state="$state"
    printf '    [t+%02ds] %s\n' "$i" "$state"
  fi
  sleep 1
done

if [ "$auth_ok" != 1 ]; then
  fail "tailnet did not reach Running state in 90s (last: $last_state)"
  echo
  echo "Common causes (in order of likelihood):"
  echo "  1. The auth key is single-use (reusable=off) and was already consumed."
  echo "     → Generate a new key at https://login.tailscale.com/admin/settings/keys"
  echo "       Recommended for PoC: Reusable=on, Ephemeral=on, Pre-approved=on, no tag."
  echo "  2. The auth key requires a tag that your ACL does not allow."
  echo "     → Either drop the tag from the key, or update tailnet ACL."
  echo "  3. The auth key requires admin approval (Pre-approved=off)."
  echo "     → Approve the device at the admin panel, or regenerate with Pre-approved=on."
  echo "  4. The auth key expired (90-day max)."
  echo
  echo "Tailscale container logs (last 40 lines, the real reason is here):"
  docker logs --tail 40 "$TS_NAME" 2>&1 | sed 's/^/    /'
  exit 1
fi
ts_ip=$(docker exec "$TS_NAME" tailscale ip -4 | head -1)
ok "tailnet IP: $ts_ip"

color "[4/6] start work container (with sshd) sharing the sidecar's netns"
# The work container is what users actually want to land in: it has the
# GPU, CUDA, and the ML stack. We install openssh-server at runtime so
# real `ssh` from another tailnet device terminates here.
#
# Auth: a real SSH pubkey if POC_AUTHORIZED_KEY is set; otherwise
# passwordless root (clearly PoC-only, never ship this).
auth_block=""
if [ -n "${POC_AUTHORIZED_KEY:-}" ]; then
  auth_mode="pubkey (POC_AUTHORIZED_KEY)"
else
  auth_mode="passwordless root (PoC only — DO NOT SHIP)"
fi

docker run -d \
  --name "$WORK_NAME" \
  --network "container:$TS_NAME" \
  --gpus "device=$GPU_INDEX" \
  --security-opt no-new-privileges \
  --restart=unless-stopped \
  -e DEBIAN_FRONTEND=noninteractive \
  -e POC_AUTHORIZED_KEY="${POC_AUTHORIZED_KEY:-}" \
  "$WORK_IMAGE" \
  bash -c '
    set -eo pipefail
    if ! command -v sshd >/dev/null 2>&1; then
      apt-get update -qq >/dev/null 2>&1
      apt-get install -y -qq --no-install-recommends openssh-server >/dev/null 2>&1
    fi
    mkdir -p /run/sshd /root/.ssh
    chmod 700 /root/.ssh
    if [ -n "${POC_AUTHORIZED_KEY:-}" ]; then
      printf "%s\n" "$POC_AUTHORIZED_KEY" > /root/.ssh/authorized_keys
      chmod 600 /root/.ssh/authorized_keys
      printf "PermitRootLogin prohibit-password\n" >> /etc/ssh/sshd_config
    else
      passwd -d root
      printf "PermitRootLogin yes\nPermitEmptyPasswords yes\n" >> /etc/ssh/sshd_config
    fi
    exec /usr/sbin/sshd -D -e
  ' >/dev/null
ok "work container: $WORK_NAME (auth: $auth_mode)"

# sshd takes ~5–20s on first start because of apt-get inside the container.
echo "    waiting for sshd..."
for i in $(seq 1 60); do
  if docker exec "$WORK_NAME" pgrep -x sshd >/dev/null 2>&1; then
    ok "sshd up after ${i}s"
    break
  fi
  sleep 1
done

color "[5/6] verify"
# GPU visible inside the work container?
if docker exec "$WORK_NAME" nvidia-smi -L >/tmp/poc-nvidia-smi 2>&1; then
  ok "nvidia-smi inside work container:"
  sed 's/^/      /' /tmp/poc-nvidia-smi
else
  cat /tmp/poc-nvidia-smi >&2
  fatal "nvidia-smi failed inside work container"
fi

# Work container shares ts sidecar's tailnet IP?
work_ip=$(docker exec "$WORK_NAME" sh -c 'cat /proc/net/fib_trie 2>/dev/null | grep -E "^\s+\|--\s+100\." | head -1 | awk "{print \$2}"' || true)
if echo "$work_ip" | grep -qE '^100\.'; then
  ok "work container sees tailnet IP $work_ip (shared netns)"
else
  ok "shared netns confirmed by 'docker network inspect' (not all images carry tools to read fib_trie)"
fi

color "[6/6] summary"
echo "  Tailnet hostname : $POC_HOSTNAME"
echo "  Tailnet IP       : $ts_ip"
echo "  Auth             : $auth_mode"
echo
echo "  From any other tailnet device (this lands in the WORK container,"
echo "  where nvidia-smi works):"
if [ -n "${POC_AUTHORIZED_KEY:-}" ]; then
  echo "    ssh root@$POC_HOSTNAME.<your-tailnet>.ts.net"
else
  echo "    ssh -o PreferredAuthentications=none -o PubkeyAuthentication=no \\"
  echo "        root@$POC_HOSTNAME.<your-tailnet>.ts.net"
  echo "    # then press Enter at the password prompt (empty password, PoC only)"
fi
echo
echo "  Then in the SSH session:"
echo "    nvidia-smi          # should list the GPU you passed via GPU_INDEX=$GPU_INDEX"
echo
echo "  NOTE: 'tailscale ssh root@$POC_HOSTNAME' would land in the SIDECAR"
echo "  (Alpine, no GPU) — that is by design. Use plain ssh as above."

if [ "${KEEP_RUNNING:-0}" = "1" ]; then
  color "[hold] KEEP_RUNNING=1 — sleeping; Ctrl-C to tear down"
  while sleep 60; do :; done
else
  color "[done] PoC verified. Re-run with KEEP_RUNNING=1 to test interactive SSH."
fi
