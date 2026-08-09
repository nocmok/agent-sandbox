package sandbox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Sandbox is the transport-agnostic view of a sandbox handed back to
// callers of Service.
type Sandbox struct {
	ID    string
	Image string
}

// podRunner is the subset of Executor that Service depends on, declared
// here so tests can substitute a fake without touching Kubernetes.
type podRunner interface {
	CreatePod(ctx context.Context, sandboxID, image string) error
	Exec(ctx context.Context, sandboxID, command string, out io.Writer) error
	DeletePod(ctx context.Context, sandboxID string) error
}

type Service struct {
	store *Store
	exec  podRunner

	// execLocks holds one mutex per sandbox, serializing Exec (and Delete)
	// calls against that sandbox's Pod. It's in-process only: correct as
	// long as sandboxd runs as a single instance. If sandboxd is ever
	// scaled to multiple replicas, this needs to become a distributed
	// lock (e.g. a coordination.k8s.io Lease per sandbox) instead.
	locksMu   sync.Mutex
	execLocks map[string]*sync.Mutex
}

func NewService(store *Store, exec podRunner) *Service {
	return &Service{store: store, exec: exec, execLocks: make(map[string]*sync.Mutex)}
}

// execLock returns the mutex serializing calls against sandboxID's Pod,
// creating it on first use.
func (s *Service) execLock(id string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	l, ok := s.execLocks[id]
	if !ok {
		l = &sync.Mutex{}
		s.execLocks[id] = l
	}
	return l
}

// forgetExecLock drops sandboxID's mutex once the sandbox is gone, so
// execLocks doesn't grow without bound over the service's lifetime.
func (s *Service) forgetExecLock(id string) {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	delete(s.execLocks, id)
}

// Create records the sandbox and starts its Pod, waiting for the Pod to be
// running before returning. If the Pod fails to start, the sandbox record
// is rolled back so the store — the source of truth for sandbox existence
// — never points at a Pod that doesn't exist.
func (s *Service) Create(ctx context.Context, id, image string) error {
	if err := validateID(id); err != nil {
		return err
	}
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("%w: image is required", ErrValidation)
	}
	if err := s.store.Create(ctx, id, image); err != nil {
		return err
	}
	if err := s.exec.CreatePod(ctx, id, image); err != nil {
		_ = s.store.Delete(ctx, id)
		return err
	}
	return nil
}

func (s *Service) List(ctx context.Context, page, size int) ([]Sandbox, error) {
	if page < 0 {
		return nil, fmt.Errorf("%w: page must be >= 0", ErrValidation)
	}
	if size <= 0 {
		return nil, fmt.Errorf("%w: size must be > 0", ErrValidation)
	}
	resources, err := s.store.List(ctx, page, size)
	if err != nil {
		return nil, err
	}
	sandboxes := make([]Sandbox, 0, len(resources))
	for _, r := range resources {
		sandboxes = append(sandboxes, toSandbox(r))
	}
	return sandboxes, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Sandbox, error) {
	r, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	sb := toSandbox(*r)
	return &sb, nil
}

// Delete removes the sandbox's Pod and then its record. It takes the same
// per-sandbox lock Exec does, so a sandbox can't be deleted out from under
// a command that's still running in it; if an exec is in flight, Delete
// fails fast with ErrExecuting instead of blocking.
func (s *Service) Delete(ctx context.Context, id string) error {
	if _, err := s.store.Get(ctx, id); err != nil {
		return err
	}

	lock := s.execLock(id)
	if !lock.TryLock() {
		return ErrExecuting
	}
	defer lock.Unlock()

	if err := s.exec.DeletePod(ctx, id); err != nil {
		return err
	}
	if err := s.store.Delete(ctx, id); err != nil {
		return err
	}
	s.forgetExecLock(id)
	return nil
}

// Exec validates the sandbox and command, then acquires the sandbox's exec
// lock before returning — failing fast with ErrExecuting if another exec
// is already in flight, rather than queuing behind it. The returned start
// function runs the command in the sandbox's already-running Pod, streams
// its output, and releases the lock; it is only ever invoked after HTTP
// response headers have already committed to text/event-stream, so any
// error it returns must be surfaced as stream content, not a status code.
func (s *Service) Exec(ctx context.Context, id, command string) (start func(out io.Writer) error, err error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("%w: command is required", ErrValidation)
	}
	if _, err := s.store.Get(ctx, id); err != nil {
		return nil, err
	}

	lock := s.execLock(id)
	if !lock.TryLock() {
		return nil, ErrExecuting
	}

	start = func(out io.Writer) error {
		defer lock.Unlock()
		return s.exec.Exec(ctx, id, command, out)
	}
	return start, nil
}

func validateID(id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("%w: id must be a uuid", ErrValidation)
	}
	if parsed.String() != id {
		return fmt.Errorf("%w: id must be a canonically-formatted lowercase uuid", ErrValidation)
	}
	return nil
}

func toSandbox(r SandboxResource) Sandbox {
	return Sandbox{ID: r.Name, Image: r.Spec.Image}
}
