package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/google/uuid"
)

func (h *handlers) createSandbox(w http.ResponseWriter, r *http.Request) {
	idemKey, err := uuid.Parse(r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Idempotency-Key header must be a valid UUID")
		return
	}

	var req createSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return
	}

	sb, err := h.svc.CreateSandbox(r.Context(), req.Image, req.Workspace, idemKey)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": sb.ID.String()})
}

func (h *handlers) listSandboxes(w http.ResponseWriter, r *http.Request) {
	page, ok := parseRequiredIntQuery(w, r, "page")
	if !ok {
		return
	}
	size, ok := parseRequiredIntQuery(w, r, "size")
	if !ok {
		return
	}

	list, err := h.svc.ListSandboxes(r.Context(), page, size)
	if err != nil {
		writeError(w, err)
		return
	}
	dtos := make([]sandboxDTO, len(list))
	for i, sb := range list {
		dtos[i] = toDTO(sb)
	}
	writeJSON(w, http.StatusOK, dtos)
}

// parseRequiredIntQuery reads a required integer query parameter, writing a
// 400 VALIDATION_ERROR and returning ok=false if it's missing or malformed.
func parseRequiredIntQuery(w http.ResponseWriter, r *http.Request, name string) (value int, ok bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", name+" is required")
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", name+" must be an integer")
		return 0, false
	}
	return n, true
}

func (h *handlers) getSandbox(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "id must be a valid UUID")
		return
	}
	sb, err := h.svc.GetSandbox(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(sb))
}

func (h *handlers) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "id must be a valid UUID")
		return
	}
	if err := h.svc.DeleteSandbox(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
