//go:build !linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

var errLinuxOnly = errors.New("the sandbox needs Linux (Landlock and user namespaces)")

func Check() error { return errLinuxOnly }

func ABI() (int, error) { return 0, errLinuxOnly }

func UnprivilegedPortStart() (int, error) { return 0, errLinuxOnly }

func Command(Policy, []string) (*exec.Cmd, error) { return nil, errLinuxOnly }

func Main([]string) {
	fmt.Fprintln(os.Stderr, "dboss sandbox:", errLinuxOnly)
	os.Exit(126)
}
