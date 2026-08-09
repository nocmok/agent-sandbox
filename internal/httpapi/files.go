package httpapi

import (
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
)

func (h *handlers) getFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "id must be a valid UUID")
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "path is required")
		return
	}

	headerWritten := false
	err = h.svc.DownloadFile(r.Context(), id, path, func(f io.Reader) error {
		headerWritten = true
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", mw.FormDataContentType())
		w.WriteHeader(http.StatusOK)

		part, err := mw.CreateFormFile("file", filepath.Base(path))
		if err != nil {
			return err
		}
		if _, err := io.Copy(part, f); err != nil {
			return err
		}
		return mw.Close()
	})
	if err != nil {
		if headerWritten {
			return // status already committed, nothing more we can do
		}
		writeError(w, err)
		return
	}
}

func (h *handlers) postFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "id must be a valid UUID")
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "path is required")
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "expected multipart/form-data body")
		return
	}
	part, err := mr.NextPart()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "VALIDATION_ERROR", "missing file part")
		return
	}
	defer part.Close()

	if err := h.svc.UploadFile(r.Context(), id, path, part); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *handlers) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
