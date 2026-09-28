package children

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// startTime is field 22 of /proc/<pid>/stat: clock ticks since boot, fixed at fork.
func startTime(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return "", errGone
	}
	if err != nil {
		return "", err
	}
	// comm (field 2) may hold spaces and parens, so count from its closing paren.
	end := strings.LastIndexByte(string(data), ')')
	fields := strings.Fields(string(data[end+1:]))
	if end < 0 || len(fields) < 20 {
		return "", fmt.Errorf("parse /proc/%d/stat", pid)
	}
	return fields[19], nil
}
