package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/PaulOh5/flexctl/internal/container"
	"github.com/PaulOh5/flexctl/internal/environments"
	"github.com/PaulOh5/flexctl/internal/scheduler"
	"github.com/PaulOh5/flexctl/internal/users"
)

// imageOption is a curated row in the env-creation dropdown.
type imageOption struct {
	Name        string // human-readable
	Image       string // docker image tag
	Description string // why pick this one
	Recommended bool
}

var imageOptions = []imageOption{
	{
		Name:        "PyTorch 2.4 + CUDA 12.4 (recommended)",
		Image:       "nvidia/cuda:12.4.1-base-ubuntu22.04",
		Description: "Pre-warmed on the host; fastest start.",
		Recommended: true,
	},
	{
		Name:        "PyTorch 2.4 (full runtime)",
		Image:       "pytorch/pytorch:2.4.0-cuda12.4-cudnn9-runtime",
		Description: "Pulls on first use; ~5 GB.",
	},
	{
		Name:        "TensorFlow 2.18 GPU",
		Image:       "tensorflow/tensorflow:2.18.0-gpu",
		Description: "For TF/Keras work; first pull is slow.",
	},
	{
		Name:        "CUDA 12.4 + build tools",
		Image:       "nvidia/cuda:12.4.1-devel-ubuntu22.04",
		Description: "Use when you need to compile kernels.",
	},
}

// envCardView is a render-friendly slice of an environment + its
// allocated GPU + SSH command lines, scoped to one page.
type envCardView struct {
	Env       *environments.Environment
	GPU       *scheduler.GPUStatus // nil if env has no GPU yet
	SSHCmd    string
	Hostname  string
	StateHelp string // human-readable hint per state
}

// dashboardData is the view-model passed to dashboard.html.
type dashboardData struct {
	GPUs       []scheduler.GPUStatus
	GPUsFree   int
	GPUsTotal  int
	MyEnvs     []envCardView
	HasEnvs    bool
	BoxAtCap   bool // 6th request would be rejected
	UserCount  int
	UserAtCap  bool
}

// dashboard handler. Replaces the placeholder index with a real view.
func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, u *users.User, all []users.User, fl *flash) {
	ctx := r.Context()

	gpus, err := s.sched.ListGPUs(ctx)
	if err != nil {
		s.log.Error("list gpus", "err", err)
	}
	free := 0
	for _, g := range gpus {
		if g.Free() {
			free++
		}
	}

	envs, err := s.envs.ListByUser(ctx, u.ID)
	if err != nil {
		s.log.Error("list envs", "err", err)
	}

	gpuByUUID := map[string]scheduler.GPUStatus{}
	for _, g := range gpus {
		gpuByUUID[g.UUID] = g
	}
	myEnvs := make([]envCardView, 0, len(envs))
	for i := range envs {
		e := envs[i]
		card := envCardView{Env: &e, Hostname: e.TailscaleHostname}
		// Find which GPU (if any) this env currently holds.
		for _, g := range gpus {
			if g.EnvID == e.ID && !g.Free() {
				card.GPU = &g
				break
			}
		}
		card.SSHCmd = sshCommandFor(detectOS(r.UserAgent()), e.TailscaleHostname, s.tailnetName)
		card.StateHelp = stateHelp(e.State)
		myEnvs = append(myEnvs, card)
	}

	s.render(w, "dashboard", pageData{
		Title:       "flexctl",
		CurrentUser: u,
		Users:       all,
		Flash:       fl,
		Data: dashboardData{
			GPUs:      gpus,
			GPUsFree:  free,
			GPUsTotal: len(gpus),
			MyEnvs:    myEnvs,
			HasEnvs:   len(myEnvs) > 0 && hasNonTerminal(myEnvs),
			BoxAtCap:  free == 0,
			UserCount: len(all),
			UserAtCap: len(all) >= MaxUsers,
		},
	})
}

