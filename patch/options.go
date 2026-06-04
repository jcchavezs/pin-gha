package patch

import (
	"context"
	"log/slog"
)

const defaultTagetBranch = "pin-gha"

const defaultCommitMsg = "chore(sec|gha): uses pinned versions of github actions"

type PRDetails struct {
	URL        string
}

type SkippedReason int

const (
	SkippedReasonUnknown		 SkippedReason = iota
	SkippedReasonEmptyRepository
	SkippedReasonAlreadyPinned
)

func (r SkippedReason) String() string {
	switch r {
	case SkippedReasonEmptyRepository:
		return "empty repository"
	case SkippedReasonAlreadyPinned:
		return "already pinned actions"
	default:
		return "unknown reason"
	}
}

// PatchOptions controls the behaviour of the patching process.
type PatchOptions struct {
	// TargetBranch is the branch name used when creating/updating PRs.
	// Defaults to "pin-actions" when empty.
	TargetBranch string
	// PRBody is the content of the PR body.
	PRBody string
	// PRAsDraft indicates whether the created PR should be a draft.
	PRAsDraft bool
	// NeedsFork indicates whether to fork the repository when creating the PR.
	NeedsFork      bool
	// TrustedOrgs is the list of GitHub organisations whose actions are left
	// untouched.
	TrustedOrgs []string
	// CommitMsg is the commit message used when committing the pinned actions.
	// Defaults to "chore(security): uses pinned versions of actions" when empty.
	CommitMsg string
	// LogHandler is a function that retrieves a slog.Handler from the context.
	LogHandler slog.Handler

	// OnRepositoryErr is a callback that will be called when an error occurs while processing a repository.
	OnRepositoryErr func(context.Context, string, error) error
	
	// OnRepositorySkipped is a callback that will be called when a repository is skipped, with the reason for skipping.
	OnRepositorySkipped func(context.Context, string, SkippedReason)

	// OnPRCreated is a callback that will be called when a PR is created.
	OnPRCreated func(context.Context, string, PRDetails)
}

const defaultPRBody = `This pull request updates the GitHub Actions workflow files to use pinned commit SHAs for all third-party actions, improving security and reproducibility.`

func (o PatchOptions) withDefaults() PatchOptions {
	if o.TargetBranch == "" {
		o.TargetBranch = defaultTagetBranch
	}
	if o.CommitMsg == "" {
		o.CommitMsg = defaultCommitMsg
	}

	if o.PRBody == "" {
		o.PRBody = defaultPRBody
	}

	if o.LogHandler == nil {
		o.LogHandler = slog.Default().Handler()
	}

	if o.OnRepositoryErr == nil {
		o.OnRepositoryErr = func(_ context.Context, _ string, err error) error { return err }
	}

	if o.OnRepositorySkipped == nil {
		o.OnRepositorySkipped = func(context.Context, string, SkippedReason) {}
	}

	if o.OnPRCreated == nil {
		o.OnPRCreated = func(context.Context, string, PRDetails) {}
	}

	return o
}

type LocalPatchOptions struct {
	// TrustedOrgs is the list of GitHub organisations whose actions are left
	// untouched.
	TrustedOrgs []string

	LogHandler slog.Handler
}

func (lo LocalPatchOptions) withDefaults() LocalPatchOptions {
	if lo.LogHandler == nil {
		lo.LogHandler = slog.Default().Handler()
	}

	return lo
}
