package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileName is an app's config and ServerFileName the host's; each has a server-only local file
// that, when present, replaces the committed one entirely. ConfigDir is the subfolder searched
// when the folder itself has neither, so an app can keep its dboss.yaml under config/ next to
// the rest of its configuration.
const (
	FileName            = "dboss.yaml"
	LocalFileName       = "dboss.local.yaml"
	ServerFileName      = "dboss-server.yaml"
	ServerLocalFileName = "dboss-server.local.yaml"
	ConfigDir           = "config"
)

// ErrNoConfig is FindInDir's answer for a folder with no config file in either place.
var ErrNoConfig = errors.New("no config file")

// pair is one role's committed file and its local override.
type pair struct{ base, local string }

var (
	appPair    = pair{FileName, LocalFileName}
	serverPair = pair{ServerFileName, ServerLocalFileName}
)

// pairOf names the role path's file name belongs to; ok is false for any other name.
func pairOf(path string) (p pair, ok bool) {
	switch filepath.Base(path) {
	case FileName, LocalFileName:
		return appPair, true
	case ServerFileName, ServerLocalFileName:
		return serverPair, true
	}
	return appPair, false
}

// FindInDir returns the app config to use for dir: dboss.local.yaml, else dboss.yaml, looked up
// in dir and then in dir/config. Files in both places are an error, so only one can be live.
func FindInDir(dir string) (string, error) { return findIn(dir, appPair) }

// FindServerInDir is FindInDir for the host config, dboss-server(.local).yaml.
func FindServerInDir(dir string) (string, error) { return findIn(dir, serverPair) }

func findIn(dir string, p pair) (string, error) {
	nestedDir := filepath.Join(dir, ConfigDir)
	root, nested := p.find(dir), p.find(nestedDir)
	switch {
	case root != "" && nested != "":
		return "", fmt.Errorf("both %s and %s exist; keep one", root, nested)
	case root != "":
		return root, nil
	case nested != "":
		return nested, nil
	}
	return "", fmt.Errorf("%w: no %s in %s or %s", ErrNoConfig, p.base, dir, nestedDir)
}

// Live is the file that holds path's config right now: a local file created next to its base
// after start replaces it, and a removed one hands back to the base. A file with any other name
// is used as given.
func Live(path string) string {
	p, ok := pairOf(path)
	if !ok {
		return path
	}
	if found := p.find(filepath.Dir(path)); found != "" {
		return found
	}
	return path
}

// LocalFor is the local override next to the config file at path: dboss-server.local.yaml for
// the host file, dboss.local.yaml for an app file and any other name.
func LocalFor(path string) string {
	p, _ := pairOf(path)
	return filepath.Join(filepath.Dir(path), p.local)
}

func (p pair) find(dir string) string {
	for _, name := range []string{p.local, p.base} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// BaseDir is the folder a config file belongs to, which relative paths resolve against: the
// parent of config/ when that is where FindInDir found it, else the file's own folder.
func BaseDir(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != ConfigDir {
		return dir
	}
	parent := filepath.Dir(dir)
	p, _ := pairOf(path)
	if found, err := findIn(parent, p); err == nil && found == path {
		return parent
	}
	return dir
}
