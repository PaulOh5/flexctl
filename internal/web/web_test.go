package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PaulOh5/flexctl/internal/container"
	"github.com/PaulOh5/flexctl/internal/db"
	"github.com/PaulOh5/flexctl/internal/environments"
	"github.com/PaulOh5/flexctl/internal/scheduler"
	"github.com/PaulOh5/flexctl/internal/tailscale"
	"github.com/PaulOh5/flexctl/internal/users"
)

// ---- mocks ----

type mockTS struct {
	mu       sync.Mutex
	issued   []string
	revoked  []string
	issueErr error
}

func (m *mockTS) IssueEphemeralKey(_ context.Context, envID string) (*tailscale.AuthKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.issueErr != nil {
		return nil, m.issueErr
	}
	m.issued = append(m.issued, envID)
	return &tailscale.AuthKey{
		Key:       "tskey-fake-" + envID[:8],
		ID:        "k-" + envID[:6],
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}
func (m *mockTS) RevokeKey(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revoked = append(m.revoked, id)
	return nil
}

type mockCM struct {
	mu       sync.Mutex
	starts   []container.EnvSpec
	stops    []container.Pair
	startErr error
}

func (m *mockCM) StartPair(_ context.Context, spec container.EnvSpec) (container.Pair, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startErr != nil {
		return container.Pair{}, m.startErr
	}
	m.starts = append(m.starts, spec)
	short := strings.ReplaceAll(spec.EnvID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return container.Pair{SidecarID: "sc-" + short, WorkID: "wk-" + short}, nil
}
func (m *mockCM) StopPair(_ context.Context, p container.Pair) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops = append(m.stops, p)
	return nil
}

// startsFor returns the spec captured for envID, or nil if not yet.
func (m *mockCM) startsFor(envID string) *container.EnvSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.starts {
		if m.starts[i].EnvID == envID {
			return &m.starts[i]
		}
	}
	return nil
}

