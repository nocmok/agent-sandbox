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

// Service orchestrates sandbox lifecycle operations across the Repository,
// DockerClient and Storage interfaces, serializing exec/delete/files per
// sandbox via the DB-backed distributed lock (see locker.go).
type Service struct {
	repo    Repository
	docker  DockerClient
	storage Storage
}

func NewService(repo Repository, docker DockerClient, storage Storage) *Service {
	return &Service{
		repo:    repo,
		docker:  docker,
		storage: storage,
	}
}

func (s *Service) CreateSandbox(ctx context.Context, image, workspace string, idempotencyKey uuid.UUID) (Sandbox, error) {
	if strings.TrimSpace(image) == "" {
		return Sandbox{}, fmt.Errorf("%w: image is required", ErrValidation)
	}
	if !strings.HasPrefix(workspace, "/") {
		return Sandbox{}, fmt.Errorf("%w: workspace must be an absolute path", ErrValidation)
	}

	sb, _, err := s.repo.CreateSandbox(ctx, NewSandbox{
		ID:        uuid.New(),
		Image:     image,
		Workspace: workspace,
	}, idempotencyKey)
	if err != nil {
		return Sandbox{}, err
	}

	// Idempotent regardless of whether this call inserted a fresh row or
	// returned an existing one — covers retries after a partial failure here.
	if err := s.storage.AllocateSandboxDir(sb.ID); err != nil {
		return Sandbox{}, fmt.Errorf("allocating sandbox storage: %w", err)
	}

	return sb, nil
}

func (s *Service) GetSandbox(ctx context.Context, id uuid.UUID) (Sandbox, error) {
	return s.repo.GetSandbox(ctx, id)
}

func (s *Service) ListSandboxes(ctx context.Context, page, size int) ([]Sandbox, error) {
	if page < 0 {
		return nil, fmt.Errorf("%w: page must be >= 0", ErrValidation)
	}
	if size <= 0 {
		return nil, fmt.Errorf("%w: size must be > 0", ErrValidation)
	}
	return s.repo.ListSandboxes(ctx, page, size)
}

func (s *Service) DeleteSandbox(ctx context.Context, id uuid.UUID) error {
	return s.withLock(ctx, id, func(ctx context.Context) error {
		if _, err := s.repo.GetSandbox(ctx, id); err != nil {
			return err
		}
		if err := s.ensureNotExecuting(ctx, id); err != nil {
			return err
		}
		if err := s.storage.RemoveSandboxDir(id); err != nil {
			return fmt.Errorf("removing sandbox storage: %w", err)
		}
		return s.repo.DeleteSandbox(ctx, id)
	})
}

