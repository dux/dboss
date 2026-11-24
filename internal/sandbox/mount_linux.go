package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"golang.org/x/sys/unix"
)

// keepPath is an allowed path as written and as resolved. The plan binds the resolved path back;
// link recreates the written one when it was a symlink inside a hidden folder, so the command
// still finds it by the name it was given.
type keepPath struct {
	written, resolved string
}

// mount builds the command's view of the file system in the launcher's own mount namespace. The
// launcher holds CAP_SYS_ADMIN only inside its user namespace and drops it before the exec.
func mount(plan []mountOp, keeps []keepPath) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make the mounts private: %w", err)
	}
	// An allowed path inside a hidden folder is opened before the folder is covered, then bound
	// back from that handle.
	sources := map[string]int{}
	defer func() {
		for _, fd := range sources {
			unix.Close(fd)
		}
	}()
	for _, op := range plan {
		if op.kind != opKeep {
			continue
		}
		fd, err := unix.Open(op.path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", op.path, err)
		}
		sources[op.path] = fd
	}
	// covered are the hidden folders, made read-only once the allowed paths are back; emptied adds
	// the private ones, where a link to an allowed path is recreated too.
	var covered, emptied []string
	for _, op := range plan {
		switch op.kind {
		case opPrivate:
			if err := unix.Mount("tmpfs", op.path, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777"); err != nil {
				return fmt.Errorf("mount a private %s: %w", op.path, err)
			}
			emptied = append(emptied, op.path)
		case opHide:
			if err := unix.Mount("tmpfs", op.path, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755"); err != nil {
				return fmt.Errorf("hide %s: %w", op.path, err)
			}
			covered = append(covered, op.path)
			emptied = append(emptied, op.path)
		case opDeny:
			if err := deny(op.path); err != nil {
				return err
			}
		case opKeep:
			if err := bindBack(sources[op.path], op.path); err != nil {
				return err
			}
		}
	}
	for _, keep := range keeps {
		if keep.written == keep.resolved || !slices.ContainsFunc(emptied, func(dir string) bool { return within(dir, keep.written) }) {
			continue
		}
		if _, err := os.Lstat(keep.written); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(keep.written), 0o755); err != nil {
			return fmt.Errorf("link %s: %w", keep.written, err)
		}
		if err := os.Symlink(keep.resolved, keep.written); err != nil {
			return fmt.Errorf("link %s: %w", keep.written, err)
		}
	}
	// The covering folders were writable only so the allowed paths could get their mount points.
	for _, path := range covered {
		if err := unix.Mount("", path, "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755"); err != nil {
			return fmt.Errorf("make %s read-only: %w", path, err)
		}
	}
	return nil
}

// deny covers a folder with an empty one nobody may enter, and a file with an unopenable device
// node (nodev), so reading either fails with permission denied.
func deny(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("deny %s: %w", path, err)
	}
	if info.IsDir() {
		if err := unix.Mount("tmpfs", path, "tmpfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0"); err != nil {
			return fmt.Errorf("deny %s: %w", path, err)
		}
		return nil
	}
	if err := unix.Mount("/dev/null", path, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("deny %s: %w", path, err)
	}
	// A bind keeps the flags of the mount it came from, and a user namespace may not drop them.
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return fmt.Errorf("deny %s: %w", path, err)
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	for st, ms := range map[int64]uintptr{unix.ST_NOATIME: unix.MS_NOATIME, unix.ST_NODIRATIME: unix.MS_NODIRATIME, unix.ST_RELATIME: unix.MS_RELATIME} {
		if int64(stat.Flags)&st != 0 {
			flags |= ms
		}
	}
	if err := unix.Mount("", path, "", flags, ""); err != nil {
		return fmt.Errorf("deny %s: %w", path, err)
	}
	return nil
}

// bindBack mounts the path the source handle points at over its own name inside a covering
// folder, creating the mount point first.
func bindBack(source int, path string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(source, &stat); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("allow %s: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
		if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("allow %s: %w", path, err)
		}
	} else if file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		return fmt.Errorf("allow %s: %w", path, err)
	} else {
		file.Close()
	}
	if err := unix.Mount("/proc/self/fd/"+strconv.Itoa(source), path, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("allow %s: %w", path, err)
	}
	return nil
}
