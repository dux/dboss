package config

import (
	"path/filepath"
	"testing"
)

func TestSplitTarget(t *testing.T) {
	host, path, err := SplitTarget("deploy@box.example.com:/srv/dboss/apps/myapp/")
	if err != nil || host != "deploy@box.example.com" || path != "/srv/dboss/apps/myapp" {
		t.Fatalf("got %q %q %v", host, path, err)
	}
	for _, bad := range []string{"box", "box:relative/path", ":/srv/x", "-oProxyCommand=x:/srv/x", "box:/", "box:/srv/my app", "box:/srv/$(id)"} {
		if _, _, err := SplitTarget(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

// A bare destination lands in the default host's apps folder under the checkout's own name; a
// full target is taken as written.
func TestParseAppServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	for server, want := range map[string]string{
		"deb1":                  "deb1:/srv/dboss/apps/shop",
		"deploy@deb1":           "deploy@deb1:/srv/dboss/apps/shop",
		"deb1:/opt/apps/store/": "deb1:/opt/apps/store/",
	} {
		app, err := ParseApp([]byte("procfile:\n  web: ./server\nserver: "+server+"\n"), path, Default().Defaults)
		if err != nil {
			t.Fatalf("server %q: %v", server, err)
		}
		if got := app.ServerTarget("shop"); got != want {
			t.Fatalf("server %q targets %q, want %q", server, got, want)
		}
	}
	for _, server := range []string{"-oProxyCommand=x", "deb1:relative", "'deb 1'", "deb1:/"} {
		if _, err := ParseApp([]byte("procfile:\n  web: ./server\nserver: "+server+"\n"), path, Default().Defaults); err == nil {
			t.Fatalf("server %q was accepted", server)
		}
	}
}
