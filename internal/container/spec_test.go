package container

import (
	"strings"
	"testing"
)

func sampleSpec() EnvSpec {
	return EnvSpec{
		EnvID:             "11111111-2222-3333-4444-555555555555",
		TailscaleHostname: "alice-flexctl",
		TailscaleAuthKey:  "tskey-auth-FAKE-DO-NOT-COMMIT",
		WorkImage:         "nvidia/cuda:12.4.1-base-ubuntu22.04",
		GPUUUID:           "GPU-deadbeef-0000-0000-0000-aabbccddeeff",
		HostUID:           10001,
		HostHomeDir:       "/var/lib/flexctl/home/alice",
		AuthorizedKey:     "ssh-ed25519 AAAA...",
	}
}

func TestNamesAreDeterministicAndShort(t *testing.T) {
	s := sampleSpec()
	ts := SidecarName(s.EnvID)
	work := WorkName(s.EnvID)

	if ts != "flexctl-ts-111111112222" {
		t.Errorf("sidecar name = %s", ts)
	}
	if work != "flexctl-work-111111112222" {
		t.Errorf("work name = %s", work)
	}
	if len(ts) > 63 || len(work) > 63 {
		t.Errorf("docker max name length is 63: ts=%d work=%d", len(ts), len(work))
	}
}

func TestSidecarRunArgsKernelTUNByDefault(t *testing.T) {
	args := SidecarRunArgs(sampleSpec())

	mustContain(t, args, "--cap-add=NET_ADMIN")
	mustContain(t, args, "--device=/dev/net/tun:/dev/net/tun")
	mustHaveEnv(t, args, "TS_USERSPACE=false")
	mustHaveEnv(t, args, "TS_AUTHKEY=tskey-auth-FAKE-DO-NOT-COMMIT")
	mustHaveEnv(t, args, "TS_HOSTNAME=alice-flexctl")
	mustHaveEnv(t, args, "TS_EXTRA_ARGS=--accept-dns=false")

	// Sanity: hostname flag points to the right name.
	hostFlag := getFlag(args, "--hostname")
	if hostFlag != "alice-flexctl" {
		t.Errorf("--hostname = %q, want alice-flexctl", hostFlag)
	}

	// Crucially, no --ssh: that would steal :22 from the work container.
	for _, a := range args {
		if strings.Contains(a, "--ssh") {
			t.Errorf("sidecar must not enable --ssh (steals :22 from work): saw %q", a)
		}
	}
}

func TestSidecarUserspaceOverride(t *testing.T) {
	s := sampleSpec()
	s.SidecarUserspace = true
	mustHaveEnv(t, SidecarRunArgs(s), "TS_USERSPACE=true")
}

// Regression: Docker rejects --hostname when combined with
// `--network container:<sidecar>` (exit 125 at create time). The
// sidecar already set the hostname when it joined the tailnet; the
// work container shares its netns and must not try to set a separate
// one.
func TestWorkRunArgsHasNoHostnameFlag(t *testing.T) {
	args := WorkRunArgs(sampleSpec())
	for i, a := range args {
		if a == "--hostname" {
			t.Errorf("argv must not contain --hostname (it conflicts with --network container:): saw at %d, full argv: %v", i, args)
		}
	}
}

func TestWorkRunArgsSharesNamespaceAndPinsGPU(t *testing.T) {
	s := sampleSpec()
	args := WorkRunArgs(s)

	if got := getFlag(args, "--network"); got != "container:flexctl-ts-111111112222" {
		t.Errorf("--network = %q, want container:<sidecar>", got)
	}
	if got := getFlag(args, "--gpus"); got != "device="+s.GPUUUID {
		t.Errorf("--gpus = %q, want device=%s", got, s.GPUUUID)
	}
	mustHaveEnv(t, args, "NVIDIA_VISIBLE_DEVICES="+s.GPUUUID)
	mustHaveEnv(t, args, "FLEXCTL_AUTHORIZED_KEY=ssh-ed25519 AAAA...")
	mustHaveEnv(t, args, "FLEXCTL_HOST_UID=10001")
	mustContain(t, args, "--security-opt=no-new-privileges:true")

	// Bind mount points work container's /root at the user's host home.
	bind := getFlag(args, "-v")
	if bind != "/var/lib/flexctl/home/alice:/root:rw" {
		t.Errorf("-v = %q", bind)
	}

	// Last three args should be: image, "bash", "-c", entrypoint.
	if len(args) < 4 {
		t.Fatalf("argv too short: %v", args)
	}
	tail := args[len(args)-4:]
	if tail[0] != s.WorkImage || tail[1] != "bash" || tail[2] != "-c" {
		t.Errorf("argv tail = %v, want [image bash -c entrypoint]", tail)
	}
	if !strings.Contains(tail[3], "exec /usr/sbin/sshd") {
		t.Errorf("entrypoint must end with sshd; got: %s", tail[3])
	}
}

func TestWorkEntrypointIdempotentAndAuthAware(t *testing.T) {
	if !strings.Contains(workEntrypoint, "command -v sshd") {
		t.Errorf("entrypoint should skip apt-get when sshd already installed")
	}
	if !strings.Contains(workEntrypoint, "FLEXCTL_AUTHORIZED_KEY") {
		t.Errorf("entrypoint must read FLEXCTL_AUTHORIZED_KEY env to install authorized_keys")
	}
	if !strings.Contains(workEntrypoint, "PermitEmptyPasswords yes") {
		t.Errorf("entrypoint should fall back to passwordless root when no key set")
	}
	if !strings.Contains(workEntrypoint, "exec /usr/sbin/sshd -D") {
		t.Errorf("entrypoint must exec sshd in foreground (PID 1)")
	}
}

// mustContain fails the test if argv does not contain literal target.
func mustContain(t *testing.T, args []string, target string) {
	t.Helper()
	for _, a := range args {
		if a == target {
			return
		}
	}
	t.Errorf("argv missing %q\nargv: %v", target, args)
}

// mustHaveEnv asserts there is an "-e VAR=val" pair matching target.
func mustHaveEnv(t *testing.T, args []string, target string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" && args[i+1] == target {
			return
		}
	}
	t.Errorf("argv missing -e %q\nargv: %v", target, args)
}

// getFlag returns the value following the first occurrence of flag in
// args, or "" if absent. Handles only space-separated form (--flag val).
func getFlag(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}
