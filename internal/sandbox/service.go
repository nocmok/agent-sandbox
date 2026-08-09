package sandbox

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sandbox is the transport-agnostic view of a sandbox handed back to
// callers of Service.
type Sandbox struct {
	ID    string
	Image string
}

// jobRunner is the subset of Executor that Service depends on, declared
// here so tests can substitute a fake without touching Kubernetes.
type jobRunner interface {
	CreateJob(ctx context.Context, sandboxID, image, command string) error
	StreamLogs(ctx context.Context, sandboxID string, out io.Writer) error
	IsExecuting(ctx context.Context, sandboxID string) (bool, error)
	DeleteJob(ctx context.Context, sandboxID string) error
}

type Service struct {
	store *Store
	exec  jobRunner
}

func NewService(store *Store, exec jobRunner) *Service {
	return &Service{store: store, exec: exec}
}

func (s *Service) Create(ctx context.Context, id, image string) error {
	if err := validateID(id); err != nil {
		return err
	}
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("%w: image is required", ErrValidation)
	}
	return s.store.Create(ctx, id, image)
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

func (s *Service) Delete(ctx context.Context, id string) error {
	if _, err := s.store.Get(ctx, id); err != nil {
		return err
	}
	executing, err := s.exec.IsExecuting(ctx, id)
	if err != nil {
		return err
	}
	if executing {
		return ErrExecuting
	}
	return s.store.Delete(ctx, id)
}

// Exec validates the sandbox and command, then acquires the exec lock (by
// creating the Job) before returning. The returned start function performs
// the actual wait/stream/cleanup; it is only ever invoked after HTTP
// response headers have already committed to text/event-stream, so any
// error it returns must be surfaced as stream content, not a status code.
func (s *Service) Exec(ctx context.Context, id, command string) (start func(out io.Writer) error, err error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("%w: command is required", ErrValidation)
	}
	r, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.exec.CreateJob(ctx, id, r.Spec.Image, command); err != nil {
		return nil, err
	}

	start = func(out io.Writer) error {
		defer s.cleanupJob(id)
		return s.exec.StreamLogs(ctx, id, out)
	}
	return start, nil
}

func (s *Service) cleanupJob(id string) {
	// A fresh, un-canceled context: the request's context may already be
	// done (client disconnected) by the time streaming ends.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.exec.DeleteJob(ctx, id); err != nil {
		log.Printf("cleanup exec job for sandbox %s: %v", id, err)
	}
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
