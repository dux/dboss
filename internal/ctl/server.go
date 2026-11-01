package ctl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dboss/internal/children"
	"dboss/internal/fsutil"
	"dboss/internal/logx"
	"dboss/internal/ops"
)

type Server struct {
	path     string
	listener net.Listener
	service  *ops.Service
	login    func() (string, string, error)
}

// Idle refuses a socket path another session still answers on and removes a stale socket left by
// one that died. A session calls it before it touches ports or children.
func Idle(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("control socket path is not a socket: %s", path)
		}
		connection, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			_ = connection.Close()
			return fmt.Errorf("control socket already active: %s", path)
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return fmt.Errorf("check control socket: %w", dialErr)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		logx.Warnf("warning: removed stale control socket %s; no service is listening, continuing startup", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// owner is the pid file of the session serving a control socket (`dboss.sock` -> `dboss.pid`):
// its pid and the kernel start time that tells it apart from a later process given the same pid.
type owner struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

func ownerPath(socket string) string { return strings.TrimSuffix(socket, ".sock") + ".pid" }

// TakeOver stops the earlier session of the socket when its pid file still names that very
// process, waits for it to exit (killing it after timeout; the next start reaps what it left),
// then clears the socket like Idle. A live socket no pid file vouches for is still refused, and an
// unreadable pid file vouches for nothing.
func TakeOver(path string, timeout time.Duration) error {
	var previous owner
	_ = fsutil.ReadJSON(ownerPath(path), &previous)
	if previous.PID > 1 && previous.PID != os.Getpid() && same(previous) {
		logx.Warnf("stopping the earlier session (pid %d) still serving %s", previous.PID, path)
		if err := syscall.Kill(previous.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stop the earlier session (pid %d): %w", previous.PID, err)
		}
		if !exited(previous, timeout) {
			_ = syscall.Kill(previous.PID, syscall.SIGKILL)
			if !exited(previous, 5*time.Second) {
				return fmt.Errorf("the earlier session (pid %d) did not exit", previous.PID)
			}
		}
	}
	return Idle(path)
}

// same reports whether the recorded process still runs.
func same(o owner) bool {
	start, err := children.StartTime(o.PID)
	return err == nil && o.Start != "" && start == o.Start
}

// exited waits up to timeout for the recorded process to be gone.
func exited(o owner, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for same(o) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// Listen serves the control socket. login mints console login links and is nil when the
// management console is not enabled.
func Listen(path string, service *ops.Service, login func() (string, string, error)) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := Idle(path); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	if start, err := children.StartTime(os.Getpid()); err == nil {
		if err := fsutil.WriteJSON(ownerPath(path), owner{PID: os.Getpid(), Start: start}, 0o640); err != nil {
			_ = listener.Close()
			return nil, err
		}
	}
	server := &Server{path: path, listener: listener, service: service, login: login}
	go server.serve()
	return server, nil
}

func (s *Server) Close() error {
	err := s.listener.Close()
	removeErr := os.Remove(s.path)
	_ = os.Remove(ownerPath(s.path))
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

func (s *Server) serve() {
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(connection)
	}
}

func (s *Server) handle(connection net.Conn) {
	defer connection.Close()
	decoder := json.NewDecoder(bufio.NewReader(connection))
	encoder := json.NewEncoder(connection)
	for {
		var request Request
		if err := decoder.Decode(&request); err != nil {
			return
		}
		response := s.dispatch(request)
		if err := encoder.Encode(response); err != nil {
			return
		}
	}
}

const LoginMethod = "login"

// dispatch runs one control request. Everything but login is the shared ops action; login mints
// a console link and so stays with the server.
func (s *Server) dispatch(request Request) Response {
	if request.Method == LoginMethod {
		return s.loginResponse()
	}
	// The control socket has no user, so audited actions are attributed to the CLI.
	if request.Actor == "" {
		request.Actor = "cli"
	}
	data, err := s.service.Do(request)
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Data: data}
}

func (s *Server) loginResponse() Response {
	if s.login == nil {
		return Response{Error: "management console is not enabled: set management.host in the host config"}
	}
	local, public, err := s.login()
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Data: map[string]string{"url": local, "public_url": public}}
}
