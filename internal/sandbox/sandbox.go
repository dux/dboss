// Package sandbox confines the commands dboss runs for an app on Linux. The supervisor turns the
// app's sandbox keys into a Policy and starts the command through Command, which runs the dboss
// binary again as the launcher (Main) in a new user and mount namespace. The launcher hides what
// the app must not reach, applies Landlock and then execs the command, so the confinement holds
// for the command and everything it starts.
package sandbox

import (
	"path/filepath"
	"slices"
	"strings"
)

// Arg is the first argument that makes the dboss binary the sandbox launcher instead of the CLI.
const Arg = "sandbox-exec"

// MinABI is the Landlock version the sandbox needs: TCP bind and connect rules (4) and the signal
// and abstract socket scopes (6), Linux 6.12. An older kernel gets no sandbox at all rather than
// a partial one.
const MinABI = 6

// Policy is one confined command. Every path is absolute. Dir is the app folder: the command
// starts there, reads all of it and writes its ./tmp and ./log. Read and Write add paths, Deny
// hides paths even inside an allowed folder, Hide covers folders the command must not see but
// whose allowed children stay visible (the apps folder), Sockets are the Unix sockets it may
// reach, Bind and Connect the TCP ports it may listen on and dial.
type Policy struct {
	Dir     string   `json:"dir"`
	Read    []string `json:"read,omitempty"`
	Write   []string `json:"write,omitempty"`
	Deny    []string `json:"deny,omitempty"`
	Hide    []string `json:"hide,omitempty"`
	Sockets []string `json:"sockets,omitempty"`
	Bind    []int    `json:"bind,omitempty"`
	Connect []int    `json:"connect,omitempty"`
}

// toolchainDirs maps each PATH entry to what a program found there needs: the install prefix
// above a bin or sbin folder (its lib and share), or the entry alone when that prefix is the
// root, the home folder or ~/.local, which hold far more than one tool. mise puts each tool's
// own bin folder on PATH, so a mise toolchain is readable without opening the home folder.
func toolchainDirs(path, home string) []string {
	var dirs []string
	for _, entry := range filepath.SplitList(path) {
		if !filepath.IsAbs(entry) {
			continue
		}
		entry = filepath.Clean(entry)
		dir := entry
		if base := filepath.Base(entry); base == "bin" || base == "sbin" {
			prefix := filepath.Dir(entry)
			if prefix != "/" && (home == "" || prefix != home && prefix != filepath.Join(home, ".local")) {
				dir = prefix
			}
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

type opKind int

const (
	opPrivate opKind = iota // a fresh writable tmpfs
	opHide                  // an empty folder; allowed children are bound back into it
	opDeny                  // an empty, unreadable folder, or an unreadable file
	opKeep                  // an allowed path bound back into a hidden folder
)

type mountOp struct {
	kind opKind
	path string
}

// mountPlan orders the mounts that build the command's view of the file system. Shallow paths
// come first, so a folder is covered before its allowed children are bound back and a deny
// deeper down covers those again. Deny wins over every allowed path, a private folder starts
// empty whatever is allowed in it, and a path already hidden by a shallower mount, or already
// visible through an allowed one, needs no mount of its own.
func mountPlan(private, hide, deny, keep []string) []mountOp {
	var ops []mountOp
	add := func(kind opKind, paths []string) {
		for _, path := range paths {
			op := mountOp{kind, path}
			if path != "" && !slices.Contains(ops, op) {
				ops = append(ops, op)
			}
		}
	}
	add(opPrivate, private)
	add(opHide, hide)
	add(opDeny, deny)
	for _, path := range keep {
		if slices.ContainsFunc(deny, func(d string) bool { return within(d, path) }) || slices.Contains(private, path) {
			continue
		}
		add(opKeep, []string{path})
	}
	slices.SortStableFunc(ops, func(a, b mountOp) int {
		if da, db := depth(a.path), depth(b.path); da != db {
			return da - db
		}
		if a.kind != b.kind {
			return int(a.kind) - int(b.kind)
		}
		return strings.Compare(a.path, b.path)
	})
	var plan []mountOp
	for _, op := range ops {
		var cover *mountOp
		for i := range plan {
			if within(plan[i].path, op.path) && (cover == nil || depth(plan[i].path) >= depth(cover.path)) {
				cover = &plan[i]
			}
		}
		hidden := cover != nil && cover.kind != opKeep
		if op.kind == opKeep && !hidden || op.kind != opKeep && hidden {
			continue
		}
		plan = append(plan, op)
	}
	return plan
}

// within reports whether path is parent or lies below it.
func within(parent, path string) bool {
	return path == parent || parent == "/" || strings.HasPrefix(path, parent+"/")
}

func depth(path string) int {
	if path == "/" {
		return 0
	}
	return strings.Count(path, "/")
}
