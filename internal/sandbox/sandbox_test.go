package sandbox

import (
	"slices"
	"testing"
)

func TestMountPlan(t *testing.T) {
	plan := mountPlan(
		[]string{"/tmp"},
		[]string{"/run", "/srv/dboss/apps", "/run/dboss"},
		[]string{"/srv/dboss/.dboss", "/srv/dboss/apps/shop/.env", "/run/secret"},
		[]string{"/usr", "/srv/dboss/apps/shop", "/srv/dboss/apps/shop/storage", "/srv/dboss/apps/shop/.env/inner", "/run/postgresql", "/tmp", "/tmp/.X11-unix", "/srv/dboss/.dboss/log"},
	)
	want := []mountOp{
		{opPrivate, "/tmp"},
		{opHide, "/run"},
		{opKeep, "/run/postgresql"},
		{opKeep, "/tmp/.X11-unix"},
		{opHide, "/srv/dboss/apps"},
		{opDeny, "/srv/dboss/.dboss"},
		{opKeep, "/srv/dboss/apps/shop"},
		{opDeny, "/srv/dboss/apps/shop/.env"},
	}
	if !slices.Equal(plan, want) {
		t.Fatalf("plan:\n got %v\nwant %v", plan, want)
	}
}

func TestMountPlanKeepsAHiddenFolderNamedOutright(t *testing.T) {
	plan := mountPlan(nil, []string{"/run"}, nil, []string{"/run", "/run/postgresql"})
	if want := []mountOp{{opHide, "/run"}, {opKeep, "/run"}}; !slices.Equal(plan, want) {
		t.Fatalf("plan = %v, want %v", plan, want)
	}
}

func TestToolchainDirs(t *testing.T) {
	got := toolchainDirs("/home/app/.local/share/mise/installs/ruby/3.4.1/bin:/home/app/.local/bin:/home/app/bin:/usr/local/bin:/usr/bin:/bin:relative/bin:/opt/tool/sbin:/usr/bin", "/home/app")
	want := []string{"/home/app/.local/share/mise/installs/ruby/3.4.1", "/home/app/.local/bin", "/home/app/bin", "/usr/local", "/usr", "/bin", "/opt/tool"}
	if !slices.Equal(got, want) {
		t.Fatalf("toolchainDirs:\n got %v\nwant %v", got, want)
	}
}
