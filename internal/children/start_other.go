//go:build !linux && !darwin

package children

import "errors"

// startTime has no source here, so Reap never signals a group it cannot verify.
func startTime(int) (string, error) { return "", errors.ErrUnsupported }
