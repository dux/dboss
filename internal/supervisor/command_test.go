package supervisor

import (
	"os"
	"slices"
	"strings"
	"testing"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/sandbox"
)

// TestMain lets the test binary act as the sandbox launcher, which the supervisor starts as
// /proc/self/exe, so a sandboxed command in a test runs the real launcher.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == sandbox.Arg {
		sandbox.Main(os.Args[2:])
	}
	os.Exit(m.Run())
}

func TestNewCommandRunsConfigLinesThroughTheShell(t *testing.T) {
	env := map[string]string{"PATH": os.Getenv("PATH"), "NAME": "dboss"}
	cmd, err := newCommand(t.TempDir(), apps.Command{Line: `printf '%s\n' "a b" && echo $NAME`}, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil || string(out) != "a b\ndboss\n" {
		t.Fatalf("output = %q, err = %v", out, err)
	}
}

func TestNewCommandExecsArgvDirectly(t *testing.T) {
	cmd, err := newCommand(t.TempDir(), apps.Command{Argv: []string{"echo", "a && b"}}, map[string]string{"PATH": os.Getenv("PATH")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "a && b" {
		t.Fatalf("output = %q, err = %v", out, err)
	}
}

func TestSandboxPolicyConfinesAppCommandsOnly(t *testing.T) {
	host := config.Default()
	host.Apps, host.RuntimeDir = "/srv/dboss/apps", "/srv/dboss/.dboss"
	spec := &apps.App{Name: "shop", Dir: "/srv/dboss/apps/shop"}
	box := config.Sandbox{Enabled: true, Read: config.List{"/srv/shared"}, Write: config.List{"./storage"}, Deny: config.List{"./.env"}, Sockets: config.List{"/run/postgresql"}, Bind: config.Ports{2525}, Connect: config.Ports{5432}}
	web := apps.Command{Name: "web", Line: "./server"}
	if policy, err := sandboxPolicy(host, spec, config.Sandbox{}, web, 3100); policy != nil || err != nil {
		t.Fatalf("sandbox off = %+v, %v", policy, err)
	}
	if policy, err := sandboxPolicy(host, spec, box, apps.Command{Name: "branch", Line: "git checkout main", Builtin: true}, 0); policy != nil || err != nil {
		t.Fatalf("a builtin command = %+v, %v; it rewrites the checkout and must run unconfined", policy, err)
	}
	policy, err := sandboxPolicy(host, spec, box, web, 3100)
	if sandbox.Check() != nil {
		if policy != nil || err == nil || !strings.Contains(err.Error(), "sandbox") {
			t.Fatalf("a host without a sandbox = %+v, %v; it must refuse the process", policy, err)
		}
		dev := host
		dev.App = &config.App{}
		if policy, err := sandboxPolicy(dev, spec, box, web, 3100); policy != nil || err != nil {
			t.Fatalf("a dev session without a sandbox = %+v, %v; it must run unconfined", policy, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if policy.Dir != spec.Dir || !slices.Equal(policy.Read, []string{"/srv/shared"}) || !slices.Equal(policy.Write, []string{"/srv/dboss/apps/shop/storage"}) ||
		!slices.Equal(policy.Deny, []string{"/srv/dboss/apps/shop/.env", "/srv/dboss/.dboss", "/srv/dboss/apps/shop/dboss.yaml", "/srv/dboss/apps/shop/dboss.local.yaml", "/srv/dboss/apps/shop/config/dboss.yaml", "/srv/dboss/apps/shop/config/dboss.local.yaml"}) || !slices.Equal(policy.Hide, []string{"/srv/dboss/apps"}) ||
		!slices.Equal(policy.Bind, []int{2525, 3100}) || !slices.Equal(policy.Connect, []int{5432}) {
		t.Fatalf("policy = %+v", policy)
	}
}
