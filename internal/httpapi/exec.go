package httpapi

import (
	"encoding/json"
	"net/http"
)

func (h *handlers) execSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "malformed request body")
		return
	}

	// Validation, sandbox-existence, and exec-lock errors are all resolved
	// here, before any bytes are written, so they come back as clean JSON
	// errors (400/404/409) rather than SSE content.
	start, err := h.svc.Exec(r.Context(), id, req.Command)
	if err != nil {
		writeError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// From here on, headers are committed to a 200 SSE stream: any failure
	// during execution can only be surfaced as stream content.
	sw := &sseWriter{w: w, f: flusher}
	if err := start(sw); err != nil {
		_ = sw.writeEvent("stderr", err.Error())
	}
}
