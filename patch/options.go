package patch

import (
	"context"
	"log/slog"
)

const defaultTagetBranch = "pin-gha"

const defaultCommitMsg = "chore(sec|gha): uses pinned versions of github actions"

type PRDetails struct {
	URL        string
	Repository string
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
	// TrustedOrgs is the list of GitHub organisations whose actions are left
	// untouched.
	TrustedOrgs []string
	// CommitMsg is the commit message used when committing the pinned actions.
	// Defaults to "chore(security): uses pinned versions of actions" when empty.
	CommitMsg string
	// LogHandler is a function that retrieves a slog.Handler from the context.
	LogHandler slog.Handler
	// OnPRCreated is a callback that will be called when a PR is created.
	OnPRCreated func(context.Context, PRDetails)
}

func (o PatchOptions) withDefaults() PatchOptions {
	if o.TargetBranch == "" {
		o.TargetBranch = defaultTagetBranch
	}
	if o.CommitMsg == "" {
		o.CommitMsg = defaultCommitMsg
	}

	if o.PRBody == "" {
		o.PRBody = o.CommitMsg
	}

	if o.LogHandler == nil {
		o.LogHandler = slog.Default().Handler()
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
