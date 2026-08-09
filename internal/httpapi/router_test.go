package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-sandbox/internal/sandbox"
)

type fakeService struct {
	createErr error

	listResult []sandbox.Sandbox
	listErr    error

	getResult *sandbox.Sandbox
	getErr    error

	deleteErr error

	execStart func(io.Writer) error
	execErr   error
}

func (f *fakeService) Create(ctx context.Context, id, image string) error {
	return f.createErr
}

func (f *fakeService) List(ctx context.Context, page, size int) ([]sandbox.Sandbox, error) {
	return f.listResult, f.listErr
}

func (f *fakeService) Get(ctx context.Context, id string) (*sandbox.Sandbox, error) {
	return f.getResult, f.getErr
}

func (f *fakeService) Delete(ctx context.Context, id string) error {
	return f.deleteErr
}

func (f *fakeService) Exec(ctx context.Context, id, command string) (func(io.Writer) error, error) {
	return f.execStart, f.execErr
}

func TestCreateSandbox(t *testing.T) {
	router := NewRouter(&fakeService{})
	body := strings.NewReader(`{"id":"d290f1ee-6c54-4b01-90e6-d701748f0851","image":"alpine"}`)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", body)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateSandboxValidationError(t *testing.T) {
	router := NewRouter(&fakeService{createErr: sandbox.ErrValidation})
	body := strings.NewReader(`{"id":"bad","image":"alpine"}`)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", body)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	assertErrorCode(t, rec.Body.Bytes(), "VALIDATION_ERROR")
}

func TestCreateSandboxMalformedBody(t *testing.T) {
	router := NewRouter(&fakeService{})
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestListSandboxes(t *testing.T) {
	router := NewRouter(&fakeService{listResult: []sandbox.Sandbox{{ID: "a", Image: "alpine"}}})
	req := httptest.NewRequest(http.MethodGet, "/sandboxes?page=0&size=10", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got []sandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("unexpected list response: %+v", got)
	}
}

func TestListSandboxesMissingQueryParams(t *testing.T) {
	router := NewRouter(&fakeService{})
	req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestGetSandboxNotFound(t *testing.T) {
	router := NewRouter(&fakeService{getErr: sandbox.ErrNotFound})
	req := httptest.NewRequest(http.MethodGet, "/sandboxes/missing", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	assertErrorCode(t, rec.Body.Bytes(), "SANDBOX_NOT_FOUND")
}

func TestDeleteSandboxConflict(t *testing.T) {
	router := NewRouter(&fakeService{deleteErr: sandbox.ErrExecuting})
	req := httptest.NewRequest(http.MethodDelete, "/sandboxes/id", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	assertErrorCode(t, rec.Body.Bytes(), "SANDBOX_EXECUTING")
}

func TestDeleteSandboxSuccess(t *testing.T) {
	router := NewRouter(&fakeService{})
	req := httptest.NewRequest(http.MethodDelete, "/sandboxes/id", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
}

func TestExecSandboxConflictBeforeStreaming(t *testing.T) {
	router := NewRouter(&fakeService{execErr: sandbox.ErrExecuting})
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/id/exec", strings.NewReader(`{"command":"echo hi"}`))
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected json content-type on pre-stream error, got %q", ct)
	}
	assertErrorCode(t, rec.Body.Bytes(), "SANDBOX_EXECUTING")
}

func TestExecSandboxStreamsSSE(t *testing.T) {
	start := func(w io.Writer) error {
		_, _ = w.Write([]byte("line one\nline two\n"))
		return nil
	}
	router := NewRouter(&fakeService{execStart: start})
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/id/exec", strings.NewReader(`{"command":"echo hi"}`))
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data: {"type":"stdout","value":"line one"}`) ||
		!strings.Contains(body, `data: {"type":"stdout","value":"line two"}`) {
		t.Fatalf("unexpected SSE body: %q", body)
	}
}

func TestExecSandboxRuntimeErrorSurfacesAsStreamContent(t *testing.T) {
	start := func(w io.Writer) error {
		_, _ = w.Write([]byte("partial output\n"))
		return errors.New("pod crashed")
	}
	router := NewRouter(&fakeService{execStart: start})
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/id/exec", strings.NewReader(`{"command":"echo hi"}`))
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	// Headers are already committed to 200 by the time a runtime error
	// happens, so it must show up as stream content, not a different status.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "partial output") || !strings.Contains(body, "pod crashed") {
		t.Fatalf("expected error to be appended to stream, got %q", body)
	}
}

func TestHealthz(t *testing.T) {
	router := NewRouter(&fakeService{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func assertErrorCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal error body: %v (body=%s)", err, body)
	}
	if got.Error.Code != want {
		t.Fatalf("expected error code %q, got %q", want, got.Error.Code)
	}
}
