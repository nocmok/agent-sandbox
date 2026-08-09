package httpapi

import (
	"fmt"
	"net/http"
	"path/filepath"
)

// uploadFile streams the request body straight into the sandbox's Pod, so
// the handler itself never buffers the file in memory. Because the whole
// upload completes before any response is written, a failure can still be
// reported as a normal JSON error, unlike downloadFile below.
func (h *handlers) uploadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.PathValue("path")

	if err := h.svc.UploadFile(r.Context(), id, path, r.Body); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// downloadFile resolves the sandbox, lock, and file-existence errors inside
// DownloadFile before anything is written, so those come back as clean JSON
// errors (404/409) rather than a truncated body.
func (h *handlers) downloadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.PathValue("path")

	start, err := h.svc.DownloadFile(r.Context(), id, path)
	if err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	w.WriteHeader(http.StatusOK)

	// From here on, headers are committed to a 200 with a streamed body:
	// any failure during the transfer can only truncate the response, the
	// same constraint execSandbox has once its SSE stream starts.
	_ = start(w)
}
