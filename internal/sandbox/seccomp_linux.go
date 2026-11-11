package sandbox

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// filterSockets closes the ways around Landlock's TCP rules. Landlock checks TCP sockets only, so
// a stream socket of another protocol that still reaches TCP ports (MPTCP, which Go's listeners
// use by default and which falls back to plain TCP; SCTP; SMC) is refused at socket(), and the
// program falls back to TCP. io_uring is refused too, since it can open sockets without a socket
// call a filter sees. A system call of another ABI (32-bit code on amd64) kills the process, so
// the filter cannot be stepped around through a second syscall table.
func filterSockets() error {
	const (
		offsetNr       = 0
		offsetArch     = 4
		offsetDomain   = 16 // args[0], low half (both architectures are little-endian)
		offsetProtocol = 32 // args[2], low half
		x32SyscallBit  = 0x40000000
	)
	load := func(offset uint32) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: offset}
	}
	jump := func(op uint16, value uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | op | unix.BPF_K, K: value, Jt: jt, Jf: jf}
	}
	answer := func(value uint32) unix.SockFilter { return unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: value} }
	errno := func(err unix.Errno) unix.SockFilter { return answer(unix.SECCOMP_RET_ERRNO | uint32(err)) }
	filter := []unix.SockFilter{
		load(offsetArch),
		jump(unix.BPF_JEQ, auditArch, 1, 0),
		answer(unix.SECCOMP_RET_KILL_PROCESS),
		load(offsetNr),
		jump(unix.BPF_JGE, x32SyscallBit, 0, 1),
		errno(unix.ENOSYS),
		jump(unix.BPF_JEQ, unix.SYS_IO_URING_SETUP, 0, 1),
		errno(unix.EPERM),
		jump(unix.BPF_JEQ, unix.SYS_SOCKET, 1, 0),
		answer(unix.SECCOMP_RET_ALLOW),
		load(offsetDomain),
		jump(unix.BPF_JEQ, unix.AF_SMC, 0, 1),
		errno(unix.EAFNOSUPPORT),
		load(offsetProtocol),
		jump(unix.BPF_JEQ, unix.IPPROTO_MPTCP, 1, 0),
		jump(unix.BPF_JEQ, unix.IPPROTO_SCTP, 0, 1),
		errno(unix.EPROTONOSUPPORT),
		answer(unix.SECCOMP_RET_ALLOW),
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		return fmt.Errorf("install the socket filter: %w", err)
	}
	return nil
}
