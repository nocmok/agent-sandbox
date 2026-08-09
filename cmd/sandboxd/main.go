// Command sandboxd runs the Sandbox API Service described in
// docs/00-agent-sandbox-architecture.md.
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"agent-sandbox/internal/config"
	"agent-sandbox/internal/db"
	"agent-sandbox/internal/dockerengine"
	"agent-sandbox/internal/httpapi"
	"agent-sandbox/internal/sandbox"
	"agent-sandbox/internal/storage"
)

const gracefulShutdownTimeout = 10 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connecting to database: %v", err)
	}
	defer pool.Close()

	repo := sandbox.NewPostgresRepository(pool)
	fs := storage.New(cfg.NFSExportHost)

	docker, err := dockerengine.New(cfg.NFSExportHost)
	if err != nil {
		log.Fatalf("connecting to docker engine: %v", err)
	}

	svc := sandbox.NewService(repo, docker, fs)
	router := httpapi.NewRouter(svc)

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: router}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}()

	log.Printf("sandboxd listening on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
