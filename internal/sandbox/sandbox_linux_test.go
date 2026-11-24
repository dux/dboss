package sandbox

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestMain lets the test binary be the launcher (Command runs /proc/self/exe) and the probe the
// tests run inside the sandbox.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case Arg:
			Main(os.Args[2:])
		case "probe-op":
			if err := probeOp(os.Args[2:]); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			fmt.Println("ok")
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// probeOp is one thing a sandboxed process tries.
func probeOp(args []string) error {
	switch args[0] {
	case "read":
		_, err := os.ReadFile(args[1])
		return err
	case "list":
		_, err := os.ReadDir(args[1])
		return err
	case "write":
		return os.WriteFile(args[1], []byte("x"), 0o644)
	case "dial":
		conn, err := net.Dial(args[1], args[2])
		if err == nil {
			conn.Close()
		}
		return err
	case "listen":
		listener, err := net.Listen("tcp", "127.0.0.1:"+args[1])
		if err == nil {
			listener.Close()
		}
		return err
	case "socket":
		// A raw socket() call, as code that asks for MPTCP or SCTP directly makes it.
		protocol, _ := strconv.Atoi(args[1])
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, protocol)
		if err == nil {
			syscall.Close(fd)
		}
		return err
	case "signal":
		pid, _ := strconv.Atoi(args[1])
		return syscall.Kill(pid, 0)
	case "env":
		return checkEnv(args[1])
	}
	return fmt.Errorf("unknown op %q", args[0])
}

func checkEnv(name string) error {
	if _, ok := os.LookupEnv(name); ok {
		return nil
	}
	return fmt.Errorf("%s is unset", name)
}

