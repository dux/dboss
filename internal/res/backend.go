package res

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

type Stats struct {
	MemoryBytes int64   `json:"memory_bytes"`
	CPUPercent  float64 `json:"cpu_percent"`
	Approximate bool    `json:"approximate"`
}

// Limits are the per-process resource limits from config. They are only enforced by the cgroup
// backend; the procgroup backend ignores them.
type Limits struct {
	MemoryMax int64
	CPUMax    int
}

// Backend places a process in a resource domain, reports its use and releases the domain when the
// process is gone. app is the app name, proc the procfile process name.
type Backend interface {
	Name() string
	Place(app, proc string, pid int, limits Limits) error
	Stats(app, proc string, pids []int) (Stats, error)
	Release(app, proc string) error
	// OOMKills is the cumulative count of kernel OOM kills in the process's domain; a backend that
	// cannot tell returns 0.
	OOMKills(app, proc string) int64
}

type Procgroup struct{}

func (Procgroup) Name() string { return "procgroup" }

func (Procgroup) Place(string, string, int, Limits) error { return nil }

func (Procgroup) Release(string, string) error { return nil }

func (Procgroup) OOMKills(string, string) int64 { return 0 }

// Stats sums the whole process group of each pid. Every child leads its own session, so the
// group is the leader plus everything it forked; the sh -c wrapper alone is a few MB.
func (Procgroup) Stats(_ string, _ string, pids []int) (Stats, error) {
	output, err := exec.Command("ps", "-A", "-o", "pgid=,rss=,%cpu=").Output()
	if err != nil {
		return Stats{Approximate: true}, nil
	}
	return groupStats(string(output), pids), nil
}

// groupStats sums every `pgid rss %cpu` row whose pgid is one of pids.
func groupStats(output string, pids []int) Stats {
	stats := Stats{Approximate: true}
	groups := map[int64]bool{}
	for _, pid := range pids {
		groups[int64(pid)] = true
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		if group, _ := strconv.ParseInt(fields[0], 10, 64); !groups[group] {
			continue
		}
		rss, _ := strconv.ParseInt(fields[1], 10, 64)
		cpu, _ := strconv.ParseFloat(strings.ReplaceAll(fields[2], ",", "."), 64)
		stats.MemoryBytes += rss * 1024
		stats.CPUPercent += cpu
	}
	return stats
}

func Signal(name string) (syscall.Signal, error) {
	switch name {
	case "TERM":
		return syscall.SIGTERM, nil
	case "INT":
		return syscall.SIGINT, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	default:
		return 0, fmt.Errorf("unsupported signal %q", name)
	}
}
