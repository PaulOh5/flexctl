#!/usr/bin/env bash
# Install flexctl on a freshly-imaged Ubuntu 22.04+ GPU host.
#
# Idempotent: safe to re-run after partial failure or for upgrades.
# Each step skips its work when it detects the desired state already
# exists. Re-running this script after a binary swap is also the
# documented hotfix procedure; see docs/HOTFIX.md.
#
# Usage:
#   sudo ./install.sh                    # full install
#   sudo ./install.sh verify             # check state, no changes
#   sudo ./install.sh --skip-prewarm     # skip the docker pull step
#
# Required preflight (manual, on shipping line):
#   - nvidia driver >= 535 already installed (`nvidia-smi -L` works)
#   - the flexctl binary sits next to this script (or at ../bin/flexctl)
#   - the flexctl.service unit is in the same dir
#
# After a successful install the OAuth wizard prompts interactively for
# Tailscale client credentials. To skip the wizard pass --no-oauth and
# write /etc/flexctl/oauth.env (mode 0640, root:flexctl) by hand.

set -euo pipefail

# --- knobs -------------------------------------------------------------------

FLEXCTL_BIN="/usr/local/bin/flexctl"
FLEXCTL_DATA="/var/lib/flexctl"
FLEXCTL_ETC="/etc/flexctl"
FLEXCTL_USER="flexctl"
FLEXCTL_GROUP="flexctl"
DRIVER_MIN="535"
PREWARM_IMAGES=(
    "nvidia/cuda:12.4.1-base-ubuntu22.04"
)

DO_PREWARM=1
DO_OAUTH=1
MODE="install"

while [ $# -gt 0 ]; do
    case "$1" in
        verify)            MODE="verify"; shift ;;
        --skip-prewarm)    DO_PREWARM=0; shift ;;
        --no-oauth)        DO_OAUTH=0; shift ;;
        -h|--help)
            sed -n '2,/^$/p' "$0" | sed 's/^# \?//'
            exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
done

# --- output helpers ----------------------------------------------------------

step() { printf '\n\033[1;36m[%s]\033[0m %s\n' "$1" "$2"; }
ok()   { printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[1;33m!\033[0m %s\n' "$*" >&2; }
fail() { printf '  \033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

run() {
    if [ "$MODE" = "verify" ]; then
        printf '  \033[2m[would]\033[0m %s\n' "$*"
        return 0
    fi
    "$@"
}

# --- step 0: must be root, sane environment ---------------------------------

step "0/12" "preflight"
[ "$EUID" -eq 0 ] || fail "must run as root (use sudo)"
[ -r /etc/os-release ] || fail "/etc/os-release missing"

# shellcheck disable=SC1091
. /etc/os-release
if [ "${ID:-}" != "ubuntu" ] || [ "${VERSION_ID%%.*}" -lt 22 ]; then
    fail "Ubuntu 22.04+ required; saw ${PRETTY_NAME:-unknown}"
fi
ok "OS: $PRETTY_NAME"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- step 1: nvidia driver --------------------------------------------------

step "1/12" "nvidia driver"
if ! command -v nvidia-smi >/dev/null 2>&1; then
    fail "nvidia driver not installed; install before running this script"
fi
DRIVER_VER="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null \
              | head -1 | awk -F. '{print $1}' || true)"
if [ -z "$DRIVER_VER" ] || [ "$DRIVER_VER" -lt "$DRIVER_MIN" ]; then
    fail "driver $DRIVER_VER < $DRIVER_MIN minimum; upgrade with apt"
fi
GPU_COUNT="$(nvidia-smi -L | wc -l)"
ok "driver $DRIVER_VER · $GPU_COUNT GPU(s)"

# --- step 2: docker engine --------------------------------------------------

step "2/12" "docker engine"
if ! command -v docker >/dev/null 2>&1; then
    run apt-get update -qq
    run apt-get install -y -qq docker.io
fi
ok "$(docker --version 2>/dev/null || echo 'docker would be installed')"

# --- step 3: nvidia-container-toolkit ---------------------------------------

step "3/12" "nvidia-container-toolkit"
if ! command -v nvidia-container-runtime >/dev/null 2>&1; then
    KEYRING="/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg"
    LIST="/etc/apt/sources.list.d/nvidia-container-toolkit.list"
    if [ ! -f "$KEYRING" ]; then
        run bash -c "curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
            | gpg --dearmor -o $KEYRING"
    fi
    if [ ! -f "$LIST" ]; then
        run bash -c "curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
            | sed 's#deb https://#deb [signed-by=$KEYRING] https://#g' > $LIST"
    fi
    run apt-get update -qq
    run apt-get install -y -qq nvidia-container-toolkit
fi
ok "nvidia-container-runtime present"

# --- step 4: docker daemon configuration -------------------------------------

step "4/12" "docker daemon (live-restore + nvidia runtime + overlay2)"
DAEMON="/etc/docker/daemon.json"
run mkdir -p "$(dirname "$DAEMON")"

# nvidia-ctk handles the runtimes block; we then layer additional keys
# via python so we don't shell-parse JSON.
needs_restart=0
if [ "$MODE" != "verify" ]; then
    if ! grep -q '"nvidia"' "$DAEMON" 2>/dev/null; then
        nvidia-ctk runtime configure --runtime=docker --config="$DAEMON" >/dev/null
        needs_restart=1
    fi
    python3 - "$DAEMON" <<'PY'
import json, os, sys
path = sys.argv[1]
try:
    with open(path) as f:
        cfg = json.load(f)
except FileNotFoundError:
    cfg = {}
desired = {
    "live-restore": True,
    "storage-driver": "overlay2",
    "log-driver": "journald",
}
changed = False
for k, v in desired.items():
    if cfg.get(k) != v:
        cfg[k] = v
        changed = True
if changed:
    with open(path, "w") as f:
        json.dump(cfg, f, indent=2)
        f.write("\n")
    print("daemon.json updated")
PY
    if [ -n "${needs_restart}" ] || [ -f "$DAEMON" ]; then
        run systemctl restart docker
    fi
fi
ok "daemon.json present"

# --- step 5: docker --gpus smoke test ---------------------------------------

step "5/12" "docker --gpus smoke"
if [ "$MODE" != "verify" ]; then
    if ! docker run --rm --gpus all "${PREWARM_IMAGES[0]}" nvidia-smi -L >/dev/null 2>&1; then
        fail "docker --gpus all failed; toolkit + daemon not wired correctly"
    fi
fi
ok "docker --gpus all reaches nvidia-smi"

# --- step 6: prewarm base images --------------------------------------------

if [ "$DO_PREWARM" = "1" ]; then
    step "6/12" "prewarm base images"
    for img in "${PREWARM_IMAGES[@]}"; do
        if [ "$MODE" != "verify" ]; then
            docker pull -q "$img" >/dev/null
        fi
        ok "$img"
    done
else
    step "6/12" "prewarm (skipped via --skip-prewarm)"
fi

# --- step 7: flexctl user + directories -------------------------------------

step "7/12" "flexctl user + /var/lib/flexctl + /etc/flexctl"
if ! getent group "$FLEXCTL_GROUP" >/dev/null; then
    run groupadd --system "$FLEXCTL_GROUP"
fi
if ! getent passwd "$FLEXCTL_USER" >/dev/null; then
    run useradd --system -g "$FLEXCTL_GROUP" \
        --home-dir "$FLEXCTL_DATA" --no-create-home \
        --shell /usr/sbin/nologin "$FLEXCTL_USER"
fi
run install -d -o "$FLEXCTL_USER" -g "$FLEXCTL_GROUP" -m 0750 "$FLEXCTL_DATA"
run install -d -o "$FLEXCTL_USER" -g "$FLEXCTL_GROUP" -m 0750 "$FLEXCTL_DATA/home"
run install -d -o root            -g "$FLEXCTL_GROUP" -m 0750 "$FLEXCTL_ETC"
# flexctl needs to call docker; SupplementaryGroups in the unit handles
# the runtime side, but the user must be in the docker group too so
# manual debug shells work.
if ! id -nG "$FLEXCTL_USER" | grep -qw docker; then
    run usermod -a -G docker "$FLEXCTL_USER"
fi
ok "$FLEXCTL_DATA, $FLEXCTL_ETC, $FLEXCTL_USER:docker"

# --- step 8: install binary --------------------------------------------------

step "8/12" "install flexctl binary"
SRC=""
for cand in "$SCRIPT_DIR/flexctl" "$SCRIPT_DIR/../bin/flexctl"; do
    [ -x "$cand" ] && SRC="$cand" && break
done
if [ -z "$SRC" ]; then
    fail "flexctl binary not found alongside install.sh; build with: go build -o ./bin/flexctl ./cmd/flexctl"
fi
run install -m 0755 "$SRC" "$FLEXCTL_BIN"
ok "$FLEXCTL_BIN -> $($SRC -h 2>&1 | head -1 || echo flexctl)"

# --- step 9: systemd unit ----------------------------------------------------

step "9/12" "systemd unit"
UNIT_SRC="$SCRIPT_DIR/flexctl.service"
[ -f "$UNIT_SRC" ] || fail "flexctl.service missing alongside install.sh"
run install -m 0644 "$UNIT_SRC" /etc/systemd/system/flexctl.service
run systemctl daemon-reload
ok "/etc/systemd/system/flexctl.service"

# --- step 10: OAuth wizard ---------------------------------------------------

step "10/12" "Tailscale OAuth"
OAUTH_FILE="$FLEXCTL_ETC/oauth.env"
if [ -f "$OAUTH_FILE" ] && grep -q FLEXCTL_TS_CLIENT_ID "$OAUTH_FILE" 2>/dev/null; then
    ok "already configured ($OAUTH_FILE)"
elif [ "$DO_OAUTH" = "0" ]; then
    warn "skipped (--no-oauth); place creds in $OAUTH_FILE later (see docs/SHIPPING.md)"
elif [ ! -t 0 ]; then
    warn "non-interactive shell; skipping wizard. Place creds in $OAUTH_FILE manually."
elif [ "$MODE" = "verify" ]; then
    warn "verify mode; would prompt for OAuth"
else
    cat <<EOM
  Create an OAuth client at https://login.tailscale.com/admin/settings/oauth
    - Required scope: auth_keys:write
    - Required tag:   tag:flexctl-user (must be allowed in tailnet ACL)
  Leave any field blank to skip; you can fill in $OAUTH_FILE later.
EOM
    read -rp "  Client ID:     " CLIENT_ID
    if [ -n "$CLIENT_ID" ]; then
        read -rsp "  Client secret: " CLIENT_SECRET; echo
        read -rp  "  Tailnet name (e.g. tail1234.ts.net), optional: " TAILNET
        umask 0077
        cat > "$OAUTH_FILE" <<EOF2
FLEXCTL_TS_CLIENT_ID=$CLIENT_ID
FLEXCTL_TS_CLIENT_SECRET=$CLIENT_SECRET
FLEXCTL_TAILNET=${TAILNET:-}
EOF2
        chown root:"$FLEXCTL_GROUP" "$OAUTH_FILE"
        chmod 0640 "$OAUTH_FILE"
        ok "saved to $OAUTH_FILE"
    else
        warn "no creds entered; env creation will fail until $OAUTH_FILE is filled in"
    fi
fi

# --- step 11: enable + start -------------------------------------------------

step "11/12" "enable + start flexctl service"
if [ "$MODE" != "verify" ]; then
    systemctl enable flexctl >/dev/null
    systemctl restart flexctl
    # Give the binary up to 5s to bind its listener.
    for _ in 1 2 3 4 5; do
        if systemctl is-active --quiet flexctl; then break; fi
        sleep 1
    done
    if ! systemctl is-active --quiet flexctl; then
        warn "service not active; recent logs:"
        journalctl -u flexctl -n 30 --no-pager >&2
        fail "flexctl failed to start"
    fi
    PID="$(systemctl show -p MainPID flexctl --value)"
    ok "flexctl running (pid $PID)"
else
    ok "would enable + start"
fi

# --- step 12: post-install smoke --------------------------------------------

step "12/12" "post-install smoke (/healthz)"
if [ "$MODE" != "verify" ]; then
    sleep 1
    body="$(curl -fsS --max-time 3 http://127.0.0.1:8080/healthz || true)"
    if echo "$body" | grep -q '"db_ok":true'; then
        ok "/healthz: $body"
    else
        warn "/healthz did not return db_ok=true: $body"
    fi
fi

# --- summary ----------------------------------------------------------------

cat <<EOM

  flexctl installed.

  Web UI:    http://$(hostname -I | awk '{print $1}'):8080
             (front this with a tailnet ACL — see docs/SHIPPING.md)
  Logs:      journalctl -u flexctl -f
  Binary:    $FLEXCTL_BIN
  Data:      $FLEXCTL_DATA
  Config:    $FLEXCTL_ETC/oauth.env

  Acceptance: docs/SHIPPING.md
  Hotfix SOP: docs/HOTFIX.md
  Sales demo: docs/DEMO.md

EOM
