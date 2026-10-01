package supervisor

import "testing"

func TestWebForHostPrefersTheConcreteService(t *testing.T) {
	snapshot := Snapshot{WebProcesses: []WebProcessSnapshot{
		{Name: "tenant", Hosts: []string{"*.foo.com"}},
		{Name: "admin", Hosts: []string{"baz.foo.com"}},
	}}
	for host, want := range map[string]string{
		"baz.foo.com": "admin",
		"qux.foo.com": "tenant",
	} {
		web, ok := snapshot.WebForHost(host)
		if !ok || web.Name != want {
			t.Fatalf("WebForHost(%q) = %q (%v), want %q", host, web.Name, ok, want)
		}
	}
}

func TestSnapshotServing(t *testing.T) {
	cases := []struct {
		name     string
		snapshot Snapshot
		want     bool
	}{
		{"running", Snapshot{State: Running}, true},
		{"stopped wakes on request", Snapshot{State: Stopped}, true},
		{"stopped button app", Snapshot{State: Stopped, WakeButton: true}, false},
		{"running button app", Snapshot{State: Running, WakeButton: true}, true},
		{"starting", Snapshot{State: Starting}, false},
		{"stopping", Snapshot{State: Stopping}, false},
		{"crashed", Snapshot{State: Crashed}, false},
		{"maintenance", Snapshot{State: Running, Maintenance: true}, false},
		{"draining", Snapshot{State: Running, Draining: true}, false},
	}
	for _, c := range cases {
		if got := c.snapshot.Serving(); got != c.want {
			t.Errorf("%s: Serving() = %v, want %v", c.name, got, c.want)
		}
	}
}
