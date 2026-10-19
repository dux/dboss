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
	toApex := &HostRedirect{From: "www.foo.bar", To: "foo.bar"}
	toWWW := &HostRedirect{From: "foo.bar", To: "www.foo.bar"}
	for _, test := range []struct {
		name       string
		subdomains []string
		want       []string
		redirect   *HostRedirect
	}{
		{"apex redirects www to itself", []string{"@"}, []string{"foo.bar"}, toApex},
		{"www redirects the apex to itself", []string{"www"}, []string{"www.foo.bar"}, toWWW},
		{"apex and www need no redirect", []string{"@", "www"}, []string{"foo.bar", "www.foo.bar"}, nil},
		{"wildcard leaves out the apex", []string{"*"}, []string{"*.foo.bar"}, nil},
		{"apex and wildcard", []string{"@", "*"}, []string{"foo.bar", "*.foo.bar"}, toApex},
		{"the redirect target leads", []string{"api", "www"}, []string{"www.foo.bar", "api.foo.bar"}, toWWW},
		{"wildcard subdomain", []string{"*.staging"}, []string{"*.staging.foo.bar"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, redirect := subdomainHosts("foo.bar", test.subdomains)
			if !slices.Equal(got, test.want) {
				t.Fatalf("subdomainHosts(foo.bar, %v) = %v, want %v", test.subdomains, got, test.want)
			}
			if (redirect == nil) != (test.redirect == nil) || redirect != nil && *redirect != *test.redirect {
				t.Fatalf("redirect = %v, want %v", redirect, test.redirect)
			}
		})
	}
}

// The www/apex redirect host goes last, after the served hosts, and never contends with another
// process: one that covers it takes it.
func TestParseAppRedirectHost(t *testing.T) {
	for _, test := range []struct {
		name, document string
		web            []string
		redirect       *HostRedirect
	}{
		{"apex", "domain: foo.bar\nprocfile:\n  web:\n    command: ./web\n    subdomain: \"@\"\n", []string{"foo.bar", "www.foo.bar"}, &HostRedirect{From: "www.foo.bar", To: "foo.bar"}},
		{"www", "domain: foo.bar\nprocfile:\n  web:\n    command: ./web\n    subdomain: [api, www]\n", []string{"www.foo.bar", "api.foo.bar", "foo.bar"}, &HostRedirect{From: "foo.bar", To: "www.foo.bar"}},
		{"another process serves www", "domain: foo.bar\nprocfile:\n  web:\n    command: ./web\n    subdomain: \"@\"\n  blog:\n    command: ./blog\n    subdomain: www\n", []string{"foo.bar"}, nil},
		{"another process covers www", "domain: foo.bar\nprocfile:\n  web:\n    command: ./web\n    subdomain: \"@\"\n  tenants:\n    command: ./tenants\n    subdomain: \"*\"\n", []string{"foo.bar"}, nil},
		{"a host process takes the target", "domain: foo.bar\nprocfile:\n  web:\n    command: ./web\n    subdomain: [\"@\", api]\n  landing:\n    command: ./landing\n    host: foo.bar\n", []string{"api.foo.bar"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, err := ParseApp([]byte(test.document), "/srv/apps/demo/"+FileName, Default().Defaults)
			if err != nil {
				t.Fatal(err)
			}
			web := app.WebProcesses[slices.IndexFunc(app.WebProcesses, func(web WebProcess) bool { return web.Name == "web" })]
			if !slices.Equal(web.Hosts, test.web) {
				t.Fatalf("web hosts = %v, want %v", web.Hosts, test.web)
			}
			if (web.Redirect == nil) != (test.redirect == nil) || web.Redirect != nil && *web.Redirect != *test.redirect {
				t.Fatalf("redirect = %v, want %v", web.Redirect, test.redirect)
			}
			if got := PrimaryHost(web.CanonicalHost, web.Hosts); test.redirect != nil && got != test.redirect.To {
				t.Fatalf("primary host = %q, want the redirect target %q", got, test.redirect.To)
			}
		})
	}
}

func TestParseAppDomainAndSubdomain(t *testing.T) {
	document := `domain: foo.bar
procfile:
  web:
    command: ./web
    subdomain: ["@", www, api]
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
		"web":    {"foo.bar", "www.foo.bar", "api.foo.bar"},
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
	document := "domain: myapp.com\nprocfile:\n  app:\n    command: ./app\n    subdomain: [\"@\", \"*\"]\n  landing:\n    command: ./landing\n    host: myapp.com\n"
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
	document := "domain: myapp.com\nprocfile:\n  web:\n    command: ./web\n    subdomain: [\"@\", \"*\"]\n    canonical_host: myapp.com\n  admin:\n    command: ./admin\n    host: admin.other.com\n"
	app, err := ParseApp([]byte(document), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Yield("admin.other.com", "x"); err != nil || len(app.Hosts) != 4 {
		t.Fatalf("a host pattern never yields: %v %v", err, app.Hosts)
	}
	if err := app.Yield("MyApp.com", "landing"); err == nil || !strings.Contains(err.Error(), "canonical_host: myapp.com is served by app landing") {
		t.Fatalf("losing the canonical host: %v", err)
	}
	if got := hostsOf(app, "web"); len(got) != 3 {
		t.Fatalf("a refused yield must leave the app as it was: %v", got)
	}
	plain, err := ParseApp([]byte("domain: myapp.com\nprocfile:\n  web:\n    command: ./web\n    subdomain: [\"@\", \"*\"]\n"), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Yield("*.myapp.com", "tenants"); err != nil || !slices.Equal(plain.Hosts, []string{"myapp.com", "www.myapp.com"}) {
		t.Fatalf("yielding a host keeps the redirect to the apex: %v %v", err, plain.Hosts)
	}
	plain, err = ParseApp([]byte("domain: myapp.com\nprocfile:\n  web:\n    command: ./web\n    subdomain: [\"@\", \"*\"]\n"), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Yield("myapp.com", "landing"); err != nil || !slices.Equal(plain.Hosts, []string{"*.myapp.com"}) || plain.WebProcesses[0].Redirect != nil {
		t.Fatalf("yielding the apex drops the redirect to it: %v %v", err, plain.Hosts)
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
		{"two subdomains", "domain: myapp.com\nprocfile:\n  a:\n    command: ./a\n    subdomain: \"@\"\n  b:\n    command: ./b\n    subdomain: [\"@\", api]\n", "already used by process"},
		{"invalid subdomain", "domain: foo.bar\nprocfile:\n  web:\n    command: ./web\n    subdomain: \"a b\"\n", "invalid subdomain"},
		{"empty subdomain", "domain: foo.bar\nprocfile:\n  web:\n    command: ./web\n    subdomain: \"\"\n", `use "@" for the bare domain`},
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
	if got := hostsOf(*cfg.App, "web"); !slices.Equal(got, []string{"www.lvh.me", "lvh.me"}) || cfg.App.WebProcesses[0].Redirect.To != "www.lvh.me" {
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
