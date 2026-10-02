package apps

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// gitSource is the branch an app runs and that branch's page on its git host. A git checkout
// answers from .git; connected reports its tracking remote (or origin) even without a branch page.
// A packed release (lux-deploy) has none, so the deploy writes GIT_BRANCH and
// GIT_REPO to its .env instead. A detached HEAD has no branch, and a remote that is not an
// http(s) or ssh URL has no page, so either may come back empty.
func gitSource(dir string, fileEnv map[string]string) (branch, branchURL string, connected bool) {
	repo := ""
	if gitDir, ok := findGitDir(dir); ok {
		branch = headBranch(gitDir)
		repo = remoteURL(gitDir, branch)
	} else {
		branch = strings.TrimSpace(fileEnv["GIT_BRANCH"])
		repo = strings.TrimSpace(fileEnv["GIT_REPO"])
	}
	web := repoWebURL(repo)
	connected = repo != ""
	if branch == "" || web == "" {
		return branch, "", connected
	}
	segments := strings.Split(branch, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return branch, web + "/tree/" + strings.Join(segments, "/"), connected
}

// findGitDir resolves dir/.git, following the "gitdir:" file a worktree or submodule has.
func findGitDir(dir string) (string, bool) {
	gitDir := filepath.Join(dir, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return gitDir, true
	}
	data, err := os.ReadFile(gitDir)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return target, true
}

func headBranch(gitDir string) string {
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	if branch, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/"); ok {
		return branch
	}
	return ""
}

// remoteURL reads the tracking remote's URL, or origin, from the config; a worktree keeps it in the
// common dir its commondir file names.
func remoteURL(gitDir, branch string) string {
	configDir := gitDir
	if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		configDir = strings.TrimSpace(string(common))
		if !filepath.IsAbs(configDir) {
			configDir = filepath.Join(gitDir, configDir)
		}
	}
	file, err := os.Open(filepath.Join(configDir, "config"))
	if err != nil {
		return ""
	}
	defer file.Close()
	section, tracking := "", ""
	remotes := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if section == `[branch "`+branch+`"]` && key == "remote" {
				tracking = value
			}
			if name, ok := strings.CutPrefix(section, `[remote "`); ok && key == "url" {
				if name, ok = strings.CutSuffix(name, `"]`); ok {
					remotes[name] = value
				}
			}
		}
	}
	if remote := remotes[tracking]; remote != "" {
		return remote
	}
	return remotes["origin"]
}

// repoWebURL turns a clone URL into the repository's https page: scp-style and ssh:// remotes
// map onto the same host, and credentials, ports and a trailing .git are dropped.
func repoWebURL(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if !strings.Contains(remote, "://") {
		// scp-style: git@github.com:owner/repo.git
		userHost, path, ok := strings.Cut(remote, ":")
		if !ok {
			return ""
		}
		_, host, found := strings.Cut(userHost, "@")
		if !found {
			host = userHost
		}
		remote = "ssh://" + host + "/" + strings.TrimPrefix(path, "/")
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	switch parsed.Scheme {
	case "https", "http", "ssh", "git":
	default:
		return ""
	}
	path := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if path == "" {
		return ""
	}
	return "https://" + parsed.Hostname() + "/" + path
}
