package vibe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dboss/internal/fault"
	"dboss/internal/git"
	"dboss/internal/supervisor"
)

const (
	maxSteps    = 40
	turnTimeout = 5 * time.Minute
	// contextBudget bounds the characters a turn sends; older tool output is stubbed past it.
	contextBudget = 240000
	maxNoteFile   = 8000
	maxPromptList = 300
)

// startTurn runs one chat turn in the background: the owner's message, then the model and its
// tool calls until it answers without one.
func (s *Service) startTurn(app supervisor.Snapshot, web supervisor.WebProcessSnapshot, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fault.Invalidf("the message is empty")
	}
	key := chatKey(app)
	if key == "" {
		return fault.Invalidf("the built-in chat needs deepseek_api_key in the app config or host defaults")
	}
	h := s.harness(app.Name, web.Name)
	ctx, cancel := context.WithTimeout(s.ctx, turnTimeout)
	if err := h.begin(cancel); err != nil {
		cancel()
		return err
	}
	h.add(Entry{Kind: KindUser, Text: text})
	h.appendMessages(Message{Role: "user", Content: text})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		h.end(s.runTurn(ctx, h, app, web, key))
	}()
	return nil
}

func (s *Service) runTurn(ctx context.Context, h *harness, app supervisor.Snapshot, web supervisor.WebProcessSnapshot, key string) turnState {
	c := &call{s: s, app: app.Name, web: web.Name, actor: actor(app, web), harness: h}
	system := Message{Role: "system", Content: s.systemPrompt(app, web)}
	state := turnState{}
	fail := func(err error) turnState {
		message := err.Error()
		if errors.Is(err, context.Canceled) {
			message = "stopped"
		} else if errors.Is(err, context.DeadlineExceeded) {
			message = fmt.Sprintf("stopped after %s", turnTimeout)
		}
		h.add(Entry{Kind: KindError, Text: message})
		state.Error, state.Wrote, state.Restarted = message, c.wrote, c.restarted
		return state
	}
	for step := 0; step < maxSteps; step++ {
		reply, err := s.stream(ctx, key, append([]Message{system}, fit(repair(h.messages()), contextBudget)...), h.delta)
		if err != nil {
			return fail(err)
		}
		h.appendMessages(reply)
		if reply.Content != "" {
			h.add(Entry{Kind: KindAssistant, Text: reply.Content})
		}
		if len(reply.ToolCalls) == 0 {
			state.Wrote, state.Restarted = c.wrote, c.restarted
			return state
		}
		for index, toolCall := range reply.ToolCalls {
			if ctx.Err() != nil {
				// Every call the model made needs an answer, or the next request is refused.
				for _, skipped := range reply.ToolCalls[index:] {
					h.appendMessages(Message{Role: "tool", ToolCallID: skipped.ID, Content: "not run: the turn was stopped"})
				}
				return fail(ctx.Err())
			}
			var args arguments
			_ = json.Unmarshal([]byte(toolCall.Function.Arguments), &args)
			h.toolStarted(toolCall.Function.Name, summary(toolCall.Function.Name, args))
			result, _ := s.runTool(ctx, c, toolCall.Function.Name, json.RawMessage(toolCall.Function.Arguments))
			h.appendMessages(Message{Role: "tool", ToolCallID: toolCall.ID, Content: result})
		}
	}
	return fail(fmt.Errorf("stopped after %d tool steps; ask again to continue", maxSteps))
}

// repair answers every tool call that has no result yet, so a transcript cut by a crash still
// makes a valid request.
func repair(messages []Message) []Message {
	answered := map[string]bool{}
	for _, message := range messages {
		if message.Role == "tool" {
			answered[message.ToolCallID] = true
		}
	}
	repaired := make([]Message, 0, len(messages))
	for index, message := range messages {
		if message.Role == "tool" && !answeredBy(messages[:index], message.ToolCallID) {
			continue
		}
		repaired = append(repaired, message)
		for _, toolCall := range message.ToolCalls {
			if !answered[toolCall.ID] {
				repaired = append(repaired, Message{Role: "tool", ToolCallID: toolCall.ID, Content: "not run"})
			}
		}
	}
	return repaired
}

// answeredBy reports whether an assistant message before a tool result asked for it.
func answeredBy(before []Message, id string) bool {
	for _, message := range before {
		for _, toolCall := range message.ToolCalls {
			if toolCall.ID == id {
				return true
			}
		}
	}
	return false
}

