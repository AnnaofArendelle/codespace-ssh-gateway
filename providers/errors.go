package providers

import (
	"errors"
	"fmt"
)

// Sentinel errors that the core reacts to. Providers must wrap these rather
// than inventing their own equivalents.
var (
	// ErrNotFound means the environment does not exist (a candidate for Create).
	ErrNotFound = errors.New("environment not found")
	// ErrNotSupported means the provider cannot do this at all.
	ErrNotSupported = errors.New("not supported by provider")
	// ErrAuth means the provider's credentials are missing, invalid or lack scope.
	ErrAuth = errors.New("provider authentication failed")
	// ErrConflict means a competing operation is already in progress upstream.
	ErrConflict = errors.New("conflicting operation in progress")
	// ErrUnavailable means the environment exists but cannot serve right now.
	ErrUnavailable = errors.New("environment unavailable")
)

// TemporaryError marks an error worth retrying with backoff.
type TemporaryError struct {
	Err error
}

func (e *TemporaryError) Error() string   { return e.Err.Error() }
func (e *TemporaryError) Unwrap() error   { return e.Err }
func (e *TemporaryError) Temporary() bool { return true }

// Temporary wraps err as retryable.
func Temporary(err error) error {
	if err == nil {
		return nil
	}
	return &TemporaryError{Err: err}
}

// Temporaryf wraps a formatted error as retryable.
func Temporaryf(format string, args ...any) error {
	return &TemporaryError{Err: fmt.Errorf(format, args...)}
}

// IsTemporary reports whether err (or anything it wraps) is retryable.
func IsTemporary(err error) bool {
	var t interface{ Temporary() bool }
	if errors.As(err, &t) {
		return t.Temporary()
	}
	return false
}

// NotFoundError describes a missing environment by id.
type NotFoundError struct {
	Provider string
	ID       string
}

func (e *NotFoundError) Error() string {
	if e.ID == "" {
		return fmt.Sprintf("%s: %v", e.Provider, ErrNotFound)
	}
	return fmt.Sprintf("%s: environment %q not found", e.Provider, e.ID)
}

func (e *NotFoundError) Unwrap() error { return ErrNotFound }
