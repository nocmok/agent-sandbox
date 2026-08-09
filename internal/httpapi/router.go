// Package httpapi is the HTTP transport layer: routing, request/response
// (de)serialization, and error mapping. All orchestration logic lives in
// internal/sandbox.Service; handlers here stay thin.
package httpapi

import (
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"agent-sandbox/internal/sandbox"
)

// sandboxService is the subset of sandbox.Service the HTTP layer needs. It
// is declared here, the consumer, so tests can substitute a fake without
// touching Kubernetes.
type sandboxService interface {
	Create(ctx context.Context, id, image string) error
	List(ctx context.Context, page, size int) ([]sandbox.Sandbox, error)
	Get(ctx context.Context, id string) (*sandbox.Sandbox, error)
	Delete(ctx context.Context, id string) error
	Exec(ctx context.Context, id, command string) (func(out io.Writer) error, error)
}

type handlers struct {
	svc sandboxService
}

func NewRouter(svc sandboxService) http.Handler {
	h := &handlers{svc: svc}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", h.createSandbox)
	mux.HandleFunc("GET /sandboxes", h.listSandboxes)
	mux.HandleFunc("GET /sandboxes/{id}", h.getSandbox)
	mux.HandleFunc("DELETE /sandboxes/{id}", h.deleteSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/exec", h.execSandbox)
	mux.HandleFunc("GET /healthz", h.healthz)

	return recoverMiddleware(loggingMiddleware(mux))
}

func (h *handlers) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic handling %s %s: %v", r.Method, r.URL.Path, rec)
				writeJSONError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
