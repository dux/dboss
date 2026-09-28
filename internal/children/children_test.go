package children

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	// The shell backgrounds a grandchild in the same group, like a worker that forks.
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	return cmd
}

func TestReapStopsTheGroupAnEarlierSessionLeft(t *testing.T) {
	ledger := New(t.TempDir())
	cmd := sleeper(t)
	if err := ledger.Start(cmd, "app/job"); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	labels, err := ledger.Reap(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if len(labels) != 1 || labels[0] != "app/job (pid "+strconv.Itoa(pid)+")" {
		t.Fatalf("labels = %v", labels)
	}
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("leader still running")
	}
	for range 30 {
		if syscall.Kill(-pid, 0) != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if syscall.Kill(-pid, 0) == nil {
		t.Fatal("group still has members")
	}
	if _, err := os.Stat(ledger.path(pid)); !os.IsNotExist(err) {
		t.Fatalf("entry kept: %v", err)
	}
}

func TestReapLeavesAReusedPidAlone(t *testing.T) {
	ledger := New(t.TempDir())
	cmd := sleeper(t)
	if err := ledger.Start(cmd, "app/web"); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	data, err := os.ReadFile(ledger.path(pid))
	if err != nil {
		t.Fatal(err)
	}
	// A different start time is what a later process on the same pid looks like.
	_, label, _ := strings.Cut(string(data), "\n")
	if err := os.WriteFile(ledger.path(pid), []byte("1\n"+label), 0o640); err != nil {
		t.Fatal(err)
	}
	labels, err := ledger.Reap(time.Second)
	if err != nil || len(labels) != 0 {
		t.Fatalf("labels = %v, err = %v", labels, err)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("unrelated process was signaled")
	}
	if _, err := os.Stat(ledger.path(pid)); !os.IsNotExist(err) {
		t.Fatalf("entry kept: %v", err)
	}
}

func TestDoneForgetsAnExitedChild(t *testing.T) {
	ledger := New(t.TempDir())
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := ledger.Start(cmd, "app/cron"); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ledger.Done(cmd.Process.Pid)
	if labels, err := ledger.Reap(time.Second); err != nil || len(labels) != 0 {
		t.Fatalf("labels = %v, err = %v", labels, err)
	}
}
