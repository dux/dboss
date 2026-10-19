package apps

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"dboss/internal/config"
)

// discoverApps writes one app folder per entry and runs discovery over them.
func discoverApps(t *testing.T, files map[string]string) (map[string]*App, map[string]string) {
	t.Helper()
	appsDir := t.TempDir()
	for name, contents := range files {
		if err := os.MkdirAll(filepath.Join(appsDir, name), 0o750); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(appsDir, name, config.FileName), contents)
	}
	cfg := config.Default()
	cfg.Apps = appsDir
	found, invalid, err := Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	apps := map[string]*App{}
	for _, app := range found {
		apps[app.Name] = app
	}
	refused := map[string]string{}
	for _, err := range invalid {
		scan := err.(ScanError)
		refused[scan.Name] = scan.Err.Error()
	}
	return apps, refused
}

const tenantApp = "domain: myapp.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: [\"@\", \"*\"]\n"

// A host beats a subdomain-derived pattern of another app, whichever sorts first.
func TestDiscoverHostBeatsAnotherAppsSubdomain(t *testing.T) {
	for _, names := range [][2]string{{"a-tenant", "b-landing"}, {"b-tenant", "a-landing"}} {
		apps, refused := discoverApps(t, map[string]string{
			names[0]: tenantApp,
			names[1]: "procfile:\n  web:\n    command: ./server\n    host: myapp.com\n",
		})
		if len(refused) != 0 {
			t.Fatalf("%v: refused %v", names, refused)
		}
		if got := apps[names[0]].Config.Hosts; !slices.Equal(got, []string{"*.myapp.com"}) {
			t.Fatalf("%v: tenant hosts = %v", names, got)
		}
		if got := apps[names[1]].Config.Hosts; !slices.Equal(got, []string{"myapp.com"}) {
			t.Fatalf("%v: landing hosts = %v", names, got)
		}
	}
}

// A www/apex redirect gives its hostname to any other app that covers it, and never clashes.
func TestDiscoverRedirectYieldsToOtherApps(t *testing.T) {
	for name, test := range map[string]struct {
		other string
		want  []string
	}{
		"subdomain www":    {"domain: myapp.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: www\n", []string{"myapp.com"}},
		"wildcard":         {"domain: myapp.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: \"*\"\n", []string{"myapp.com"}},
		"host":             {"procfile:\n  web:\n    command: ./server\n    host: www.myapp.com\n", []string{"myapp.com"}},
		"unrelated domain": {"domain: other.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: \"*\"\n", []string{"myapp.com", "www.myapp.com"}},
	} {
		for _, names := range [][2]string{{"a-apex", "b-other"}, {"b-apex", "a-other"}} {
			apps, refused := discoverApps(t, map[string]string{
				names[0]: "domain: myapp.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: \"@\"\n",
				names[1]: test.other,
			})
			if len(refused) != 0 {
				t.Fatalf("%s %v: refused %v", name, names, refused)
			}
			if got := apps[names[0]].Config.Hosts; !slices.Equal(got, test.want) {
				t.Fatalf("%s %v: apex hosts = %v, want %v", name, names, got, test.want)
			}
		}
	}
}

func TestDiscoverHostClaimsStillClash(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"host and host": {
			"a": "procfile:\n  web:\n    command: ./server\n    host: myapp.com\n",
			"b": "procfile:\n  web:\n    command: ./server\n    host: myapp.com\n",
		},
		"subdomain and subdomain": {
			"a": tenantApp,
			"b": "domain: myapp.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: \"@\"\n",
		},
	} {
		apps, refused := discoverApps(t, files)
		if apps["a"] == nil || !strings.Contains(refused["b"], "already owned by a") {
			t.Fatalf("%s: apps %v refused %v", name, apps, refused)
		}
	}
}

func TestDiscoverRefusesAnAppLeftWithoutItsHosts(t *testing.T) {
	_, refused := discoverApps(t, map[string]string{
		"a": "procfile:\n  web:\n    command: ./server\n    host: [www.myapp.com, myapp.com]\n",
		"b": "domain: myapp.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: www\n",
		"d": "domain: myapp.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: [\"@\", \"*\"]\n    canonical_host: myapp.com\n",
	})
	if !strings.Contains(refused["b"], "every host of its subdomain is served by app a") {
		t.Fatalf("b: %q", refused["b"])
	}
	if !strings.Contains(refused["d"], "canonical_host: myapp.com is served by app a") {
		t.Fatalf("d: %q", refused["d"])
	}
}

// An app refused over its subdomain hosts gives back the host patterns it reserved, so an app
// that only clashed with it is served.
func TestDiscoverRetriesWithoutALateRefusal(t *testing.T) {
	apps, refused := discoverApps(t, map[string]string{
		"a": tenantApp,
		"b": "domain: myapp.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: \"*\"\n  admin:\n    command: ./admin\n    host: admin.other.com\n",
		"c": "procfile:\n  web:\n    command: ./server\n    host: admin.other.com\n",
	})
	if !strings.Contains(refused["b"], "already owned by a") || apps["c"] == nil || len(refused) != 1 {
		t.Fatalf("apps %v refused %v", apps, refused)
	}
}
