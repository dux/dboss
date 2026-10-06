package supervisor

import (
	"testing"
	"time"

	"dboss/internal/fault"
	"dboss/internal/ports"
)

// The errors a caller can fix carry the fault mark, so the HTTP API answers them 400, not 500.
func TestCallerErrorsAreMarkedInvalid(t *testing.T) {
	cfg := hookConfig(t, [2]int{34020, 34040}, "procfile:\n  worker: /bin/sleep 30\nautostart: false\ncron:\n  off:\n    schedule: every 1h\n    command: /usr/bin/true\n    disabled: true\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	for name, err := range map[string]error{
		"unknown app":       manager.Start("ghost"),
		"unknown process":   manager.StartProcess("demo", "web"),
		"unknown hook":      manager.RunHook("demo", "nope"),
		"unknown cron job":  manager.RunCron("demo", "nope"),
		"disabled cron job": manager.RunCron("demo", "off"),
		"not deletable":     manager.Destroy("demo"),
		"empty exec":        execErr(manager.Exec("demo", nil, time.Second)),
		"missing binary":    execErr(manager.Exec("demo", []string{"no-such-binary-dboss"}, time.Second)),
	} {
		if err == nil || !fault.IsInvalid(err) {
			t.Errorf("%s = %v, want a caller error", name, err)
		}
	}
}

func execErr(_ ExecResult, err error) error { return err }
