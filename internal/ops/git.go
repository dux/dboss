package ops

import (
	"context"
	"time"

	"dboss/internal/git"
)

// GitCommitResult is the commit git-commit made.
type GitCommitResult struct {
	Hash string `json:"hash"`
}

// GitPushResult is git's own report of a push.
type GitPushResult struct {
	Output string `json:"output"`
}

// GitResetResult names the stash that holds what git-reset cleared.
type GitResetResult struct {
	Stash string `json:"stash"`
}

func (s *Service) gitCommit(name, message string) (GitCommitResult, error) {
	snapshot, err := s.runtime.Snapshot(name)
	if err != nil {
		return GitCommitResult{}, err
	}
	hash, err := git.CommitAll(context.Background(), snapshot.Dir, message, gitAuthor(snapshot.Hosts))
	return GitCommitResult{Hash: hash}, err
}

func (s *Service) gitPush(name string) (GitPushResult, error) {
	snapshot, err := s.runtime.Snapshot(name)
	if err != nil {
		return GitPushResult{}, err
	}
	output, err := git.Push(context.Background(), snapshot.Dir, s.runtime.HostConfig().Tokens.Github, snapshot.RequiredBranch)
	return GitPushResult{Output: output}, err
}

func (s *Service) gitReset(name string) (GitResetResult, error) {
	snapshot, err := s.runtime.Snapshot(name)
	if err != nil {
		return GitResetResult{}, err
	}
	stash, err := git.Reset(context.Background(), snapshot.Dir, "dboss: reset "+time.Now().Format(time.DateTime), gitAuthor(snapshot.Hosts))
	return GitResetResult{Stash: stash}, err
}

// gitAuthor is the identity a checkout with none of its own commits as.
func gitAuthor(hosts []string) string {
	domain := "localhost"
	for _, host := range hosts {
		if host != "" && host[0] != '.' && host[0] != '*' {
			domain = host
			break
		}
	}
	return "dboss <dboss@" + domain + ">"
}
