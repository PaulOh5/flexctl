package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/PaulOh5/flexctl/internal/db"
)

func main() {
	var (
		dbPath  = flag.String("db", "data/flexctl.db", "SQLite database path")
		listen  = flag.String("listen", "127.0.0.1:8080", "HTTP listen address (Tailnet ACL should front this)")
		logJSON = flag.Bool("log-json", false, "JSON logs (default: text)")
	)
	flag.Parse()

	var handler slog.Handler
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	log := slog.New(handler)
	slog.SetDefault(log)

	if err := os.MkdirAll("data", 0o750); err != nil {
		log.Error("mkdir data", "err", err)
		os.Exit(1)
	}

	store, err := db.Open(*dbPath)
	if err != nil {
		log.Error("db open", "path", *dbPath, "err", err)
		os.Exit(1)
	}
	defer store.Close()

	if err := db.Migrate(context.Background(), store); err != nil {
		log.Error("db migrate", "err", err)
		os.Exit(1)
	}
	log.Info("db ready", "path", *dbPath)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("http listening", "addr", *listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	log.Info("bye")
}
