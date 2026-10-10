// Package vibe is the AI harness a web process serves at /_dboss_/vibe on its own hosts: the app
// in a frame, a chat that edits it through a fixed tool set, the changed files with their diffs,
// Restart, Reset, Commit and Push. The same tools are served over MCP, so any LLM client can drive
// the app, and the AI handover prompt tells an outside agent how to connect.
package vibe

import (
	"context"
	"io/fs"
	"net/http"
	"path/filepath"
	"sync"

	"dboss/internal/authcog"
	"dboss/internal/logstore"
	"dboss/internal/ops"
	"dboss/internal/secret"
	"dboss/internal/supervisor"
	"dboss/internal/throttle"
)

// Ops is the slice of ops.Service the harness drives. Every action that changes the app goes
// through Do, so it writes the same audit row as the console.
type Ops interface {
	Do(ops.Request) (any, error)
	Audit(actor, app, action, detail string, err error)
	SearchLogs(app string, filter logstore.LogFilter) ([]logstore.LogEntry, error)
	Exceptions(app string, filter logstore.ExceptionFilter) ([]logstore.ExceptionSummary, error)
	HostPages() string
}

// Service owns every harness of the session. It is a module, so a running chat turn ends with the
// daemon, and a proxy stage, an authorizer and a response hook for the app proxy.
type Service struct {
	ops        Ops
	flow       *authcog.Flow
	secrets    *secret.Store
	stateDir   string
	runtimeDir string
	cloudflare bool
	assets     fs.FS
	console    fs.FS
	logins     *throttle.Throttle
	client     *http.Client
	proxy      http.Handler

	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	harnesses map[string]*harness
	wg        sync.WaitGroup
}

// New builds the service. console is the management console's static folder, whose fez runtime
// and stylesheet the harness page shares.
func New(service Ops, flow *authcog.Flow, stateDir, runtimeDir string, cloudflare bool, console fs.FS) (*Service, error) {
	secrets, err := secret.Open(filepath.Join(stateDir, "vibe-secrets.json"))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		ops: service, flow: flow, secrets: secrets, stateDir: stateDir, runtimeDir: runtimeDir, cloudflare: cloudflare,
		assets: staticFS(), console: console, logins: throttle.New(), client: &http.Client{},
		ctx: ctx, cancel: cancel, harnesses: map[string]*harness{},
	}, nil
}

func (s *Service) Name() string { return "vibe" }

func (s *Service) Start(context.Context) error { return nil }

// Close stops every running chat turn and waits for it to save its transcript.
func (s *Service) Close() error {
	s.cancel()
	s.wg.Wait()
	return nil
}

// harness returns the state of one web process's harness, loading its transcript on first use.
func (s *Service) harness(app, process string) *harness {
	key := app + "/" + process
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.harnesses[key]; ok {
		return h
	}
	h := newHarness(app, process, filepath.Join(s.stateDir, app, "vibe-"+process+".json"))
	s.harnesses[key] = h
	return h
}

// actor names who did something through a harness, for the audit trail.
func actor(app supervisor.Snapshot, web supervisor.WebProcessSnapshot) string {
	return "vibe:" + app.Name + "/" + web.Name
}
