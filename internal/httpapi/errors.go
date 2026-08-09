package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"agent-sandbox/internal/sandbox"
	"agent-sandbox/internal/storage"
)

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeError maps a Service/Storage error to the HTTP status/code table from
// docs/00-agent-sandbox-architecture.md and writes it as the JSON error body.
func writeError(w http.ResponseWriter, err error) {
	status, code := mapError(err)
	if status == http.StatusInternalServerError {
		log.Printf("internal error: %v", err)
	}
	writeJSONError(w, status, code, err.Error())
}

func mapError(err error) (int, string) {
	switch {
	case errors.Is(err, sandbox.ErrValidation):
		return http.StatusBadRequest, "VALIDATION_ERROR"
	case errors.Is(err, sandbox.ErrNotFound):
		return http.StatusNotFound, "SANDBOX_NOT_FOUND"
	case errors.Is(err, sandbox.ErrExecuting):
		return http.StatusConflict, "SANDBOX_EXECUTING"
	case errors.Is(err, sandbox.ErrLocked):
		return http.StatusConflict, "SANDBOX_LOCKED"
	case errors.Is(err, storage.ErrInvalidPath), errors.Is(err, storage.ErrIsDirectory):
		return http.StatusBadRequest, "VALIDATION_ERROR"
	case errors.Is(err, storage.ErrNotFound):
		// Not in the doc's error table (which only defines SANDBOX_NOT_FOUND
		// for missing sandboxes) — NOT_FOUND is used here for a missing file
		// to avoid conflating the two.
		return http.StatusNotFound, "NOT_FOUND"
	default:
		return http.StatusInternalServerError, "INTERNAL_ERROR"
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
