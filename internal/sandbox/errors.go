package sandbox

import "errors"

var (
	ErrValidation    = errors.New("validation error")
	ErrNotFound      = errors.New("sandbox not found")
	ErrAlreadyExists = errors.New("sandbox already exists")
	ErrExecuting     = errors.New("an exec is already in progress for this sandbox")
	ErrFileNotFound  = errors.New("file not found in sandbox")
)
