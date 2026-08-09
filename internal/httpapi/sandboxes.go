package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
)

func (h *handlers) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req createSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "malformed request body")
		return
	}
	if err := h.svc.Create(r.Context(), req.ID, req.Image); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *handlers) listSandboxes(w http.ResponseWriter, r *http.Request) {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "page must be an integer")
		return
	}
	size, err := strconv.Atoi(r.URL.Query().Get("size"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "size must be an integer")
		return
	}

	sandboxes, err := h.svc.List(r.Context(), page, size)
	if err != nil {
		writeError(w, err)
		return
	}

	resp := make([]sandboxResponse, 0, len(sandboxes))
	for _, sb := range sandboxes {
		resp = append(resp, sandboxResponse{ID: sb.ID, Image: sb.Image})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) getSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxResponse{ID: sb.ID, Image: sb.Image})
}

func (h *handlers) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.Delete(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
