package sandbox

import (
	"io"

	"github.com/google/uuid"
)

// Storage is implemented by internal/storage against sandboxd's NFS server.
// Declared here (the consumer package) so unit tests can supply hand-written
// fakes.
type Storage interface {
	// AllocateSandboxDir ensures the sandbox's NFS-backed directory exists.
	// Idempotent: safe to call even if the directory already exists (retries
	// after a partial CreateSandbox failure rely on this).
	AllocateSandboxDir(id uuid.UUID) error

	RemoveSandboxDir(id uuid.UUID) error

	// ReadFile opens relPath for reading within the sandbox's volume. The
	// caller must Close the returned reader.
	ReadFile(id uuid.UUID, relPath string) (io.ReadCloser, error)

	// WriteFile writes r to relPath within the sandbox's volume, creating any
	// missing parent directories and overwriting the file if it exists.
	WriteFile(id uuid.UUID, relPath string, r io.Reader) error
}
