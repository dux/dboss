//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestNextSessionStopsWhatAKilledOneLeft SIGKILLs the daemon, which skips every cleanup. A quiet
// worker never writes to its closed stdout pipe and holds no port, so without the children ledger
// it would keep running next to its replacement. It restarts the shared session, so it runs last.
func TestNextSessionStopsWhatAKilledOneLeft(t *testing.T) {
	file := filepath.Join(host.root, "apps", "sinatra", "dboss.yaml")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	write := func(data string) {
		if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		api(t, "rescan", nil, nil)
	}
	write(strings.Replace(string(original), "procfile:\n", "procfile:\n  quiet: sleep 600\n", 1))
	defer write(string(original))
	ensureRunning(t, "sinatra")
	api(t, "start", map[string]any{"app": "sinatra", "process": "quiet"}, nil)
	var orphan int
	eventually(t, 20*time.Second, func() error {
		if orphan = status(t, "sinatra").pids()["quiet"]; orphan == 0 {
			return fmt.Errorf("quiet has not started")
		}
		return nil
	})

	_ = host.daemon.Process.Signal(syscall.SIGKILL)
	<-host.daemonDone
	if syscall.Kill(-orphan, 0) != nil {
		t.Fatal("quiet died with the daemon, so nothing is left to reap")
	}
	if err := startDaemon(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() error {
		if syscall.Kill(-orphan, 0) == nil {
			return fmt.Errorf("pid %d from the killed session still runs", orphan)
		}
		return nil
	})
	if log := readFile(host.daemonLog); !strings.Contains(log, fmt.Sprintf("sinatra/quiet (pid %d)", orphan)) {
		t.Fatalf("daemon log does not name the reaped worker:\n%s", log)
	}
	// sinatra is autostart: false, so the new session only runs it once asked.
	ensureRunning(t, "sinatra")
	eventually(t, 30*time.Second, func() error {
		if pid := status(t, "sinatra").pids()["quiet"]; pid == 0 || pid == orphan {
			return fmt.Errorf("quiet pid %d, want a new one", pid)
		}
		return nil
	})
}
