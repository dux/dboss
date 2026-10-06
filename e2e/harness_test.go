//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// reply is one HTTP answer with its body read.
type reply struct {
	Status int
	Header http.Header
	Body   string
}

// call sends one request through the proxy (or to addr when set) with the given Host header.
// Redirects are never followed, so a test sees the redirect dboss answered.
type call struct {
	method string
	host   string
	path   string
	body   []byte
	header map[string]string
	addr   string
	user   string
	pass   string
	client *http.Client
}

var noRedirect = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (c call) do(t *testing.T) reply {
	t.Helper()
	r, err := c.try()
	if err != nil {
		t.Fatalf("%s %s%s: %v", c.method, c.host, c.path, err)
	}
	return r
}

func (c call) try() (reply, error) {
	method := c.method
	if method == "" {
		method = http.MethodGet
	}
	addr := c.addr
	if addr == "" {
		addr = host.proxy
	}
	request, err := http.NewRequest(method, "http://"+addr+c.path, bytes.NewReader(c.body))
	if err != nil {
		return reply{}, err
	}
	if c.host != "" {
		request.Host = c.host
	}
	for key, value := range c.header {
		request.Header.Set(key, value)
	}
	if c.user != "" {
		request.SetBasicAuth(c.user, c.pass)
	}
	client := c.client
	if client == nil {
		client = noRedirect
	}
	response, err := client.Do(request)
	if err != nil {
		return reply{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return reply{}, err
	}
	return reply{Status: response.StatusCode, Header: response.Header, Body: string(body)}, nil
}

func get(t *testing.T, hostname, path string) reply {
	t.Helper()
	return call{host: hostname, path: path}.do(t)
}

// html is a browser-like GET: dboss renders its own pages only for a request that wants HTML.
func html(t *testing.T, hostname, path string) reply {
	t.Helper()
	return call{host: hostname, path: path, header: map[string]string{"Accept": "text/html"}}.do(t)
}

func expectStatus(t *testing.T, r reply, want int) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status %d, want %d; body: %.400s", r.Status, want, r.Body)
	}
}

func expectBody(t *testing.T, r reply, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(r.Body, part) {
			t.Fatalf("body is missing %q:\n%.800s", part, r.Body)
		}
	}
}

// eventually retries check until it returns nil or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for {
		if err = check(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gave up after %s: %v", timeout, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// serving waits until the path answers 200 through the proxy; a stopped app wakes on it.
func serving(t *testing.T, hostname, path string) reply {
	t.Helper()
	return servingCall(t, call{host: hostname, path: path})
}

// servingCall is serving for a request that carries its own headers, such as a session cookie.
func servingCall(t *testing.T, c call) reply {
	t.Helper()
	var last reply
	eventually(t, 60*time.Second, func() error {
		r, err := c.try()
		if err != nil {
			return err
		}
		last = r
		if r.Status != http.StatusOK {
			return fmt.Errorf("%s%s answered %d", c.host, c.path, r.Status)
		}
		return nil
	})
	return last
}

// passwordLogin posts the demo password page and returns the session cookie as a request header.
func passwordLogin(t *testing.T, hostname, password string) map[string]string {
	t.Helper()
	r := passwordPost(t, hostname, password)
	expectStatus(t, r, http.StatusSeeOther)
	cookie, _, _ := strings.Cut(r.Header.Get("Set-Cookie"), ";")
	if !strings.HasPrefix(cookie, "dboss_password=") {
		t.Fatalf("password login set %q", r.Header.Get("Set-Cookie"))
	}
	return map[string]string{"Cookie": cookie}
}

func passwordPost(t *testing.T, hostname, password string) reply {
	t.Helper()
	form := url.Values{"password": {password}, "to": {"/"}}
	return call{method: http.MethodPost, host: hostname, path: "/.well-known/dboss/password", body: []byte(form.Encode()), header: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "text/html"}}.do(t)
}

// apiEnvelope is the HTTP API's one answer shape.
type apiEnvelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func apiRaw(t *testing.T, bearer, action string, params map[string]any) (int, apiEnvelope) {
	t.Helper()
	body, _ := json.Marshal(params)
	if params == nil {
		body = []byte("{}")
	}
	r := call{
		method: http.MethodPost,
		addr:   host.console,
		path:   "/api/" + action,
		body:   body,
		header: map[string]string{"Authorization": "Bearer " + bearer, "Content-Type": "application/json"},
	}.do(t)
	var envelope apiEnvelope
	if err := json.Unmarshal([]byte(r.Body), &envelope); err != nil {
		t.Fatalf("api %s: %v: %.400s", action, err, r.Body)
	}
	return r.Status, envelope
}

