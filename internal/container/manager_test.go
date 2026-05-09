package container

import (
	"context"
	"os/exec"
	"testing"
)

// TestSpecValidation runs without Docker and exercises the cheap
// preflight in StartPair so misuse fails before we touch the daemon.
func TestSpecValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*EnvSpec)
		want string
	}{
		{"missing env id", func(s *EnvSpec) { s.EnvID = "" }, "EnvID"},
		{"missing hostname", func(s *EnvSpec) { s.TailscaleHostname = "" }, "TailscaleHostname"},
		{"missing auth key", func(s *EnvSpec) { s.TailscaleAuthKey = "" }, "TailscaleAuthKey"},
		{"missing image", func(s *EnvSpec) { s.WorkImage = "" }, "WorkImage"},
		{"missing gpu", func(s *EnvSpec) { s.GPUUUID = "" }, "GPUUUID"},
		{"low uid", func(s *EnvSpec) { s.HostUID = 500 }, "HostUID"},
		{"missing home", func(s *EnvSpec) { s.HostHomeDir = "" }, "HostHomeDir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sampleSpec()
			tc.mut(&s)
			if err := s.validate(); err == nil || !contains(err.Error(), tc.want) {
				t.Errorf("validate(): got %v, want error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestManagerCanReachDocker is a smoke test that skips when the docker
// CLI is unavailable. It verifies the no-such-container branch of
// FindPairByEnvID without mutating any state.
func TestManagerCanReachDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not on PATH")
	}
	if err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}

	m := NewManager(nil)
	p, err := m.FindPairByEnvID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Errorf("find: %v", err)
	}
	if p.SidecarID != "" || p.WorkID != "" {
		t.Errorf("expected empty pair for non-existent env, got %+v", p)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
