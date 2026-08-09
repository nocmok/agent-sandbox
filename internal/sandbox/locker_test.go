package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestWithLock_ExcludesConcurrentCallOnSameSandbox(t *testing.T) {
	svc, _, _, _ := newTestService()
	sb := mustCreate(t, svc)

	if err := svc.repo.(*fakeRepository).markLocked(sb.ID); err != nil {
		t.Fatalf("markLocked: %v", err)
	}

	err := svc.withLock(context.Background(), sb.ID, func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked while already locked, got %v", err)
	}
}

func TestWithLock_ReleasesOnSuccessAndOnError(t *testing.T) {
	svc, repo, _, _ := newTestService()
	sb := mustCreate(t, svc)

	if err := svc.withLock(context.Background(), sb.ID, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("withLock (success): %v", err)
	}
	if _, locked := repo.lockedUntil[sb.ID]; locked {
		t.Fatal("expected lock to be released after successful fn")
	}

	boom := errors.New("boom")
	if err := svc.withLock(context.Background(), sb.ID, func(ctx context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("expected fn's error to propagate, got %v", err)
	}
	if _, locked := repo.lockedUntil[sb.ID]; locked {
		t.Fatal("expected lock to be released after failing fn")
	}
}

func TestWithLock_NotFound(t *testing.T) {
	svc, _, _, _ := newTestService()
	err := svc.withLock(context.Background(), uuid.New(), func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// markLocked is a test helper simulating another in-flight operation already
// holding the lock.
func (r *fakeRepository) markLocked(id uuid.UUID) error {
	acquired, err := r.TryAcquireLock(context.Background(), id)
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("already locked")
	}
	return nil
}