// api runs one action with tokens.dboss and decodes its data into out (when not nil).
func api(t *testing.T, action string, params map[string]any, out any) {
	t.Helper()
	status, envelope := apiRaw(t, token, action, params)
	if status != http.StatusOK || !envelope.OK {
		message := ""
		if envelope.Error != nil {
			message = envelope.Error.Code + ": " + envelope.Error.Message
		}
		t.Fatalf("api %s %v: %d %s", action, params, status, message)
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			t.Fatalf("api %s data: %v: %.400s", action, err, envelope.Data)
		}
	}
}

// snapshot is the part of supervisor.Snapshot the tests read.
type snapshot struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Maintenance bool   `json:"maintenance"`
	Rolling     bool   `json:"rolling"`
	Dir         string `json:"dir"`
	Error       string `json:"error"`
	Exceptions  int    `json:"exceptions"`
	Processes   []struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		State string `json:"state"`
		PID   int    `json:"pid"`
		Port  int    `json:"port"`
	} `json:"processes"`
	Cron []struct {
		Name      string    `json:"name"`
		LastEnd   time.Time `json:"last_end"`
		LastExit  int       `json:"last_exit"`
		LastError string    `json:"last_error"`
	} `json:"cron"`
}

func status(t *testing.T, app string) snapshot {
	t.Helper()
	var s snapshot
	api(t, "status", map[string]any{"app": app}, &s)
	return s
}

// pids maps each running instance of an app to its pid.
func (s snapshot) pids() map[string]int {
	out := map[string]int{}
	for _, process := range s.Processes {
		if process.PID != 0 {
			out[process.Name] = process.PID
		}
	}
	return out
}

func waitState(t *testing.T, app, state string) snapshot {
	t.Helper()
	var s snapshot
	eventually(t, 60*time.Second, func() error {
		s = status(t, app)
		if s.State != state || s.Rolling {
			return fmt.Errorf("%s is %s (rolling %v), want %s; error %q", app, s.State, s.Rolling, state, s.Error)
		}
		return nil
	})
	return s
}

// ensureRunning starts an app when an earlier test left it stopped.
func ensureRunning(t *testing.T, app string) snapshot {
	t.Helper()
	if s := status(t, app); s.State != "running" {
		api(t, "start", map[string]any{"app": app}, nil)
	}
	return waitState(t, app, "running")
}

// cli runs the built binary against the session's config and returns its combined output.
func cli(t *testing.T, args ...string) string {
	t.Helper()
	output, err := cliTry(args...)
	if err != nil {
		t.Fatalf("dboss %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func cliTry(args ...string) (string, error) {
	// Options go right after the command: exec passes everything after the app through.
	cmd := exec.Command(host.bin, append([]string{args[0], "-c", host.config}, args[1:]...)...)
	cmd.Dir = host.root
	cmd.Env = environment()
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// consoleClient signs in through a `dboss login` link and returns a client carrying the session.
func consoleClient(t *testing.T) *http.Client {
	t.Helper()
	output := cli(t, "login")
	link := ""
	for _, field := range strings.Fields(output) {
		if strings.HasPrefix(field, "http://"+host.console+"/login?token=") {
			link = field
		}
	}
	if link == "" {
		t.Fatalf("dboss login printed no loopback link:\n%s", output)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	response, err := client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login link answered %d", response.StatusCode)
	}
	return client
}

// notifySink is the host's notify webhook: it keeps every event dboss posts.
type notifySink struct {
	url    string
	mu     sync.Mutex
	events []notifyEvent
}

type notifyEvent struct {
	Event string `json:"event"`
	App   string `json:"app"`
	Error string `json:"error"`
}

func startNotifySink() *notifySink {
	sink := &notifySink{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event notifyEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err == nil {
			sink.mu.Lock()
			sink.events = append(sink.events, event)
			sink.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	sink.url = server.URL + "/notify"
	return sink
}

// wait blocks until dboss posted event for app.
func (s *notifySink) wait(t *testing.T, event, app string, timeout time.Duration) notifyEvent {
	t.Helper()
	var found notifyEvent
	eventually(t, timeout, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, e := range s.events {
			if e.Event == event && e.App == app {
				found = e
				return nil
			}
		}
		return fmt.Errorf("no %s event for %s; got %v", event, app, s.events)
	})
	return found
}