func hasNonTerminal(envs []envCardView) bool {
	for _, e := range envs {
		switch e.Env.State {
		case environments.StatePending, environments.StateRunning, environments.StateStopping:
			return true
		}
	}
	return false
}

// GET /envs/new — image selection form.
func (s *Server) handleEnvNew(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	gpus, err := s.sched.ListGPUs(r.Context())
	if err != nil {
		s.log.Error("list gpus", "err", err)
	}
	free := 0
	for _, g := range gpus {
		if g.Free() {
			free++
		}
	}

	// One active env per user (Phase 1 simplification + Eng review #9
	// for "6th user" math: 5 users * 1 env = 5 GPUs at most).
	active, err := s.envs.ListActive(r.Context())
	if err != nil {
		s.log.Error("list active", "err", err)
	}
	mine := false
	for _, e := range active {
		if e.UserID == u.ID {
			mine = true
			break
		}
	}

	data := map[string]any{
		"Images":      imageOptions,
		"DefaultImg":  imageOptions[0].Image,
		"GPUsFree":    free,
		"GPUsTotal":   len(gpus),
		"BoxAtCap":    free == 0,
		"UserHasEnv":  mine,
		"TSConfigured": s.ts != nil,
	}
	s.render(w, "env_new", pageData{
		Title:       "New environment",
		CurrentUser: u,
		Users:       s.listUsers(r),
		Data:        data,
	})
}

// POST /envs — env creation.
//
// Flow:
//  1. Validate (TS configured, image valid, no existing active env)
//  2. Create env row (state=pending)
//  3. Allocate GPU
//  4. Issue Tailscale ephemeral key
//  5. Spawn goroutine to call container.StartPair + AssignContainers
//  6. Redirect to /envs/{id}; reconciler promotes pending→running.
//
// On any setup error we roll back the parts already done before the
// async goroutine runs.
func (s *Server) handleEnvCreate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	image := strings.TrimSpace(r.PostForm.Get("image"))
	if image == "" || !knownImage(image) {
		s.envCreateError(w, r, u, "이미지를 골라주세요.")
		return
	}
	authKey := strings.TrimSpace(r.PostForm.Get("authorized_key"))

	if s.ts == nil {
		s.envCreateError(w, r, u,
			"Tailscale OAuth가 설정되지 않았습니다. -ts-client-id / -ts-client-secret 플래그(또는 FLEXCTL_TS_CLIENT_ID/SECRET 환경변수)로 자격을 주입한 뒤 컨트롤 플레인을 재시작하세요.")
		return
	}

	ctx := r.Context()

	// One active env per user.
	active, _ := s.envs.ListActive(ctx)
	for _, e := range active {
		if e.UserID == u.ID {
			http.Redirect(w, r, "/envs/"+e.ID, http.StatusSeeOther)
			return
		}
	}

	env, err := s.envs.Create(ctx, u.ID, image)
	if err != nil {
		s.log.Error("env create", "err", err)
		s.envCreateError(w, r, u, "환경 생성에 실패했습니다: "+err.Error())
		return
	}

	gpus, err := s.sched.ListGPUs(ctx)
	if err != nil {
		s.envCreateError(w, r, u, "GPU 인벤토리 조회 실패: "+err.Error())
		return
	}
	candidates := make([]string, 0, len(gpus))
	for _, g := range gpus {
		if g.Free() {
			candidates = append(candidates, g.UUID)
		}
	}
	if len(candidates) == 0 {
		_ = s.envs.SetState(ctx, env.ID, environments.StateFailed)
		_ = s.envs.SetExitReason(ctx, env.ID, "no GPU free")
		s.envCreateError(w, r, u, "지금 사용 가능한 GPU가 없습니다.")
		return
	}

	alloc, err := s.sched.Allocate(ctx, u.ID, env.ID, candidates, time.Minute)
	if err != nil {
		_ = s.envs.SetState(ctx, env.ID, environments.StateFailed)
		_ = s.envs.SetExitReason(ctx, env.ID, "allocate: "+err.Error())
		s.envCreateError(w, r, u, "GPU 할당 실패: "+err.Error())
		return
	}

	hostname := envHostname(u.Email, env.ID)
	tsKey, err := s.ts.IssueEphemeralKey(ctx, env.ID)
	if err != nil {
		_, _ = s.sched.ReleaseByEnvID(ctx, env.ID, scheduler.StateFailed)
		_ = s.envs.SetState(ctx, env.ID, environments.StateFailed)
		_ = s.envs.SetExitReason(ctx, env.ID, "tailscale: "+err.Error())
		s.envCreateError(w, r, u, "Tailscale 키 발급 실패: "+err.Error())
		return
	}

	homeDir := filepath.Join(s.hostHomeRoot, fmt.Sprintf("u%d", u.HostUID))

	// Hand off to a goroutine. Use a fresh context: the request ctx
	// dies on response write. The waitgroup gives Close() a way to
	// drain before the DB is torn down on shutdown / in tests.
	s.asyncWG.Add(1)
	go func() {
		defer s.asyncWG.Done()
		s.startPairAsync(env.ID, container.EnvSpec{
			EnvID:             env.ID,
			TailscaleHostname: hostname,
			TailscaleAuthKey:  tsKey.Key,
			WorkImage:         image,
			GPUUUID:           alloc.GPUUUID,
			HostUID:           u.HostUID,
			HostHomeDir:       homeDir,
			AuthorizedKey:     authKey,
		})
	}()

	// Update env with the hostname so the detail page can render the
	// SSH command immediately.
	_ = s.envs.AssignContainers(ctx, env.ID, "", "", hostname)

	http.Redirect(w, r, "/envs/"+env.ID, http.StatusSeeOther)
}

