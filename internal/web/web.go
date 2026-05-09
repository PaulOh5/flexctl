// Package web is the HTML/HTMX user interface for flexctl.
//
// All templates and static assets are embedded into the binary via
// go:embed so deployments stay one file. Templates are parsed once at
// startup; partials used by HTMX live as named templates inside the
// pages that own them.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/PaulOh5/flexctl/internal/container"
	"github.com/PaulOh5/flexctl/internal/environments"
	"github.com/PaulOh5/flexctl/internal/scheduler"
	"github.com/PaulOh5/flexctl/internal/tailscale"
	"github.com/PaulOh5/flexctl/internal/users"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// TailscaleProvisioner is the slice of *tailscale.Provisioner the web
// package needs. Defining the interface here lets handler tests use a
// fake without touching the OAuth wire.
type TailscaleProvisioner interface {
	IssueEphemeralKey(ctx context.Context, envID string) (*tailscale.AuthKey, error)
	RevokeKey(ctx context.Context, keyID string) error
}

// ContainerManager is the slice of *container.Manager the web package
// needs. Mirror of pieces consumed by the env-creation goroutine and
// the stop handler.
type ContainerManager interface {
	StartPair(ctx context.Context, spec container.EnvSpec) (container.Pair, error)
	StopPair(ctx context.Context, p container.Pair) error
}

// Compile-time assertions: real types must satisfy the web interfaces.
var (
	_ TailscaleProvisioner = (*tailscale.Provisioner)(nil)
	_ ContainerManager     = (*container.Manager)(nil)
)

// Server hosts the HTTP handlers and renders templates.
type Server struct {
	users *users.Store
	envs  *environments.Store
	sched *scheduler.Scheduler
	cm    ContainerManager
	ts    TailscaleProvisioner
	log   *slog.Logger

	tailnetName  string
	hostHomeRoot string

	r *renderer

	// pages enumerates the templates we expose so renderer parsing is
	// driven from one place. Update here when adding a new page.
	pages []string

	// asyncWG tracks goroutines spawned to run StartPair after a
	// POST /envs returns. Close blocks on this so tests and graceful
	// shutdown observe a clean handoff before the DB closes.
	asyncWG sync.WaitGroup
}

// Close waits for in-flight async work (env-creation goroutines) to
// finish. Safe to call multiple times.
func (s *Server) Close() error {
	s.asyncWG.Wait()
	return nil
}

// Deps groups the domain stores the web package needs. cmd/flexctl
// constructs them and passes them in; that keeps web from importing
// every internal package transitively in tests.
type Deps struct {
	Users     *users.Store
	Envs      *environments.Store
	Sched     *scheduler.Scheduler
	Container ContainerManager

	// Tailscale is optional. nil means env creation will fail with a
	// readable error pointing at the OAuth setup wizard.
	Tailscale TailscaleProvisioner

	// TailnetName, when set (e.g. "tail1234.ts.net"), is appended to
	// SSH command renderings so users can copy-paste a single line.
	TailnetName string

	// HostHomeRoot is the parent directory under which per-user home
	// volumes live. Default: /var/lib/flexctl/home.
	HostHomeRoot string
}

// NewServer wires templates and stores. Returns an error if any
// template fails to parse.
func NewServer(d Deps, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	pages := []string{"dashboard", "user_select", "user_new", "env_new", "env_detail"}
	r, err := newRenderer(templatesFS, pages)
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	homeRoot := d.HostHomeRoot
	if homeRoot == "" {
		homeRoot = "/var/lib/flexctl/home"
	}
	return &Server{
		users:        d.Users,
		envs:         d.Envs,
		sched:        d.Sched,
		cm:           d.Container,
		ts:           d.Tailscale,
		tailnetName:  d.TailnetName,
		hostHomeRoot: homeRoot,
		log:          log,
		r:            r,
		pages:        pages,
	}, nil
}

