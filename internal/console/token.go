package console

import (
	"crypto/subtle"
	"math"
	"net/http"
	"strconv"
	"time"

	"dboss/internal/httpx"
)

type tokenResult int

const (
	tokenOK tokenResult = iota
	tokenWrong
	tokenLimited
)

// checkToken is the one check behind every token-guarded route (/api, /hooks, /metrics). It books
// a throttle slot for the client IP and waits for it before comparing, so parallel guesses queue
// and the reply time never tells a wrong token from a right one. A match gives the slot back, so a
// client that never fails never waits. Past throttle.MaxWait of queue it answers tokenLimited and
// sets Retry-After.
func (h *Handler) checkToken(w http.ResponseWriter, r *http.Request, matches func() bool) tokenResult {
	slot, ok := h.tokens.Reserve(h.clientIP(r))
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(slot.Wait.Seconds()))))
		return tokenLimited
	}
	if slot.Wait > 0 {
		timer := time.NewTimer(slot.Wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return tokenWrong
		}
	}
	if !matches() {
		return tokenWrong
	}
	h.tokens.Release(slot)
	return tokenOK
}

// tokenIn reports whether presented equals one of the accepted tokens; empty ones never match.
// Every accepted token is compared in constant time, a match or not.
func tokenIn(presented string, accepted ...string) bool {
	found := false
	for _, token := range accepted {
		if token != "" && presented != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1 {
			found = true
		}
	}
	return found
}

// clientIP keys the throttle: the console shares the proxy's listener, so behind Cloudflare the
// connection is the edge and the visitor is CF-Connecting-IP, as for the proxy.
func (h *Handler) clientIP(r *http.Request) string {
	return httpx.ClientIP(r, h.cloudflare)
}

// tokenRetry is the message of a refused check, matching the Retry-After checkToken set.
func tokenRetry(w http.ResponseWriter) string {
	return "too many wrong tokens from this address; retry in " + w.Header().Get("Retry-After") + "s"
}