// startPairAsync runs StartPair on a fresh background context and
// records the resulting container IDs (or marks the env failed).
func (s *Server) startPairAsync(envID string, spec container.EnvSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pair, err := s.cm.StartPair(ctx, spec)
	if err != nil {
		s.log.Error("start pair", "env", envID, "err", err)
		_, _ = s.sched.ReleaseByEnvID(ctx, envID, scheduler.StateFailed)
		_ = s.envs.SetExitReason(ctx, envID, "docker start: "+err.Error())
		_ = s.envs.SetState(ctx, envID, environments.StateFailed)
		return
	}
	if err := s.envs.AssignContainers(ctx, envID, pair.SidecarID, pair.WorkID, spec.TailscaleHostname); err != nil {
		s.log.Error("assign containers", "env", envID, "err", err)
	}
	// Reconciler will pick this up next tick and promote pending→running.
}

// GET /envs/{id} — detail.
func (s *Server) handleEnvDetail(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	env, err := s.envs.GetByID(r.Context(), id)
	if errors.Is(err, environments.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("env detail", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if env.UserID != u.ID {
		http.Error(w, "not yours", http.StatusForbidden)
		return
	}

	// Find allocated GPU (if any).
	gpus, _ := s.sched.ListGPUs(r.Context())
	var gpu *scheduler.GPUStatus
	for _, g := range gpus {
		if g.EnvID == env.ID && !g.Free() {
			g := g
			gpu = &g
			break
		}
	}

	osHint := detectOS(r.UserAgent())
	cmds := allSSHCommands(env.TailscaleHostname, s.tailnetName)

	s.render(w, "env_detail", pageData{
		Title:       "Environment",
		CurrentUser: u,
		Users:       s.listUsers(r),
		Data: map[string]any{
			"Env":        env,
			"GPU":        gpu,
			"PreferredOS": osHint,
			"Commands":   cmds,
			"StateHelp":  stateHelp(env.State),
			"CanStop":    env.State == environments.StateRunning || env.State == environments.StatePending,
		},
	})
}

// POST /envs/{id}/stop — request stop. Reconciler does the actual work.
func (s *Server) handleEnvStop(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	env, err := s.envs.GetByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if env.UserID != u.ID {
		http.Error(w, "not yours", http.StatusForbidden)
		return
	}
	switch env.State {
	case environments.StatePending, environments.StateRunning:
		// pending → failed (no clean run yet) or running → stopping
		// (reconciler stops the pair on next tick).
		newState := environments.StateStopping
		if env.State == environments.StatePending {
			newState = environments.StateFailed
			_ = s.envs.SetExitReason(r.Context(), env.ID, "stopped before reaching running")
		}
		if err := s.envs.SetState(r.Context(), env.ID, newState); err != nil {
			s.log.Error("set state", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/envs/"+env.ID, http.StatusSeeOther)
}

func (s *Server) envCreateError(w http.ResponseWriter, r *http.Request, u *users.User, msg string) {
	gpus, _ := s.sched.ListGPUs(r.Context())
	free := 0
	for _, g := range gpus {
		if g.Free() {
			free++
		}
	}
	s.render(w, "env_new", pageData{
		Title:       "New environment",
		CurrentUser: u,
		Users:       s.listUsers(r),
		Flash:       &flash{Kind: "error", Msg: msg},
		Data: map[string]any{
			"Images":       imageOptions,
			"DefaultImg":   imageOptions[0].Image,
			"GPUsFree":     free,
			"GPUsTotal":    len(gpus),
			"BoxAtCap":     free == 0,
			"UserHasEnv":   false,
			"TSConfigured": s.ts != nil,
		},
	})
}

// envHostname builds a short, magic-DNS-friendly hostname from the
// user's email and env id. Lowercase; no special chars beyond `-`.
func envHostname(email, envID string) string {
	user := strings.SplitN(email, "@", 2)[0]
	user = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return -1
		}
	}, user)
	if user == "" {
		user = "user"
	}
	short := strings.ReplaceAll(envID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return user + "-" + short + "-flexctl"
}

func knownImage(s string) bool {
	for _, opt := range imageOptions {
		if opt.Image == s {
			return true
		}
	}
	return false
}

// detectOS sniffs the User-Agent for a coarse OS guess. Used to
// pre-select the matching SSH command in the detail page.
func detectOS(ua string) string {
	ua = strings.ToLower(ua)
	switch {
	case strings.Contains(ua, "windows"):
		return "windows"
	case strings.Contains(ua, "mac os x"), strings.Contains(ua, "macintosh"):
		return "macos"
	default:
		return "linux"
	}
}

// sshCommand pair: rendered for the dashboard card (single line) and
// for the detail page (per-OS variants).
func sshCommandFor(os, hostname, tailnet string) string {
	host := hostname
	if tailnet != "" {
		host = hostname + "." + tailnet
	}
	return "ssh root@" + host
}

type sshLine struct {
	OS  string
	Cmd string
	// Hint shown next to the command for OS-specific quirks.
	Hint string
}

func allSSHCommands(hostname, tailnet string) []sshLine {
	cmd := sshCommandFor("", hostname, tailnet)
	tailcmd := "tailscale ssh root@" + hostname
	return []sshLine{
		{OS: "macos", Cmd: cmd, Hint: "Built-in OpenSSH; works as-is in Terminal."},
		{OS: "linux", Cmd: cmd, Hint: "Same command; OpenSSH ships everywhere."},
		{OS: "windows", Cmd: cmd, Hint: "Use Windows Terminal + the bundled OpenSSH (Win10+)."},
		{OS: "tailscale", Cmd: tailcmd, Hint: "Tailscale CLI; no separate SSH client needed."},
	}
}

func stateHelp(state string) string {
	switch state {
	case environments.StatePending:
		return "Container is starting. The first PyTorch image takes ~5 minutes; pre-warmed images are <1 minute."
	case environments.StateRunning:
		return "Ready. SSH in over Tailscale to use the GPU."
	case environments.StateStopping:
		return "Stop requested. Reconciler is tearing the pair down; next tick will release the GPU."
	case environments.StateStopped:
		return "Cleanly stopped. Create a new environment to get a fresh GPU."
	case environments.StateFailed:
		return "Something went wrong. The reason field below has the docker exit detail."
	}
	return ""
}
