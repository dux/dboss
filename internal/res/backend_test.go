package res

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGroupStatsSumsGroup(t *testing.T) {
	output := "  100   1000  1,5\n  100  20000  2.0\n  100  30000  0.5\n  200  99999  9.0\ngarbage\n\n"
	stats := groupStats(output, []int{100})
	if stats.MemoryBytes != 51000*1024 {
		t.Fatalf("memory = %d, want %d", stats.MemoryBytes, 51000*1024)
	}
	if stats.CPUPercent != 4.0 {
		t.Fatalf("cpu = %v, want 4", stats.CPUPercent)
	}
	if !stats.Approximate {
		t.Fatal("procgroup stats must be approximate")
	}
}

func TestProcgroupStatsCountsChildren(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	leader := func() int64 {
		output, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			t.Fatal(err)
		}
		rss, _ := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
		return rss * 1024
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		stats, err := Procgroup{}.Stats("demo", "web", []int{pid})
		if err != nil {
			t.Fatal(err)
		}
		if stats.MemoryBytes > leader() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("group memory %d never exceeded the leader's", stats.MemoryBytes)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
