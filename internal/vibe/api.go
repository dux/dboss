package vibe

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"dboss/internal/authcog"
	"dboss/internal/fault"
	"dboss/internal/git"
	"dboss/internal/ops"
	"dboss/internal/supervisor"
)

const (
	maxAPIBody     = 64 << 10
	eventKeepAlive = 25 * time.Second
)

var commitHash = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// stateResponse is what the harness page reads on load.
type stateResponse struct {
	App      string `json:"app"`
	Process  string `json:"process"`
	Multi    bool   `json:"multi"`
	State    string `json:"state"`
	Chat     bool   `json:"chat"`
	Model    string `json:"model"`
	Password bool   `json:"password"`
	MCPURL   string `json:"mcp_url"`
}

// serveAPI answers the harness page's JSON calls. A POST must carry the X-Dboss-Vibe header and,
// when the browser sends one, the harness's own Origin, so another site cannot drive it.
func (s *Service) serveAPI(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, web supervisor.WebProcessSnapshot, route string) {
	base := authcog.Scheme(r) + "://" + r.Host
	if r.Method == http.MethodPost {
		if r.Header.Get("X-Dboss-Vibe") != "1" || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != base) {
			writeError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxAPIBody)
	}
	h := s.harness(app.Name, web.Name)
	who := actor(app, web)
	switch r.Method + " " + route {
	case "GET state":
		token, err := s.Token(app.Name, web.Name)
		if err != nil {
			writeFailure(w, err)
			return
		}
		writeJSON(w, stateResponse{App: app.Name, Process: web.Name, Multi: len(app.WebProcesses) > 1, State: string(app.State), Chat: chatKey(app) != "", Model: deepseekModel, Password: web.Vibe.Password != "", MCPURL: mcpURL(base, token)})
	case "GET events":
		s.serveEvents(w, r, h)
	case "GET changes":
		status, err := git.Status(r.Context(), app.Dir)
		if err != nil {
			writeFailure(w, fault.Invalid(err))
			return
		}
		writeJSON(w, status)
	case "GET diff":
		s.serveDiff(w, r, app)
	case "GET handover":
		prompt, err := s.handover(base, app, web)
		if err != nil {
			writeFailure(w, err)
			return
		}
		writeJSON(w, map[string]string{"prompt": prompt})
	case "POST chat":
		var body struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		writeResult(w, nil, s.startTurn(app, web, body.Message))
	case "POST stop":
		h.stop()
		writeJSON(w, nil)
	case "POST new":
		writeResult(w, nil, h.reset())
	case "POST restart":
		_, err := s.ops.Do(ops.Request{Method: ops.ActionRestart, App: app.Name, Actor: who})
		writeResult(w, nil, err)
	case "POST commit":
		var body struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		result, err := s.ops.Do(ops.Request{Method: ops.ActionGitCommit, App: app.Name, Message: body.Message, Actor: who})
		h.notifyGit()
		writeResult(w, result, err)
	case "POST commit-message":
		message, err := s.commitMessage(r.Context(), app)
		writeResult(w, map[string]string{"message": message}, err)
	case "POST push":
		result, err := s.ops.Do(ops.Request{Method: ops.ActionGitPush, App: app.Name, Actor: who})
		h.notifyGit()
		writeResult(w, result, err)
	case "POST reset":
		result, err := s.ops.Do(ops.Request{Method: ops.ActionGitReset, App: app.Name, Actor: who})
		h.notifyGit()
		writeResult(w, result, err)
	case "POST mcp-rotate":
		token, err := s.rotate(app.Name, web.Name)
		writeResult(w, map[string]string{"mcp_url": mcpURL(base, token)}, err)
	default:
		writeError(w, http.StatusNotFound, "no such harness call")
	}
}

// serveDiff answers one file's diff: of the working tree, or of one unpushed commit. Only a path
// the working tree or that commit lists is accepted, so this never reads anything else.
func (s *Service) serveDiff(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot) {
	path, commit := r.URL.Query().Get("path"), r.URL.Query().Get("commit")
	status, err := git.Status(r.Context(), app.Dir)
	if err != nil {
		writeFailure(w, fault.Invalid(err))
		return
	}
	changes := status.Changes
	if commit != "" {
		changes = nil
		if commitHash.MatchString(commit) {
			for _, unpushed := range status.Unpushed {
				if strings.HasPrefix(unpushed.Hash, commit) {
					changes, commit = unpushed.Files, unpushed.Hash
				}
			}
		}
		if changes == nil {
			writeError(w, http.StatusBadRequest, "not an unpushed commit")
			return
		}
	}
	for _, change := range changes {
		if change.Path != path {
			continue
		}
		var diff string
		if commit != "" {
			diff, err = git.CommitDiff(r.Context(), app.Dir, commit, change)
		} else {
			diff, err = git.Diff(r.Context(), app.Dir, change)
		}
		if fault.IsInvalid(err) {
			writeJSON(w, map[string]any{"change": change, "too_large": err.Error()})
			return
		}
		writeResult(w, map[string]any{"change": change, "diff": diff}, err)
		return
	}
	writeError(w, http.StatusBadRequest, fmt.Sprintf("%s has no change to show", path))
}

// serveEvents streams the harness: a snapshot first, then every chat line, streamed answer text,
// tool call and working tree change as it happens.
func (s *Service) serveEvents(w http.ResponseWriter, r *http.Request, h *harness) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	if suppressor, ok := w.(interface{ SuppressRecord() }); ok {
		suppressor.SuppressRecord()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	first, ch, done := h.subscribe()
	defer done()
	send := func(ev event) bool {
		data, err := json.Marshal(ev)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send(first) {
		return
	}
	keepAlive := time.NewTicker(eventKeepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case ev := <-ch:
			if !send(ev) {
				return
			}
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// writeFailure answers 400 for an error the owner can fix and 500 for the rest.
func writeFailure(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if fault.IsInvalid(err) {
		status = http.StatusBadRequest
	}
	writeError(w, status, err.Error())
}

func writeResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, value)
}
