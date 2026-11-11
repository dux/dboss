package sandbox

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The rights the sandbox handles, all of Landlock ABI 6. A right a ruleset handles is denied
// everywhere no rule grants it; newer kernels' extra rights stay unhandled and so allowed.
const (
	fsAll     = unix.LANDLOCK_ACCESS_FS_IOCTL_DEV<<1 - 1
	fsRead    = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	fsOnFile  = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	netAll    = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	scopedAll = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL

	ruleNetPort = 2
)

// netPortAttr is struct landlock_net_port_attr, which x/sys/unix does not define.
type netPortAttr struct {
	allowed uint64
	port    uint64
}

// ABI returns the Landlock version the kernel supports.
func ABI() (int, error) {
	version, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		if errno == unix.ENOSYS || errno == unix.EOPNOTSUPP {
			return 0, errors.New("this kernel has Landlock disabled or not built in")
		}
		return 0, errno
	}
	return int(version), nil
}

// restrict confines the calling thread, and whatever it execs, to the given paths and ports. read
// paths may be read and executed, write paths are fully writable. A path that is gone or hidden
// is skipped: there is nothing there to allow.
func restrict(read, write []string, bind, connect []int) error {
	attr := unix.LandlockRulesetAttr{Access_fs: fsAll, Access_net: netAll, Scoped: scopedAll}
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create the Landlock ruleset: %w", errno)
	}
	defer unix.Close(int(ruleset))
	for _, rule := range []struct {
		paths  []string
		access uint64
	}{{read, fsRead}, {write, fsAll}} {
		for _, path := range rule.paths {
			if err := allowPath(int(ruleset), path, rule.access); err != nil {
				return err
			}
		}
	}
	for _, rule := range []struct {
		ports  []int
		access uint64
	}{{bind, unix.LANDLOCK_ACCESS_NET_BIND_TCP}, {connect, unix.LANDLOCK_ACCESS_NET_CONNECT_TCP}} {
		for _, port := range rule.ports {
			attr := netPortAttr{allowed: rule.access, port: uint64(port)}
			if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, ruleset, ruleNetPort, uintptr(unsafe.Pointer(&attr)), 0, 0, 0); errno != 0 {
				return fmt.Errorf("allow port %d: %w", port, errno)
			}
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, ruleset, 0, 0); errno != 0 {
		return fmt.Errorf("apply the Landlock ruleset: %w", errno)
	}
	return nil
}

func allowPath(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EACCES) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= fsOnFile
	}
	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&attr)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("allow %s: %w", path, errno)
	}
	return nil
}