// Routes registers handlers on mux. Caller may also register /healthz
// and other infra routes.
func (s *Server) Routes(mux *http.ServeMux) {
	// Static assets, served straight from the embedded FS.
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheStatic(http.FileServerFS(staticSub))))

	// Pages
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /users", s.handleUserSelect)
	mux.HandleFunc("GET /users/new", s.handleUserNew)
	mux.HandleFunc("POST /users", s.handleUserCreate)
	mux.HandleFunc("POST /users/select", s.handleUserSelectSubmit)
	mux.HandleFunc("POST /users/sign-out", s.handleSignOut)

	mux.HandleFunc("GET /envs/new", s.withUser(s.handleEnvNew))
	mux.HandleFunc("POST /envs", s.withUser(s.handleEnvCreate))
	mux.HandleFunc("GET /envs/{id}", s.withUser(s.handleEnvDetail))
	mux.HandleFunc("POST /envs/{id}/stop", s.withUser(s.handleEnvStop))
}

// pageData is the standard view-model embedded in every page. Page
// handlers extend it via composition.
type pageData struct {
	Title       string
	CurrentUser *users.User
	Users       []users.User
	Flash       *flash
	// Page-specific data bag.
	Data any
}

type flash struct {
	Kind string // success | warning | error
	Msg  string
}

// renderer owns the parsed templates.
type renderer struct {
	templates map[string]*template.Template
}

func newRenderer(efs embed.FS, pages []string) (*renderer, error) {
	r := &renderer{templates: make(map[string]*template.Template, len(pages))}

	// Functions used inside templates.
	funcs := template.FuncMap{
		"truncID": func(id string) string {
			if len(id) <= 12 {
				return id
			}
			return id[:12]
		},
		"upper": strings.ToUpper,
		"add":   func(a, b int) int { return a + b },
		"sub":   func(a, b int) int { return a - b },
	}

	// All component partials are loaded into every page so any page
	// can include any component without surprise.
	componentPaths, err := globPaths(efs, "templates/components")
	if err != nil {
		return nil, err
	}

	for _, p := range pages {
		paths := append([]string{
			"templates/layout.html",
			"templates/" + p + ".html",
		}, componentPaths...)
		t, err := template.New("layout").Funcs(funcs).ParseFS(efs, paths...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		r.templates[p] = t
	}
	return r, nil
}

// render writes the named page. Errors after the first byte are logged
// (we cannot rewrite the response).
func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	t, ok := s.r.templates[name]
	if !ok {
		http.Error(w, "template not found: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("template execute", "name", name, "err", err)
	}
}

// renderPartial writes a named partial without the layout. Used for
// HTMX swaps where only a fragment is returned.
func (s *Server) renderPartial(w io.Writer, page, partial string, data any) error {
	t, ok := s.r.templates[page]
	if !ok {
		return fmt.Errorf("template not found: %s", page)
	}
	if rw, isHTTP := w.(http.ResponseWriter); isHTTP {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	return t.ExecuteTemplate(w, partial, data)
}

func globPaths(efs embed.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(efs, dir)
	if err != nil {
		// Missing components dir is fine.
		return nil, nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, path.Join(dir, e.Name()))
	}
	return out, nil
}

// cacheStatic adds long-lived cache headers to embedded assets. Safe
// because the asset paths are content-stable per binary; a redeploy
// gives users a fresh path either way.
func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		h.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------
// User context (cookie-based)
// ---------------------------------------------------------------------

const userCookie = "flexctl_user"

type ctxKey struct{ name string }

var userKey = ctxKey{"user"}

// withUser injects the resolved user (if any) into the request context
// based on the flexctl_user cookie. Handlers read it via currentUser().
func (s *Server) withUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(userCookie); err == nil && c.Value != "" {
			u, err := s.users.GetByEmail(r.Context(), c.Value)
			if err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userKey, u))
			}
		}
		h(w, r)
	}
}

func currentUser(r *http.Request) *users.User {
	if v, ok := r.Context().Value(userKey).(*users.User); ok {
		return v
	}
	return nil
}

// listUsers gets the user set; small set so a slice is fine.
func (s *Server) listUsers(r *http.Request) []users.User {
	out, err := s.users.List(r.Context())
	if err != nil {
		s.log.Warn("list users", "err", err)
		return nil
	}
	return out
}
