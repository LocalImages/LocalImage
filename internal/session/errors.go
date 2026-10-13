package session

import (
	"errors"
	"fmt"
)

// Sentinel errors. The API layer maps them to stable HTTP statuses and error
// codes so clients never have to parse messages.
var (
	ErrLimit    = errors.New("limit exceeded")
	ErrNotFound = errors.New("session not found")
	ErrInvalid  = errors.New("invalid request")
	ErrConflict = errors.New("conflict")
	// ErrNotReady means a browser image has not been built yet (see Prepare).
	ErrNotReady = errors.New("image not ready")
)

// LimitError carries the numbers behind a refused create.
type LimitError struct {
	Max    int // configured session limit
	Active int // sessions counted against it
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("session limit reached: %d of %d sessions are in use (LOCALIMAGE_MAX_SESSIONS=%d)", e.Active, e.Max, e.Max)
}
func (e *LimitError) Unwrap() error { return ErrLimit }

type messageError struct {
	msg string
	is  error
}

func (e *messageError) Error() string        { return e.msg }
func (e *messageError) Is(target error) bool { return target == e.is }

// invalid reports a request the caller got wrong.
func invalid(format string, a ...any) error {
	return &messageError{msg: fmt.Sprintf(format, a...), is: ErrInvalid}
}

// notReady reports that the image for browser still has to be built.
func notReady(browser string) error {
	return &messageError{
		msg: fmt.Sprintf("the %s image is not built yet: run `localimage pull %s` (or call prepare()) first, or create without noBuild to build it now", browser, browser),
		is:  ErrNotReady,
	}
}

// conflict reports a request that clashes with current state.
func conflict(format string, a ...any) error {
	return &messageError{msg: fmt.Sprintf(format, a...), is: ErrConflict}
}
