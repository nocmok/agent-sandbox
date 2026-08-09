package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockLeaseDuration is how long a lock acquired by TryAcquireLock is valid
// for before it's considered stale, per the doc's Concurrency algorithm.
const lockLeaseDuration = 1 * time.Minute

// Repository is implemented by PostgresRepository below. Declared as an
// interface so unit tests can supply hand-written fakes instead of a real
// database.
type Repository interface {
	// CreateSandbox inserts a new sandbox row keyed by idempotencyKey. If a
	// live row with that key already exists, it is returned instead with
	// existed=true and no new row is inserted.
	CreateSandbox(ctx context.Context, in NewSandbox, idempotencyKey uuid.UUID) (sb Sandbox, existed bool, err error)

	// GetSandbox returns ErrNotFound if no live sandbox has this id.
	GetSandbox(ctx context.Context, id uuid.UUID) (Sandbox, error)

	// ListSandboxes returns page (0-indexed) of up to size live sandboxes,
	// ordered by creation time.
	ListSandboxes(ctx context.Context, page, size int) ([]Sandbox, error)

	// DeleteSandbox soft-deletes the sandbox row. Returns ErrNotFound if missing.
	DeleteSandbox(ctx context.Context, id uuid.UUID) error

	// TryAcquireLock implements the algorithm from the doc's Concurrency
	// section: under a row-level lock, acquires the sandbox's distributed
	// lock (locked_until = now() + 1 minute) and reports whether it was free
	// to take. Returns ErrNotFound if the sandbox doesn't exist.
	TryAcquireLock(ctx context.Context, id uuid.UUID) (acquired bool, err error)

	// RenewLock extends locked_until for a lock the caller already holds.
	// Called periodically (every 5s) by Service while an operation is in
	// flight, per the doc.
	RenewLock(ctx context.Context, id uuid.UUID) error

	// ReleaseLock clears locked_until, freeing the lock.
	ReleaseLock(ctx context.Context, id uuid.UUID) error
}

// PostgresRepository implements Repository against Postgres using pgx.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) CreateSandbox(ctx context.Context, in NewSandbox, idempotencyKey uuid.UUID) (Sandbox, bool, error) {
	row := r.pool.QueryRow(ctx, `
		insert into sandbox (id, idempotency_key, image, workspace)
		values ($1, $2, $3, $4)
		on conflict (idempotency_key) where deleted_at is null do nothing
		returning id, image, workspace, created_at
	`, in.ID, idempotencyKey, in.Image, in.Workspace)

	sb, err := scanSandbox(row)
	if err == nil {
		return sb, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, false, fmt.Errorf("inserting sandbox: %w", err)
	}

	// No row inserted: a live sandbox already has this idempotency key.
	row = r.pool.QueryRow(ctx, `
		select id, image, workspace, created_at
		from sandbox
		where idempotency_key = $1 and deleted_at is null
	`, idempotencyKey)
	sb, err = scanSandbox(row)
	if err != nil {
		return Sandbox{}, false, fmt.Errorf("fetching existing sandbox: %w", err)
	}
	return sb, true, nil
}

func (r *PostgresRepository) GetSandbox(ctx context.Context, id uuid.UUID) (Sandbox, error) {
	row := r.pool.QueryRow(ctx, `
		select id, image, workspace, created_at
		from sandbox
		where id = $1 and deleted_at is null
	`, id)
	sb, err := scanSandbox(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	return sb, err
}

func (r *PostgresRepository) ListSandboxes(ctx context.Context, page, size int) ([]Sandbox, error) {
	rows, err := r.pool.Query(ctx, `
		select id, image, workspace, created_at
		from sandbox
		where deleted_at is null
		order by created_at
		limit $1 offset $2
	`, size, page*size)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) DeleteSandbox(ctx context.Context, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `
		update sandbox set deleted_at = now() where id = $1 and deleted_at is null
	`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TryAcquireLock implements the doc's lock algorithm: row-level lock, then
// check-and-set locked_until under that lock, all in one transaction so the
// check and the set are atomic against concurrent callers.
func (r *PostgresRepository) TryAcquireLock(ctx context.Context, id uuid.UUID) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var lockedUntil *time.Time
	err = tx.QueryRow(ctx, `
		select locked_until from sandbox
		where id = $1 and deleted_at is null
		for update
	`, id).Scan(&lockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}

	if lockedUntil != nil && lockedUntil.After(time.Now()) {
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		update sandbox set locked_until = now() + $2 where id = $1
	`, id, lockLeaseDuration); err != nil {
		return false, err
	}

	return true, tx.Commit(ctx)
}

func (r *PostgresRepository) RenewLock(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		update sandbox set locked_until = now() + $2 where id = $1
	`, id, lockLeaseDuration)
	return err
}

func (r *PostgresRepository) ReleaseLock(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		update sandbox set locked_until = null where id = $1
	`, id)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSandbox(row scanner) (Sandbox, error) {
	var sb Sandbox
	if err := row.Scan(&sb.ID, &sb.Image, &sb.Workspace, &sb.CreatedAt); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}
