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

func TestPrefixHosts(t *testing.T) {
	for _, test := range []struct {
		name     string
		hosts    []string
		prefixes []string
		want     []string
	}{
		{"no prefixes keeps hosts", []string{"foo.bar"}, nil, []string{"foo.bar"}},
		{"www also answers the apex", []string{"foo.bar"}, []string{"www"}, []string{"www.foo.bar", "foo.bar"}},
		{"wildcard also answers the apex", []string{"foo.bar"}, []string{"*"}, []string{"*.foo.bar", "foo.bar"}},
		{"several prefixes", []string{"foo.bar"}, []string{"www", "api"}, []string{"www.foo.bar", "foo.bar", "api.foo.bar"}},
		{"wildcard prefix", []string{"foo.bar"}, []string{"*.staging"}, []string{"*.staging.foo.bar"}},
		{"leading dot stays outermost", []string{".foo.bar"}, []string{"www"}, []string{".www.foo.bar", ".foo.bar"}},
		{"wildcard host stays outermost", []string{"*.foo.bar"}, []string{"api"}, []string{"*.api.foo.bar"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := prefixHosts(test.hosts, test.prefixes)
			if !slices.Equal(got, test.want) {
				t.Fatalf("prefixHosts(%v, %v) = %v, want %v", test.hosts, test.prefixes, got, test.want)
			}
		})
	}
}

func TestParseAppBaseHostAndHostPrefix(t *testing.T) {
	document := `base_host: foo.bar
procfile:
  web:
    command: ./web
    host_prefix: [www, api]
  admin:
    command: ./admin
    host_prefix: "*.staging"
`
	app, err := ParseApp([]byte(document), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.BaseHost != "foo.bar" {
		t.Fatalf("base_host = %q", app.BaseHost)
	}
	want := map[string][]string{
		"web":   {"www.foo.bar", "foo.bar", "api.foo.bar"},
		"admin": {"*.staging.foo.bar"},
	}
	for name, hosts := range want {
		if got := hostsOf(app, name); !slices.Equal(got, hosts) {
			t.Fatalf("%s hosts = %v, want %v", name, got, hosts)
		}
	}
	if len(app.Hosts) != 4 {
		t.Fatalf("app hosts = %v", app.Hosts)
	}
}

func TestParseAppHostPrefixOnExplicitHosts(t *testing.T) {
	document := `base_host: foo.bar
procfile:
  web:
    command: ./web
    hosts: [example.com]
    host_prefix: www
`
	app, err := ParseApp([]byte(document), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostsOf(app, "web"); !slices.Equal(got, []string{"www.example.com", "example.com"}) {
		t.Fatalf("web hosts = %v", got)
	}
}

func TestParseAppHostPrefixNeedsBase(t *testing.T) {
	document := "procfile:\n  web:\n    command: ./web\n    host_prefix: www\n"
	_, err := ParseApp([]byte(document), "/srv/apps/demo/"+FileName, Default().Defaults)
	if err == nil || !strings.Contains(err.Error(), "base_host") {
		t.Fatalf("expected a base_host error, got %v", err)
	}
}

// A dev session defaults the base host to lvh.me, so a host_prefix works with no config.
func TestParseAppDevBaseHostDefaultsToLvhMe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo", FileName)
	cfg, err := Parse([]byte("procfile:\n  web:\n    command: ./web\n    host_prefix: www\n"), path)
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

// base_host is an app key: neither the host top level nor a host defaults block accepts it.
func TestBaseHostIsAppOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	for _, document := range []string{
		"apps: ./apps\nbase_host: foo.bar\n",
		"apps: ./apps\ndefaults:\n  base_host: foo.bar\n",
	} {
		if _, err := Parse([]byte(document), path); err == nil {
			t.Fatalf("%q: expected an error", document)
		}
	}
}
