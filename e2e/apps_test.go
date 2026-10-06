//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRollingRestartDropsNoRequest(t *testing.T) {
	before := ensureRunning(t, "bun").pids()
	var sent, failed atomic.Int64
	var firstFailure atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r, err := call{host: "bun.lvh.me", path: "/up"}.try()
				sent.Add(1)
				if err != nil || r.Status != http.StatusOK {
					failed.Add(1)
					firstFailure.CompareAndSwap(nil, fmt.Sprintf("%d %v %.200s", r.Status, err, r.Body))
				}
			}
		}()
	}
	api(t, "restart", map[string]any{"app": "bun"}, nil)
	after := waitState(t, "bun", "running").pids()
	close(stop)
	wg.Wait()
	if failed.Load() > 0 {
		t.Fatalf("%d of %d requests failed during the roll, first: %v", failed.Load(), sent.Load(), firstFailure.Load())
	}
	for name, pid := range before {
		if after[name] == pid {
			t.Fatalf("%s kept pid %d across the restart", name, pid)
		}
	}
	t.Logf("%d requests during the roll, none failed", sent.Load())
}

func TestKilledProcessIsRestarted(t *testing.T) {
	serving(t, "sinatra.lvh.me", "/up")
	s := waitState(t, "sinatra", "running")
	pid := s.pids()["job"]
	if pid == 0 {
		t.Fatalf("no job process in %+v", s.Processes)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// restart: always under processes.job brings it back on a new pid.
	eventually(t, 30*time.Second, func() error {
		if now := status(t, "sinatra").pids()["job"]; now == 0 || now == pid {
			return fmt.Errorf("job pid %d", now)
		}
		return nil
	})
}

func TestProcessStopAndStart(t *testing.T) {
	ensureRunning(t, "bun")
	api(t, "stop", map[string]any{"app": "bun", "process": "admin"}, nil)
	eventually(t, 30*time.Second, func() error {
		if pid := status(t, "bun").pids()["admin"]; pid != 0 {
			return fmt.Errorf("admin still runs as %d", pid)
		}
		return nil
	})
	// The rest of the app keeps serving.
	expectStatus(t, get(t, "bun.lvh.me", "/up"), http.StatusOK)
	api(t, "start", map[string]any{"app": "bun", "process": "admin"}, nil)
	expectBody(t, servingCall(t, call{host: "admin.bun.lvh.me", path: "/", header: passwordLogin(t, "admin.bun.lvh.me", "demo")}), "PROC_TYPE=admin")
}

func TestIdleStopAndWake(t *testing.T) {
	file := filepath.Join(host.root, "apps", "sinatra", "dboss.yaml")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(original), "idle_stop: 2m") {
		t.Fatal("demo sinatra config no longer sets idle_stop: 2m")
	}
	write := func(data string) {
		if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		api(t, "rescan", nil, nil)
	}
	write(strings.Replace(string(original), "idle_stop: 2m", "idle_stop: 2s", 1))
	defer write(string(original))
	serving(t, "sinatra.lvh.me", "/up")
	waitState(t, "sinatra", "stopped")
	expectBody(t, serving(t, "sinatra.lvh.me", "/"), "Hello from Sinatra")
}

func TestLifecycleCreateRanOnce(t *testing.T) {
	ensureRunning(t, "bun")
	data, err := os.ReadFile(filepath.Join(host.root, "apps", "bun", ".dboss", "demo.db"))
	if err != nil {
		t.Fatalf("create step left no database: %v", err)
	}
	api(t, "restart", map[string]any{"app": "bun"}, nil)
	waitState(t, "bun", "running")
	again, _ := os.ReadFile(filepath.Join(host.root, "apps", "bun", ".dboss", "demo.db"))
	if string(again) != string(data) {
		t.Fatal("create ran again on restart")
	}
}

func TestButtonAppStartsOnlyWhenAsked(t *testing.T) {
	if s := status(t, "button"); s.State != "stopped" {
		t.Fatalf("button app is %s at boot, want stopped", s.State)
	}
	api(t, "start", map[string]any{"app": "button"}, nil)
	waitState(t, "button", "running")
	expectStatus(t, get(t, "button.lvh.me", "/.well-known/dboss/health"), http.StatusOK)
	api(t, "stop", map[string]any{"app": "button"}, nil)
	waitState(t, "button", "stopped")
}

