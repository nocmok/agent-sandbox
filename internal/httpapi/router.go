// Package httpapi is the HTTP transport layer: routing, request/response
// (de)serialization, and error mapping. All orchestration logic lives in
// internal/sandbox.Service; handlers here stay thin.
package httpapi

import (
	"log"
	"net/http"
	"time"

	"agent-sandbox/internal/sandbox"
)

type handlers struct {
	svc *sandbox.Service
}

func NewRouter(svc *sandbox.Service) http.Handler {
	h := &handlers{svc: svc}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", h.createSandbox)
	mux.HandleFunc("GET /sandboxes", h.listSandboxes)
	mux.HandleFunc("GET /sandboxes/{id}", h.getSandbox)
	mux.HandleFunc("DELETE /sandboxes/{id}", h.deleteSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/exec", h.execSandbox)
	mux.HandleFunc("GET /sandboxes/{id}/files", h.getFile)
	mux.HandleFunc("POST /sandboxes/{id}/files", h.postFile)
	mux.HandleFunc("GET /healthz", h.healthz)

	return recoverMiddleware(loggingMiddleware(mux))
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
