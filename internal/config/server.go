package config

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultServerDir is where `install.sh --server` puts the host on a box: a host sync goes there,
// and an app's short `server` names a folder under its apps directory.
const DefaultServerDir = "/srv/dboss"

// RemoteSafe limits what goes into an ssh command line, which the remote shell parses.
var RemoteSafe = regexp.MustCompile(`^[A-Za-z0-9._/@+=:,-]+$`)

// SplitTarget reads rsync's host:path form. The path must be absolute, and both parts are kept
// to characters the remote shell passes through untouched.
func SplitTarget(target string) (string, string, error) {
	host, remotePath, ok := strings.Cut(target, ":")
	if !ok || host == "" || strings.HasPrefix(host, "-") {
		return "", "", fmt.Errorf("target %q is not user@host:/path", target)
	}
	if !path.IsAbs(remotePath) {
		return "", "", fmt.Errorf("remote path %q must be absolute", remotePath)
	}
	remotePath = path.Clean(remotePath)
	if remotePath == "/" || !RemoteSafe.MatchString(host) || !RemoteSafe.MatchString(remotePath) {
		return "", "", fmt.Errorf("target %q has characters the remote shell would read", target)
	}
	return host, remotePath, nil
}

// ServerTarget is the deploy target of the app checked out in a folder named folder: `server` as
// written when it names a path, else that folder under the default host's apps directory. Empty
// when the app names no server.
func (a App) ServerTarget(folder string) string {
	if a.Server == "" || strings.Contains(a.Server, ":") {
		return a.Server
	}
	return a.Server + ":" + path.Join(DefaultServerDir, "apps", folder)
}

func validateServer(server string) error {
	if server == "" {
		return nil
	}
	if _, _, err := SplitTarget(App{Server: server}.ServerTarget("app")); err != nil {
		return keyErr("server", "%v", err)
	}
	return nil
}

// Deploy is the app's half of `dboss deploy sync`, read on the operator's machine only. Build runs
// in the app folder before anything ships; Dir, when set, ships every file under that folder
// instead of the files git tracks, so a build can produce the release itself.
type Deploy struct {
	Build string `yaml:"build" json:"build,omitempty"`
	Dir   string `yaml:"dir" json:"dir,omitempty"`
}

func validateDeploy(deploy Deploy) error {
	if deploy.Dir == "" {
		return nil
	}
	// "." would ship the whole folder ignoring git, which is exactly what plain sync guards against
	if !filepath.IsLocal(deploy.Dir) || filepath.Clean(deploy.Dir) == "." {
		return keyErr("deploy.dir", "%q must be a folder inside the app folder", deploy.Dir)
	}
	return nil
}
