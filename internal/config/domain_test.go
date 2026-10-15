package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func hostsOf(app App, name string) []string {
	for _, web := range app.WebProcesses {
		if web.Name == name {
			return web.Hosts
		}
	}
	return nil
}

func TestSubdomainHosts(t *testing.T) {
	for _, test := range []struct {
		name       string
		subdomains []string
		want       []string
	}{
		{"www also answers the apex", []string{"www"}, []string{"www.foo.bar", "foo.bar"}},
		{"wildcard also answers the apex", []string{"*"}, []string{"*.foo.bar", "foo.bar"}},
		{"several subdomains", []string{"www", "api"}, []string{"www.foo.bar", "foo.bar", "api.foo.bar"}},
		{"wildcard subdomain", []string{"*.staging"}, []string{"*.staging.foo.bar"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := subdomainHosts("foo.bar", test.subdomains); !slices.Equal(got, test.want) {
				t.Fatalf("subdomainHosts(foo.bar, %v) = %v, want %v", test.subdomains, got, test.want)
			}
		})
	}
}

func TestParseAppDomainAndSubdomain(t *testing.T) {
	document := `domain: foo.bar
procfile:
  web:
    command: ./web
    subdomain: [www, api]
  admin:
    command: ./admin
    subdomain: "*.staging"
  other:
    command: ./other
    host: [example.com]
  worker: ./worker
`
	app, err := ParseApp([]byte(document), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.Domain != "foo.bar" {
		t.Fatalf("domain = %q", app.Domain)
	}
	want := map[string][]string{
		"web":    {"www.foo.bar", "foo.bar", "api.foo.bar"},
		"admin":  {"*.staging.foo.bar"},
		"other":  {"example.com"},
		"worker": nil,
	}
	for name, hosts := range want {
		if got := hostsOf(app, name); !slices.Equal(got, hosts) {
			t.Fatalf("%s hosts = %v, want %v", name, got, hosts)
		}
	}
	if app.IsWeb("worker") || len(app.Hosts) != 5 {
		t.Fatalf("web processes = %v, app hosts = %v", app.WebProcesses, app.Hosts)
	}
}

// A host process takes its patterns from a subdomain process of the same app.
func TestParseAppHostBeatsSubdomain(t *testing.T) {
	document := "domain: myapp.com\nprocfile:\n  app:\n    command: ./app\n    subdomain: \"*\"\n  landing:\n    command: ./landing\n    host: myapp.com\n"
	app, err := ParseApp([]byte(document), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostsOf(app, "app"); !slices.Equal(got, []string{"*.myapp.com"}) {
		t.Fatalf("app hosts = %v", got)
	}
	if got := hostsOf(app, "landing"); !slices.Equal(got, []string{"myapp.com"}) {
		t.Fatalf("landing hosts = %v", got)
	}
}

func TestAppYield(t *testing.T) {
	document := "domain: myapp.com\nprocfile:\n  web:\n    command: ./web\n    subdomain: \"*\"\n    canonical_host: myapp.com\n  admin:\n    command: ./admin\n    host: admin.other.com\n"
	app, err := ParseApp([]byte(document), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Yield("admin.other.com", "x"); err != nil || len(app.Hosts) != 3 {
		t.Fatalf("a host pattern never yields: %v %v", err, app.Hosts)
	}
	if err := app.Yield("MyApp.com", "landing"); err == nil || !strings.Contains(err.Error(), "canonical_host: myapp.com is served by app landing") {
		t.Fatalf("losing the canonical host: %v", err)
	}
	if got := hostsOf(app, "web"); len(got) != 2 {
		t.Fatalf("a refused yield must leave the app as it was: %v", got)
	}
	plain, err := ParseApp([]byte("domain: myapp.com\nprocfile:\n  web:\n    command: ./web\n    subdomain: \"*\"\n"), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Yield("myapp.com", "landing"); err != nil || !slices.Equal(plain.Hosts, []string{"*.myapp.com"}) {
		t.Fatalf("yield: %v %v", err, plain.Hosts)
	}
	if err := plain.Yield("*.myapp.com", "landing"); err == nil || !strings.Contains(err.Error(), "every host") {
		t.Fatalf("yielding the last host: %v", err)
	}
}

func TestParseAppHostRules(t *testing.T) {
	for _, test := range []struct{ name, document, want string }{
		{"subdomain needs a domain", "procfile:\n  web:\n    command: ./web\n    subdomain: www\n", "needs a domain"},
		{"host and subdomain", "domain: foo.bar\nprocfile:\n  web:\n    command: ./web\n    host: example.com\n    subdomain: www\n", "host and subdomain cannot both be set"},
		{"domain without a subdomain", "domain: foo.bar\nprocfile:\n  web: ./web\n", "no procfile entry has a subdomain"},
		{"subdomain left empty", "domain: myapp.com\nprocfile:\n  web:\n    command: ./web\n    subdomain: www\n  landing:\n    command: ./landing\n    host: [www.myapp.com, myapp.com]\n", "served by process \"landing\" through host"},
		{"two hosts", "procfile:\n  a:\n    command: ./a\n    host: myapp.com\n  b:\n    command: ./b\n    host: myapp.com\n", "already used by process"},
		{"two subdomains", "domain: myapp.com\nprocfile:\n  a:\n    command: ./a\n    subdomain: \"*\"\n  b:\n    command: ./b\n    subdomain: www\n", "already used by process"},
		{"invalid subdomain", "domain: foo.bar\nprocfile:\n  web:\n    command: ./web\n    subdomain: \"a b\"\n", "invalid subdomain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseApp([]byte(test.document), "/srv/apps/demo/"+FileName, Default().Defaults)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want an error naming %q", err, test.want)
			}
		})
	}
}

// A dev session defaults the domain to lvh.me, so a subdomain works with no config.
func TestParseAppDevDomainDefaultsToLvhMe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo", FileName)
	cfg, err := Parse([]byte("procfile:\n  web:\n    command: ./web\n    subdomain: www\n"), path)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostsOf(*cfg.App, "web"); !slices.Equal(got, []string{"www.lvh.me", "lvh.me"}) {
		t.Fatalf("web hosts = %v", got)
	}
}

// With no host config at all, dev keeps binding the app to <app>.lvh.me.
func TestParseAppDevWithoutHostConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo", FileName)
	cfg, err := Parse([]byte("procfile:\n  web: ./web\n"), path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.App.Hosts; !slices.Equal(got, []string{".demo.lvh.me"}) {
		t.Fatalf("app hosts = %v", got)
	}
}

// domain is an app key: neither the host top level nor a host defaults block accepts it.
func TestDomainIsAppOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	for _, document := range []string{
		"apps: ./apps\ndomain: foo.bar\n",
		"apps: ./apps\ndefaults:\n  domain: foo.bar\n",
	} {
		if _, err := Parse([]byte(document), path); err == nil {
			t.Fatalf("%q: expected an error", document)
		}
	}
}
