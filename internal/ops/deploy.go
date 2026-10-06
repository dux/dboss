package ops

import (
	"context"

	"dboss/internal/fault"
	"dboss/internal/git"
)

// DeployPreview compares the checkout with its live upstream before an operator runs its hook.
func (s *Service) DeployPreview(ctx context.Context, name string) (git.Comparison, error) {
	snapshot, err := s.runtime.Snapshot(name)
	if err != nil {
		return git.Comparison{}, err
	}
	if !snapshot.GitConnected {
		return git.Comparison{}, fault.Invalidf("app has no connected Git repository")
	}
	return git.Compare(ctx, snapshot.Dir, s.runtime.HostConfig().Tokens.Github, snapshot.RequiredBranch)
}
