// Package container places the per-user pair on the host and watches
// it for the reconciler.
//
// Each environment is two containers sharing one network namespace:
//
//	flexctl-ts-<envID>    Tailscale identity (NET_ADMIN, /dev/net/tun,
//	                      ephemeral auth key, no GPU, no shell shipped
//	                      to users).
//	flexctl-work-<envID>  ML image (nvidia/cuda or whatever the user
//	                      picked), --gpus pointing at one GPU UUID,
//	                      sshd installed at runtime so external users
//	                      land here when they ssh through the tailnet.
//
// We talk to dockerd by shelling out to the `docker` CLI. The SDK's
// transitive deps fight with each other across versions; the CLI is a
// stable interface and trivial to debug (every command we issue can be
// pasted into a shell). For Phase 1 single-node scale, the per-call
// fork+exec cost is irrelevant.
//
// This file is the pure argv-building half. manager.go uses these
// builders to talk to dockerd via os/exec.
package container

import (
	"fmt"
	"strings"
)

// EnvSpec is everything the manager needs to materialize one pair.
type EnvSpec struct {
	// EnvID is the environments.id. Used as a stable suffix for
	// container names so a crash/restart can rediscover the pair.
	EnvID string

	// TailscaleHostname is the name the node advertises in the tailnet
	// (also the magic-DNS prefix). Stable per env.
	TailscaleHostname string

	// TailscaleAuthKey is a single-use, ephemeral auth key minted by
	// the tailscale package right before StartPair. We never persist
	// this key; it is consumed once on tailscaled boot inside the
	// sidecar.
	TailscaleAuthKey string

	// WorkImage is the user's chosen ML image (e.g.
	// "nvidia/cuda:12.4.1-base-ubuntu22.04"). Must be pre-pullable.
	WorkImage string

	// GPUUUID is the NVIDIA UUID assigned by the scheduler. We bind
	// the container to this exact GPU so a host reboot's index reorder
	// does not leak access to a different device.
	GPUUUID string

	// HostUID is the per-user host UID (10001+N). Used as the work
	// container's runtime user and as the owner of the host home
	// volume mount.
	HostUID int

	// HostHomeDir is the path on the host that backs /root inside the
	// work container. The reconciler is responsible for ensuring it
	// exists with the right ownership before StartPair.
	HostHomeDir string

	// AuthorizedKey, when non-empty, is written to /root/.ssh/authorized_keys
	// inside the work container. When empty the work container falls
	// back to passwordless root login (PoC only — production must set
	// this).
	AuthorizedKey string

	// TailscaleImage and SidecarUserspace let tests/the host override
	// the defaults. Production leaves them zero-valued.
	TailscaleImage   string
	SidecarUserspace bool
}

// SidecarName returns the deterministic Docker name for the Tailscale
// sidecar of envID.
func SidecarName(envID string) string {
	return "flexctl-ts-" + shortID(envID)
}

// WorkName returns the deterministic Docker name for the work container
// of envID.
func WorkName(envID string) string {
	return "flexctl-work-" + shortID(envID)
}

// shortID compresses a full UUID to 12 chars. UUIDs share the first 8
// hex chars rarely enough for our scale (5 users) that 12 is plenty.
func shortID(envID string) string {
	clean := strings.ReplaceAll(envID, "-", "")
	if len(clean) >= 12 {
		return clean[:12]
	}
	return clean
}

// sidecarImage returns the Tailscale image to use, defaulting to
// :stable.
func sidecarImage(spec EnvSpec) string {
	if spec.TailscaleImage != "" {
		return spec.TailscaleImage
	}
	return "tailscale/tailscale:stable"
}

