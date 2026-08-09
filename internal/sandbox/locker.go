package sandbox

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
)

// lockRenewInterval is how often withLock refreshes locked_until while an
// operation is in flight, per the doc's Concurrency algorithm (every 5s
// against a 1-minute lease).
const lockRenewInterval = 5 * time.Second

// withLock implements the doc's per-sandbox distributed lock: acquire via
// Repository.TryAcquireLock, run fn while a background goroutine periodically
// renews the lease, then always release. Used by every operation the doc
// lists as lock-holding (exec, delete, files).
func (s *Service) withLock(ctx context.Context, id uuid.UUID, fn func(ctx context.Context) error) error {
	acquired, err := s.repo.TryAcquireLock(ctx, id)
	if err != nil {
		return err
	}
	if !acquired {
		return ErrLocked
	}

	renewCtx, stopRenew := context.WithCancel(context.Background())
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(lockRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				if err := s.repo.RenewLock(renewCtx, id); err != nil {
					log.Printf("renewing lock for sandbox %s: %v", id, err)
				}
			}
		}
	}()

	err = fn(ctx)

	stopRenew()
	<-renewDone
	if relErr := s.repo.ReleaseLock(context.Background(), id); relErr != nil {
		log.Printf("releasing lock for sandbox %s: %v", id, relErr)
	}
	return err
}
