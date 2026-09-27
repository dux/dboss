//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCronRunNow(t *testing.T) {
	ensureRunning(t, "bun")
	api(t, "cron-run", map[string]any{"app": "bun", "job": "report"}, nil)
	eventually(t, 20*time.Second, func() error {
		for _, job := range status(t, "bun").Cron {
			if job.Name == "report" && !job.LastEnd.IsZero() {
				if job.LastExit != 0 || job.LastError != "" {
					return fmt.Errorf("report exited %d: %s", job.LastExit, job.LastError)
				}
				return nil
			}
		}
		return fmt.Errorf("report has not finished")
	})
}

func TestCronRunsOnSchedule(t *testing.T) {
	ensureRunning(t, "bun")
	// heartbeat runs every 30s on its own.
	eventually(t, 45*time.Second, func() error {
		for _, job := range status(t, "bun").Cron {
			if job.Name == "heartbeat" && !job.LastEnd.IsZero() && job.LastExit == 0 {
				return nil
			}
		}
		return fmt.Errorf("heartbeat has not run")
	})
}

// hookStatus is the hook view GET /hooks/<app>/<hook> answers.
type hookStatus struct {
	Running    bool      `json:"running"`
	Restarting bool      `json:"restarting"`
	LastEnd    time.Time `json:"last_end"`
	LastExit   int       `json:"last_exit"`
	LastError  string    `json:"last_error"`
	Output     string    `json:"output"`
}

func TestDeployHookPing(t *testing.T) {
	ensureRunning(t, "bun")
	ping := func(path, bearer string) reply {
		header := map[string]string{}
		if bearer != "" {
			header["Authorization"] = "Bearer " + bearer
		}
		return call{method: http.MethodPost, addr: host.console, path: path, header: header}.do(t)
	}
	if r := ping("/hooks/bun/deploy", "wrong"); r.Status < 400 {
		t.Fatalf("a wrong token answered %d", r.Status)
	}
	if r := ping("/hooks/bun/deploy", ""); r.Status < 400 {
		t.Fatalf("no token answered %d", r.Status)
	}
	expectStatus(t, ping("/hooks/bun/nope", token), http.StatusNotFound)
	if r := ping("/hooks/bun/deploy", token); r.Status >= 300 {
		t.Fatalf("hook ping answered %d: %s", r.Status, r.Body)
	}
	// The same poll `dboss deploy git` runs, through the management host this time.
	var hook hookStatus
	eventually(t, 30*time.Second, func() error {
		r := call{host: "dboss.lvh.me", path: "/hooks/bun/deploy", header: map[string]string{"Authorization": "Bearer " + token}}.do(t)
		if r.Status != http.StatusOK {
			return fmt.Errorf("hook status %d", r.Status)
		}
		if err := json.Unmarshal([]byte(r.Body), &hook); err != nil {
			return err
		}
		if hook.Running || hook.Restarting || hook.LastEnd.IsZero() {
			return fmt.Errorf("hook still running")
		}
		return nil
	})
	if hook.LastExit != 0 || !strings.Contains(hook.Output, "deploy hook ran") {
		t.Fatalf("hook exit %d output %q", hook.LastExit, hook.Output)
	}
	var rows []struct {
		Actor string `json:"actor"`
	}
	api(t, "audit", map[string]any{"app": "bun", "by_actor": "hook:bun/deploy"}, &rows)
	if len(rows) == 0 {
		t.Fatal("the hook ping wrote no audit row")
	}
}
