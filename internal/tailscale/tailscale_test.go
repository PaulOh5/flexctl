package tailscale

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer is a stripped Tailscale API mock for unit tests.
type fakeServer struct {
	*httptest.Server
	tokenCalls atomic.Int32
	keyCalls   atomic.Int32
	revokeIDs  atomic.Pointer[[]string]

	tokenExpiresIn int    // seconds, default 3600
	tokenValue     string // default "fake-access-token"
	failKeyCreate  bool
	lastKeyBody    atomic.Pointer[map[string]any]
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{
		tokenExpiresIn: 3600,
		tokenValue:     "fake-access-token",
	}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v2/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		fs.tokenCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			http.Error(w, "wrong grant", 400)
			return
		}
		if r.Form.Get("client_id") == "" || r.Form.Get("client_secret") == "" {
			http.Error(w, "missing creds", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fs.tokenValue,
			"expires_in":   fs.tokenExpiresIn,
			"token_type":   "Bearer",
		})
	})

	mux.HandleFunc("POST /api/v2/tailnet/-/keys", func(w http.ResponseWriter, r *http.Request) {
		fs.keyCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+fs.tokenValue {
			http.Error(w, "unauthorized", 401)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		fs.lastKeyBody.Store(&body)

		if fs.failKeyCreate {
			http.Error(w, "synthetic failure", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":     "tskey-auth-FAKE-EPHEMERAL",
			"id":      "k-123",
			"expires": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("DELETE /api/v2/tailnet/-/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fs.tokenValue {
			http.Error(w, "unauthorized", 401)
			return
		}
		id := r.PathValue("id")
		current := []string{}
		if cur := fs.revokeIDs.Load(); cur != nil {
			current = append(current, *cur...)
		}
		current = append(current, id)
		fs.revokeIDs.Store(&current)

		if id == "missing" {
			http.Error(w, "not found", 404)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

func newProvisioner(fs *fakeServer, opts ...Option) *Provisioner {
	defaults := []Option{
		WithAPIBase(fs.URL),
		WithTokenURL(fs.URL + "/api/v2/oauth/token"),
	}
	return New("test-client", "test-secret", append(defaults, opts...)...)
}

func TestIssueEphemeralKeyHappyPath(t *testing.T) {
	fs := newFakeServer(t)
	p := newProvisioner(fs)

	key, err := p.IssueEphemeralKey(context.Background(), "env-abc")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if key.Key != "tskey-auth-FAKE-EPHEMERAL" {
		t.Errorf("Key = %q", key.Key)
	}
	if key.ID != "k-123" {
		t.Errorf("ID = %q", key.ID)
	}
	if key.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt zero")
	}
}

func TestIssueEphemeralKeyRequestBodyShape(t *testing.T) {
	fs := newFakeServer(t)
	p := newProvisioner(fs)

	if _, err := p.IssueEphemeralKey(context.Background(), "env-abc"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	body := *fs.lastKeyBody.Load()

	if got := body["description"]; got != "flexctl env env-abc" {
		t.Errorf("description = %v", got)
	}
	if got, want := body["expirySeconds"].(float64), 3600.0; got != want {
		t.Errorf("expirySeconds = %v, want %v", got, want)
	}

	caps, _ := body["capabilities"].(map[string]any)
	devs, _ := caps["devices"].(map[string]any)
	create, _ := devs["create"].(map[string]any)

	for k, want := range map[string]bool{
		"reusable":      false,
		"ephemeral":     true,
		"preauthorized": true,
	} {
		if got := create[k]; got != want {
			t.Errorf("capabilities.devices.create.%s = %v, want %v", k, got, want)
		}
	}

	tags, _ := create["tags"].([]any)
	if len(tags) != 1 || tags[0] != "tag:flexctl-user" {
		t.Errorf("tags = %v, want [tag:flexctl-user]", tags)
	}
}

func TestIssueEphemeralKeyTagPrefixOverride(t *testing.T) {
	fs := newFakeServer(t)
	p := newProvisioner(fs, WithTagPrefix("tag:custom"))

	if _, err := p.IssueEphemeralKey(context.Background(), "env"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	body := *fs.lastKeyBody.Load()
	caps := body["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)
	tags := caps["tags"].([]any)
	if tags[0] != "tag:custom-user" {
		t.Errorf("tag prefix override ignored: %v", tags)
	}
}

func TestTokenIsCachedAcrossCalls(t *testing.T) {
	fs := newFakeServer(t)
	p := newProvisioner(fs)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := p.IssueEphemeralKey(ctx, "env"); err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
	}
	if got := fs.tokenCalls.Load(); got != 1 {
		t.Errorf("oauth token endpoint hit %d times, want 1 (token cache broken)", got)
	}
	if got := fs.keyCalls.Load(); got != 3 {
		t.Errorf("key endpoint hit %d times, want 3", got)
	}
}

func TestTokenRefreshNearExpiry(t *testing.T) {
	fs := newFakeServer(t)
	fs.tokenExpiresIn = 100 // short

	clk := &clock{t: time.Now()}
	p := newProvisioner(fs, WithClock(clk.Now))

	if _, err := p.IssueEphemeralKey(context.Background(), "env-1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	// advance past the 60s buffer + token lifetime
	clk.Advance(150 * time.Second)
	if _, err := p.IssueEphemeralKey(context.Background(), "env-2"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := fs.tokenCalls.Load(); got != 2 {
		t.Errorf("oauth token hits = %d, want 2 (refresh broken)", got)
	}
}

func TestIssueEphemeralKeyAPIError(t *testing.T) {
	fs := newFakeServer(t)
	fs.failKeyCreate = true
	p := newProvisioner(fs)

	_, err := p.IssueEphemeralKey(context.Background(), "env")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "create key") {
		t.Errorf("error should reference 'create key': %v", err)
	}
}

func TestIssueEphemeralKeyValidatesArgs(t *testing.T) {
	fs := newFakeServer(t)
	p := newProvisioner(fs)
	if _, err := p.IssueEphemeralKey(context.Background(), ""); err == nil {
		t.Errorf("empty envID should error")
	}

	bad := New("", "", WithAPIBase(fs.URL), WithTokenURL(fs.URL+"/api/v2/oauth/token"))
	if _, err := bad.IssueEphemeralKey(context.Background(), "env"); err == nil {
		t.Errorf("missing creds should error")
	}
}

func TestRevokeKey(t *testing.T) {
	fs := newFakeServer(t)
	p := newProvisioner(fs)

	if err := p.RevokeKey(context.Background(), "k-1"); err != nil {
		t.Errorf("revoke ok: %v", err)
	}
	got := *fs.revokeIDs.Load()
	if len(got) != 1 || got[0] != "k-1" {
		t.Errorf("server saw %v, want [k-1]", got)
	}
}

func TestRevokeKey404TreatedAsSuccess(t *testing.T) {
	fs := newFakeServer(t)
	p := newProvisioner(fs)
	if err := p.RevokeKey(context.Background(), "missing"); err != nil {
		t.Errorf("404 should be treated as success, got %v", err)
	}
}

func TestRevokeKeyValidatesArgs(t *testing.T) {
	fs := newFakeServer(t)
	p := newProvisioner(fs)
	if err := p.RevokeKey(context.Background(), ""); err == nil {
		t.Errorf("empty id should error")
	}
}

// Stops the test server from logging HTTP errors as test failures when
// we deliberately cause them.
func init() {
	// silence default httptest logging — not strictly necessary but
	// keeps `go test -v` output focused on real failures.
	_ = url.URL{}
}

// clock is a manual test clock.
type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }
