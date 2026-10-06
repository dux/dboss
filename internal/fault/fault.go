// Package fault marks the errors a caller can fix (an unknown name, a bad param, a missing
// confirmation, a state or a feature that refuses the action), so a transport can tell them from
// the server's own failures: the HTTP API answers the first 400 and every other error 500.
package fault

import (
	"errors"
	"fmt"
)

type invalid struct{ err error }

func (e invalid) Error() string { return e.err.Error() }
func (e invalid) Unwrap() error { return e.err }

// Invalid marks err as the caller's; the message stays err's and errors.Is/As still see it.
func Invalid(err error) error {
	if err == nil {
		return nil
	}
	return invalid{err}
}

// Invalidf is Invalid(fmt.Errorf(format, args...)).
func Invalidf(format string, args ...any) error {
	return invalid{fmt.Errorf(format, args...)}
}

// IsInvalid reports whether err, or an error it wraps, was marked Invalid.
func IsInvalid(err error) bool {
	var target invalid
	return errors.As(err, &target)
}
