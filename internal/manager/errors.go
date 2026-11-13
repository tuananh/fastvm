package manager

import "errors"

// Sentinel errors mapped to HTTP status codes by the API.
var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid request")
	ErrConflict = errors.New("conflict")
)
