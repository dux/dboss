//go:build linux

package daemon

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// dropAmbientCaps clears the ambient capability set on every thread once the privileged ports are
// bound. The systemd unit grants CAP_NET_BIND_SERVICE as an ambient capability, and an ambient
// capability survives exec, so every app, hook and job would inherit it: a process that can bind a
// port below 1024 itself, and one a plain `lsof` by the same user cannot inspect (ptrace access
// needs the caller's capabilities to cover the target's), which hides its sockets from
// `dboss doctor`. dboss keeps its own effective set; only what its children inherit goes.
func dropAmbientCaps() error {
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
