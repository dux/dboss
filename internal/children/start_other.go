//go:build !linux && !darwin

package children

import "errors"

// StartTime has no source here, so nothing signals a process it cannot verify.
func StartTime(int) (string, error) { return "", errors.ErrUnsupported }
