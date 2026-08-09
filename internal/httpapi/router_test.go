package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"agent-sandbox/internal/sandbox"

	"github.com/google/uuid"
)

// Minimal in-memory Repository/DockerClient/Storage, local to this package's
// tests so httpapi tests don't reach into internal/sandbox's own test-only
// fakes.

type fakeRepo struct {
	mu          sync.Mutex
	sandboxes   map[uuid.UUID]sandbox.Sandbox
	deleted     map[uuid.UUID]bool
	idemToID    map[uuid.UUID]uuid.UUID
	lockedUntil map[uuid.UUID]time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		sandboxes:   make(map[uuid.UUID]sandbox.Sandbox),
		deleted:     make(map[uuid.UUID]bool),
		idemToID:    make(map[uuid.UUID]uuid.UUID),
		lockedUntil: make(map[uuid.UUID]time.Time),
	}
}

func (r *fakeRepo) CreateSandbox(ctx context.Context, in sandbox.NewSandbox, idemKey uuid.UUID) (sandbox.Sandbox, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existingID, ok := r.idemToID[idemKey]; ok && !r.deleted[existingID] {
		return r.sandboxes[existingID], true, nil
	}
	sb := sandbox.Sandbox{ID: in.ID, Image: in.Image, Workspace: in.Workspace, CreatedAt: time.Now()}
	r.sandboxes[in.ID] = sb
	r.idemToID[idemKey] = in.ID
	return sb, false, nil
}

func (r *fakeRepo) GetSandbox(ctx context.Context, id uuid.UUID) (sandbox.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb, ok := r.sandboxes[id]
	if !ok || r.deleted[id] {
		return sandbox.Sandbox{}, sandbox.ErrNotFound
	}
	return sb, nil
}

func (r *fakeRepo) ListSandboxes(ctx context.Context, page, size int) ([]sandbox.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []sandbox.Sandbox
	for id, sb := range r.sandboxes {
		if r.deleted[id] {
			continue
		}
		out = append(out, sb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })

	start := page * size
	if start >= len(out) {
		return nil, nil
	}
	end := start + size
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], nil
}

func (r *fakeRepo) DeleteSandbox(ctx context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sandboxes[id]; !ok || r.deleted[id] {
		return sandbox.ErrNotFound
	}
	r.deleted[id] = true
	return nil
}

func (r *fakeRepo) TryAcquireLock(ctx context.Context, id uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sandboxes[id]; !ok || r.deleted[id] {
		return false, sandbox.ErrNotFound
	}
	if until, ok := r.lockedUntil[id]; ok && until.After(time.Now()) {
		return false, nil
	}
	r.lockedUntil[id] = time.Now().Add(time.Minute)
	return true, nil
}

func (r *fakeRepo) RenewLock(ctx context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lockedUntil[id] = time.Now().Add(time.Minute)
	return nil
}

func (r *fakeRepo) ReleaseLock(ctx context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.lockedUntil, id)
	return nil
}

type fakeDockerClient struct{}

func (fakeDockerClient) PullImage(ctx context.Context, image string) error         { return nil }
func (fakeDockerClient) CreateVolume(ctx context.Context, name, path string) error { return nil }
func (fakeDockerClient) RemoveVolume(ctx context.Context, name string) error       { return nil }
func (fakeDockerClient) ContainerStatus(ctx context.Context, name string) (sandbox.ContainerStatus, error) {
	return sandbox.ContainerStatusNotExist, nil
}
func (fakeDockerClient) RemoveContainer(ctx context.Context, name string) error { return nil }
func (fakeDockerClient) RunContainer(ctx context.Context, spec sandbox.ContainerSpec, command string, out io.Writer) (int, error) {
	_, _ = out.Write([]byte("ok\n"))
	return 0, nil
}

// fakeStorage stands in for internal/storage.NFS in tests that exercise
// sandbox lifecycle only (create/delete), not the Files API.
type fakeStorage struct{}

func (fakeStorage) AllocateSandboxDir(id uuid.UUID) error { return nil }
func (fakeStorage) RemoveSandboxDir(id uuid.UUID) error   { return nil }
func (fakeStorage) ReadFile(id uuid.UUID, relPath string) (io.ReadCloser, error) {
	return nil, sandbox.ErrNotFound
}
func (fakeStorage) WriteFile(id uuid.UUID, relPath string, r io.Reader) error { return nil }

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	svc := sandbox.NewService(newFakeRepo(), fakeDockerClient{}, fakeStorage{})
	return NewRouter(svc)
}

