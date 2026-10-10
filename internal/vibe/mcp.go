package vibe

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"dboss/internal/config"
	"dboss/internal/httpx"
	"dboss/internal/supervisor"
	"dboss/internal/version"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpToolTimeout bounds one MCP tool call; run has its own, shorter cap.
const mcpToolTimeout = 10 * time.Minute

// Token is the MCP token of one web process's harness, generated on first use.
func (s *Service) Token(app, process string) (string, error) {
	return s.secrets.Ensure(app, process)
}

// rotate replaces the MCP token, so a leaked handover prompt stops working.
func (s *Service) rotate(app, process string) (string, error) {
	return s.secrets.Rotate(app, process)
}

// mcpURL is the address an MCP client connects to, token included.
func mcpURL(base, token string) string { return base + config.VibePath + "/mcp/" + token }

// serveMCP answers the MCP endpoint. The token travels in the path, so a client that cannot set
// headers still connects; a bearer header works too. Every check books a slot in the per-IP
// throttle and a right token gives it back, so a guess queues and the owner never waits.
func (s *Service) serveMCP(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, web supervisor.WebProcessSnapshot, token string) {
	if token == "" {
		token = httpx.BearerToken(r)
	}
	want, err := s.Token(app.Name, web.Name)
	if err != nil {
		http.Error(w, "mcp token unavailable", http.StatusInternalServerError)
		return
	}
	slot, ok := s.logins.Reserve(httpx.ClientIP(r, s.cloudflare))
	if !ok {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	time.Sleep(slot.Wait)
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		http.Error(w, "invalid mcp token", http.StatusUnauthorized)
		return
	}
	s.logins.Release(slot)
	server := s.mcpServer(app.Name, web.Name)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// The token is the credential; dev hosts are *.lvh.me on loopback, which the guard refuses.
		DisableLocalhostProtection: true,
	})
	handler.ServeHTTP(w, r)
}

// mcpServer serves the tool registry for one harness. Every call shows up in its chat.
func (s *Service) mcpServer(app, process string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "dboss-vibe", Title: "dboss vibe: " + app, Version: version.String()}, &mcp.ServerOptions{
		Instructions: "Tools to read and change the web app " + app + " that dboss runs, restart it, read its logs and exceptions and verify pages with http_get. Never push; the owner pushes.",
	})
	for _, tool := range tools {
		server.AddTool(&mcp.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.Schema()}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
			defer cancel()
			c := &call{s: s, app: app, web: process, actor: "mcp:" + app + "/" + process, harness: s.harness(app, process), external: true}
			var raw json.RawMessage
			if request.Params != nil {
				raw = request.Params.Arguments
			}
			result, ok := s.runTool(ctx, c, tool.Name, raw)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}, IsError: !ok}, nil
		})
	}
	return server
}