func TestExecRunsInTheAppEnvironment(t *testing.T) {
	var result struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}
	api(t, "exec", map[string]any{"app": "bun", "argv": []string{"sh", "-c", "echo $GREETING $SOURCE $APP_NAME; exit 3"}}, &result)
	if strings.TrimSpace(result.Output) != "from-dotenv config bun" || result.ExitCode != 3 {
		t.Fatalf("exec gave %q exit %d", result.Output, result.ExitCode)
	}
}

// TestScratch walks the throwaway app to its end: basic auth, headers, allow_ips, a failing
// start step, and destroy with its lifecycle step.
func TestScratch(t *testing.T) {
	ensureRunning(t, "scratch")
	t.Run("basic auth", func(t *testing.T) {
		missing := get(t, "scratch.lvh.me", "/up")
		expectStatus(t, missing, http.StatusUnauthorized)
		if !strings.HasPrefix(missing.Header.Get("WWW-Authenticate"), "Basic") {
			t.Fatalf("WWW-Authenticate %q", missing.Header.Get("WWW-Authenticate"))
		}
		expectStatus(t, call{host: "scratch.lvh.me", path: "/up", user: "demo", pass: "wrong"}.do(t), http.StatusUnauthorized)
		// demo has a bcrypt hash, demo2 a plain password.
		for user, pass := range map[string]string{"demo": "demo", "demo2": "demo2"} {
			r := call{host: "scratch.lvh.me", path: "/up", user: user, pass: pass}.do(t)
			expectStatus(t, r, http.StatusOK)
			if r.Header.Get("X-Frame-Options") != "DENY" {
				t.Fatalf("headers: X-Frame-Options %q", r.Header.Get("X-Frame-Options"))
			}
		}
	})
	t.Run("failed start step", func(t *testing.T) {
		db := filepath.Join(host.root, "apps", "scratch", ".dboss", "demo.db")
		data, err := os.ReadFile(db)
		if err != nil {
			t.Fatalf("create step left no database: %v", err)
		}
		api(t, "stop", map[string]any{"app": "scratch"}, nil)
		waitState(t, "scratch", "stopped")
		if err := os.Remove(db); err != nil {
			t.Fatal(err)
		}
		// create already ran, so start finds no database and the app crashes.
		_, _ = apiRaw(t, token, "start", map[string]any{"app": "scratch"})
		waitState(t, "scratch", "crashed")
		host.notify.wait(t, "crash", "scratch", 10*time.Second)
		if err := os.WriteFile(db, data, 0o644); err != nil {
			t.Fatal(err)
		}
		api(t, "start", map[string]any{"app": "scratch"}, nil)
		waitState(t, "scratch", "running")
	})
	t.Run("destroy", func(t *testing.T) {
		dir := filepath.Join(host.root, "apps", "scratch")
		api(t, "destroy", map[string]any{"app": "scratch"}, nil)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("app folder survived destroy: %v", err)
		}
		if status, envelope := apiRaw(t, token, "status", map[string]any{"app": "scratch"}); status != http.StatusBadRequest || envelope.OK {
			t.Fatalf("status of a destroyed app answered %d ok=%v", status, envelope.OK)
		}
		expectStatus(t, html(t, "scratch.lvh.me", "/"), http.StatusNotFound)
		// The audit row lives in the host database, which outlives the folder.
		var rows []map[string]any
		eventually(t, 15*time.Second, func() error {
			api(t, "audit", map[string]any{"app": "scratch", "action": "destroy"}, &rows)
			if len(rows) == 0 {
				return fmt.Errorf("no destroy audit row")
			}
			return nil
		})
	})
}

func TestNonDeletableAppRefusesDestroy(t *testing.T) {
	status, envelope := apiRaw(t, token, "destroy", map[string]any{"app": "bun"})
	if status != http.StatusBadRequest || envelope.OK || envelope.Error == nil || envelope.Error.Code != "failed" {
		t.Fatalf("destroy of bun answered %d %+v", status, envelope.Error)
	}
	if _, err := os.Stat(filepath.Join(host.root, "apps", "bun")); err != nil {
		t.Fatal(err)
	}
}