// Exec runs command in a freshly created container for the sandbox,
// streaming demuxed output to out as it arrives, then disposes of the
// container and volume. onStart is called once preflight checks (sandbox
// exists, lock acquired, not already executing) have passed and the docker
// run is about to begin - the httpapi layer uses it as the last point at
// which a clean, non-streamed error response is still possible, before SSE
// headers are committed.
func (s *Service) Exec(ctx context.Context, id uuid.UUID, command string, onStart func(), out io.Writer) (exitCode int, err error) {
	if strings.TrimSpace(command) == "" {
		return 0, fmt.Errorf("%w: command is required", ErrValidation)
	}

	err = s.withLock(ctx, id, func(ctx context.Context) error {
		t0 := time.Now()
		sb, err := s.repo.GetSandbox(ctx, id)
		if err != nil {
			return err
		}
		log.Printf("[profile] %s GetSandbox took %s", id, time.Since(t0))

		t1 := time.Now()
		if err := s.ensureNotExecuting(ctx, id); err != nil {
			return err
		}
		log.Printf("[profile] %s ensureNotExecuting took %s", id, time.Since(t1))

		onStart()

		name := id.String()
		// The NFS export is configured with fsid=0 (see
		// deploy/nfs-server/entrypoint.sh), which makes the export root
		// itself the NFSv4 pseudo-root - so the device path is just the
		// sandbox id, not the export's real filesystem path.
		nfsDevicePath := ":/" + name

		// Pulling unconditionally makes Docker round-trip to the registry to
		// resolve the tag on every single exec, even when the image is
		// already cached locally (measured ~2.5s of an otherwise ~200ms
		// exec). Only pull when the image isn't present locally already;
		// this self-heals if it was later evicted, at the cost of not
		// picking up a mutable tag's new digest until then.
		t2 := time.Now()
		exists, err := s.docker.ImageExists(ctx, sb.Image)
		if err != nil {
			return fmt.Errorf("checking image: %w", err)
		}
		if !exists {
			if err := s.docker.PullImage(ctx, sb.Image); err != nil {
				return fmt.Errorf("pulling image: %w", err)
			}
		}
		log.Printf("[profile] %s image check/pull (existed=%v) took %s", id, exists, time.Since(t2))

		t3 := time.Now()
		if err := s.docker.CreateVolume(ctx, name, nfsDevicePath); err != nil {
			return fmt.Errorf("creating volume: %w", err)
		}
		log.Printf("[profile] %s CreateVolume took %s", id, time.Since(t3))

		t4 := time.Now()
		code, runErr := s.docker.RunContainer(ctx, ContainerSpec{
			Image:      sb.Image,
			Name:       name,
			VolumeName: name,
			MountPath:  sb.Workspace,
		}, command, out)
		log.Printf("[profile] %s RunContainer (incl. command exec) took %s", id, time.Since(t4))

		t5 := time.Now()
		if rmErr := s.docker.RemoveVolume(ctx, name); rmErr != nil && runErr == nil {
			runErr = fmt.Errorf("removing volume: %w", rmErr)
		}
		log.Printf("[profile] %s RemoveVolume took %s", id, time.Since(t5))
		if runErr != nil {
			return runErr
		}
		exitCode = code
		return nil
	})
	return exitCode, err
}

// DownloadFile streams the sandbox's relPath to write while the distributed
// lock is held, rejecting the request with ErrExecuting if an exec is in
// flight.
func (s *Service) DownloadFile(ctx context.Context, id uuid.UUID, relPath string, write func(io.Reader) error) error {
	return s.withLock(ctx, id, func(ctx context.Context) error {
		if _, err := s.repo.GetSandbox(ctx, id); err != nil {
			return err
		}
		if err := s.ensureNotExecuting(ctx, id); err != nil {
			return err
		}
		f, err := s.storage.ReadFile(id, relPath)
		if err != nil {
			return err
		}
		defer f.Close()
		return write(f)
	})
}

// UploadFile writes r to the sandbox's relPath while the distributed lock is
// held, rejecting the request with ErrExecuting if an exec is in flight.
func (s *Service) UploadFile(ctx context.Context, id uuid.UUID, relPath string, r io.Reader) error {
	return s.withLock(ctx, id, func(ctx context.Context) error {
		if _, err := s.repo.GetSandbox(ctx, id); err != nil {
			return err
		}
		if err := s.ensureNotExecuting(ctx, id); err != nil {
			return err
		}
		return s.storage.WriteFile(id, relPath, r)
	})
}

// ensureNotExecuting implements the doc's three-step execution check: no
// container by this name means not executing; a running container means
// executing (ErrExecuting); an exited-but-not-yet-removed container is a
// leftover from a crashed exec and is cleaned up (container + volume) as a
// safety net before reporting not executing.
func (s *Service) ensureNotExecuting(ctx context.Context, id uuid.UUID) error {
	name := id.String()
	status, err := s.docker.ContainerStatus(ctx, name)
	if err != nil {
		return fmt.Errorf("checking container status: %w", err)
	}
	switch status {
	case ContainerStatusRunning:
		return ErrExecuting
	case ContainerStatusExited:
		if err := s.docker.RemoveContainer(ctx, name); err != nil {
			return fmt.Errorf("removing leftover container: %w", err)
		}
		if err := s.docker.RemoveVolume(ctx, name); err != nil {
			return fmt.Errorf("removing leftover volume: %w", err)
		}
	}
	return nil
}