func TestSandboxConfinesTheCommand(t *testing.T) {
	if err := Check(); err != nil {
		t.Skip("no sandbox here:", err)
	}
	// Everything under /tmp is the command's private tmpfs, writable as a whole, so the test
	// builds its apps folder in the working directory instead of the usual temp dir.
	root, err := os.MkdirTemp(".", "sandbox-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if root, err = filepath.Abs(root); err != nil {
		t.Fatal(err)
	}
	apps, runtimeDir, shared := filepath.Join(root, "apps"), filepath.Join(root, "runtime"), filepath.Join(root, "shared")
	app, other, sockets := filepath.Join(root, "real-app"), filepath.Join(apps, "other"), filepath.Join(apps, "sockets")
	for _, dir := range []string{apps, runtimeDir, shared, app, other, sockets, filepath.Join(app, "storage")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The policy names the app folder through a link inside the hidden apps folder, so the launcher
	// has to bind the target back and recreate the link.
	link := filepath.Join(apps, "shop")
	if err := os.Symlink(app, link); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(app, "config.yml"):   "ok",
		filepath.Join(app, ".env"):         "SECRET=1",
		filepath.Join(other, ".env"):       "SECRET=2",
		filepath.Join(shared, "asset.txt"): "ok",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(app, "probe")
	copyFile(t, mustExecutable(t), helper)

	allowedTCP, deniedTCP := listenTCP(t), listenTCP(t)
	control := listenUnix(t, filepath.Join(runtimeDir, "dboss.sock"))
	database := listenUnix(t, filepath.Join(sockets, "db.sock"))
	stray := listenUnix(t, filepath.Join(other, "app.sock"))
	abstract := listenUnix(t, "@dboss-sandbox-test-"+strconv.Itoa(os.Getpid()))
	freeBind, otherBind := freePorts(t)

	policy := Policy{
		Dir:     link,
		Read:    []string{shared},
		Write:   []string{filepath.Join(app, "storage")},
		Deny:    []string{filepath.Join(app, ".env"), runtimeDir},
		Hide:    []string{apps},
		Sockets: []string{sockets},
		Bind:    []int{freeBind},
		Connect: []int{allowedTCP},
	}
	t.Setenv("DBOSS_SANDBOX_TEST", "1")
	for _, test := range []struct {
		name  string
		op    []string
		allow bool
	}{
		{"reads its own folder through the link", []string{"read", filepath.Join(link, "config.yml")}, true},
		{"reads a listed folder", []string{"read", filepath.Join(shared, "asset.txt")}, true},
		{"reads the system", []string{"read", "/etc/passwd"}, true},
		{"keeps its environment", []string{"env", "DBOSS_SANDBOX_TEST"}, true},
		{"denied file inside its folder", []string{"read", filepath.Join(app, ".env")}, false},
		{"another app's file", []string{"read", filepath.Join(other, ".env")}, false},
		{"the apps folder listing", []string{"list", apps}, false},
		{"writes ./tmp", []string{"write", filepath.Join(link, "tmp", "x")}, true},
		{"writes ./log", []string{"write", filepath.Join(link, "log", "x")}, true},
		{"writes a listed folder", []string{"write", filepath.Join(app, "storage", "x")}, true},
		{"writes its private /tmp", []string{"write", "/tmp/x"}, true},
		{"writes the rest of its folder", []string{"write", filepath.Join(app, "x")}, false},
		{"writes outside", []string{"write", filepath.Join(shared, "x")}, false},
		{"dials a listed port", []string{"dial", "tcp", "127.0.0.1:" + strconv.Itoa(allowedTCP)}, true},
		{"dials another port", []string{"dial", "tcp", "127.0.0.1:" + strconv.Itoa(deniedTCP)}, false},
		{"listens on a listed port", []string{"listen", strconv.Itoa(freeBind)}, true},
		{"listens on another port", []string{"listen", strconv.Itoa(otherBind)}, false},
		{"opens a plain TCP socket", []string{"socket", "0"}, true},
		{"opens an MPTCP socket, which Landlock does not check", []string{"socket", "262"}, false},
		{"opens an SCTP socket", []string{"socket", "132"}, false},
		{"reaches a listed socket", []string{"dial", "unix", database}, true},
		{"reaches the control socket", []string{"dial", "unix", control}, false},
		{"reaches another app's socket", []string{"dial", "unix", stray}, false},
		{"reaches an abstract socket outside", []string{"dial", "unix", abstract}, false},
		{"reads the environment of a process outside", []string{"read", "/proc/" + strconv.Itoa(os.Getpid()) + "/environ"}, false},
		{"signals a process outside", []string{"signal", strconv.Itoa(os.Getpid())}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd, err := Command(policy, append([]string{helper, "probe-op"}, test.op...))
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.CombinedOutput()
			t.Logf("%s", out)
			if allowed := err == nil; allowed != test.allow {
				t.Fatalf("allowed = %v, want %v: %v %s", allowed, test.allow, err, out)
			}
		})
	}
	if _, err := os.Stat("/tmp/x"); err == nil {
		t.Fatal("the private /tmp leaked onto the host")
	}
}

func TestSandboxRefusesAPrivilegedBindPort(t *testing.T) {
	if err := Check(); err != nil {
		t.Skip("no sandbox here:", err)
	}
	if start, err := UnprivilegedPortStart(); err != nil || start == 0 {
		t.Skip("every port is unprivileged here")
	}
	cmd, err := Command(Policy{Dir: t.TempDir(), Bind: []int{25}}, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 126 || !strings.Contains(string(out), "ip_unprivileged_port_start") {
		t.Fatalf("err = %v, out = %s", err, out)
	}
}

func mustExecutable(t *testing.T) string {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func copyFile(t *testing.T, from, to string) {
	source, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
}

func listenTCP(t *testing.T) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go accept(listener)
	return listener.Addr().(*net.TCPAddr).Port
}

func listenUnix(t *testing.T, path string) string {
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go accept(listener)
	return path
}

func accept(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}

// freePorts picks two distinct free ports; both are held until picked, since the kernel may hand a
// port just released straight out again.
func freePorts(t *testing.T) (int, int) {
	var ports [2]int
	for i := range ports {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		ports[i] = listener.Addr().(*net.TCPAddr).Port
	}
	return ports[0], ports[1]
}
