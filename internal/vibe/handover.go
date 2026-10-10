package vibe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dboss/internal/config"
	"dboss/internal/git"
	"dboss/internal/supervisor"
)

// stackFiles hint at what an app is built with, in the order the prompt names them.
var stackFiles = []string{"Gemfile", "package.json", "go.mod", "requirements.txt", "pyproject.toml", "composer.json", "Cargo.toml", "mix.exs", "deno.json", "bun.lockb"}

// handover is the prompt the AI handover button copies: everything an outside agent needs to start
// on the app through MCP, from live state.
func (s *Service) handover(base string, app supervisor.Snapshot, web supervisor.WebProcessSnapshot) (string, error) {
	token, err := s.Token(app.Name, web.Name)
	if err != nil {
		return "", err
	}
	endpoint := mcpURL(base, token)
	var stack []string
	for _, name := range stackFiles {
		if _, err := os.Stat(filepath.Join(app.Dir, name)); err == nil {
			stack = append(stack, name)
		}
	}
	readme := ""
	if data, err := os.ReadFile(filepath.Join(app.Dir, "README.md")); err == nil {
		readme = firstLines(string(data), 6)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are working on the web app %q, served by dboss at %s.\n", app.Name, base)
	fmt.Fprintf(&b, "The owner watches it live at %s%s.\n\n", base, config.VibePath)
	if status, err := git.Status(context.Background(), app.Dir); err == nil {
		fmt.Fprintf(&b, "Branch: %s (%d uncommitted changes, %d unpushed commits)\n", status.Branch, len(status.Changes), status.Ahead)
	}
	if len(stack) > 0 {
		fmt.Fprintf(&b, "Stack hints: %s\n", strings.Join(stack, ", "))
	}
	if readme != "" {
		fmt.Fprintf(&b, "README:\n%s\n", indent(readme))
	}
	fmt.Fprintf(&b, `
Connect this MCP server; it is how you read and change the app:

  %s

Claude Code: claude mcp add --transport http %s %s
Other clients: add a remote MCP server (Streamable HTTP) with the URL above.

Tools: app_info, list_files, read_file, search, write_file, edit_file, run, restart, logs, exceptions, http_get, git_status, commit.

Rules:
* Start with app_info, then read before you change.
* Change files only through the MCP tools; the app runs on the dboss host, not on your machine.
* After a change the app does not pick up on its own, call restart, then check logs and exceptions.
* Verify with http_get before you say a change works.
* Commit with a short subject when a change is done. Never push; the owner pushes from the harness.

Task: `, endpoint, mcpName(app.Name, web.Name), endpoint)
	return b.String(), nil
}

// mcpName is the server name the handover suggests to a client: the app, plus the process when it
// is not the usual web.
func mcpName(app, process string) string {
	if process == "web" {
		return app
	}
	return app + "-" + process
}

func firstLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.Join(lines[:min(len(lines), count)], "\n")
}

func indent(text string) string {
	return "  " + strings.ReplaceAll(text, "\n", "\n  ")
}