// waitForStart polls until StartPair is called for envID, or t.Fatal on timeout.
func waitForStart(t *testing.T, m *mockCM, envID string) container.EnvSpec {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := m.startsFor(envID); s != nil {
			return *s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("StartPair not called for %s within 2s", envID)
	return container.EnvSpec{}
}

// ---- fixture ----

type fixture struct {
	srv *Server
	db  *sql.DB

	users *users.Store
	envs  *environments.Store
	sched *scheduler.Scheduler
	ts    *mockTS
	cm    *mockCM
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "web.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := func() time.Time { return time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC) }
	u := users.New(d, now)
	envs := environments.New(d, now)
	sch := scheduler.New(d, now)
	ts := &mockTS{}
	cm := &mockCM{}

	srv, err := NewServer(Deps{
		Users:        u,
		Envs:         envs,
		Sched:        sch,
		Container:    cm,
		Tailscale:    ts,
		TailnetName:  "tail8a3b2.ts.net",
		HostHomeRoot: dir,
	}, nil)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	// LIFO: Close() drains async goroutines BEFORE the DB closes (which
	// db.Open's Cleanup registered first).
	t.Cleanup(func() { _ = srv.Close() })
	return &fixture{srv: srv, db: d, users: u, envs: envs, sched: sch, ts: ts, cm: cm}
}

// seedGPUs inserts n inventory rows so allocations work in tests.
func (f *fixture) seedGPUs(t *testing.T, n int) []string {
	t.Helper()
	uuids := make([]string, n)
	for i := 0; i < n; i++ {
		uuid := "GPU-test-" + strings.Repeat("0", 4) + string(rune('a'+i))
		uuids[i] = uuid
		if _, err := f.db.Exec(`INSERT INTO gpu_inventory (uuid, device_index, name, memory_total_mb, last_seen_at)
			VALUES (?, ?, 'RTX 5090', 32768, ?)`, uuid, i, time.Now().Unix()); err != nil {
			t.Fatalf("seed gpu: %v", err)
		}
	}
	return uuids
}

// muxFor wires routes against an httptest.NewServer for end-to-end requests.
func (f *fixture) muxFor() http.Handler {
	mux := http.NewServeMux()
	f.srv.Routes(mux)
	return mux
}

// do is a tiny client that follows redirect chain manually (so we can
// assert on each hop) and persists a cookie jar across calls.
type doer struct {
	t       *testing.T
	mux     http.Handler
	cookies []*http.Cookie
}

func (d *doer) request(method, path string, form url.Values) *httptest.ResponseRecorder {
	d.t.Helper()
	var body strings.Reader
	req := httptest.NewRequest(method, path, &body)
	if form != nil {
		body = *strings.NewReader(form.Encode())
		req = httptest.NewRequest(method, path, &body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range d.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	d.mux.ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		// Replace if same name; otherwise append.
		replaced := false
		for i := range d.cookies {
			if d.cookies[i].Name == c.Name {
				d.cookies[i] = c
				replaced = true
			}
		}
		if !replaced {
			d.cookies = append(d.cookies, c)
		}
	}
	return rec
}

// ---- user tests ----

func TestIndexRedirectsToUserNewWhenEmpty(t *testing.T) {
	f := newFixture(t)
	d := &doer{t: t, mux: f.muxFor()}

	rec := d.request("GET", "/", nil)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status=%d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/users/new" {
		t.Errorf("Location=%q, want /users/new", loc)
	}
}

func TestPostUsersCreatesAndSetsCookie(t *testing.T) {
	f := newFixture(t)
	d := &doer{t: t, mux: f.muxFor()}

	rec := d.request("POST", "/users", url.Values{"email": {"alice@test"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location=%q, want /", loc)
	}

	hasCookie := false
	for _, c := range d.cookies {
		if c.Name == "flexctl_user" && c.Value == "alice@test" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Errorf("flexctl_user cookie not set; got %+v", d.cookies)
	}

	// User actually created.
	if _, err := f.users.GetByEmail(context.Background(), "alice@test"); err != nil {
		t.Errorf("user not in db: %v", err)
	}
}

func TestPostUsersDuplicateEmail(t *testing.T) {
	f := newFixture(t)
	d := &doer{t: t, mux: f.muxFor()}

	d.request("POST", "/users", url.Values{"email": {"alice@test"}})
	rec := d.request("POST", "/users", url.Values{"email": {"alice@test"}})

	if rec.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (form re-render)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "이미 등록된") {
		t.Errorf("body should mention duplicate; got: %s", rec.Body.String())
	}
}

func TestPostUsersInvalidEmail(t *testing.T) {
	f := newFixture(t)
	d := &doer{t: t, mux: f.muxFor()}

	rec := d.request("POST", "/users", url.Values{"email": {"not-an-email"}})
	if !strings.Contains(rec.Body.String(), "올바르지 않습니다") {
		t.Errorf("expected validation message; got: %s", rec.Body.String())
	}
}

func TestPostUsersSixthRejected(t *testing.T) {
	f := newFixture(t)
	d := &doer{t: t, mux: f.muxFor()}

	for i := 0; i < MaxUsers; i++ {
		d.request("POST", "/users", url.Values{"email": {string(rune('a'+i)) + "@test"}})
		// fresh cookie jar per request to avoid sharing
		d.cookies = nil
	}
	rec := d.request("POST", "/users", url.Values{"email": {"sixth@test"}})
	if !strings.Contains(rec.Body.String(), "최대 5명까지 권장") {
		t.Errorf("expected at-capacity message; got: %s", rec.Body.String())
	}

	// Sixth user not created.
	all, _ := f.users.List(context.Background())
	if len(all) != MaxUsers {
		t.Errorf("user count = %d, want %d", len(all), MaxUsers)
	}
}

// ---- env tests ----

func TestEnvNewWithoutTSShowsAlert(t *testing.T) {
	f := newFixture(t)
	f.srv.ts = nil // simulate missing OAuth creds

	d := &doer{t: t, mux: f.muxFor()}
	d.request("POST", "/users", url.Values{"email": {"alice@test"}})

	rec := d.request("GET", "/envs/new", nil)
	if !strings.Contains(rec.Body.String(), "Tailscale OAuth is not configured") {
		t.Errorf("expected TS-not-configured alert; got: %s", rec.Body.String())
	}
}

func TestEnvCreateHappyPath(t *testing.T) {
	f := newFixture(t)
	f.seedGPUs(t, 2)

	d := &doer{t: t, mux: f.muxFor()}
	d.request("POST", "/users", url.Values{"email": {"alice@test"}})

	rec := d.request("POST", "/envs", url.Values{
		"image":          {"nvidia/cuda:12.4.1-base-ubuntu22.04"},
		"authorized_key": {"ssh-ed25519 AAAA"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/envs/") {
		t.Fatalf("Location=%q", loc)
	}

	envID := strings.TrimPrefix(loc, "/envs/")

	// TS key was minted exactly once for this env.
	f.ts.mu.Lock()
	issued := append([]string(nil), f.ts.issued...)
	f.ts.mu.Unlock()
	if len(issued) != 1 || issued[0] != envID {
		t.Errorf("TS issued = %v, want [%s]", issued, envID)
	}

	// GPU was allocated to this env.
	allocs, _ := f.sched.ListAllocated(context.Background())
	if len(allocs) != 1 || allocs[0].EnvID != envID {
		t.Errorf("active allocs = %+v, want one for %s", allocs, envID)
	}

	// Async StartPair eventually fires with the right wiring.
	spec := waitForStart(t, f.cm, envID)
	if spec.WorkImage != "nvidia/cuda:12.4.1-base-ubuntu22.04" {
		t.Errorf("WorkImage = %s", spec.WorkImage)
	}
	if spec.AuthorizedKey != "ssh-ed25519 AAAA" {
		t.Errorf("AuthorizedKey not propagated")
	}
	if spec.TailscaleAuthKey == "" {
		t.Errorf("TS key not injected")
	}
	if !strings.HasPrefix(spec.TailscaleHostname, "alice-") {
		t.Errorf("hostname = %s, want alice-... prefix", spec.TailscaleHostname)
	}
	if spec.HostUID < 10001 {
		t.Errorf("HostUID = %d, want >= 10001", spec.HostUID)
	}

	// Once async start finishes, AssignContainers is called and the
	// detail page knows the IDs.
	deadline := time.Now().Add(2 * time.Second)
	var env *environments.Environment
	for time.Now().Before(deadline) {
		got, _ := f.envs.GetByID(context.Background(), envID)
		if got != nil && got.SidecarContainerID != "" {
			env = got
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if env == nil || env.SidecarContainerID == "" {
		t.Errorf("AssignContainers not called for %s", envID)
	}
}

func TestEnvCreateRejectsUnknownImage(t *testing.T) {
	f := newFixture(t)
	f.seedGPUs(t, 1)
	d := &doer{t: t, mux: f.muxFor()}
	d.request("POST", "/users", url.Values{"email": {"alice@test"}})

	rec := d.request("POST", "/envs", url.Values{"image": {"evil/image:latest"}})
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 form re-render", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "이미지를 골라주세요") {
		t.Errorf("expected image validation message")
	}
}

func TestEnvCreateBlocksSecondActiveEnv(t *testing.T) {
	f := newFixture(t)
	f.seedGPUs(t, 2)
	d := &doer{t: t, mux: f.muxFor()}
	d.request("POST", "/users", url.Values{"email": {"alice@test"}})

	first := d.request("POST", "/envs", url.Values{"image": {"nvidia/cuda:12.4.1-base-ubuntu22.04"}})
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first create: %d", first.Code)
	}
	firstLoc := first.Header().Get("Location")

	// Second create attempt redirects to the existing env.
	second := d.request("POST", "/envs", url.Values{"image": {"nvidia/cuda:12.4.1-base-ubuntu22.04"}})
	if loc := second.Header().Get("Location"); loc != firstLoc {
		t.Errorf("expected redirect to existing env %s, got %s", firstLoc, loc)
	}
}

func TestEnvDetailScopedToOwner(t *testing.T) {
	f := newFixture(t)
	f.seedGPUs(t, 2)

	// Alice creates an env.
	dA := &doer{t: t, mux: f.muxFor()}
	dA.request("POST", "/users", url.Values{"email": {"alice@test"}})
	rec := dA.request("POST", "/envs", url.Values{"image": {"nvidia/cuda:12.4.1-base-ubuntu22.04"}})
	envID := strings.TrimPrefix(rec.Header().Get("Location"), "/envs/")

	// Bob signs up; trying to view alice's env returns 403.
	dB := &doer{t: t, mux: f.muxFor()}
	dB.request("POST", "/users", url.Values{"email": {"bob@test"}})
	got := dB.request("GET", "/envs/"+envID, nil)
	if got.Code != http.StatusForbidden {
		t.Errorf("bob can read alice's env: code=%d body=%s", got.Code, got.Body.String())
	}

	// Alice can.
	hers := dA.request("GET", "/envs/"+envID, nil)
	if hers.Code != http.StatusOK {
		t.Errorf("alice cannot view her own env: code=%d", hers.Code)
	}
	if !strings.Contains(hers.Body.String(), "Connect") {
		t.Errorf("detail page missing Connect section")
	}
}

func TestEnvStopTransitionsState(t *testing.T) {
	f := newFixture(t)
	f.seedGPUs(t, 1)
	d := &doer{t: t, mux: f.muxFor()}
	d.request("POST", "/users", url.Values{"email": {"alice@test"}})

	rec := d.request("POST", "/envs", url.Values{"image": {"nvidia/cuda:12.4.1-base-ubuntu22.04"}})
	envID := strings.TrimPrefix(rec.Header().Get("Location"), "/envs/")

	// Drive to running directly so the stop -> stopping transition is allowed.
	if err := f.envs.SetState(context.Background(), envID, environments.StateRunning); err != nil {
		t.Fatalf("set running: %v", err)
	}

	stop := d.request("POST", "/envs/"+envID+"/stop", url.Values{})
	if stop.Code != http.StatusSeeOther {
		t.Errorf("stop status = %d", stop.Code)
	}

	got, _ := f.envs.GetByID(context.Background(), envID)
	if got.State != environments.StateStopping {
		t.Errorf("state = %s, want stopping", got.State)
	}
}

func TestSSHCommandDetectsOSFromUserAgent(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 12) AppleWebKit": "macos",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)":              "windows",
		"Mozilla/5.0 (X11; Linux x86_64)":                        "linux",
		"":                                                        "linux",
	}
	for ua, want := range cases {
		if got := detectOS(ua); got != want {
			t.Errorf("ua %q: got %s, want %s", ua, got, want)
		}
	}
}

func TestEnvHostnameSafeAndShort(t *testing.T) {
	got := envHostname("Alice.Smith+test@flexctl.io", "11111111-2222-3333-4444-555555555555")
	if got != "alicesmithtest-11111111-flexctl" {
		t.Errorf("got %q", got)
	}
	if len(got) > 63 {
		t.Errorf("hostname too long: %d", len(got))
	}
}
