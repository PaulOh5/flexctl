#!/usr/bin/env bash
# Remove flexctl from this host. Useful for re-imaging the same box
# during shipping-line testing, or for clean tear-down before returning
# a customer demo unit.
#
# By default we keep customer data: SQLite DB, per-user home volumes,
# and the OAuth env file are NOT touched. Add --purge to also wipe
# /var/lib/flexctl and /etc/flexctl.
#
# Usage:
#   sudo ./uninstall.sh           # stop service, remove binary + unit
#   sudo ./uninstall.sh --purge   # also remove data and OAuth creds
#
# We intentionally leave docker, nvidia-container-toolkit, and the
# nvidia driver in place. They were probably installed for other
# reasons and are not flexctl-specific.

set -euo pipefail

PURGE=0
case "${1:-}" in
    --purge) PURGE=1 ;;
    "") ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
esac

[ "$EUID" -eq 0 ] || { echo "must run as root"; exit 1; }

ok()   { printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[1;33m!\033[0m %s\n' "$*" >&2; }
step() { printf '\n\033[1;36m%s\033[0m\n' "$1"; }

step "stop service"
if systemctl list-unit-files flexctl.service >/dev/null 2>&1; then
    systemctl disable --now flexctl 2>/dev/null || true
    rm -f /etc/systemd/system/flexctl.service
    systemctl daemon-reload
    ok "service removed"
else
    ok "service not installed"
fi

step "remove binary"
if [ -e /usr/local/bin/flexctl ]; then
    rm -f /usr/local/bin/flexctl
    ok "binary removed"
fi

if [ "$PURGE" = "1" ]; then
    step "purge data + creds"
    # Stop containers we created so volumes are not held.
    if command -v docker >/dev/null 2>&1; then
        for c in $(docker ps -aq --filter "label=com.flexctl.env" 2>/dev/null); do
            docker rm -f "$c" >/dev/null 2>&1 || true
        done
        ok "flexctl-labeled containers removed"
    fi
    rm -rf /var/lib/flexctl /etc/flexctl
    if getent passwd flexctl >/dev/null; then
        userdel flexctl 2>/dev/null || true
    fi
    if getent group flexctl >/dev/null; then
        groupdel flexctl 2>/dev/null || true
    fi
    ok "data and user removed"
else
    warn "data preserved at /var/lib/flexctl and /etc/flexctl (use --purge to remove)"
fi

echo
echo "  flexctl uninstalled."