// fit keeps a transcript under budget characters: old tool output becomes a stub first, then
// whole old exchanges go, always cutting at a user message so tool calls keep their results.
func fit(messages []Message, budget int) []Message {
	size := func() int {
		total := 0
		for _, message := range messages {
			total += len(message.Content)
			for _, toolCall := range message.ToolCalls {
				total += len(toolCall.Function.Arguments)
			}
		}
		return total
	}
	for index := 0; index < len(messages) && size() > budget; index++ {
		if messages[index].Role == "tool" && len(messages[index].Content) > 200 {
			messages[index].Content = "[older tool output dropped to save room]"
		}
	}
	for size() > budget && len(messages) > 1 {
		next := 1
		for next < len(messages) && messages[next].Role != "user" {
			next++
		}
		if next >= len(messages) {
			break
		}
		messages = messages[next:]
	}
	return messages
}

// systemPrompt is rebuilt for every turn, so it always carries the app's current files and state.
func (s *Service) systemPrompt(app supervisor.Snapshot, web supervisor.WebProcessSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are the coding agent of the dboss vibe harness. You change the web app %q, which runs under dboss; the owner watches it live in a frame next to this chat.

Work in small verified steps:
* Read before you change: list_files, search and read_file. Prefer edit_file for small changes and write_file for new files.
* After a change the app does not pick up on its own (server code, config, dependencies), call restart, then check logs and exceptions.
* Verify with http_get before you say a change works; say what you checked.
* Use run for installs, migrations and tests. Never run destructive commands on data you did not create.
* Never push and never rewrite git history. Suggest a commit with a short subject when a change is done; commit only when asked.
* Answer briefly in plain language. The owner may not be a programmer.

`, app.Name)
	fmt.Fprintf(&b, "App folder: %s\nWeb process: %s, hosts %s\nState: %s\n", app.Dir, web.Name, strings.Join(web.Hosts, ", "), app.State)
	if app.Branch != "" {
		fmt.Fprintf(&b, "Git branch: %s\n", app.Branch)
	}
	for _, process := range app.Processes {
		fmt.Fprintf(&b, "Process %s: %s\n", process.Name, process.State)
	}
	for _, name := range []string{"dboss.yaml", "AGENTS.md", "CLAUDE.md", "README.md"} {
		data, err := os.ReadFile(filepath.Join(app.Dir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n%s\n", name, capText(string(data), maxNoteFile))
	}
	if files, err := s.appFiles(app.Dir); err == nil {
		fmt.Fprintf(&b, "\n## Files (%d)\n%s\n", len(files), strings.Join(files[:min(len(files), maxPromptList)], "\n"))
		if len(files) > maxPromptList {
			b.WriteString("[... more files; use list_files]\n")
		}
	}
	return b.String()
}

// unsetRef is a whole value that is still a $NAME: the config leaves a reference the daemon
// environment does not set as it is.
var unsetRef = regexp.MustCompile(`^\$[A-Z_][A-Z0-9_]*$`)

// chatKey is the app's DeepSeek API key. An unset $NAME counts as no key, so the chat reads as
// off instead of sending the reference to DeepSeek as a key.
func chatKey(app supervisor.Snapshot) string {
	if unsetRef.MatchString(app.Web.DeepseekAPIKey) {
		return ""
	}
	return app.Web.DeepseekAPIKey
}

// commitMessage asks DeepSeek for a commit message that describes the uncommitted diff.
func (s *Service) commitMessage(ctx context.Context, app supervisor.Snapshot) (string, error) {
	key := chatKey(app)
	if key == "" {
		return "", fault.Invalidf("set deepseek_api_key to suggest commit messages")
	}
	status, err := git.Status(ctx, app.Dir)
	if err != nil {
		return "", err
	}
	if len(status.Changes) == 0 {
		return "", fault.Invalidf("nothing to commit")
	}
	var diff strings.Builder
	for _, change := range status.Changes {
		text, err := git.Diff(ctx, app.Dir, change)
		if err != nil {
			fmt.Fprintf(&diff, "%s %s (diff too large)\n", change.Status, change.Path)
			continue
		}
		diff.WriteString(text)
		if diff.Len() > 30000 {
			break
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	message, err := s.complete(ctx, key, []Message{
		{Role: "system", Content: "Write a git commit message for the diff. First line: an imperative subject under 60 characters, no trailing period. Add a short body only when the subject cannot say enough. Answer with the message only, no quotes or code fences."},
		{Role: "user", Content: capText(diff.String(), 30000)},
	})
	return strings.Trim(message, "`\n "), err
}
