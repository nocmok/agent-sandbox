package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func (h *handlers) execSandbox(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "id must be a valid UUID")
		return
	}

	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "command is required")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "streaming unsupported")
		return
	}

	// onStart is called by Service.Exec only once preflight checks (sandbox
	// exists, lock acquired, not already executing) have passed - the last
	// point at which a clean, non-streamed error response is still possible,
	// before SSE headers are committed.
	started := false
	onStart := func() {
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}

	sw := &sseWriter{w: w, f: flusher}
	exitCode, err := h.svc.Exec(r.Context(), id, req.Command, onStart, sw)
	if err != nil {
		if !started {
			writeError(w, err)
			return
		}
		msg, _ := json.Marshal(map[string]string{"message": err.Error()})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", msg)
		flusher.Flush()
		return
	}
	fmt.Fprintf(w, "event: exit\ndata: {\"code\":%d}\n\n", exitCode)
	flusher.Flush()
}
