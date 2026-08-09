package sandbox

import "errors"

// Sentinel errors returned by Service methods. internal/httpapi maps these to
// the HTTP status/code table from docs/00-agent-sandbox-architecture.md.
var (
	ErrNotFound   = errors.New("sandbox not found")
	ErrExecuting  = errors.New("sandbox is executing")
	ErrLocked     = errors.New("sandbox is locked")
	ErrValidation = errors.New("validation error")
)
