package sandbox

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// fakeRepository is an in-memory Repository used by service_test.go. It
// mirrors the Postgres schema's constraints closely enough for the tests:
// idempotency-key uniqueness among live sandboxes, and a locked_until-style
// lock keyed by sandbox id.
type fakeRepository struct {
	mu          sync.Mutex
	sandboxes   map[uuid.UUID]Sandbox
	deleted     map[uuid.UUID]bool
	idemToID    map[uuid.UUID]uuid.UUID
	lockedUntil map[uuid.UUID]time.Time
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		sandboxes:   make(map[uuid.UUID]Sandbox),
		deleted:     make(map[uuid.UUID]bool),
		idemToID:    make(map[uuid.UUID]uuid.UUID),
		lockedUntil: make(map[uuid.UUID]time.Time),
	}
}

func (r *fakeRepository) CreateSandbox(ctx context.Context, in NewSandbox, idempotencyKey uuid.UUID) (Sandbox, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existingID, ok := r.idemToID[idempotencyKey]; ok && !r.deleted[existingID] {
		return r.sandboxes[existingID], true, nil
	}

	sb := Sandbox{
		ID:        in.ID,
		Image:     in.Image,
		Workspace: in.Workspace,
		CreatedAt: time.Now(),
	}
	r.sandboxes[in.ID] = sb
	r.idemToID[idempotencyKey] = in.ID
	return sb, false, nil
}

func (r *fakeRepository) GetSandbox(ctx context.Context, id uuid.UUID) (Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb, ok := r.sandboxes[id]
	if !ok || r.deleted[id] {
		return Sandbox{}, ErrNotFound
	}
	return sb, nil
}

func (r *fakeRepository) ListSandboxes(ctx context.Context, page, size int) ([]Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Sandbox
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

func (r *fakeRepository) DeleteSandbox(ctx context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sandboxes[id]; !ok || r.deleted[id] {
		return ErrNotFound
	}
	r.deleted[id] = true
	return nil
}

func (r *fakeRepository) TryAcquireLock(ctx context.Context, id uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sandboxes[id]; !ok || r.deleted[id] {
		return false, ErrNotFound
	}
	if until, ok := r.lockedUntil[id]; ok && until.After(time.Now()) {
		return false, nil
	}
	r.lockedUntil[id] = time.Now().Add(time.Minute)
	return true, nil
}

func (r *fakeRepository) RenewLock(ctx context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lockedUntil[id] = time.Now().Add(time.Minute)
	return nil
}

func (r *fakeRepository) ReleaseLock(ctx context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.lockedUntil, id)
	return nil
}

// fakeDocker is an in-memory DockerClient recording call order for
// assertions, with injectable errors and an optional blocking hook to force
// deterministic goroutine interleaving in concurrency tests.
type fakeDocker struct {
	mu    sync.Mutex
	calls []string

	imageExists    bool
	imageExistsErr error
	pullErr        error

	createVolumeErr error
	removeVolumeErr error
	runErr          error

	// containerStatus is returned by ContainerStatus for any name; defaults
	// to ContainerStatusNotExist (zero value).
	containerStatus    ContainerStatus
	containerStatusErr error
	removeContainerErr error

	runExitCode int
	runOutput   string

	// blockOn, if set, makes the named method signal entered (once) and then
	// wait on unblock before returning. This lets a test deterministically
	// wait for a goroutine to be holding the per-sandbox lock inside that
	// call before acting, instead of racing on a sleep.
	blockOn string
	unblock chan struct{}
	entered chan struct{}
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{}
}

func (d *fakeDocker) record(name string) {
	d.mu.Lock()
	d.calls = append(d.calls, name)
	blocked := d.blockOn == name
	ch := d.unblock
	entered := d.entered
	d.mu.Unlock()
	if blocked && ch != nil {
		if entered != nil {
			close(entered)
		}
		<-ch
	}
}

func (d *fakeDocker) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func (d *fakeDocker) ImageExists(ctx context.Context, image string) (bool, error) {
	d.record("ImageExists")
	return d.imageExists, d.imageExistsErr
}

func (d *fakeDocker) PullImage(ctx context.Context, image string) error {
	d.record("PullImage")
	return d.pullErr
}

func (d *fakeDocker) CreateVolume(ctx context.Context, name string, nfsDevicePath string) error {
	d.record("CreateVolume")
	return d.createVolumeErr
}

func (d *fakeDocker) RemoveVolume(ctx context.Context, name string) error {
	d.record("RemoveVolume")
	return d.removeVolumeErr
}

func (d *fakeDocker) ContainerStatus(ctx context.Context, name string) (ContainerStatus, error) {
	d.record("ContainerStatus")
	return d.containerStatus, d.containerStatusErr
}

func (d *fakeDocker) RemoveContainer(ctx context.Context, name string) error {
	d.record("RemoveContainer")
	return d.removeContainerErr
}

func (d *fakeDocker) RunContainer(ctx context.Context, spec ContainerSpec, command string, out io.Writer) (int, error) {
	d.record("RunContainer")
	if d.runOutput != "" {
		_, _ = out.Write([]byte(d.runOutput))
	}
	return d.runExitCode, d.runErr
}

// fakeStorage is an in-memory Storage.
type fakeStorage struct {
	mu        sync.Mutex
	allocated map[uuid.UUID]bool
	removed   map[uuid.UUID]bool
	files     map[string][]byte
	allocErr  error
	removeErr error
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{
		allocated: make(map[uuid.UUID]bool),
		removed:   make(map[uuid.UUID]bool),
		files:     make(map[string][]byte),
	}
}

func (s *fakeStorage) AllocateSandboxDir(id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.allocErr != nil {
		return s.allocErr
	}
	s.allocated[id] = true
	return nil
}

func (s *fakeStorage) RemoveSandboxDir(id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removeErr != nil {
		return s.removeErr
	}
	s.removed[id] = true
	return nil
}

func (s *fakeStorage) key(id uuid.UUID, relPath string) string {
	return id.String() + "/" + relPath
}

func (s *fakeStorage) ReadFile(id uuid.UUID, relPath string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[s.key(id, relPath)]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeStorage) WriteFile(id uuid.UUID, relPath string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[s.key(id, relPath)] = data
	return nil
}
