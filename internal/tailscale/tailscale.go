// Package tailscale issues per-environment Tailscale auth keys via the
// Tailscale OAuth client-credentials flow.
//
// Why this package exists: Eng review #4 (secret injection). We must
// never bake a long-lived auth key into the host image — every customer
// box would join the same tailnet identity. Instead, the control plane
// holds an OAuth client_id + client_secret in /etc/flexctl/oauth.secret
// (mode 0600, root only) and mints a single-use, ephemeral auth key
// at the moment a user creates an environment. The key is consumed by
// tailscaled on first start and the node deletes itself when the
// container stops (ephemeral=true).
//
// Setup outside this package: at https://login.tailscale.com/admin/settings/oauth
// the operator creates an OAuth client with auth_keys:write scope and
// the tag this package will use (default tag:flexctl-user). The ACL
// must allow that tag to enter the tailnet.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Defaults that callers rarely override outside tests.
const (
	defaultAPIBase   = "https://api.tailscale.com"
	defaultTokenURL  = "https://api.tailscale.com/api/v2/oauth/token"
	defaultTagPrefix = "tag:flexctl"
	defaultKeyExpiry = time.Hour
)

// Provisioner mints and revokes tailnet auth keys.
//
// Safe for concurrent use; the OAuth access token is cached behind a
// mutex and refreshed transparently when it nears expiry.
type Provisioner struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client
	apiBase      string
	tokenURL     string
	tagPrefix    string
	keyExpiry    time.Duration
	now          func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// AuthKey is a freshly minted single-use, ephemeral key.
//
// The Key field is the value to inject as TS_AUTHKEY into the sidecar
// container's environment. Callers MUST NOT persist it: the key is
// consumed on first use and we re-mint per environment.
type AuthKey struct {
	Key       string    // tskey-auth-... the actual auth key
	ID        string    // tailscale internal id, for revocation
	ExpiresAt time.Time // when the key (not the device) expires
}

// Option mutates a Provisioner during New.
type Option func(*Provisioner)

// WithAPIBase overrides the Tailscale API host (used in tests).
func WithAPIBase(u string) Option { return func(p *Provisioner) { p.apiBase = u } }

// WithTokenURL overrides the OAuth token endpoint (used in tests).
func WithTokenURL(u string) Option { return func(p *Provisioner) { p.tokenURL = u } }

// WithTagPrefix sets the tag prefix; the actual tag used is
// "<prefix>-user". Default is "tag:flexctl".
func WithTagPrefix(t string) Option { return func(p *Provisioner) { p.tagPrefix = t } }

// WithHTTPClient injects a custom *http.Client (for testing or to set
// a different timeout/transport).
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provisioner) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// WithClock overrides the clock used for token expiry tracking. Tests
// pass a fake clock; production uses time.Now (the default).
func WithClock(now func() time.Time) Option {
	return func(p *Provisioner) {
		if now != nil {
			p.now = now
		}
	}
}

// WithKeyExpiry sets how long the issued auth key remains valid for
// being consumed. Default is 1 hour. Note: the resulting tailnet node
// is ephemeral and lives only as long as its container.
func WithKeyExpiry(d time.Duration) Option {
	return func(p *Provisioner) {
		if d > 0 {
			p.keyExpiry = d
		}
	}
}

// New constructs a Provisioner from OAuth credentials.
func New(clientID, clientSecret string, opts ...Option) *Provisioner {
	p := &Provisioner{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		apiBase:      defaultAPIBase,
		tokenURL:     defaultTokenURL,
		tagPrefix:    defaultTagPrefix,
		keyExpiry:    defaultKeyExpiry,
		now:          time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// IssueEphemeralKey mints a single-use, ephemeral, preauthorized key
// scoped to the configured tag, tagged in the description with envID
// for traceability.
func (p *Provisioner) IssueEphemeralKey(ctx context.Context, envID string) (*AuthKey, error) {
	if p.clientID == "" || p.clientSecret == "" {
		return nil, errors.New("tailscale: OAuth client id/secret unset")
	}
	if envID == "" {
		return nil, errors.New("tailscale: envID required")
	}

	tok, err := p.accessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}

	reqBody := map[string]any{
		"capabilities": map[string]any{
			"devices": map[string]any{
				"create": map[string]any{
					"reusable":      false,
					"ephemeral":     true,
					"preauthorized": true,
					"tags":          []string{p.tagPrefix + "-user"},
				},
			},
		},
		"expirySeconds": int(p.keyExpiry.Seconds()),
		"description":   "flexctl env " + envID,
	}
	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.apiBase+"/api/v2/tailnet/-/keys", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create key: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("create key: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var parsed struct {
		Key     string    `json:"key"`
		ID      string    `json:"id"`
		Expires time.Time `json:"expires"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode key response: %w", err)
	}
	if parsed.Key == "" {
		return nil, fmt.Errorf("create key: empty key in response: %s", raw)
	}
	return &AuthKey{
		Key:       parsed.Key,
		ID:        parsed.ID,
		ExpiresAt: parsed.Expires,
	}, nil
}

// RevokeKey deletes an unused key. Called for cleanup if a sidecar
// fails to start before consuming the key. Not needed for healthy
// ephemeral keys: those are consumed on first use and the node
// disappears from the tailnet when its container stops.
//
// 404 from the API is treated as success (already gone).
func (p *Provisioner) RevokeKey(ctx context.Context, keyID string) error {
	if keyID == "" {
		return errors.New("tailscale: keyID required")
	}
	tok, err := p.accessToken(ctx)
	if err != nil {
		return fmt.Errorf("oauth: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		p.apiBase+"/api/v2/tailnet/-/keys/"+url.PathEscape(keyID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("revoke %s: %s: %s", keyID, resp.Status, strings.TrimSpace(string(raw)))
}

// accessToken returns a valid Bearer token, refreshing via the OAuth
// token endpoint when the cached one is missing or within 60s of expiry.
func (p *Provisioner) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.token != "" && p.now().Add(60*time.Second).Before(p.tokenExp) {
		return p.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("token: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("token: empty access_token in response: %s", raw)
	}
	p.token = parsed.AccessToken
	p.tokenExp = p.now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	return p.token, nil
}
