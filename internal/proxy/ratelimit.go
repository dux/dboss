package proxy

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dboss/internal/config"
	"dboss/internal/pages"
	"dboss/internal/supervisor"
)

// rateSweepInterval is how often the background pass reclaims idle client counters. It only
// decides how promptly memory is freed, never the limit itself.
const rateSweepInterval = 30 * time.Second

// rateMaxClients caps the tracked clients; past it the sweep evicts the least recently seen.
const rateMaxClients = 100_000

// rateCounters is one client's sliding window: slots[i] holds a single second's request count
// (addressed by second % len), total their sum, last the unix second they have advanced to.
type rateCounters struct {
	slots []uint32
	total uint32
	last  int64
}

// rateLimiter keeps one counter set per (app, client IP). now is injectable for tests.
type rateLimiter struct {
	mu      sync.Mutex
	clients map[rateKey]*rateCounters
	now     func() int64
}

type rateKey struct {
	app string
	ip  string
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{clients: map[rateKey]*rateCounters{}, now: func() int64 { return time.Now().Unix() }}
}

// allow charges one request to app/ip and reports whether it fits, plus the Retry-After seconds
// when it does not.
func (l *rateLimiter) allow(app, ip string, rl config.RateLimit) (bool, int) {
	seconds := rl.WindowSeconds()
	if seconds < 1 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	key := rateKey{app: app, ip: ip}
	c := l.clients[key]
	if c == nil || len(c.slots) != seconds {
		// a new client, or the window changed under a rescan: start the window fresh
		c = &rateCounters{slots: make([]uint32, seconds), last: now}
		l.clients[key] = c
	}
	c.advance(now, seconds)

	limit := uint32(rl.Requests)
	if c.total >= limit {
		return false, c.retryAfter(now, seconds)
	}
	c.slots[int(now%int64(seconds))]++
	c.total++
	return true, 0
}

// sweep drops counters whose whole window has aged out and trims the map back to rateMaxClients.
func (l *rateLimiter) sweep(now int64) { l.trim(now, rateMaxClients) }

// trim ages out idle counters and, past max, evicts the least recently seen until it fits.
func (l *rateLimiter) trim(now int64, max int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, c := range l.clients {
		if now-c.last >= int64(len(c.slots)) {
			delete(l.clients, key)
		}
	}
	over := len(l.clients) - max
	if over <= 0 {
		return
	}
	keys := make([]rateKey, 0, len(l.clients))
	for key := range l.clients {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return l.clients[keys[i]].last < l.clients[keys[j]].last })
	for _, key := range keys[:over] {
		delete(l.clients, key)
	}
}

// advance rolls the window forward to now, clearing the seconds it passes.
func (c *rateCounters) advance(now int64, seconds int) {
	if now <= c.last {
		return
	}
	if now-c.last >= int64(seconds) {
		for i := range c.slots {
			c.slots[i] = 0
		}
		c.total = 0
	} else {
		for k := c.last + 1; k <= now; k++ {
			idx := int(k % int64(seconds))
			c.total -= c.slots[idx]
			c.slots[idx] = 0
		}
	}
	c.last = now
}

// retryAfter is the seconds until the oldest live second ages out of the window.
func (c *rateCounters) retryAfter(now int64, seconds int) int {
	for s := now - int64(seconds) + 1; s <= now; s++ {
		if c.slots[int(s%int64(seconds))] > 0 {
			if wait := s + int64(seconds) - now; wait > 0 {
				return int(wait)
			}
			return 1
		}
	}
	return 1
}

// rateLimit is the pipeline stage. It charges only requests that match both the configured methods
// and paths; anything else passes without touching the limiter.
func (h *Handler) rateLimit(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	rl := app.Web.RateLimit
	if !rl.Enabled() || !rateMatch(r, rl) {
		next()
		return
	}
	ip := clientIP(r, h.cfg.Proxy.Cloudflare)
	if ip == "" {
		next()
		return
	}
	if ok, retry := h.limiter.allow(app.Name, ip, rl); !ok {
		h.limited(w, r, app, retry)
		return
	}
	next()
}

// rateMatch reports whether a request is subject to the limit. An empty methods or paths list is a
// wildcard for that dimension, and a request must match both.
func rateMatch(r *http.Request, rl config.RateLimit) bool {
	if len(rl.Methods) > 0 && !hasMethod(rl.Methods, r.Method) {
		return false
	}
	if len(rl.Paths) > 0 && !denied(r.URL.Path, rl.Paths) {
		return false
	}
	return true
}

func hasMethod(methods []string, method string) bool {
	for _, candidate := range methods {
		if strings.EqualFold(candidate, method) {
			return true
		}
	}
	return false
}

// limited answers 429 with Retry-After; an HTML request gets the page, anything else the status.
func (h *Handler) limited(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, retryAfter int) {
	w.Header().Set("Cache-Control", "no-store")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	if !wantsHTML(r) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	pages.Page{Name: pages.RateLimited, App: app.Name}.Write(w, h.pageDirs(app)...)
}
