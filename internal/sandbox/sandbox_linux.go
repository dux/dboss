package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// The system part of every sandbox. systemRead is what a program needs to start and run: the
// system folders, /etc, /proc and /sys (another process's environment and memory stay closed,
// Landlock refuses ptrace access across sandboxes) and the random devices. deviceWrite are the
// sink devices. privateDirs get a fresh, empty tmpfs per command, so nothing is shared through
// them. hiddenDirs are covered with an empty folder: /run holds the Unix sockets of every service
// on the box (the database, Docker, the dboss control socket link) and only Sockets come back.
var (
	systemRead  = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/proc", "/sys", "/dev/random", "/dev/urandom"}
	deviceWrite = []string{"/dev/null", "/dev/zero", "/dev/full"}
	privateDirs = []string{"/tmp", "/var/tmp", "/dev/shm"}
	hiddenDirs  = []string{"/run"}
)

var checked struct {
	once sync.Once
	err  error
}

// Check reports whether this machine can run a sandbox: Landlock MinABI and an unprivileged user
// namespace that may mount. It probes once per process.
func Check() error {
	checked.once.Do(func() { checked.err = check() })
	return checked.err
}

func check() error {
	if auditArch == 0 {
		return errors.New("the sandbox supports amd64 and arm64 only")
	}
	version, err := ABI()
	if err != nil {
		return err
	}
	if version < MinABI {
		return fmt.Errorf("the kernel has Landlock ABI %d and the sandbox needs %d (Linux 6.12+)", version, MinABI)
	}
	cmd := exec.Command("/proc/self/exe", Arg, "--probe")
	cmd.SysProcAttr = isolation()
	if out, err := cmd.CombinedOutput(); err != nil {
		if text := strings.TrimSpace(string(out)); text != "" {
			return errors.New(text)
		}
		return fmt.Errorf("start a process in a new user namespace: %w (unprivileged user namespaces may be disabled)", err)
	}
	return nil
}

// Command starts argv under the policy: the dboss binary itself (/proc/self/exe, the running
// version even after an update replaced the file) as the launcher, in a new user and mount
// namespace that maps only the current user and group.
func Command(policy Policy, argv []string) (*exec.Cmd, error) {
	data, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("/proc/self/exe", append([]string{Arg, string(data), "--"}, argv...)...)
	cmd.SysProcAttr = isolation()
	return cmd, nil
}

// isolation keeps CAP_SYS_ADMIN of the new user namespace across the launcher's exec, so it can
// mount; it never holds any capability outside that namespace.
func isolation() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}},
		AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN},
	}
}

// UnprivilegedPortStart is the lowest port a process without CAP_NET_BIND_SERVICE may bind. A
// sandboxed command has no capability outside its user namespace, so a bind port below it needs
// net.ipv4.ip_unprivileged_port_start lowered.
func UnprivilegedPortStart() (int, error) {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// Main is the launcher: `dboss sandbox-exec <policy json> -- <argv...>`. It never returns: it
// execs the command, or prints why it could not and exits 126, the shell's "cannot execute".
func Main(args []string) {
	// Landlock and no_new_privs bind the calling thread, so the thread that sets them must be the
	// one that execs.
	runtime.LockOSThread()
	var err error
	if len(args) == 1 && args[0] == "--probe" {
		err = probe()
	} else {
		err = launch(args)
	}
	fmt.Fprintln(os.Stderr, "dboss sandbox:", err)
	os.Exit(126)
}

func probe() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("mount in a user namespace: %w (unprivileged user namespaces may be restricted)", err)
	}
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount in a user namespace: %w (unprivileged user namespaces may be restricted)", err)
	}
	os.Exit(0)
	return nil
}

func launch(args []string) error {
	if len(args) < 3 || args[1] != "--" {
		return errors.New("usage: dboss " + Arg + " <policy> -- <command> [args...]")
	}
	var policy Policy
	if err := json.Unmarshal([]byte(args[0]), &policy); err != nil {
		return fmt.Errorf("read the policy: %w", err)
	}
	argv := args[2:]
	if err := checkBind(policy.Bind); err != nil {
		return err
	}
	if !filepath.IsAbs(argv[0]) {
		path, err := exec.LookPath(argv[0])
		if err != nil {
			return err
		}
		argv[0] = path
	}
	// The app's own folders and the write paths it names are created when missing, while the
	// launcher still sees the real file system.
	write := append([]string{filepath.Join(policy.Dir, "tmp"), filepath.Join(policy.Dir, "log")}, policy.Write...)
	for _, dir := range write {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	home, _ := os.UserHomeDir()
	read := slices.Concat(systemRead, toolchainDirs(os.Getenv("PATH"), home), []string{policy.Dir}, policy.Read)
	// /etc/resolv.conf is often a link into /run, which the sandbox hides; its folder comes back.
	if target, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil {
		read = append(read, filepath.Dir(target))
	}
	write = slices.Concat(deviceWrite, privateDirs, write)
	readKeeps, writeKeeps := resolveKeeps(read), resolveKeeps(write)
	keeps := slices.Concat(readKeeps, writeKeeps, resolveKeeps(policy.Sockets))
	plan := mountPlan(resolveAll(privateDirs), resolveAll(slices.Concat(hiddenDirs, policy.Hide)), resolveAll(policy.Deny), resolved(keeps))
	if err := mount(plan, keeps); err != nil {
		return err
	}
	if err := restrict(resolved(readKeeps), resolved(writeKeeps), policy.Bind, policy.Connect); err != nil {
		return err
	}
	if err := filterSockets(); err != nil {
		return err
	}
	if err := unix.Chdir(policy.Dir); err != nil {
		return fmt.Errorf("enter %s: %w", policy.Dir, err)
	}
	if err := dropCapabilities(); err != nil {
		return err
	}
	return unix.Exec(argv[0], argv, os.Environ())
}

// checkBind refuses a bind port the kernel would refuse anyway, with the setting that fixes it,
// instead of letting the app fail with a bare permission denied.
func checkBind(ports []int) error {
	start, err := UnprivilegedPortStart()
	if err != nil {
		return nil
	}
	for _, port := range ports {
		if port < start {
			return fmt.Errorf("bind port %d is below net.ipv4.ip_unprivileged_port_start (%d); a sandboxed process holds no capability on the host, so lower it: sysctl -w net.ipv4.ip_unprivileged_port_start=%d", port, start, port)
		}
	}
	return nil
}

// dropCapabilities empties the ambient set, so the exec leaves the command no capability even
// inside its namespace. A launcher started as root is root in the namespace too and would keep
// them across the exec, so its bounding set is emptied as well.
func dropCapabilities() error {
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clear ambient capabilities: %w", err)
	}
	if os.Geteuid() != 0 {
		return nil
	}
	for capability := 0; capability <= unix.CAP_LAST_CAP; capability++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("drop capability %d: %w", capability, err)
		}
	}
	return nil
}

// resolveAll follows symlinks, since a link inside a hidden folder stops resolving once it is
// covered, and skips what does not exist.
func resolveAll(paths []string) []string { return resolved(resolveKeeps(paths)) }

func resolved(keeps []keepPath) []string {
	result := make([]string, 0, len(keeps))
	for _, keep := range keeps {
		if !slices.Contains(result, keep.resolved) {
			result = append(result, keep.resolved)
		}
	}
	return result
}

func resolveKeeps(paths []string) []keepPath {
	var result []keepPath
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		keep := keepPath{written: filepath.Clean(path), resolved: resolved}
		if !slices.Contains(result, keep) {
			result = append(result, keep)
		}
	}
	return result
}