// SidecarRunArgs returns the argv (after the leading "docker") for
// creating and starting the Tailscale sidecar.
//
// Important details:
//   - kernel TUN mode by default (TS_USERSPACE=false), so apps in the
//     shared netns bind transparently to the tailnet IP.
//   - --ssh NOT enabled: tailscale ssh would steal :22 from the work
//     container's sshd; we want plain ssh into the work container.
//   - --accept-dns=false keeps tailnet DNS off the work container's
//     resolv.conf, which would otherwise break apt-get during the
//     work entrypoint.
func SidecarRunArgs(spec EnvSpec) []string {
	userspace := "false"
	if spec.SidecarUserspace {
		userspace = "true"
	}
	return []string{
		"run", "-d",
		"--name", SidecarName(spec.EnvID),
		"--hostname", spec.TailscaleHostname,
		"--cap-add=NET_ADMIN",
		"--device=/dev/net/tun:/dev/net/tun",
		"--restart=unless-stopped",
		"--label", "com.flexctl.env=" + spec.EnvID,
		"--label", "com.flexctl.role=tailscale-sidecar",
		"-e", "TS_AUTHKEY=" + spec.TailscaleAuthKey,
		"-e", "TS_HOSTNAME=" + spec.TailscaleHostname,
		"-e", "TS_USERSPACE=" + userspace,
		"-e", "TS_STATE_DIR=/var/lib/tailscale",
		"-e", "TS_EXTRA_ARGS=--accept-dns=false",
		sidecarImage(spec),
	}
}

// WorkRunArgs returns the argv for creating and starting the work
// container. The container CMD installs openssh-server at runtime and
// runs sshd in the foreground so external `ssh` lands here.
//
// Hardening (Eng review #5):
//   - --security-opt no-new-privileges
//   - Default seccomp + AppArmor (we just don't override them).
//   - Future: userns-remap is daemon-level; the install script enables
//     it on the host.
//
// GPU pinning is belt-and-suspenders:
//   - --gpus device=<UUID> tells nvidia-container-toolkit which device
//   - NVIDIA_VISIBLE_DEVICES=<UUID> reinforces it as an env var
//
// Both reference UUID, never index, so a host reboot's device reorder
// can't leak access to the wrong device.
func WorkRunArgs(spec EnvSpec) []string {
	// NOTE: do NOT pass --hostname here. Docker rejects --hostname when
	// combined with `--network container:<name>` because the joining
	// container does not own its network namespace (and therefore not
	// its hostname); the sidecar already set TailscaleHostname when it
	// joined the tailnet, and the work container inherits it via the
	// shared netns. Adding --hostname here gets you:
	//   docker: Error response from daemon: conflicting options:
	//   hostname and the network mode
	// — which is exit 125 at create time and a FAILED env in the UI.
	return []string{
		"run", "-d",
		"--name", WorkName(spec.EnvID),
		"--network", "container:" + SidecarName(spec.EnvID),
		"--gpus", "device=" + spec.GPUUUID,
		"--security-opt=no-new-privileges:true",
		"--restart=unless-stopped",
		"--label", "com.flexctl.env=" + spec.EnvID,
		"--label", "com.flexctl.role=work",
		"-v", spec.HostHomeDir + ":/root:rw",
		"-e", "DEBIAN_FRONTEND=noninteractive",
		"-e", "NVIDIA_VISIBLE_DEVICES=" + spec.GPUUUID,
		"-e", "NVIDIA_DRIVER_CAPABILITIES=compute,utility",
		"-e", "FLEXCTL_AUTHORIZED_KEY=" + spec.AuthorizedKey,
		"-e", fmt.Sprintf("FLEXCTL_HOST_UID=%d", spec.HostUID),
		spec.WorkImage,
		"bash", "-c", workEntrypoint,
	}
}

// workEntrypoint installs sshd inside the work container on first run
// and execs it in the foreground. Idempotent: dpkg detects already-
// installed packages, the authorized_keys file is rewritten each start.
const workEntrypoint = `
set -eo pipefail
if ! command -v sshd >/dev/null 2>&1; then
  apt-get update -qq >/dev/null 2>&1
  apt-get install -y -qq --no-install-recommends openssh-server >/dev/null 2>&1
fi
mkdir -p /run/sshd /root/.ssh /etc/ssh/sshd_config.d
chmod 700 /root/.ssh
if [ -n "${FLEXCTL_AUTHORIZED_KEY:-}" ]; then
  printf "%s\n" "$FLEXCTL_AUTHORIZED_KEY" > /root/.ssh/authorized_keys
  chmod 600 /root/.ssh/authorized_keys
  printf "PermitRootLogin prohibit-password\n" > /etc/ssh/sshd_config.d/flexctl.conf
else
  passwd -d root
  printf "PermitRootLogin yes\nPermitEmptyPasswords yes\n" > /etc/ssh/sshd_config.d/flexctl.conf
fi
exec /usr/sbin/sshd -D -e
`
