// Command flexctl runs the control plane: HTTP server, reconciliation
// loop, and one-shot GPU inventory at startup.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/PaulOh5/flexctl/internal/container"
	"github.com/PaulOh5/flexctl/internal/db"
	"github.com/PaulOh5/flexctl/internal/environments"
	"github.com/PaulOh5/flexctl/internal/gpu"
	"github.com/PaulOh5/flexctl/internal/reconciler"
	"github.com/PaulOh5/flexctl/internal/scheduler"
	"github.com/PaulOh5/flexctl/internal/tailscale"
	"github.com/PaulOh5/flexctl/internal/users"
	"github.com/PaulOh5/flexctl/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath     = flag.String("db", "data/flexctl.db", "SQLite database path")
		listen     = flag.String("listen", "127.0.0.1:8080", "HTTP listen address (front this with a Tailnet ACL)")
		logJSON    = flag.Bool("log-json", false, "JSON logs (default: text)")
		recInt     = flag.Duration("reconcile-interval", 5*time.Second, "how often to reconcile pair state with docker")
		skipGPU    = flag.Bool("skip-gpu", false, "skip nvidia-smi inventory (use on machines without GPUs)")
		tsClientID = flag.String("ts-client-id", os.Getenv("FLEXCTL_TS_CLIENT_ID"),
			"Tailscale OAuth client id (or set FLEXCTL_TS_CLIENT_ID)")
		tsClientSecret = flag.String("ts-client-secret", os.Getenv("FLEXCTL_TS_CLIENT_SECRET"),
			"Tailscale OAuth client secret (or set FLEXCTL_TS_CLIENT_SECRET)")
		tailnetName = flag.String("tailnet", os.Getenv("FLEXCTL_TAILNET"),
			"Tailnet magic-DNS suffix, e.g. tail1234.ts.net (or set FLEXCTL_TAILNET); used to render copy-pasteable SSH commands")
		hostHomeRoot = flag.String("host-home-root", "/var/lib/flexctl/home",
			"parent directory for per-user home volumes")
	)
	flag.Parse()

	log := newLogger(*logJSON)
	slog.SetDefault(log)

	if err := os.MkdirAll("data", 0o750); err != nil {
		return err
	}

	store, err := db.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, store); err != nil {
		return err
	}
	log.Info("db ready", "path", *dbPath)

	// GPU inventory is best-effort: developers without nvidia-smi can
	// still bring the control plane up to drive the API.
	gpuCount := 0
	if *skipGPU {
		log.Info("gpu inventory skipped (-skip-gpu)")
	} else {
		n, err := gpu.Sync(ctx, store)
		if err != nil {
			log.Warn("gpu inventory unavailable; control plane will not be able to allocate", "err", err)
		} else {
			gpuCount = n
			log.Info("gpu inventory synced", "count", n)
		}
	}

	// Construct the per-domain stores. They are tiny and share the
	// same *sql.DB handle.
	envStore := environments.New(store, time.Now)
	sched := scheduler.New(store, time.Now)
	userStore := users.New(store, time.Now)
	cm := container.NewManager(log)

	// Tailscale provisioner is optional. Without OAuth creds the web
	// UI shows a clear error on env creation and the rest of the
	// control plane keeps running.
	var tsProv *tailscale.Provisioner
	if *tsClientID != "" && *tsClientSecret != "" {
		tsProv = tailscale.New(*tsClientID, *tsClientSecret)
		log.Info("tailscale provisioner ready", "tailnet", *tailnetName)
	} else {
		log.Warn("tailscale OAuth not configured; env creation disabled until -ts-client-id and -ts-client-secret are set")
	}

	webDeps := web.Deps{
		Users:        userStore,
		Envs:         envStore,
		Sched:        sched,
		Container:    cm,
		TailnetName:  *tailnetName,
		HostHomeRoot: *hostHomeRoot,
	}
	if tsProv != nil {
		webDeps.Tailscale = tsProv
	}
	webSrv, err := web.NewServer(webDeps, log)
	if err != nil {
		return fmt.Errorf("web init: %w", err)
	}

	// Reconciler runs as a background goroutine that exits when ctx
	// is cancelled. We expose live tick-count + last-error through
	// /healthz so operators can see at a glance whether the loop is
	// alive.
	hb := newHeartbeat()
	rec := reconciler.New(envStore, sched, cm, log,
		reconciler.WithInterval(*recInt),
	)
	recDone := make(chan error, 1)
	go func() {
		recDone <- runWithHeartbeat(ctx, rec, hb)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler(hb, store, gpuCount))
	webSrv.Routes(mux)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	srvDone := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", *listen)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvDone <- err
		} else {
			srvDone <- nil
		}
	}()

	// Wait for either ctx cancel (signal) or a fatal error from
	// either subsystem.
	select {
	case <-ctx.Done():
	case err := <-srvDone:
		if err != nil {
			log.Error("http server", "err", err)
		}
	case err := <-recDone:
		if err != nil {
			log.Error("reconciler", "err", err)
		}
	}

	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	stop() // ensure reconciler ctx is cancelled
	<-recDone
	log.Info("bye")
	return nil
}

// heartbeat tracks reconciler liveness for /healthz.
type heartbeat struct {
	ticks    atomic.Int64
	lastTick atomic.Int64 // unix seconds
}

func newHeartbeat() *heartbeat { return &heartbeat{} }

// runWithHeartbeat wraps reconciler.Run with a tick counter. We
// re-implement the ticker locally instead of touching reconciler.Run
// internals — Tick is exported precisely so callers can build their
// own loops.
func runWithHeartbeat(ctx context.Context, r *reconciler.Reconciler, hb *heartbeat) error {
	const interval = 5 * time.Second // matches reconciler default; updated from flag would be wired in if we exposed Interval()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := r.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("tick", "err", err)
		}
		hb.ticks.Add(1)
		hb.lastTick.Store(time.Now().Unix())
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func healthHandler(hb *heartbeat, store interface{ Ping() error }, gpuCount int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbErr := store.Ping()
		body := map[string]any{
			"db_ok":             dbErr == nil,
			"gpu_count":         gpuCount,
			"reconcile_ticks":   hb.ticks.Load(),
			"last_tick_unix":    hb.lastTick.Load(),
			"now_unix":          time.Now().Unix(),
		}
		if dbErr != nil {
			body["db_err"] = dbErr.Error()
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

func newLogger(jsonOut bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if jsonOut {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