func doRequest(t *testing.T, router http.Handler, method, path string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error body %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestCreateSandbox_MissingIdempotencyKey(t *testing.T) {
	router := newTestRouter(t)
	rec := doRequest(t, router, http.MethodPost, "/sandboxes", nil, map[string]any{
		"image": "alpine", "workspace": "/workspace",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if decodeError(t, rec).Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("expected VALIDATION_ERROR, got %s", rec.Body.String())
	}
}

func TestCreateSandbox_BadJSONBody(t *testing.T) {
	router := newTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Idempotency-Key", uuid.New().String())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateSandbox_HappyPathAndGet(t *testing.T) {
	router := newTestRouter(t)
	rec := doRequest(t, router, http.MethodPost, "/sandboxes", map[string]string{"Idempotency-Key": uuid.New().String()}, map[string]any{
		"image": "alpine", "workspace": "/workspace",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := created["id"]
	if id == "" {
		t.Fatal("expected non-empty id")
	}

	getRec := doRequest(t, router, http.MethodGet, "/sandboxes/"+id, nil, nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var sb sandboxDTO
	if err := json.Unmarshal(getRec.Body.Bytes(), &sb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sb.Workspace != "/workspace" {
		t.Fatalf("expected workspace /workspace, got %s", sb.Workspace)
	}
}

func TestCreateSandbox_IdempotentReplay(t *testing.T) {
	router := newTestRouter(t)
	idemKey := uuid.New().String()
	rec1 := doRequest(t, router, http.MethodPost, "/sandboxes", map[string]string{"Idempotency-Key": idemKey}, map[string]any{
		"image": "alpine", "workspace": "/workspace",
	})
	rec2 := doRequest(t, router, http.MethodPost, "/sandboxes", map[string]string{"Idempotency-Key": idemKey}, map[string]any{
		"image": "alpine", "workspace": "/workspace",
	})
	var created1, created2 map[string]string
	json.Unmarshal(rec1.Body.Bytes(), &created1)
	json.Unmarshal(rec2.Body.Bytes(), &created2)
	if created1["id"] != created2["id"] {
		t.Fatalf("expected same id for repeated idempotency key, got %s and %s", created1["id"], created2["id"])
	}
}

func TestListSandboxes_MissingPageOrSize(t *testing.T) {
	router := newTestRouter(t)
	cases := []string{"/sandboxes", "/sandboxes?page=0", "/sandboxes?size=10"}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			rec := doRequest(t, router, http.MethodGet, path, nil, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if decodeError(t, rec).Error.Code != "VALIDATION_ERROR" {
				t.Fatalf("expected VALIDATION_ERROR, got %s", rec.Body.String())
			}
		})
	}
}

func TestListSandboxes_MalformedPageOrSize(t *testing.T) {
	router := newTestRouter(t)
	rec := doRequest(t, router, http.MethodGet, "/sandboxes?page=nope&size=10", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListSandboxes_Paginates(t *testing.T) {
	router := newTestRouter(t)
	for i := 0; i < 3; i++ {
		doRequest(t, router, http.MethodPost, "/sandboxes", map[string]string{"Idempotency-Key": uuid.New().String()}, map[string]any{
			"image": "alpine", "workspace": "/workspace",
		})
	}

	rec := doRequest(t, router, http.MethodGet, "/sandboxes?page=0&size=2", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var page0 []sandboxDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &page0); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page0) != 2 {
		t.Fatalf("expected 2 results on page 0, got %d", len(page0))
	}

	rec = doRequest(t, router, http.MethodGet, "/sandboxes?page=1&size=2", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var page1 []sandboxDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page1) != 1 {
		t.Fatalf("expected 1 result on page 1, got %d", len(page1))
	}
}

func TestGetSandbox_MalformedID(t *testing.T) {
	router := newTestRouter(t)
	rec := doRequest(t, router, http.MethodGet, "/sandboxes/not-a-uuid", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if decodeError(t, rec).Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("expected VALIDATION_ERROR, got %s", rec.Body.String())
	}
}

func TestGetSandbox_NotFound(t *testing.T) {
	router := newTestRouter(t)
	rec := doRequest(t, router, http.MethodGet, "/sandboxes/"+uuid.New().String(), nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if decodeError(t, rec).Error.Code != "SANDBOX_NOT_FOUND" {
		t.Fatalf("expected SANDBOX_NOT_FOUND, got %s", rec.Body.String())
	}
}

func TestDeleteSandbox_HappyPath(t *testing.T) {
	router := newTestRouter(t)
	idemKey := uuid.New().String()
	createRec := doRequest(t, router, http.MethodPost, "/sandboxes", map[string]string{"Idempotency-Key": idemKey}, map[string]any{
		"image": "alpine", "workspace": "/workspace",
	})
	var created map[string]string
	json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"]

	delRec := doRequest(t, router, http.MethodDelete, "/sandboxes/"+id, nil, nil)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", delRec.Code, delRec.Body.String())
	}

	getRec := doRequest(t, router, http.MethodGet, "/sandboxes/"+id, nil, nil)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", getRec.Code)
	}
}

func TestExec_HappyPath(t *testing.T) {
	router := newTestRouter(t)
	idemKey := uuid.New().String()
	createRec := doRequest(t, router, http.MethodPost, "/sandboxes", map[string]string{"Idempotency-Key": idemKey}, map[string]any{
		"image": "alpine", "workspace": "/workspace",
	})
	var created map[string]string
	json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"]

	execRec := doRequest(t, router, http.MethodPost, "/sandboxes/"+id+"/exec", nil, map[string]string{"command": "echo hi"})
	if execRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", execRec.Code, execRec.Body.String())
	}
	if !bytes.Contains(execRec.Body.Bytes(), []byte("ok")) {
		t.Fatalf("expected streamed output, got %s", execRec.Body.String())
	}
}

func TestExec_NotFoundReturns404(t *testing.T) {
	router := newTestRouter(t)
	execRec := doRequest(t, router, http.MethodPost, "/sandboxes/"+uuid.New().String()+"/exec", nil, map[string]string{"command": "echo hi"})
	if execRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", execRec.Code, execRec.Body.String())
	}
	if decodeError(t, execRec).Error.Code != "SANDBOX_NOT_FOUND" {
		t.Fatalf("expected SANDBOX_NOT_FOUND, got %s", execRec.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	router := newTestRouter(t)
	rec := doRequest(t, router, http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
