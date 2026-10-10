package vibe

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dboss/internal/config"
	"dboss/internal/fault"
	"dboss/internal/supervisor"
)

const (
	httpGetTimeout = 30 * time.Second
	maxCapture     = 1 << 20
)

// internalKey marks a request the harness itself sends through the proxy for http_get; its value
// is "<app>/<process>". Only code in this process can set a context value, so a client never can.
type internalKey struct{}

// SetProxy hands the service the app proxy, so http_get walks the same pipeline a browser does:
// static files, the starting page, the app. Call it before the proxy serves.
func (s *Service) SetProxy(handler http.Handler) { s.proxy = handler }

// internal reports whether r is the harness's own http_get for this app and web process.
func internal(r *http.Request, app supervisor.Snapshot, web supervisor.WebProcessSnapshot) bool {
	value, _ := r.Context().Value(internalKey{}).(string)
	return value != "" && value == app.Name+"/"+web.Name
}

// capture is the response writer of one http_get: headers, status and the head of the body.
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
	total  int
}

func (c *capture) Header() http.Header { return c.header }

func (c *capture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *capture) Write(data []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	c.total += len(data)
	if room := maxCapture - c.body.Len(); room > 0 {
		c.body.Write(data[:min(len(data), room)])
	}
	return len(data), nil
}

func (c *capture) Flush() {}

func httpGet(ctx context.Context, c *call, args arguments) (string, error) {
	target := args.str("path")
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return "", fault.Invalidf("path must start with /, e.g. /cart")
	}
	if c.s.proxy == nil {
		return "", fault.Invalidf("no proxy to send the request through")
	}
	app, err := c.snapshot()
	if err != nil {
		return "", err
	}
	host := ""
	for _, web := range app.WebProcesses {
		if web.Name == c.web {
			host = config.PrimaryHost(web.CanonicalHost, web.Hosts)
		}
	}
	if host == "" {
		return "", fault.Invalidf("web process %s has no concrete host to request", c.web)
	}
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, internalKey{}, c.app+"/"+c.web), httpGetTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+target, nil)
	if err != nil {
		return "", fault.Invalidf("invalid path: %v", err)
	}
	request.RemoteAddr = "127.0.0.1:0"
	request.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")
	request.Header.Set("User-Agent", "dboss-vibe")
	response := &capture{header: http.Header{}}
	c.s.proxy.ServeHTTP(response, request)
	if response.status == 0 {
		response.status = http.StatusOK
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s\n", response.status, http.StatusText(response.status))
	for _, name := range []string{"Content-Type", "Location", "Content-Length"} {
		if value := response.header.Get(name); value != "" {
			fmt.Fprintf(&b, "%s: %s\n", name, value)
		}
	}
	body := response.body.Bytes()
	switch {
	case len(body) == 0:
	case bytes.IndexByte(body[:min(len(body), 8000)], 0) >= 0:
		fmt.Fprintf(&b, "\n[binary body, %d bytes]", response.total)
	default:
		fmt.Fprintf(&b, "\n%s", capText(string(body), maxResult-200))
	}
	return b.String(), nil
}
