//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// apiText runs an action and returns its data as raw JSON text, for answers read by substring.
func apiText(t *testing.T, action string, params map[string]any) string {
	t.Helper()
	var raw json.RawMessage
	api(t, action, params, &raw)
	return string(raw)
}

// waitAPI polls an action until its data contains every part; ingest runs every 5s.
func waitAPI(t *testing.T, action string, params map[string]any, parts ...string) string {
	t.Helper()
	var text string
	eventually(t, 30*time.Second, func() error {
		text = apiText(t, action, params)
		for _, part := range parts {
			if !strings.Contains(text, part) {
				return fmt.Errorf("%s has no %q: %.300s", action, part, text)
			}
		}
		return nil
	})
	return text
}

func TestProcessLogs(t *testing.T) {
	ensureRunning(t, "bun")
	// Once the startup line is sealed out of the live file into the store, the tail still has it.
	waitAPI(t, "log-search", map[string]any{"app": "bun", "channel": "stdout", "query": "listening"}, "Bun service listening")
	waitAPI(t, "logs", map[string]any{"app": "bun"}, "Bun service listening")
	out := cli(t, "logs", "bun")
	if !strings.Contains(out, "Bun service listening") {
		t.Fatalf("dboss logs bun:\n%s", out)
	}
}

func TestStdoutIsSearchable(t *testing.T) {
	ensureRunning(t, "bun")
	api(t, "cron-run", map[string]any{"app": "bun", "job": "heartbeat"}, nil)
	waitAPI(t, "log-search", map[string]any{"app": "bun", "query": "heartbeat"}, "heartbeat")
}

func TestAppLogFileIsIngested(t *testing.T) {
	serving(t, "sinatra.lvh.me", "/up")
	// The job worker appends to log/job.log every 3s; the tailer ingests it as a file channel.
	waitAPI(t, "log-search", map[string]any{"app": "sinatra", "channel": "file", "query": "numbers"}, "numbers")
}

func TestRequestsAreRecorded(t *testing.T) {
	ensureRunning(t, "bun")
	for i := 0; i < 3; i++ {
		get(t, "bun.lvh.me", "/up")
	}
	client := consoleClient(t)
	eventually(t, 15*time.Second, func() error {
		r := call{addr: host.console, path: "/ui/traffic?app=bun&range=1h", client: client}.do(t)
		if r.Status != http.StatusOK || !strings.Contains(r.Body, "/up") {
			return fmt.Errorf("traffic %d: %.300s", r.Status, r.Body)
		}
		return nil
	})
}

func TestExceptionsAreGroupedAndResolved(t *testing.T) {
	serving(t, "sinatra.lvh.me", "/up")
	for _, n := range []string{"abc", "x", "y"} {
		expectStatus(t, get(t, "sinatra.lvh.me", "/raise?n="+n), http.StatusInternalServerError)
	}
	client := consoleClient(t)
	var page struct {
		Rows []struct {
			ExpUID  string `json:"exp_uid"`
			Count   int    `json:"count"`
			Minutes []struct {
				Message string   `json:"message"`
				Users   []string `json:"users"`
			} `json:"minutes"`
		} `json:"rows"`
	}
	eventually(t, 30*time.Second, func() error {
		r := call{addr: host.console, path: "/ui/exceptions?app=sinatra&range=24h", client: client}.do(t)
		if r.Status != http.StatusOK {
			return fmt.Errorf("exceptions %d: %.300s", r.Status, r.Body)
		}
		if err := json.Unmarshal([]byte(r.Body), &page); err != nil {
			return err
		}
		// Three inputs, one raising line: one fingerprint.
		if len(page.Rows) != 1 || page.Rows[0].Count < 3 || len(page.Rows[0].Minutes) == 0 {
			return fmt.Errorf("want one group of 3, got %+v", page.Rows)
		}
		return nil
	})
	group := page.Rows[0]
	if minute := group.Minutes[0]; !strings.HasPrefix(minute.Message, "ArgumentError") || len(minute.Users) == 0 {
		t.Fatalf("minute row %+v, want the ArgumentError with the visitor", minute)
	}
	eventually(t, 15*time.Second, func() error {
		if n := status(t, "sinatra").Exceptions; n != 1 {
			return fmt.Errorf("snapshot counts %d unresolved groups", n)
		}
		return nil
	})
	api(t, "exception-resolve", map[string]any{"app": "sinatra", "exp_uid": group.ExpUID, "on": true}, nil)
	eventually(t, 15*time.Second, func() error {
		if n := status(t, "sinatra").Exceptions; n != 0 {
			return fmt.Errorf("snapshot still counts %d unresolved groups", n)
		}
		return nil
	})
	// A new occurrence reopens a resolved group.
	get(t, "sinatra.lvh.me", "/raise")
	eventually(t, 30*time.Second, func() error {
		if n := status(t, "sinatra").Exceptions; n != 1 {
			return fmt.Errorf("snapshot counts %d unresolved groups after a new raise", n)
		}
		return nil
	})
}

func TestEventsAreStoredAndQueried(t *testing.T) {
	serving(t, "sinatra.lvh.me", "/up")
	for i := 0; i < 20; i++ {
		expectStatus(t, get(t, "sinatra.lvh.me", "/shop"), http.StatusOK)
	}
	waitAPI(t, "events", map[string]any{"app": "sinatra"}, "page_view")
	waitAPI(t, "events-latest", map[string]any{"app": "sinatra", "query": "page_view"}, "page:pricing")
	waitAPI(t, "events-facets", map[string]any{"app": "sinatra", "key": "plan"}, `"value"`)
	waitAPI(t, "events-views", map[string]any{"app": "sinatra"}, "paid", "checkout")
	if !strings.Contains(cli(t, "events", "sinatra"), "page_view") {
		t.Fatal("dboss events sinatra lists no page_view")
	}
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Log("duckdb not on PATH: skipping the funnel and SQL checks")
		return
	}
	waitAPI(t, "events-funnel", map[string]any{"app": "sinatra", "name": "checkout"}, "Pricing")
	waitAPI(t, "events-query", map[string]any{"app": "sinatra", "sql": "select count(*) as n from events where event = 'page_view'"}, `"n"`)
}
