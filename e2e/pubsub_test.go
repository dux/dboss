//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
)

func TestPubsubPublish(t *testing.T) {
	var secret struct {
		Secret string `json:"secret"`
	}
	api(t, "pubsub-secret", map[string]any{"app": "button"}, &secret)
	if secret.Secret == "" {
		t.Fatal("no publish secret")
	}
	// The hub is dboss's own, so it answers while the button app is stopped, and the publish
	// secret gets past the app's sign-in gate.
	publish := call{
		method: http.MethodPost,
		host:   "button.lvh.me",
		path:   "/socketio/lobby",
		body:   []byte(`{"event":"message","data":{"from":"e2e","text":"hi"}}`),
		header: map[string]string{"Authorization": "Bearer " + secret.Secret, "Content-Type": "application/json"},
	}
	if r := publish.do(t); r.Status >= 300 {
		t.Fatalf("publish answered %d: %s", r.Status, r.Body)
	}
	publish.header["Authorization"] = "Bearer wrong"
	if r := publish.do(t); r.Status < 400 {
		t.Fatalf("a wrong secret answered %d", r.Status)
	}
	api(t, "pubsub-publish", map[string]any{"app": "button", "channel": "lobby", "event": "message", "data": map[string]string{"text": "from api"}}, nil)
	if text := apiText(t, "pubsub", nil); !strings.Contains(text, "button") {
		t.Fatalf("pubsub lists no button hub: %s", text)
	}
	// Rotating replaces the secret, and the old one stops working.
	api(t, "pubsub-rotate", map[string]any{"app": "button"}, nil)
	publish.header["Authorization"] = "Bearer " + secret.Secret
	if r := publish.do(t); r.Status < 400 {
		t.Fatalf("the rotated-out secret answered %d", r.Status)
	}
}
