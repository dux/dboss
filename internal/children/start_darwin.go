package children

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// startTime is the kernel's process start time, fixed at fork. The sysctl answers no bytes for a
// pid with no process.
func startTime(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(info.Proc.P_pid) != pid {
		return "", errGone
	}
	start := info.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", start.Sec, start.Usec), nil
}
