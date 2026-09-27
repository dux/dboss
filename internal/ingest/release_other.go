//go:build !linux

package ingest

import (
	"errors"
	"os"
)

// Only Linux can cut the head off a file in place; elsewhere (a macOS dev session) app log files
// keep growing as the app writes them.
const canRelease = false

func collapseHead(*os.File, int64) error { return errors.ErrUnsupported }

func blockSize(os.FileInfo) int64 { return 4096 }
