//go:build e2e

// Package e2e runs the real dboss binary against a copy of ./demo: one host session with the
// four demo apps, driven through the proxy, the HTTP API, the console and the CLI. It needs bun,
// lsof and the Ruby from demo/apps/sinatra/mise.toml with its gems installed; run it with
// `make e2e`.
package e2e

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	token      = "e2e-token"
	portWindow = 40
)

// host is the one session every test drives.
var host struct {
	root       string // temp host folder: dboss.yaml, apps/, .dboss/
	bin        string
	config     string
	proxy      string // 127.0.0.1:<port> of proxy.listen
	console    string // 127.0.0.1:<first port of ports>
	daemonLog  string
	notify     *notifySink
	daemon     *exec.Cmd
	daemonDone chan error
}

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		code = 1
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	for _, tool := range []string{"bun", "lsof", "mise"} {
		if _, err := exec.LookPath(tool); err != nil {
			return 1, fmt.Errorf("%s is not on PATH", tool)
		}
	}
	root, err := os.MkdirTemp("", "dboss-e2e-")
	if err != nil {
		return 1, err
	}
	// Symlink-free, so the paths dboss reports match the ones the tests build.
	if host.root, err = filepath.EvalSymlinks(root); err != nil {
		return 1, err
	}
	keep := os.Getenv("E2E_KEEP") != ""
	defer func() {
		if !keep {
			os.RemoveAll(host.root)
		}
	}()
	if err := build(); err != nil {
		return 1, err
	}
	if err := prepareHost(); err != nil {
		return 1, err
	}
	if err := startDaemon(); err != nil {
		stopDaemon()
		return 1, err
	}
	code := m.Run()
	stopDaemon()
	if code != 0 || keep {
		fmt.Fprintf(os.Stderr, "e2e: host folder %s, daemon log %s\n", host.root, host.daemonLog)
		keep = true
	}
	return code, nil
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}

func build() error {
	host.bin = filepath.Join(host.root, "dboss")
	cmd := exec.Command("go", "build", "-ldflags", "-X dboss/internal/version.Version=v1", "-o", host.bin, "./cmd/dboss")
	cmd.Dir = repoRoot()
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, output)
	}
	return nil
}

// prepareHost copies the demo apps and writes a host file from ./demo/dboss.yaml with the
// addresses moved: a free port window, a loopback proxy port and the test's notify sink.
func prepareHost() error {
	demo := filepath.Join(repoRoot(), "demo")
	if err := copyApps(filepath.Join(demo, "apps"), filepath.Join(host.root, "apps")); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(demo, "dboss.yaml"))
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return err
	}
	first, err := freeWindow(portWindow)
	if err != nil {
		return err
	}
	proxyPort, err := freePort()
	if err != nil {
		return err
	}
	host.notify = startNotifySink()
	host.proxy = "127.0.0.1:" + strconv.Itoa(proxyPort)
	host.console = "127.0.0.1:" + strconv.Itoa(first)
	cfg["ports"] = []int{first, first + portWindow - 1}
	cfg["proxy"] = map[string]any{"listen": []string{host.proxy}}
	cfg["tokens"] = map[string]any{"dboss": token}
	cfg["notify"] = map[string]any{"url": host.notify.url}
	cfg["postgres"] = false
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	host.config = filepath.Join(host.root, "dboss.yaml")
	return os.WriteFile(host.config, out, 0o644)
}

// copyApps copies the demo apps without their runtime folders, logs and local overrides.
func copyApps(from, to string) error {
	skip := map[string]bool{".dboss": true, ".appboss": true, "log": true, "tmp": true, "dboss.local.yaml": true, ".DS_Store": true}
	return filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip[entry.Name()] {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// freeWindow finds size consecutive ports nothing listens on. dboss clears every listener in
// its range at start, so the window must be empty before it is handed over.
func freeWindow(size int) (int, error) {
	for attempt := 0; attempt < 50; attempt++ {
		first := 20000 + (os.Getpid()*7+attempt*size*3)%30000
		ok := true
		for port := first; port < first+size && ok; port++ {
			ok = portFree(port)
		}
		if ok {
			return first, nil
		}
	}
	return 0, fmt.Errorf("no free window of %d ports", size)
}

func portFree(port int) bool {
	for _, address := range []string{"127.0.0.1", "0.0.0.0"} {
		listener, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		listener.Close()
	}
	return true
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func startDaemon() error {
	host.daemonLog = filepath.Join(host.root, "daemon.log")
	logFile, err := os.Create(host.daemonLog)
	if err != nil {
		return err
	}
	cmd := exec.Command(host.bin, "start", "-c", host.config)
	cmd.Dir = host.root
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = environment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	host.daemon = cmd
	host.daemonDone = make(chan error, 1)
	go func() {
		host.daemonDone <- cmd.Wait()
		logFile.Close()
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-host.daemonDone:
			host.daemonDone <- err
			return fmt.Errorf("daemon exited: %v\n%s", err, readFile(host.daemonLog))
		default:
		}
		response, err := http.Get("http://" + host.console + "/readyz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("console never answered /readyz\n%s", readFile(host.daemonLog))
}

// environment trusts the copied apps' mise.toml files, which mise refuses in a new folder, and
// drops DEEPSEEK_API_KEY, so the vibe demo's chat stays off and the suite never calls DeepSeek.
func environment() []string {
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "DEEPSEEK_API_KEY=") })
	return append(env, "MISE_TRUSTED_CONFIG_PATHS="+filepath.Join(host.root, "apps"))
}

// stopDaemon stops the session like Ctrl-C, then makes sure nothing it started survives.
func stopDaemon() {
	if host.daemon == nil {
		return
	}
	_ = host.daemon.Process.Signal(os.Interrupt)
	select {
	case <-host.daemonDone:
	case <-time.After(40 * time.Second):
		_ = syscall.Kill(-host.daemon.Process.Pid, syscall.SIGKILL)
		<-host.daemonDone
	}
	_ = exec.Command(host.bin, "kill", "-c", host.config).Run()
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}
